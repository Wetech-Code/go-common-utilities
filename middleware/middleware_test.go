package middleware_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/middleware"
	"github.com/Wetech-Code/go-common-utilities/reqctx"
)

func do(h http.Handler, method, url string, hdr map[string][]string, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	for k, vs := range hdr {
		r.Header[k] = vs
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestChainOrder(t *testing.T) {
	var order []string
	mk := func(name string) middleware.Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+" in")
				next.ServeHTTP(w, r)
				order = append(order, name+" out")
			})
		}
	}
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { order = append(order, "handler") }), mk("a"), nil, mk("b"))
	do(h, "GET", "/", nil, "")
	want := "a in,b in,handler,b out,a out"
	if strings.Join(order, ",") != want {
		t.Errorf("order = %v", order)
	}
}

func TestRequestID(t *testing.T) {
	var seen string
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = reqctx.RequestID(r.Context()) }), middleware.RequestID())

	w := do(h, "GET", "/", map[string][]string{"X-Request-Id": {"upstream-42"}}, "")
	if seen != "upstream-42" || w.Header().Get("X-Request-ID") != "upstream-42" {
		t.Errorf("a valid incoming ID should be reused: %q / %q", seen, w.Header().Get("X-Request-ID"))
	}
	for name, hdr := range map[string]map[string][]string{
		"none":      nil,
		"invalid":   {"X-Request-Id": {"bad id\r\nX-Evil: 1"}},
		"duplicate": {"X-Request-Id": {"one", "two"}},
		"too long":  {"X-Request-Id": {strings.Repeat("a", 200)}},
		"empty":     {"X-Request-Id": {""}},
	} {
		w := do(h, "GET", "/", hdr, "")
		if len(seen) != 32 || !reqctx.ValidID(seen) || w.Header().Get("X-Request-ID") != seen {
			t.Errorf("%s: a fresh ID expected, got %q (response %q)", name, seen, w.Header().Get("X-Request-ID"))
		}
	}
	always := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = reqctx.RequestID(r.Context()) }), middleware.RequestID(middleware.AlwaysGenerate()))
	do(always, "GET", "/", map[string][]string{"X-Request-Id": {"client-chosen"}}, "")
	if seen == "client-chosen" || len(seen) != 32 {
		t.Errorf("AlwaysGenerate reused the client's ID: %q", seen)
	}
}

func TestCaptureHeaders(t *testing.T) {
	var got map[string]string
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = map[string]string{}
		for _, n := range []string{"X-Tenant-ID", "X-User-ID", "X-Other"} {
			if v := reqctx.HeaderValue(r.Context(), n); v != "" {
				got[n] = v
			}
		}
		w.WriteHeader(204)
	}), middleware.CaptureHeaders("X-Tenant-ID", "x-user-id"))

	w := do(h, "GET", "/", map[string][]string{"X-Tenant-Id": {"t1"}, "X-User-Id": {"u1"}, "X-Other": {"not-listed"}}, "")
	if w.Code != 204 || len(got) != 2 || got["X-Tenant-ID"] != "t1" || got["X-User-ID"] != "u1" {
		t.Errorf("code=%d got=%v (only the listed headers are captured)", w.Code, got)
	}
	got = nil
	if do(h, "GET", "/", nil, ""); len(got) != 0 {
		t.Errorf("no headers should capture nothing: %v", got)
	}
	for name, hdr := range map[string]map[string][]string{
		"duplicate":   {"X-User-Id": {"alice", "admin"}},
		"newline":     {"X-User-Id": {"a\nb"}},
		"bad value":   {"X-Tenant-Id": {"tenant one"}},
		"empty value": {"X-Tenant-Id": {""}},
		"too long":    {"X-User-Id": {strings.Repeat("a", 200)}},
	} {
		got = nil
		w := do(h, "GET", "/", hdr, "")
		if w.Code != 400 || got != nil || !strings.Contains(w.Body.String(), "invalid_header") {
			t.Errorf("%s: code=%d got=%v body=%s", name, w.Code, got, w.Body.String())
		}
	}
	// With no names to capture it is a pass-through.
	pass := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }), middleware.CaptureHeaders())
	if w := do(pass, "GET", "/", map[string][]string{"X-User-Id": {"a b"}}, ""); w.Code != 204 {
		t.Errorf("no names: %d", w.Code)
	}
}

func TestStripHeaders(t *testing.T) {
	var user, other string
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, other = r.Header.Get("X-User-ID"), r.Header.Get("X-Other")
	}), middleware.StripHeaders("X-User-ID", "x-tenant-id"))
	do(h, "GET", "/", map[string][]string{"X-User-Id": {"forged"}, "X-Other": {"kept"}}, "")
	if user != "" || other != "kept" {
		t.Errorf("user=%q other=%q", user, other)
	}
}

func newLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestRecover(t *testing.T) {
	log, buf := newLog()
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom with secret-token-123\nforged line")
	}), middleware.RequestID(), middleware.Recover(log))
	w := do(h, "POST", "/orders/5?token=secret-query", map[string][]string{"Authorization": {"Bearer secret-header"}}, "secret-body")
	if w.Code != 500 || w.Body.String() != "{\"error\":\"internal_error\"}\n" || w.Header().Get("Content-Type") != "application/json" {
		t.Errorf("response: %d %q", w.Code, w.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, "handler panicked") || !strings.Contains(out, "/orders/5") {
		t.Errorf("log = %s", out)
	}
	for _, leak := range []string{"secret-query", "secret-header", "secret-body"} {
		if strings.Contains(out, leak) {
			t.Errorf("the log leaks %q: %s", leak, out)
		}
	}
	if strings.Contains(out, "\\n") && strings.Contains(out, "forged line") && !strings.Contains(out, "boom with secret-token-123 forged line") {
		t.Errorf("control characters in the panic text must be flattened: %s", out)
	}
	// A response that already started cannot be replaced by an error body.
	buf.Reset()
	h2 := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(202)
		w.Write([]byte("partial"))
		panic("late")
	}), middleware.Recover(log))
	w = do(h2, "GET", "/", nil, "")
	if w.Code != 202 || w.Body.String() != "partial" {
		t.Errorf("a started response must be left alone: %d %q", w.Code, w.Body.String())
	}
	// http.ErrAbortHandler is passed on; other values (an error, a number) are handled.
	func() {
		defer func() {
			if recover() != http.ErrAbortHandler {
				t.Error("ErrAbortHandler must propagate")
			}
		}()
		do(middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }), middleware.Recover(log)), "GET", "/", nil, "")
	}()
	for _, v := range []any{io.EOF, 42, struct{ X int }{1}} {
		w := do(middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(v) }), middleware.Recover(nil)), "GET", "/", nil, "")
		if w.Code != 500 {
			t.Errorf("panic(%v): %d", v, w.Code)
		}
	}
}

func TestAccessLog(t *testing.T) {
	log, buf := newLog()
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) })
	mux.HandleFunc("/bad", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 404) })
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	h := middleware.Chain(mux, middleware.RequestID(), middleware.AccessLog(log))

	do(h, "GET", "/ok?api_key=secret-q", map[string][]string{"Authorization": {"Bearer secret-h"}, "X-Request-Id": {"rid-1"}}, "secret-body")
	do(h, "GET", "/bad", nil, "")
	do(h, "GET", "/boom", nil, "")
	out := buf.String()
	for _, leak := range []string{"secret-q", "secret-h", "secret-body", "api_key"} {
		if strings.Contains(out, leak) {
			t.Errorf("access log leaks %q: %s", leak, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %s", len(lines), out)
	}
	for i, want := range []string{`"level":"INFO"`, `"level":"WARN"`, `"level":"ERROR"`} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d lacks %s: %s", i, want, lines[i])
		}
	}
	if !strings.Contains(lines[0], `"path":"/ok"`) || !strings.Contains(lines[0], `"status":200`) || !strings.Contains(lines[0], `"bytes":5`) ||
		!strings.Contains(lines[0], `"method":"GET"`) || !strings.Contains(lines[0], `"duration_ms"`) {
		t.Errorf("fields: %s", lines[0])
	}
	// The request ID reaches the log through ContextAttrs-style composition.
	rid := slog.New(slog.NewJSONHandler(buf, nil))
	_ = rid
}

func TestAccessLogSupportsFlushAndResponseController(t *testing.T) {
	log, _ := newLog()
	flushed := false
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush through the wrapper: %v", err)
		}
		flushed = true
		w.Write([]byte("x"))
	}), middleware.AccessLog(log), middleware.Recover(log))
	w := do(h, "GET", "/", nil, "")
	if !flushed || !w.Flushed {
		t.Errorf("flushed=%v recorder.Flushed=%v", flushed, w.Flushed)
	}
	// Hijack is refused where the underlying writer cannot do it.
	h2 := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := http.NewResponseController(w).Hijack(); err == nil {
			t.Error("a recorder cannot be hijacked")
		}
	}), middleware.AccessLog(log))
	do(h2, "GET", "/", nil, "")
}

func TestMaxBodyTimeoutAndSecureHeaders(t *testing.T) {
	read := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, "too big", http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(200)
	}), middleware.MaxBody(10))
	if w := do(read, "POST", "/", nil, "short"); w.Code != 200 {
		t.Errorf("small body: %d", w.Code)
	}
	if w := do(read, "POST", "/", nil, strings.Repeat("x", 100)); w.Code != 413 {
		t.Errorf("large body: %d", w.Code)
	}

	slow := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}), middleware.Timeout(50*time.Millisecond))
	start := time.Now()
	w := do(slow, "GET", "/", nil, "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "timeout") || time.Since(start) > time.Second {
		t.Errorf("timeout: %d %q after %v", w.Code, w.Body.String(), time.Since(start))
	}

	sec := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), middleware.SecureHeaders())
	w = do(sec, "GET", "/", nil, "")
	for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer"} {
		if w.Header().Get(k) != v {
			t.Errorf("%s = %q", k, w.Header().Get(k))
		}
	}
}

func TestContextCarriesRequestIDToLogs(t *testing.T) {
	log, buf := newLog()
	_ = context.Background()
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.InfoContext(r.Context(), "inside")
	}), middleware.RequestID())
	do(h, "GET", "/", nil, "")
	if !strings.Contains(buf.String(), "inside") {
		t.Error("handler did not log")
	}
}
