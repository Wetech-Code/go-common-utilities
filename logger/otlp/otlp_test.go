package otlp_test

import (
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/logger/otlp"
)

// receiver is a fake OTLP/HTTP logs endpoint that decodes the protobuf requests.
type receiver struct {
	mu      sync.Mutex
	reqs    []*collogs.ExportLogsServiceRequest
	headers []http.Header
	paths   []string
	status  int
}

func newReceiver(t *testing.T) (*receiver, *httptest.Server) {
	t.Helper()
	r := &receiver{status: 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body io.Reader = req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(req.Body)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			body = zr
		}
		b, _ := io.ReadAll(body)
		var msg collogs.ExportLogsServiceRequest
		if err := proto.Unmarshal(b, &msg); err != nil {
			w.WriteHeader(400)
			return
		}
		r.mu.Lock()
		r.reqs = append(r.reqs, &msg)
		r.headers = append(r.headers, req.Header.Clone())
		r.paths = append(r.paths, req.URL.Path)
		status := r.status
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(status)
		if status == 200 {
			out, _ := proto.Marshal(&collogs.ExportLogsServiceResponse{})
			w.Write(out)
		}
	}))
	t.Cleanup(srv.Close)
	return r, srv
}

func (r *receiver) records() (res []*logspb.LogRecord, resourceAttrs map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	resourceAttrs = map[string]string{}
	for _, req := range r.reqs {
		for _, rl := range req.ResourceLogs {
			for _, kv := range rl.Resource.GetAttributes() {
				resourceAttrs[kv.Key] = kv.Value.GetStringValue()
			}
			for _, sl := range rl.ScopeLogs {
				res = append(res, sl.LogRecords...)
			}
		}
	}
	return res, resourceAttrs
}

func attrs(rec *logspb.LogRecord) map[string]*commonpb.AnyValue {
	m := map[string]*commonpb.AnyValue{}
	for _, kv := range rec.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

func shutdown(t *testing.T, h *otlp.Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRecordsAreExportedWithResourceAndAttributes(t *testing.T) {
	rcv, srv := newReceiver(t)
	h, err := otlp.New(context.Background(), otlp.Config{
		Endpoint: srv.URL, ServiceName: "api", ServiceVersion: "1.2.3", Environment: "test",
		Attributes: map[string]string{"k8s.namespace.name": "prod"},
		Headers:    map[string]string{"Authorization": "Basic abc", "X-Scope-OrgID": "acme"},
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(h).With("req", "r1")
	log.Info("started", "port", 8080, "ratio", 0.5, "ok", true, slog.Group("db", slog.String("name", "main")))
	log.Error("failed", "err", "boom")
	log.Debug("hidden")
	shutdown(t, h)

	recs, res := rcv.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	for k, want := range map[string]string{"service.name": "api", "service.version": "1.2.3", "deployment.environment": "test", "k8s.namespace.name": "prod"} {
		if res[k] != want {
			t.Errorf("resource %s = %q, want %q", k, res[k], want)
		}
	}
	first := recs[0]
	if first.Body.GetStringValue() != "started" || first.SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_INFO || first.SeverityText != "INFO" {
		t.Errorf("first record = %v", first)
	}
	a := attrs(first)
	if a["req"].GetStringValue() != "r1" || a["port"].GetIntValue() != 8080 || a["ratio"].GetDoubleValue() != 0.5 || !a["ok"].GetBoolValue() {
		t.Errorf("attributes = %v", a)
	}
	if db := a["db"].GetKvlistValue(); db == nil || db.Values[0].Key != "name" {
		t.Errorf("group attribute = %v", a["db"])
	}
	if recs[1].SeverityNumber != logspb.SeverityNumber_SEVERITY_NUMBER_ERROR {
		t.Errorf("second severity = %v", recs[1].SeverityNumber)
	}
	if rcv.paths[0] != "/v1/logs" {
		t.Errorf("path = %q", rcv.paths[0])
	}
	if rcv.headers[0].Get("Authorization") != "Basic abc" || rcv.headers[0].Get("X-Scope-OrgID") != "acme" {
		t.Errorf("headers = %v", rcv.headers[0])
	}
}

func TestTraceContextLinksRecords(t *testing.T) {
	rcv, srv := newReceiver(t)
	h, _ := otlp.New(context.Background(), otlp.Config{Endpoint: srv.URL + "/prefix/", ServiceName: "api"})
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		SpanID:     trace.SpanID{1, 2, 3, 4, 5, 6, 7, 8},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sc)
	slog.New(h).InfoContext(ctx, "in a span")
	slog.New(h).Info("no span")
	shutdown(t, h)

	recs, _ := rcv.records()
	if len(recs) != 2 {
		t.Fatalf("records = %d", len(recs))
	}
	tid, sid := sc.TraceID(), sc.SpanID()
	if string(recs[0].TraceId) != string(tid[:]) || string(recs[0].SpanId) != string(sid[:]) {
		t.Errorf("trace context lost: trace=%x span=%x", recs[0].TraceId, recs[0].SpanId)
	}
	if len(recs[1].TraceId) != 0 && string(recs[1].TraceId) != string(make([]byte, 16)) {
		t.Errorf("unexpected trace id on a record without a span: %x", recs[1].TraceId)
	}
	if rcv.paths[0] != "/prefix/v1/logs" {
		t.Errorf("path = %q; a base path must be kept", rcv.paths[0])
	}
}

func TestTraceAttrs(t *testing.T) {
	if got := otlp.TraceAttrs(context.Background()); got != nil {
		t.Errorf("no span should give nil, got %v", got)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0xab, 0xcd, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14},
		SpanID:  trace.SpanID{0xef, 1, 2, 3, 4, 5, 6, 7},
	})
	got := otlp.TraceAttrs(trace.ContextWithSpanContext(context.Background(), sc))
	if len(got) != 2 || got[0].Key != "trace_id" || got[0].Value.String() != sc.TraceID().String() || got[1].Key != "span_id" || got[1].Value.String() != sc.SpanID().String() {
		t.Errorf("TraceAttrs = %v", got)
	}
}

func TestSecretsAreHiddenAndLevelApplies(t *testing.T) {
	rcv, srv := newReceiver(t)
	h, _ := otlp.New(context.Background(), otlp.Config{Endpoint: srv.URL, Level: slog.LevelWarn})
	log := slog.New(h).With("api_key", "k-1")
	log.Info("below the level")
	log.Warn("login", "password", "hunter2", slog.Group("credentials", slog.String("user", "u")))
	log.WithGroup("g").Error("x", "Token", "t-9")
	if h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("Enabled ignores Level")
	}
	shutdown(t, h)

	recs, _ := rcv.records()
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2 (the info record is below Level)", len(recs))
	}
	dump := ""
	for _, r := range recs {
		dump += prototext.Format(r)
	}
	for _, leaked := range []string{"hunter2", "k-1", `"u"`, "t-9"} {
		if strings.Contains(dump, leaked) {
			t.Errorf("%q leaked: %s", leaked, dump)
		}
	}
}

func TestGzipAndAddSourceAndForceFlush(t *testing.T) {
	rcv, srv := newReceiver(t)
	h, _ := otlp.New(context.Background(), otlp.Config{Endpoint: srv.URL, Gzip: true, AddSource: true, BatchInterval: time.Hour})
	slog.New(h).Info("flush me")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	recs, _ := rcv.records()
	if len(recs) != 1 {
		t.Fatalf("ForceFlush exported %d records", len(recs))
	}
	if rcv.headers[0].Get("Content-Encoding") != "gzip" {
		t.Errorf("not compressed: %v", rcv.headers[0])
	}
	if _, ok := attrs(recs[0])["code.function.name"]; !ok {
		if _, ok := attrs(recs[0])["code.function"]; !ok {
			t.Errorf("no source attributes: %v", attrs(recs[0]))
		}
	}
	shutdown(t, h)
}

func TestWorksAsAnExtraHandlerOfTheLoggerPackage(t *testing.T) {
	rcv, srv := newReceiver(t)
	h, _ := otlp.New(context.Background(), otlp.Config{Endpoint: srv.URL})
	var console strings.Builder
	log, err := logger.New(logger.Config{Output: &console, Extra: []slog.Handler{h}, ContextAttrs: otlp.TraceAttrs})
	if err != nil {
		t.Fatal(err)
	}
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}})
	log.InfoContext(trace.ContextWithSpanContext(context.Background(), sc), "both", "password", "pw")
	shutdown(t, h)

	recs, _ := rcv.records()
	if len(recs) != 1 || strings.Contains(console.String(), "pw") || !strings.Contains(console.String(), `"trace_id":"`+sc.TraceID().String()+`"`) {
		t.Errorf("otlp=%d console=%q", len(recs), console.String())
	}
}

func TestShutdownTwiceAndUnreachableReceiver(t *testing.T) {
	// Nothing listens here; logging must not block or fail.
	h, err := otlp.New(context.Background(), otlp.Config{Endpoint: "http://127.0.0.1:1", Timeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i := 0; i < 50; i++ {
		slog.New(h).Info("into the void")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("logging blocked on an unreachable receiver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = h.Shutdown(ctx)
	_ = h.Shutdown(ctx)
}

func TestValidate(t *testing.T) {
	if err := (otlp.Config{}).Validate(); err != nil {
		t.Fatalf("the zero Config is valid (it uses the standard environment variables): %v", err)
	}
	for name, c := range map[string]otlp.Config{
		"bad scheme":    {Endpoint: "grpc://collector:4317"},
		"no host":       {Endpoint: "http://"},
		"credentials":   {Endpoint: "http://u:secretpw@collector:4318"},
		"negative":      {Timeout: -1},
		"batch > queue": {BatchSize: 10, QueueSize: 5},
		"empty header":  {Headers: map[string]string{" ": "x"}},
	} {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if strings.Contains(err.Error(), "secretpw") {
			t.Errorf("%s: error leaks the password", name)
		}
		if _, nerr := otlp.New(context.Background(), c); nerr == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}
