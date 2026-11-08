package telemetry_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
)

// collector is a fake OTLP/HTTP receiver for all three signals.
type collector struct {
	mu      sync.Mutex
	logs    []*logspb.ResourceLogs
	spans   []*tracepb.ResourceSpans
	metrics []*metricspb.ResourceMetrics
	paths   []string
	headers []http.Header
}

func newCollector(t *testing.T) (*collector, *httptest.Server) {
	t.Helper()
	c := &collector{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.paths = append(c.paths, r.URL.Path)
		c.headers = append(c.headers, r.Header.Clone())
		var resp proto.Message
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/logs"):
			var m collogs.ExportLogsServiceRequest
			if proto.Unmarshal(body, &m) == nil {
				c.logs = append(c.logs, m.ResourceLogs...)
			}
			resp = &collogs.ExportLogsServiceResponse{}
		case strings.HasSuffix(r.URL.Path, "/v1/traces"):
			var m coltrace.ExportTraceServiceRequest
			if proto.Unmarshal(body, &m) == nil {
				c.spans = append(c.spans, m.ResourceSpans...)
			}
			resp = &coltrace.ExportTraceServiceResponse{}
		case strings.HasSuffix(r.URL.Path, "/v1/metrics"):
			var m colmetrics.ExportMetricsServiceRequest
			if proto.Unmarshal(body, &m) == nil {
				c.metrics = append(c.metrics, m.ResourceMetrics...)
			}
			resp = &colmetrics.ExportMetricsServiceResponse{}
		default:
			w.WriteHeader(404)
			return
		}
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *collector) allSpans() (out []*tracepb.Span) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rs := range c.spans {
		for _, ss := range rs.ScopeSpans {
			out = append(out, ss.Spans...)
		}
	}
	return out
}

func (c *collector) allMetrics() (out []*metricspb.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rm := range c.metrics {
		for _, sm := range rm.ScopeMetrics {
			out = append(out, sm.Metrics...)
		}
	}
	return out
}

func (c *collector) allLogs() (out []*logspb.LogRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rl := range c.logs {
		for _, sl := range rl.ScopeLogs {
			out = append(out, sl.LogRecords...)
		}
	}
	return out
}

func (c *collector) serviceNames() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	names := map[string]bool{}
	for _, r := range c.logs {
		for _, kv := range r.Resource.GetAttributes() {
			if kv.Key == "service.name" {
				names["logs:"+kv.Value.GetStringValue()] = true
			}
		}
	}
	for _, r := range c.spans {
		for _, kv := range r.Resource.GetAttributes() {
			if kv.Key == "service.name" {
				names["traces:"+kv.Value.GetStringValue()] = true
			}
		}
	}
	for _, r := range c.metrics {
		for _, kv := range r.Resource.GetAttributes() {
			if kv.Key == "service.name" {
				names["metrics:"+kv.Value.GetStringValue()] = true
			}
		}
	}
	return names
}

func flush(t *testing.T, tel *telemetry.Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tel.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
}

func shutdown(t *testing.T, tel *telemetry.Telemetry) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLogsTracesAndMetricsShareOneIdentityAndTheTraceID(t *testing.T) {
	col, srv := newCollector(t)
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		Endpoint: srv.URL, ServiceName: "orders-api", ServiceVersion: "1.2.3", Environment: "test",
		Headers: map[string]string{"X-Scope-OrgID": "acme"}, Gzip: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	log, err := logger.New(logger.Config{Output: io.Discard, Extra: []slog.Handler{tel.LogHandler()}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, span := tel.Tracer("orders").Start(context.Background(), "place order")
	log.InfoContext(ctx, "order placed", "order", 42, "password", "hunter2")
	counter, err := tel.Meter("orders").Int64Counter("orders.placed")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 3)
	span.End()
	flush(t, tel)

	spans := col.allSpans()
	if len(spans) != 1 || spans[0].Name != "place order" {
		t.Fatalf("spans = %v", spans)
	}
	logs := col.allLogs()
	if len(logs) != 1 || string(logs[0].TraceId) != string(spans[0].TraceId) || string(logs[0].SpanId) != string(spans[0].SpanId) {
		t.Fatalf("log is not linked to the span: %v", logs)
	}
	if strings.Contains(logs[0].String(), "hunter2") {
		t.Error("secret reached the log exporter")
	}
	var found bool
	for _, m := range col.allMetrics() {
		if m.Name == "orders.placed" && m.GetSum().DataPoints[0].GetAsInt() == 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("counter not exported: %v", col.allMetrics())
	}
	for _, sig := range []string{"logs", "traces", "metrics"} {
		if !col.serviceNames()[sig+":orders-api"] {
			t.Errorf("%s lack service.name=orders-api: %v", sig, col.serviceNames())
		}
	}
	col.mu.Lock()
	for i, h := range col.headers {
		if h.Get("X-Scope-OrgID") != "acme" {
			t.Errorf("request %d (%s) lacks the header", i, col.paths[i])
		}
	}
	col.mu.Unlock()
	shutdown(t, tel)
}

func TestSignalsCanGoToDifferentEndpoints(t *testing.T) {
	logsCol, logsSrv := newCollector(t)
	tracesCol, tracesSrv := newCollector(t)
	metricsCol, metricsSrv := newCollector(t)
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "split",
		LogsEndpoint: logsSrv.URL + "/otlp", TracesEndpoint: tracesSrv.URL + "/", MetricsEndpoint: metricsSrv.URL + "/api/v1/otlp/v1/metrics",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := tel.Tracer("t").Start(context.Background(), "s")
	slog.New(tel.LogHandler()).InfoContext(ctx, "l")
	c, _ := tel.Meter("m").Int64Counter("c")
	c.Add(ctx, 1)
	span.End()
	flush(t, tel)

	if len(logsCol.allLogs()) != 1 || len(logsCol.allSpans()) != 0 || logsCol.paths[0] != "/otlp/v1/logs" {
		t.Errorf("logs endpoint: %v", logsCol.paths)
	}
	if len(tracesCol.allSpans()) != 1 || len(tracesCol.allLogs()) != 0 || tracesCol.paths[0] != "/v1/traces" {
		t.Errorf("traces endpoint: %v", tracesCol.paths)
	}
	if len(metricsCol.allMetrics()) == 0 || metricsCol.paths[0] != "/api/v1/otlp/v1/metrics" {
		t.Errorf("metrics endpoint (a full signal URL must be kept as is): %v", metricsCol.paths)
	}
	shutdown(t, tel)
}

func TestDisabledSignalsAreNoOpsAndSendNothing(t *testing.T) {
	col, srv := newCollector(t)
	tel, err := telemetry.New(context.Background(), telemetry.Config{
		Endpoint: srv.URL, DisableLogs: true, DisableTraces: true, DisableMetrics: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := tel.Tracer("t").Start(context.Background(), "s")
	span.End()
	if span.SpanContext().IsValid() {
		t.Error("a disabled tracer must not create real spans")
	}
	c, _ := tel.Meter("m").Int64Counter("c")
	c.Add(ctx, 1)
	log := slog.New(tel.LogHandler())
	log.Info("dropped")
	if log.Enabled(ctx, slog.LevelError) {
		t.Error("the disabled log handler must discard")
	}
	// HTTP wrappers still work as plain pass-throughs.
	h := tel.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }), "x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Body.String() != "ok" {
		t.Errorf("handler = %q", rec.Body.String())
	}
	flush(t, tel)
	shutdown(t, tel)
	if len(col.paths) != 0 {
		t.Errorf("nothing should be sent: %v", col.paths)
	}
}

func TestHTTPTracePropagatesAcrossServicesAndRecordsMetrics(t *testing.T) {
	col, srv := newCollector(t)
	tel, _ := telemetry.New(context.Background(), telemetry.Config{Endpoint: srv.URL, ServiceName: "mesh", DisableLogs: true})

	// Service B: an HTTP server with the middleware.
	var seen trace.SpanContext
	b := httptest.NewServer(tel.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = trace.SpanContextFromContext(r.Context())
		w.WriteHeader(http.StatusTeapot)
	}), "service-b"))
	defer b.Close()
	// Service A: calls B with an instrumented client inside its own span.
	client := &http.Client{Transport: tel.HTTPTransport(nil)}
	ctx, span := tel.Tracer("a").Start(context.Background(), "call b")
	req, _ := http.NewRequestWithContext(ctx, "GET", b.URL+"/ping", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	span.End()
	flush(t, tel)

	if !seen.IsValid() || seen.TraceID() != span.SpanContext().TraceID() {
		t.Errorf("server saw trace %v, client started %v", seen.TraceID(), span.SpanContext().TraceID())
	}
	kinds := map[tracepb.Span_SpanKind]int{}
	for _, s := range col.allSpans() {
		kinds[s.Kind]++
		if string(s.TraceId) != string(mustBytes(span.SpanContext().TraceID())) {
			t.Errorf("span %q is on another trace", s.Name)
		}
	}
	if kinds[tracepb.Span_SPAN_KIND_SERVER] != 1 || kinds[tracepb.Span_SPAN_KIND_CLIENT] != 1 || kinds[tracepb.Span_SPAN_KIND_INTERNAL] != 1 {
		t.Errorf("span kinds = %v", kinds)
	}
	names := map[string]bool{}
	for _, m := range col.allMetrics() {
		names[m.Name] = true
	}
	if !names["http.server.request.duration"] || !names["http.client.request.duration"] {
		t.Errorf("HTTP metrics missing: %v", names)
	}
	shutdown(t, tel)
}

func mustBytes(id trace.TraceID) []byte { return id[:] }

func TestSamplingKeepsWholeTraces(t *testing.T) {
	col, srv := newCollector(t)
	tel, _ := telemetry.New(context.Background(), telemetry.Config{
		Endpoint: srv.URL, DisableLogs: true, DisableMetrics: true, SampleRatio: 1e-9,
	})
	tr := tel.Tracer("t")
	for i := 0; i < 50; i++ {
		_, s := tr.Start(context.Background(), "root")
		s.End()
	}
	// A sampled parent forces the child to be kept.
	sampled := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: trace.FlagsSampled, Remote: true,
	})
	_, child := tr.Start(trace.ContextWithSpanContext(context.Background(), sampled), "child")
	child.End()
	flush(t, tel)
	spans := col.allSpans()
	if len(spans) != 1 || spans[0].Name != "child" {
		t.Errorf("exported %d spans, want only the child of a sampled parent", len(spans))
	}
	shutdown(t, tel)
}

func TestRuntimeMetricsAndGlobals(t *testing.T) {
	prevTP, prevMP, prevProp := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
	col, srv := newCollector(t)
	tel, _ := telemetry.New(context.Background(), telemetry.Config{
		Endpoint: srv.URL, DisableLogs: true, DisableTraces: true, RuntimeMetrics: true, SetGlobal: true,
	})
	if otel.GetMeterProvider() != tel.MeterProvider() || otel.GetTracerProvider() != tel.TracerProvider() {
		t.Error("SetGlobal did not install the providers")
	}
	if len(otel.GetTextMapPropagator().Fields()) == 0 {
		t.Error("SetGlobal did not install the propagator")
	}
	flush(t, tel)
	var goMetrics int
	for _, m := range col.allMetrics() {
		if strings.HasPrefix(m.Name, "go.") {
			goMetrics++
		}
	}
	if goMetrics == 0 {
		t.Errorf("no Go runtime metrics in %v", col.allMetrics())
	}
	shutdown(t, tel)
}

func TestGlobalsAreLeftAloneByDefault(t *testing.T) {
	before := otel.GetTracerProvider()
	_, srv := newCollector(t)
	tel, _ := telemetry.New(context.Background(), telemetry.Config{Endpoint: srv.URL})
	if otel.GetTracerProvider() != before {
		t.Error("New must not touch the global provider unless SetGlobal is set")
	}
	shutdown(t, tel)
}

func TestTraceAttrsAndPropagator(t *testing.T) {
	_, srv := newCollector(t)
	tel, _ := telemetry.New(context.Background(), telemetry.Config{Endpoint: srv.URL})
	ctx, span := tel.Tracer("t").Start(context.Background(), "s")
	attrs := telemetry.TraceAttrs(ctx)
	if len(attrs) != 2 || attrs[0].Value.String() != span.SpanContext().TraceID().String() {
		t.Errorf("TraceAttrs = %v", attrs)
	}
	if telemetry.TraceAttrs(context.Background()) != nil {
		t.Error("no span should give nil")
	}
	f := tel.Propagator().Fields()
	if len(f) == 0 {
		t.Error("propagator has no fields")
	}
	span.End()
	shutdown(t, tel)
}

func TestUnreachableReceiverNeverBlocks(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Config{Endpoint: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 100; i++ {
		_, s := tel.Tracer("t").Start(context.Background(), "s")
		s.End()
		slog.New(tel.LogHandler()).Info("x")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("instrumented code blocked on an unreachable receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tel.Shutdown(ctx) // errors are expected here; it must return
}

func TestValidate(t *testing.T) {
	if err := (telemetry.Config{}).Validate(); err != nil {
		t.Fatalf("the zero Config is valid (it uses the standard environment variables): %v", err)
	}
	for name, c := range map[string]telemetry.Config{
		"bad scheme":      {Endpoint: "grpc://c:4317"},
		"bad logs":        {LogsEndpoint: "nope"},
		"bad traces":      {TracesEndpoint: "ftp://x"},
		"bad metrics":     {MetricsEndpoint: "http://"},
		"credentials":     {Endpoint: "http://u:secretpw@c:4318"},
		"ratio high":      {SampleRatio: 1.5},
		"ratio negative":  {SampleRatio: -0.1},
		"negative timing": {MetricInterval: -1},
		"empty header":    {Headers: map[string]string{"": "x"}},
	} {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if strings.Contains(err.Error(), "secretpw") {
			t.Errorf("%s: error leaks the password", name)
		}
		if _, nerr := telemetry.New(context.Background(), c); nerr == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}
