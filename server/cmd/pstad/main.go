// Command pstad is a small HTTP API that queues and runs Playwright workflows
// on behalf of clients, with per-run isolation.
//
// It owns process supervision and per-run isolation. It does not know what Excel
// is — the contract is CSV in, artifacts out — which is what keeps template
// generation entirely on the client side.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"eagle-condor-sandbox/internal/catalog"
	"eagle-condor-sandbox/internal/config"
	"eagle-condor-sandbox/internal/httpapi"
	"eagle-condor-sandbox/internal/receipts"
	"eagle-condor-sandbox/internal/run"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)"
var version = "dev"

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintln(os.Stderr, "pstad: "+err.Error())
		os.Exit(1)
	}
}

func realMain() error {
	checkOnly := flag.Bool("check", false,
		"validate configuration and the workflow manifest, then exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("pstad", version)
		return nil
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}

	cat, err := catalog.Load(cfg.ManifestPath(), cfg.AutomationDir)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}

	if *checkOnly {
		fmt.Printf("config OK\n")
		fmt.Printf("  automation:  %s\n", cfg.AutomationDir)
		fmt.Printf("  runs:        %s\n", cfg.RunsDir)
		fmt.Printf("  receipts:    %s\n", cfg.ReceiptsDir)
		fmt.Printf("  concurrency: %d (queue depth %d)\n", cfg.MaxConcurrent, cfg.QueueDepth)
		fmt.Printf("manifest OK — %d workflows, %d schemas\n",
			len(cat.Workflows), len(cat.CSVSchemas))
		for _, wf := range cat.Workflows {
			fmt.Printf("  %-38s %s\n", wf.ID, wf.Spec)
		}
		return nil
	}

	store := run.NewStore(cfg.RunsDir)
	// Before accepting anything: adopt what is on disk and clean up any process
	// tree a previous instance left behind.
	if err := store.Recover(log); err != nil {
		return fmt.Errorf("recovering runs: %w", err)
	}

	rt := run.Runtime{
		AutomationDir: cfg.AutomationDir,
		PlaywrightCLI: cfg.PlaywrightCLI(),
		NodeBin:       cfg.NodeBin,
		ReceiptsDir:   cfg.ReceiptsDir,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	queue := run.NewQueue(store, rt, cfg.MaxConcurrent, cfg.QueueDepth, log)
	queue.Start(ctx)

	api := httpapi.New(cfg, cat, store, queue, receipts.New(cfg.ReceiptsDir), log, version)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: artifact downloads and log tailing are unbounded.
		// Short endpoints are wrapped in http.TimeoutHandler individually.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "version", version,
			"workflows", len(cat.Workflows), "concurrency", cfg.MaxConcurrent)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown was not graceful", "err", err)
	}

	// Workers observe the cancelled context, kill their process trees, and
	// record the outcome — so a stop does not leave orphaned browsers.
	log.Info("waiting for in-flight runs to stop")
	queue.Stop()
	log.Info("stopped")
	return nil
}
