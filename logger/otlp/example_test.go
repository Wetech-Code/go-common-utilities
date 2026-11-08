package otlp_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/logger/otlp"
)

// Log to the console and, over OpenTelemetry, to Grafana Cloud, Loki, a
// Collector or any other OTLP/HTTP receiver. Console lines also carry the trace
// and span IDs when the context has a span.
func Example() {
	ctx := context.Background()
	h, err := otlp.New(ctx, otlp.Config{
		Endpoint:    "http://collector.internal:4318",
		Headers:     map[string]string{"Authorization": os.Getenv("OTLP_AUTHORIZATION")},
		ServiceName: "api",
		Environment: "prod",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.Shutdown(ctx)
	}()

	log, err := logger.New(logger.Config{Extra: []slog.Handler{h}, ContextAttrs: otlp.TraceAttrs})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	log.InfoContext(ctx, "order placed", "order", 42)
}
