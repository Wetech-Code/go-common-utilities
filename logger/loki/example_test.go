package loki_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/logger/loki"
)

// Log to the console and to Grafana Loki at the same time.
func Example() {
	h, err := loki.New(loki.Config{
		URL:    "http://loki.internal:3100",
		Labels: map[string]string{"service": "api", "env": "prod"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	// Flush what is queued before exiting.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.Close(ctx)
	}()

	log, err := logger.New(logger.Config{Extra: []slog.Handler{h}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	log.Info("order placed", "order", 42)
}
