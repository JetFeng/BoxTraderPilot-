// Package web serves the BoxPilot read-only scanner as a local web
// application: a JSON API plus a single-page UI.
//
// The design keeps the HTTP layer thin. A background poller runs the
// watch engine, which owns the scan loop and the alert state machine;
// HTTP requests only ever read the engine's snapshot. That matters for
// more than latency: every scan costs one full option-chain request per
// expiry, so letting page refreshes trigger scans would exhaust the
// upstream rate limit immediately.
//
// Everything here remains strictly read-only: there is no code path
// that can place, modify or cancel an order.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"boxpilot/internal/app"
	"boxpilot/internal/config"
	"boxpilot/internal/logx"
	"boxpilot/internal/scan"
	"boxpilot/internal/watch"
)

// DefaultAddr binds to loopback only. The service holds Schwab
// credentials, so it must not be reachable from the network unless an
// operator deliberately changes this and adds authentication.
const DefaultAddr = "127.0.0.1:8080"

// Options configures a Server.
type Options struct {
	// Addr is the listen address. Defaults to DefaultAddr (loopback).
	Addr string
	// ConfigPath is the YAML configuration file.
	ConfigPath string
	// FixturePath overrides the configured broker with an offline
	// fixture — used by the tests and by demo runs.
	FixturePath string
	// Logger receives the operational log; secrets are masked.
	Logger *logx.Logger
	// Diag receives human-readable diagnostics.
	Diag io.Writer
	// Now is injectable for tests.
	Now func() time.Time
}

// Server is the web application.
type Server struct {
	opts Options

	mu     sync.RWMutex
	cfg    *config.Config
	params scan.Params
	rules  watch.Rules
	bench  float64

	engine *watch.Engine

	httpSrv *http.Server
}

// New builds a server: it loads the configuration, resolves the broker
// and the alert channels, and wires up the watch engine.
func New(opts Options) (*Server, error) {
	if opts.Addr == "" {
		opts.Addr = DefaultAddr
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Diag == nil {
		opts.Diag = os.Stderr
	}
	if opts.Logger == nil {
		opts.Logger = logx.New(opts.Diag, "boxpilotd ", nil)
	}
	if !isLoopback(opts.Addr) {
		fmt.Fprintf(opts.Diag,
			"warning: listening on %s, which is reachable from the network. "+
				"This service holds Schwab credentials and has no authentication yet — "+
				"put it behind a reverse proxy or a private network.\n", opts.Addr)
	}

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(opts.Now()); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	if err := cfg.ValidateWatch(); err != nil {
		return nil, fmt.Errorf("invalid watch config: %w", err)
	}

	b, secrets, err := app.BuildBroker(cfg, opts.FixturePath)
	if err != nil {
		return nil, err
	}
	if len(secrets) > 0 {
		opts.Logger.SetSecrets(secrets)
	}

	params, err := app.BuildParams(cfg)
	if err != nil {
		return nil, err
	}
	wopts, err := cfg.ParseWatch()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	alerter, closer, err := app.BuildAlerters(cfg, ctx, opts.Logger, opts.Diag)
	if err != nil {
		return nil, err
	}
	if closer != nil {
		// The WhatsApp session is released when the process exits; the
		// session store on disk keeps the pairing.
		defer func() { _ = closer() }()
	}

	s := &Server{
		opts:   opts,
		cfg:    cfg,
		params: params,
		bench:  cfg.Benchmark.Rate,
		rules:  watchRules(wopts),
	}
	s.engine = watch.NewEngine(b, params, cfg.Benchmark.Rate, s.rules, alerter,
		log.New(opts.Diag, "watch ", log.LstdFlags|log.LUTC), opts.Now)
	return s, nil
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assetsFS))))
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/scan", s.handleScan)
	mux.HandleFunc("GET /api/opportunities", s.handleOpportunities)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("POST /api/config", s.handleUpdateConfig)
	return mux
}

// Run starts the HTTP server and the background poller, and blocks
// until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	s.httpSrv = &http.Server{
		Addr:              s.opts.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.opts.Logger.Printf("listening on http://%s (Ctrl+C to stop)", s.opts.Addr)
		err := s.httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	go s.poll(ctx)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpSrv.Shutdown(shutdownCtx)
	}
}

// poll runs the scan loop. The interval is re-read every cycle so a
// configuration change takes effect without a restart. The first scan
// runs immediately so the UI is populated on startup.
func (s *Server) poll(ctx context.Context) {
	for {
		if err := s.engine.Round(ctx); err != nil && ctx.Err() == nil {
			s.opts.Logger.Printf("round failed: %v", err)
		}
		timer := time.NewTimer(s.currentInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// currentInterval returns the configured poll interval.
func (s *Server) currentInterval() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d, err := time.ParseDuration(s.cfg.Watch.Interval); err == nil && d > 0 {
		return d
	}
	return 60 * time.Second
}

// snapshot returns the engine's current view.
func (s *Server) snapshot() watch.Snapshot { return s.engine.Snapshot() }

// handleIndex serves the single-page UI.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	page, err := fs.ReadFile(assetsFS, "index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "page unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// handleHealth is a liveness probe that never touches the broker.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error body.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// isLoopback reports whether the address only accepts local
// connections.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// assetsFS holds the embedded front-end assets.
var assetsFS fs.FS

// init loads the embedded assets; it panics only if the binary was
// built without them, which is a build-time mistake.
func init() {
	sub, err := fs.Sub(embeddedAssets, "assets")
	if err != nil {
		panic("web: embedded assets unavailable: " + err.Error())
	}
	assetsFS = sub
}
