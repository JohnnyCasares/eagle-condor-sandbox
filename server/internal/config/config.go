// Package config holds everything the server needs to know about its
// surroundings. Resolved once at startup and treated as immutable after.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

type Config struct {
	// Addr is the listen address, e.g. "127.0.0.1:8080". Bind to localhost
	// unless something else terminates TLS in front.
	Addr string

	// AutomationDir is the Playwright project root — the directory holding
	// workflows.json, playwright.config.js and node_modules.
	AutomationDir string

	// RunsDir is the parent of every per-run directory. One subdirectory per
	// run; nothing else writes here.
	RunsDir string

	// ReceiptsDir is the curated attachment library. Read-only as far as this
	// server and the tests are concerned — mount it that way if you can.
	ReceiptsDir string

	// NodeBin is the node executable used to launch Playwright. Not npx: that
	// adds a process layer between us and the thing we need to kill, and can
	// hit the network.
	NodeBin string

	// MaxConcurrent is how many runs execute at once. Every run is a separate
	// Playwright process which itself opens several browser contexts, so this
	// is not the real parallelism figure — see PS_MAX_PARALLEL_USERS.
	MaxConcurrent int

	// QueueDepth bounds how many runs may wait. Beyond this, submissions are
	// rejected with 429 rather than queued indefinitely.
	QueueDepth int

	// Token is the bearer token required by every endpoint except /v1/health.
	Token string

	// MaxUploadBytes caps a single uploaded CSV.
	MaxUploadBytes int64

	// ShutdownGrace is how long in-flight HTTP requests get on shutdown.
	ShutdownGrace time.Duration
}

// PlaywrightCLI is the script we hand to node.
func (c Config) PlaywrightCLI() string {
	return filepath.Join(c.AutomationDir, "node_modules", "@playwright", "test", "cli.js")
}

// ManifestPath is where the workflow catalog lives.
func (c Config) ManifestPath() string {
	return filepath.Join(c.AutomationDir, "workflows.json")
}

// RunDir is the private directory for one run.
func (c Config) RunDir(id string) string {
	return filepath.Join(c.RunsDir, id)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number", key, raw)
	}
	return n, nil
}

// Load builds a Config from environment variables, applying defaults, and
// verifies that the paths it depends on actually exist. Failing here is much
// cheaper than failing on the first run.
func Load() (Config, error) {
	automation := env("PSTAD_AUTOMATION_DIR", defaultAutomationDir())
	abs, err := filepath.Abs(automation)
	if err != nil {
		return Config{}, fmt.Errorf("resolving PSTAD_AUTOMATION_DIR: %w", err)
	}
	automation = abs

	maxConcurrent, err := envInt("PSTAD_MAX_CONCURRENT", 2)
	if err != nil {
		return Config{}, err
	}
	queueDepth, err := envInt("PSTAD_QUEUE_DEPTH", 32)
	if err != nil {
		return Config{}, err
	}
	maxUpload, err := envInt("PSTAD_MAX_UPLOAD_BYTES", 10<<20)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Addr:           env("PSTAD_ADDR", "127.0.0.1:8080"),
		AutomationDir:  automation,
		RunsDir:        env("PSTAD_RUNS_DIR", filepath.Join(automation, ".runs")),
		ReceiptsDir:    env("PSTAD_RECEIPTS_DIR", filepath.Join(automation, "data", "receipts")),
		NodeBin:        env("PSTAD_NODE_BIN", "node"),
		MaxConcurrent:  maxConcurrent,
		QueueDepth:     queueDepth,
		Token:          os.Getenv("PSTAD_TOKEN"),
		MaxUploadBytes: int64(maxUpload),
		ShutdownGrace:  15 * time.Second,
	}

	return cfg, cfg.validate()
}

func (c Config) validate() error {
	var errs []error

	if c.MaxConcurrent < 1 {
		errs = append(errs, errors.New("PSTAD_MAX_CONCURRENT must be at least 1"))
	}
	if c.QueueDepth < 1 {
		errs = append(errs, errors.New("PSTAD_QUEUE_DEPTH must be at least 1"))
	}
	if c.Token == "" {
		errs = append(errs, errors.New(
			"PSTAD_TOKEN is required — every endpoint but /v1/health needs a bearer token"))
	}

	if fi, err := os.Stat(c.AutomationDir); err != nil || !fi.IsDir() {
		errs = append(errs, fmt.Errorf("automation dir not found: %s", c.AutomationDir))
	}
	if _, err := os.Stat(c.ManifestPath()); err != nil {
		errs = append(errs, fmt.Errorf("workflow manifest not found: %s", c.ManifestPath()))
	}
	if _, err := os.Stat(c.PlaywrightCLI()); err != nil {
		errs = append(errs, fmt.Errorf(
			"playwright CLI not found at %s — run `npm install` in %s",
			c.PlaywrightCLI(), c.AutomationDir))
	}

	if err := os.MkdirAll(c.RunsDir, 0o750); err != nil {
		errs = append(errs, fmt.Errorf("cannot create runs dir %s: %w", c.RunsDir, err))
	}

	return errors.Join(errs...)
}

// defaultAutomationDir guesses ../automation relative to the running binary,
// which is right when the tree is laid out as in the repo.
func defaultAutomationDir() string {
	if exe, err := os.Executable(); err == nil {
		if d := filepath.Join(filepath.Dir(exe), "..", "automation"); dirExists(d) {
			return d
		}
	}
	if wd, err := os.Getwd(); err == nil {
		for _, cand := range []string{
			filepath.Join(wd, "automation"),
			filepath.Join(wd, "..", "automation"),
		} {
			if dirExists(cand) {
				return cand
			}
		}
	}
	return "automation"
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// IsWindows is used where behaviour genuinely differs (process trees, env).
var IsWindows = runtime.GOOS == "windows"
