//go:build integration

package logger_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/Wetech-Code/go-common-utilities/internal/grafanatest"
	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/logger/loki"
	"github.com/Wetech-Code/go-common-utilities/logger/otlp"
)

// Run against real Loki and Grafana (scripts/integration.sh starts both):
//
//	GCU_TEST_LOKI_URL=http://localhost:13100 GCU_TEST_GRAFANA_URL=http://localhost:13000 \
//	GCU_TEST_GRAFANA_PASSWORD=... go test -tags integration ./logger
//
// Records are sent with the Loki push handler and with OTLP (to Loki's own OTLP
// endpoint), then read back through Grafana's Loki data source.
func TestIntegrationLokiAndOTLPThroughGrafana(t *testing.T) {
	lokiURL := os.Getenv("GCU_TEST_LOKI_URL")
	grafanaURL := os.Getenv("GCU_TEST_GRAFANA_URL")
	password := os.Getenv("GCU_TEST_GRAFANA_PASSWORD")
	if lokiURL == "" || grafanaURL == "" {
		t.Skip("GCU_TEST_LOKI_URL or GCU_TEST_GRAFANA_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var b [5]byte
	rand.Read(b[:])
	id := fmt.Sprintf("%x", b)
	pushService, otlpService := "gcu-push-"+id, "gcu-otlp-"+id

	lh, err := loki.New(loki.Config{
		URL: lokiURL, Labels: map[string]string{"service": pushService, "env": "it"},
		BatchWait: 50 * time.Millisecond, ContextAttrs: otlp.TraceAttrs,
		OnError: func(err error) { t.Errorf("loki: %v", err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	oh, err := otlp.New(ctx, otlp.Config{
		Endpoint: lokiURL + "/otlp", ServiceName: otlpService, Environment: "it",
		BatchInterval: 100 * time.Millisecond, Gzip: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var console bytes.Buffer
	log, err := logger.New(logger.Config{Output: &console, Extra: []slog.Handler{lh, oh}, ContextAttrs: otlp.TraceAttrs})
	if err != nil {
		t.Fatal(err)
	}

	var tid trace.TraceID
	rand.Read(tid[:])
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: trace.SpanID{9, 8, 7, 6, 5, 4, 3, 2}, TraceFlags: trace.FlagsSampled})
	sctx := trace.ContextWithSpanContext(ctx, sc)

	msg := "order placed " + id
	log.InfoContext(sctx, msg, "order", 42, "password", "hunter2-"+id, slog.Group("customer", slog.String("tier", "gold")))
	log.ErrorContext(sctx, "payment failed "+id, "err", "card declined")
	log.Debug("below the level " + id)

	shutdownCtx, c2 := context.WithTimeout(ctx, 20*time.Second)
	defer c2()
	if err := lh.Close(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := oh.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if lh.Sent() != 2 || lh.Dropped() != 0 {
		t.Errorf("loki handler sent=%d dropped=%d", lh.Sent(), lh.Dropped())
	}
	if strings.Contains(console.String(), "hunter2") || !strings.Contains(console.String(), tid.String()) {
		t.Errorf("console output: %s", console.String())
	}

	g := grafanatest.New(t, ctx, grafanaURL, password)
	uid := g.AddDataSource(t, ctx, "gcu-"+id, "loki", "http://loki:3100")

	// 1. Records pushed with the Loki handler, found with Grafana's query path.
	lines := g.LokiQuery(t, ctx, uid, fmt.Sprintf(`{service=%q,env="it"}`, pushService), 2)
	if len(lines) != 2 {
		t.Fatalf("push handler: %d lines found, want 2: %v", len(lines), lines)
	}
	info := parseLine(t, lines[0].Text)
	if lines[0].Labels["level"] != "info" || info["msg"] != msg || info["order"] != float64(42) || info["trace_id"] != tid.String() {
		t.Errorf("info line = %v labels=%v", info, lines[0].Labels)
	}
	if info["password"] != "[REDACTED]" || strings.Contains(lines[0].Text, "hunter2") {
		t.Errorf("secret reached Loki: %s", lines[0].Text)
	}
	if cust, _ := info["customer"].(map[string]any); cust["tier"] != "gold" {
		t.Errorf("group lost: %v", info)
	}
	if lines[1].Labels["level"] != "error" {
		t.Errorf("second line labels = %v", lines[1].Labels)
	}
	// LogQL can use the JSON fields.
	if got := g.LokiQuery(t, ctx, uid, fmt.Sprintf(`{service=%q} | json | order="42"`, pushService), 1); len(got) != 1 {
		t.Errorf("LogQL | json filter found %d lines", len(got))
	}

	// 2. Records exported with OTLP: Loki turns service.name into the service_name
	// label and the trace context into structured metadata.
	otlpLines := g.LokiQuery(t, ctx, uid, fmt.Sprintf(`{service_name=%q} | trace_id=%q`, otlpService, tid.String()), 2)
	if len(otlpLines) != 2 {
		t.Fatalf("OTLP: %d lines carry the trace id, want 2: %v", len(otlpLines), otlpLines)
	}
	if otlpLines[0].Text != msg {
		t.Errorf("OTLP body = %q, want %q", otlpLines[0].Text, msg)
	}
	all := g.LokiQuery(t, ctx, uid, fmt.Sprintf(`{service_name=%q}`, otlpService), 2)
	for _, l := range all {
		if strings.Contains(l.Text, "hunter2") {
			t.Errorf("secret reached Loki through OTLP: %s", l.Text)
		}
		if strings.Contains(l.Text, "below the level") {
			t.Error("a record below the level was exported")
		}
	}
	if got := g.LokiQuery(t, ctx, uid, fmt.Sprintf(`{service_name=%q} | order="42"`, otlpService), 1); len(got) != 1 {
		t.Errorf("OTLP attribute order=42 found in %d lines", len(got))
	}
}

func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %q", line)
	}
	return m
}
