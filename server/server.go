// Package server runs an HTTP server the way a service should: sensible timeouts,
// a listener you can inspect, graceful shutdown on cancellation or SIGINT/SIGTERM,
// and resources (databases, clients) closed in reverse order after the last
// request has finished.
//
//	srv, err := server.New(server.Config{Addr: ":8080", Handler: handler, Logger: log})
//	if err != nil {
//		return err
//	}
//	srv.OnShutdown("postgres", func(ctx context.Context) error { pool.Close(); return nil })
//	srv.OnShutdown("telemetry", tel.Shutdown)          // registered last, closed first
//	return srv.RunUntilSignal()                        // main: returns after a clean shutdown
//
// A clean shutdown returns nil. A shutdown that hits ShutdownTimeout, a failed
// closer or a server that could not start returns an error naming what failed.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Defaults applied when the matching Config field is zero. They protect against
// slow clients; raise WriteTimeout (or use http.ResponseController per request)
// for streaming endpoints.
const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	DefaultWriteTimeout      = 60 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultMaxHeaderBytes    = 1 << 20
	DefaultShutdownTimeout   = 30 * time.Second
)

// Config describes the server.
type Config struct {
	// Addr is the listen address, such as ":8080" or "127.0.0.1:0" (a free port).
	// Required.
	Addr string
	// Handler serves the requests. Required.
	Handler http.Handler
	// TLSConfig, if set, makes the server speak TLS. It must provide a certificate
	// (Certificates or GetCertificate). Set ClientAuth and ClientCAs on it to require
	// client certificates (mutual TLS). MinVersion defaults to TLS 1.2 if unset.
	TLSConfig *tls.Config
	// ReadHeaderTimeout, ReadTimeout, WriteTimeout and IdleTimeout bound each
	// connection. MaxHeaderBytes limits request headers. Zero uses the defaults.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	// ShutdownTimeout bounds how long in-flight requests get to finish, and
	// separately how long each closer gets. Default DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// Logger receives lifecycle messages (listening, shutting down, failures).
	// Default: slog.Default().
	Logger *slog.Logger
}

// Validate reports problems with the configuration without listening.
func (c Config) Validate() error {
	switch {
	case c.Addr == "":
		return errors.New("server: Addr is required")
	case c.Handler == nil:
		return errors.New("server: Handler is required")
	case c.ReadHeaderTimeout < 0 || c.ReadTimeout < 0 || c.WriteTimeout < 0 || c.IdleTimeout < 0 || c.ShutdownTimeout < 0 || c.MaxHeaderBytes < 0:
		return errors.New("server: timeouts and MaxHeaderBytes must not be negative")
	}
	if c.TLSConfig != nil && len(c.TLSConfig.Certificates) == 0 && c.TLSConfig.GetCertificate == nil {
		return errors.New("server: TLSConfig needs Certificates or GetCertificate")
	}
	return nil
}

type closer struct {
	name string
	fn   func(context.Context) error
}

// Server is an HTTP server with a managed lifecycle.
type Server struct {
	cfg  Config
	http *http.Server
	log  *slog.Logger

	mu      sync.Mutex
	closers []closer
	ln      net.Listener
	ready   chan struct{}
	started bool
}

// New validates cfg and returns a Server. It does not listen yet.
func New(cfg Config) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.MaxHeaderBytes == 0 {
		cfg.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, log: cfg.Logger, ready: make(chan struct{})}
	s.http = &http.Server{
		Handler:           cfg.Handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(cfg.Logger.Handler(), slog.LevelWarn),
	}
	if cfg.TLSConfig != nil {
		tc := cfg.TLSConfig.Clone()
		if tc.MinVersion == 0 {
			tc.MinVersion = tls.VersionTLS12
		}
		s.http.TLSConfig = tc
	}
	return s, nil
}

// OnShutdown registers a function that releases a resource (close a database pool,
// flush telemetry). After the HTTP server has stopped, the functions run in reverse
// order of registration, so register dependencies first. Each gets a context
// limited by ShutdownTimeout. name appears in logs and errors.
func (s *Server) OnShutdown(name string, fn func(ctx context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closers = append(s.closers, closer{name, fn})
}

// Ready returns a channel that is closed once the server is listening, so tests
// and supervisors can wait for it. If Run fails before listening it is never closed.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addr returns the address the server is listening on (useful with port 0). It is
// nil until Ready is closed.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Run listens and serves until ctx is cancelled or the server fails, then shuts down
// gracefully: it stops accepting connections, gives in-flight requests up to
// ShutdownTimeout to finish (then closes them), and runs the OnShutdown functions in
// reverse order. It returns nil after a clean shutdown. A Server can be run once.
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("server: Run was already called")
	}
	s.started = true
	s.mu.Unlock()

	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return errors.Join(fmt.Errorf("server: listen: %w", err), s.runClosers())
	}
	if s.http.TLSConfig != nil {
		ln = tls.NewListener(ln, s.http.TLSConfig)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	close(s.ready)
	s.log.InfoContext(ctx, "server listening", "addr", ln.Addr().String(), "tls", s.http.TLSConfig != nil)

	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(ln) }()

	var runErr error
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("server: serve: %w", err)
		}
	case <-ctx.Done():
		s.log.Info("server shutting down", "timeout", s.cfg.ShutdownTimeout.String())
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
		if err := s.http.Shutdown(sctx); err != nil {
			s.http.Close() // out of patience: cut the remaining connections
			runErr = fmt.Errorf("server: graceful shutdown did not finish within %s: %w", s.cfg.ShutdownTimeout, err)
		}
		cancel()
		<-serveErr
	}
	return errors.Join(runErr, s.runClosers())
}

func (s *Server) runClosers() error {
	s.mu.Lock()
	closers := append([]closer(nil), s.closers...)
	s.closers = nil
	s.mu.Unlock()

	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		c := closers[i]
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		err := safely(func() error { return c.fn(ctx) })
		cancel()
		if err != nil {
			s.log.Error("shutdown step failed", "step", c.name)
			errs = append(errs, fmt.Errorf("server: closing %s: %w", c.name, err))
		} else {
			s.log.Info("shutdown step done", "step", c.name)
		}
	}
	return errors.Join(errs...)
}

// safely runs fn and turns a panic into an error so one bad closer cannot stop the others.
func safely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

// RunUntilSignal is Run with a context that is cancelled by SIGINT or SIGTERM
// (the signals a terminal and an orchestrator send to stop a process). A second
// signal during shutdown stops the wait and exits the process immediately.
func (s *Server) RunUntilSignal() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Restore default signal behavior, so a second Ctrl-C kills a stuck shutdown.
		stop()
	}()
	return s.Run(ctx)
}

// Health returns a handler for a readiness or liveness endpoint. It runs every
// check (each with the request's context) and answers 200 {"status":"ok"} if all
// pass, or 503 {"status":"unavailable","failing":["name",...]} listing the names of
// the failing checks. Error details are never sent to the client, because they can
// reveal addresses and credentials; log them in the check if you need them.
func Health(checks map[string]func(ctx context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var failing []string
		for name, check := range checks {
			if err := safely(func() error { return check(r.Context()) }); err != nil {
				failing = append(failing, name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if len(failing) == 0 {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		sort.Strings(failing)
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"status": "unavailable", "failing": failing})
	})
}
