//go:build integration

package telemetry_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/internal/grafanatest"
	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
)

// Run against real Loki, Tempo, Prometheus and Grafana (scripts/integration.sh
// starts them). One telemetry.New sends logs to Loki, traces to Tempo and metrics
// to Prometheus, and everything is read back through Grafana's data sources, the
// way a person would look at it, and tied together by the trace ID.
func TestIntegrationLogsTracesMetricsThroughGrafana(t *testing.T) {
	lokiURL := os.Getenv("GCU_TEST_LOKI_URL")
	tempoURL := os.Getenv("GCU_TEST_TEMPO_OTLP_URL")
	promURL := os.Getenv("GCU_TEST_PROMETHEUS_URL")
	grafanaURL := os.Getenv("GCU_TEST_GRAFANA_URL")
	if lokiURL == "" || tempoURL == "" || promURL == "" || grafanaURL == "" {
		t.Skip("GCU_TEST_LOKI_URL, _TEMPO_OTLP_URL, _PROMETHEUS_URL or _GRAFANA_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var b [5]byte
	rand.Read(b[:])
	id := fmt.Sprintf("%x", b)
	service := "gcu-tel-" + id
	metricName := "gcu.orders." + id

	tel, err := telemetry.New(ctx, telemetry.Config{
		LogsEndpoint:    lokiURL + "/otlp",
		TracesEndpoint:  tempoURL,
		MetricsEndpoint: promURL + "/api/v1/otlp",
		ServiceName:     service, ServiceVersion: "9.9.9", Environment: "it",
		MetricInterval: 500 * time.Millisecond, BatchInterval: 100 * time.Millisecond,
		RuntimeMetrics: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var console strings.Builder
	log, err := logger.New(logger.Config{Output: &console, Extra: []slog.Handler{tel.LogHandler()}, ContextAttrs: telemetry.TraceAttrs})
	if err != nil {
		t.Fatal(err)
	}

	// A request through the instrumented HTTP server, which does traced work.
	counter, err := tel.Meter("orders").Int64Counter(metricName)
	if err != nil {
		t.Fatal(err)
	}
	var traceID string
	srv := httptest.NewServer(tel.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, span := tel.Tracer("orders").Start(r.Context(), "place order "+id)
		defer span.End()
		traceID = span.SpanContext().TraceID().String()
		counter.Add(ctx, 5)
		log.InfoContext(ctx, "order placed "+id, "order", 42, "password", "hunter2-"+id)
		w.WriteHeader(http.StatusCreated)
	}), "orders-api"))
	defer srv.Close()
	client := &http.Client{Transport: tel.HTTPTransport(nil)}
	resp, err := client.Get(srv.URL + "/orders")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	flushCtx, c2 := context.WithTimeout(ctx, 30*time.Second)
	defer c2()
	if err := tel.ForceFlush(flushCtx); err != nil {
		t.Fatal(err)
	}
	if traceID == "" || !strings.Contains(console.String(), traceID) || strings.Contains(console.String(), "hunter2") {
		t.Fatalf("trace id %q / console: %s", traceID, console.String())
	}

	g := grafanatest.New(t, ctx, grafanaURL, os.Getenv("GCU_TEST_GRAFANA_PASSWORD"))
	lokiDS := g.AddDataSource(t, ctx, "gcu-loki-"+id, "loki", "http://loki:3100")
	tempoDS := g.AddDataSource(t, ctx, "gcu-tempo-"+id, "tempo", "http://tempo:3200")
	promDS := g.AddDataSource(t, ctx, "gcu-prom-"+id, "prometheus", "http://prometheus:9090")

	// Traces: the HTTP client, HTTP server and our own span share one trace.
	names, services := g.TempoSpans(t, ctx, tempoDS, traceID)
	// The client span ("HTTP GET"), the server span ("GET") and our own span.
	for _, want := range []string{"place order " + id, "GET", "HTTP GET"} {
		if !slices.Contains(names, want) {
			t.Errorf("trace %s lacks the span %q: %v", traceID, want, names)
		}
	}
	if len(names) < 3 || !services[service] {
		t.Errorf("trace spans = %v services = %v", names, services)
	}

	// Logs: the same trace ID, as structured metadata, and no secret.
	lines := g.LokiQuery(t, ctx, lokiDS, fmt.Sprintf(`{service_name=%q} | trace_id=%q`, service, traceID), 1)
	if len(lines) != 1 || lines[0].Text != "order placed "+id {
		t.Fatalf("log lines for the trace: %v", lines)
	}
	all := g.LokiQuery(t, ctx, lokiDS, fmt.Sprintf(`{service_name=%q}`, service), 1)
	for _, l := range all {
		if strings.Contains(l.Text, "hunter2") {
			t.Errorf("secret reached Loki: %s", l.Text)
		}
	}

	// Metrics: the counter, the HTTP server histogram and the runtime metrics.
	promName := strings.NewReplacer(".", "_").Replace(metricName) + "_total"
	if got := g.PromValue(t, ctx, promDS, fmt.Sprintf(`%s{job=%q}`, promName, service)); got != "5" {
		t.Errorf("counter %s = %q, want 5", promName, got)
	}
	if got := g.PromValue(t, ctx, promDS, fmt.Sprintf(`http_server_request_duration_seconds_count{job=%q}`, service)); got != "1" {
		t.Errorf("http server request count = %q, want 1", got)
	}
	if got := g.PromValue(t, ctx, promDS, fmt.Sprintf(`go_goroutine_count{job=%q}`, service)); got == "" {
		t.Error("no Go runtime metrics in Prometheus")
	}

	sctx, c3 := context.WithTimeout(ctx, 30*time.Second)
	defer c3()
	if err := tel.Shutdown(sctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}
