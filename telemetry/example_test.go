package telemetry_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
)

// Logs, traces and metrics from one setup call, tied together by the trace ID.
func Example() {
	ctx := context.Background()
	tel, err := telemetry.New(ctx, telemetry.Config{
		Endpoint:       "http://collector.internal:4318", // or Grafana Cloud's OTLP gateway
		Headers:        map[string]string{"Authorization": os.Getenv("OTLP_AUTHORIZATION")},
		ServiceName:    "api",
		ServiceVersion: "1.4.2",
		Environment:    "prod",
		RuntimeMetrics: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tel.Shutdown(ctx) // flushes logs, spans and metrics
	}()

	log, err := logger.New(logger.Config{
		Extra:        []slog.Handler{tel.LogHandler()},
		ContextAttrs: telemetry.TraceAttrs,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	orders, _ := tel.Meter("orders").Int64Counter("orders.placed")

	mux := http.NewServeMux()
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tel.Tracer("orders").Start(r.Context(), "place order")
		defer span.End()
		orders.Add(ctx, 1)
		log.InfoContext(ctx, "order placed", "order", 42) // carries this span's trace ID
	})
	// A span and request metrics for every request; the trace continues from incoming headers.
	http.ListenAndServe(":8080", tel.HTTPHandler(mux, "api"))
}
