package server_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/Wetech-Code/go-common-utilities/middleware"
	"github.com/Wetech-Code/go-common-utilities/server"
)

// main starts everything, runs the server, and shuts down cleanly on Ctrl-C or SIGTERM,
// closing resources in reverse order after the last request has finished.
func Example() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	mux := http.NewServeMux()
	mux.Handle("GET /readyz", server.Health(map[string]func(context.Context) error{
		"db": func(ctx context.Context) error { return nil }, // e.g. pool.Ping(ctx)
	}))
	handler := middleware.Chain(mux,
		middleware.RequestID(),
		middleware.AccessLog(log),
		middleware.Recover(log),
		middleware.MaxBody(1<<20),
	)

	srv, err := server.New(server.Config{Addr: ":8080", Handler: handler, Logger: log})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	srv.OnShutdown("database", func(ctx context.Context) error { return nil })  // e.g. pool.Close()
	srv.OnShutdown("telemetry", func(ctx context.Context) error { return nil }) // e.g. tel.Shutdown(ctx): closed first
	if err := srv.RunUntilSignal(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
