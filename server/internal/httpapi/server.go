// Package httpapi exposes the run service over HTTP.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"eagle-condor-sandbox/internal/catalog"
	"eagle-condor-sandbox/internal/config"
	"eagle-condor-sandbox/internal/receipts"
	"eagle-condor-sandbox/internal/run"
)

type Server struct {
	cfg      config.Config
	catalog  *catalog.Catalog
	store    *run.Store
	queue    *run.Queue
	receipts *receipts.Library
	log      *slog.Logger
	version  string
}

func New(
	cfg config.Config,
	cat *catalog.Catalog,
	store *run.Store,
	queue *run.Queue,
	lib *receipts.Library,
	log *slog.Logger,
	version string,
) *Server {
	return &Server{cfg: cfg, catalog: cat, store: store, queue: queue,
		receipts: lib, log: log, version: version}
}

// Handler builds the router.
//
// Timeouts are applied per route rather than on http.Server: a blanket
// WriteTimeout would cut off artifact downloads and any future SSE stream
// mid-flight. Only the short, bounded endpoints get one.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated: liveness only, no catalog or run data.
	mux.Handle("GET /v1/health", http.HandlerFunc(s.health))

	short := func(h http.HandlerFunc) http.Handler {
		return http.TimeoutHandler(s.auth(h), 15*time.Second, `{"error":"request timed out"}`)
	}
	// No timeout wrapper: response size is unbounded.
	streaming := func(h http.HandlerFunc) http.Handler { return s.auth(h) }

	mux.Handle("GET /v1/workflows", short(s.listWorkflows))
	mux.Handle("GET /v1/workflows/{id}", short(s.getWorkflow))
	mux.Handle("GET /v1/receipts", short(s.listReceipts))
	mux.Handle("GET /v1/reference", streaming(s.getReference))

	mux.Handle("POST /v1/runs", streaming(s.createRun)) // multipart upload
	mux.Handle("GET /v1/runs", short(s.listRuns))
	mux.Handle("GET /v1/runs/{id}", short(s.getRun))
	mux.Handle("GET /v1/runs/{id}/log", short(s.getLog))
	mux.Handle("POST /v1/runs/{id}/cancel", short(s.cancelRun))
	mux.Handle("GET /v1/runs/{id}/results", short(s.getResults))
	mux.Handle("GET /v1/runs/{id}/artifacts", short(s.listArtifacts))
	mux.Handle("GET /v1/runs/{id}/artifacts/{artifactID}", streaming(s.getArtifact))

	return s.cors(s.recoverPanics(s.logRequests(mux)))
}

// cors lets a browser-based client call this API. It sits outside auth and
// routing: a preflight OPTIONS request carries no Authorization header and
// won't match a method-specific pattern like "POST /v1/runs", so it must be
// answered here or the real request never leaves the browser.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth requires a bearer token, compared in constant time.
func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.Token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pstad"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "dur", time.Since(start).Round(time.Millisecond))
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
		s.ResponseWriter.WriteHeader(code)
	}
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Header is already sent; nothing useful left to do but stop.
		return
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func queryInt(r *http.Request, key string, def int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

func queryInt64(r *http.Request, key string, def int64) int64 {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return def
	}
	return n
}
