package httpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/reqctx"
)

// fake is a stand-in server that records requests and answers from a script.
type fake struct {
	*httptest.Server
	mu    sync.Mutex
	reqs  []rec
	reply func(n int, r *http.Request) (status int, body string, hdr map[string]string)
}

type rec struct {
	method, path, rawPath, query string
	header                       http.Header
	body                         string
}

type script = func(int, *http.Request) (int, string, map[string]string)

func newFake(t *testing.T, reply script) *fake {
	t.Helper()
	f := &fake{reply: reply}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, rec{r.Method, r.URL.Path, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Clone(), string(b)})
		n := len(f.reqs)
		f.mu.Unlock()
		status, body, hdr := f.reply(n, r)
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.reqs) }
func (f *fake) last() rec  { f.mu.Lock(); defer f.mu.Unlock(); return f.reqs[len(f.reqs)-1] }

func okScript() script {
	return func(int, *http.Request) (int, string, map[string]string) { return 200, `{"ok":true}`, nil }
}

func newClient(t *testing.T, f *fake, mut func(*Config)) (*Client, *[]time.Duration) {
	t.Helper()
	cfg := Config{BaseURL: f.URL, PathPrefix: "/gw/orders", Headers: map[string]string{"X-Caller": "billing"}}
	if mut != nil {
		mut(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var sleeps []time.Duration
	c.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	return c, &sleeps
}

func TestBuildsTheURLAndSendsTheManagedHeaders(t *testing.T) {
	f := newFake(t, okScript())
	c, _ := newClient(t, f, nil)
	ctx := reqctx.WithRequestID(context.Background(), "req-77")
	ctx = reqctx.WithHeaders(ctx, map[string]string{"X-Tenant-ID": "t-9", "X-User-ID": "u-1"})
	resp, err := c.Do(ctx, "POST", Path("stock", "item 1", "reserve"),
		Query(url.Values{"dry": {"true"}, "a": {"x y"}}), JSONBody(map[string]int{"qty": 2}),
		IdempotencyKey("idem-1"), Header("X-Custom", "v"), ForwardFromContext("x-tenant-id", "X-Missing"))
	if err != nil || !resp.OK() || resp.RequestID != "req-77" || resp.Attempts != 1 {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
	r := f.last()
	if r.method != "POST" || r.path != "/gw/orders/stock/item 1/reserve" || r.rawPath != "/gw/orders/stock/item%201/reserve" || r.query != "a=x+y&dry=true" {
		t.Errorf("request line: %+v", r)
	}
	for k, want := range map[string]string{
		"X-Request-Id": "req-77", "X-Caller": "billing", "X-Idempotency-Key": "idem-1", "Content-Type": "application/json",
		"Accept": "application/json", "X-Custom": "v", "X-Tenant-Id": "t-9",
	} {
		if r.header.Get(k) != want {
			t.Errorf("header %s = %q, want %q", k, r.header.Get(k), want)
		}
	}
	// Only what was asked for is forwarded.
	if r.header.Get("X-User-Id") != "" || r.header.Get("X-Missing") != "" {
		t.Errorf("an unrequested or missing header was sent: %v", r.header)
	}
	if r.body != `{"qty":2}` {
		t.Errorf("body = %q", r.body)
	}

	// With no request ID in the context a new one is generated, and nothing else is sent unasked.
	if _, err := c.Do(context.Background(), "GET", "stock"); err != nil {
		t.Fatal(err)
	}
	r = f.last()
	if !reqctx.ValidID(r.header.Get("X-Request-Id")) || len(r.header.Get("X-Request-Id")) != 32 || r.header.Get("X-Idempotency-Key") != "" || r.header.Get("X-Tenant-Id") != "" {
		t.Errorf("headers = %v", r.header)
	}
}

func TestPrefixesAndBasePath(t *testing.T) {
	f := newFake(t, okScript())
	c, _ := newClient(t, f, func(cfg *Config) { cfg.PathPrefix = "/api/v1"; cfg.BaseURL = f.URL + "/base/" })
	if _, err := c.Do(context.Background(), "GET", "invoices/"); err != nil {
		t.Fatal(err)
	}
	if got := f.last().path; got != "/base/api/v1/invoices/" {
		t.Errorf("path = %q (a trailing slash must be kept)", got)
	}
	pay, err := c.WithPrefix("payments")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pay.Do(context.Background(), "GET", "charges"); err != nil {
		t.Fatal(err)
	}
	if got := f.last().path; got != "/base/api/v1/payments/charges" {
		t.Errorf("path = %q", got)
	}
	// The original client is unchanged, and a prefix can be added to a prefix.
	if _, err := c.Do(context.Background(), "GET", "x"); err != nil || f.last().path != "/base/api/v1/x" {
		t.Errorf("original changed: %q %v", f.last().path, err)
	}
	deeper, _ := pay.WithPrefix("v2")
	deeper.Do(context.Background(), "GET", "x")
	if f.last().path != "/base/api/v1/payments/v2/x" {
		t.Errorf("path = %q", f.last().path)
	}
	// No prefix at all is fine.
	bare, _ := newClient(t, f, func(cfg *Config) { cfg.PathPrefix = "" })
	bare.Do(context.Background(), "GET", "health")
	if f.last().path != "/health" {
		t.Errorf("path = %q", f.last().path)
	}
}

func TestWithPrefixIsValidatedAndRestrictedByAllowlist(t *testing.T) {
	f := newFake(t, okScript())
	c, _ := newClient(t, f, nil)
	for name, seg := range map[string]string{"empty": "", "upper": "Pay", "slash": "a/b", "dots": "..", "space": "a b", "long": strings.Repeat("a", 40), "digit first": "1a", "query": "a?b", "encoded": "a%2Fb"} {
		if _, err := c.WithPrefix(seg); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	restricted, _ := newClient(t, f, func(cfg *Config) { cfg.AllowedPrefixes = []string{"payments", "shipping"} })
	if _, err := restricted.WithPrefix("admin"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a prefix outside the allowlist: %v", err)
	}
	if _, err := restricted.WithPrefix("shipping"); err != nil {
		t.Errorf("an allowed prefix: %v", err)
	}
	if f.count() != 0 {
		t.Error("nothing should have been sent")
	}
}

func TestPathEscapesEveryValue(t *testing.T) {
	f := newFake(t, okScript())
	c, _ := newClient(t, f, nil)
	for name, id := range map[string]string{
		"slash": "a/b", "dots": "..", "dot": ".", "query": "x?y=1", "hash": "x#frag", "percent": "100%", "encoded dots": "%2e%2e",
		"backslash": `a\b`, "space": "a b", "unicode": "日本", "newline": "a\nb",
	} {
		if _, err := c.Do(context.Background(), "GET", Path("orders", id)); err != nil {
			// Path escapes, and the dot segments and control characters that cannot be made safe are refused.
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("%s: %v", name, err)
			}
			continue
		}
		r := f.last()
		if !strings.HasPrefix(r.path, "/gw/orders/orders/") || strings.Count(r.rawPath, "/") != 4 || r.query != "" {
			t.Errorf("%s: the value escaped its segment: %+v", name, r)
		}
	}
}

func TestEndpointAndMethodValidation(t *testing.T) {
	f := newFake(t, okScript())
	c, _ := newClient(t, f, nil)
	for name, e := range map[string]Endpoint{
		"empty": "", "slash only": "/", "dot dot": "a/../b", "leading dot dot": "../x", "encoded dot dot": "a/%2e%2e/b", "encoded dot dot upper": "a/%2E%2E",
		"dot": "a/./b", "double slash": "a//b", "query": "a?x=1", "fragment": "a#b", "backslash": `a\b`, "encoded slash": "a%2Fb",
		"encoded backslash": "a%5Cb", "bad escape": "a%zz", "control": "a\x00b", "encoded control": "a%0Ab", "encoded nul": "a%00",
	} {
		_, err := c.Do(context.Background(), "GET", e)
		var ce *Error
		if !errors.Is(err, ErrInvalidRequest) || !errors.As(err, &ce) {
			t.Errorf("%s (%q): %v", name, string(e), err)
		}
	}
	for _, m := range []string{"", "GE T", "GET\r\nX: y"} {
		if _, err := c.Do(context.Background(), m, "x"); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("method %q: %v", m, err)
		}
	}
	if f.count() != 0 {
		t.Errorf("%d invalid calls reached the network", f.count())
	}
}

func TestOptionsAndConfigHeadersAreChecked(t *testing.T) {
	f := newFake(t, okScript())
	c, _ := newClient(t, f, nil)
	for name, opt := range map[string]Option{
		"reserved request id":  Header("x-request-id", "forged"),
		"reserved idempotency": Header("X-Idempotency-Key", "k"),
		"content type":         Header("Content-Type", "text/html"),
		"host":                 Header("Host", "evil"),
		"traceparent":          Header("traceparent", "00-aa-bb-01"),
		"connection":           Header("Connection", "close"),
		"newline in value":     Header("X-A", "a\r\nX-Evil: 1"),
		"bad name":             Header("X A", "v"),
		"empty name":           Header("", "v"),
		"nul in value":         Header("X-A", "a\x00b"),
		"forward reserved":     ForwardFromContext("X-Request-ID"),
		"forward bad name":     ForwardFromContext("X A"),
		"bad idempotency key":  IdempotencyKey("has space"),
		"unencodable body":     JSONBody(make(chan int)),
	} {
		if _, err := c.Do(context.Background(), "POST", "x", opt); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.count() != 0 {
		t.Errorf("%d bad calls reached the network", f.count())
	}
	if _, err := c.Do(context.Background(), "POST", "x", Body("text/plain", []byte("raw")), Timeout(time.Second)); err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.body != "raw" || r.header.Get("Content-Type") != "text/plain" {
		t.Errorf("%+v", r)
	}
	// An ordinary header with an identity-like name is just a header: no special rules.
	if _, err := c.Do(context.Background(), "GET", "x", Header("X-User-ID", "u1")); err != nil || f.last().header.Get("X-User-Id") != "u1" {
		t.Errorf("%v", err)
	}
	// Static headers in the config are validated too, and per-call headers win over them.
	for name, h := range map[string]map[string]string{
		"reserved": {"X-Request-ID": "x"}, "newline": {"X-A": "a\nb"}, "bad name": {"X A": "v"}, "content length": {"Content-Length": "5"},
	} {
		if err := (Config{BaseURL: f.URL, Headers: h}).Validate(); err == nil {
			t.Errorf("config header %s: want an error", name)
		}
	}
	c.Do(context.Background(), "GET", "x", Header("X-Caller", "override"))
	if f.last().header.Get("X-Caller") != "override" {
		t.Errorf("per-call header should win: %v", f.last().header)
	}
}

func TestRetryRulesFollowIdempotency(t *testing.T) {
	failing := func(failures int, status int) script {
		return func(n int, r *http.Request) (int, string, map[string]string) {
			if n <= failures {
				return status, `{"error":"busy"}`, nil
			}
			return 200, `{"ok":true}`, nil
		}
	}
	for name, tc := range map[string]struct {
		method string
		opts   []Option
		fail   int
		status int
		calls  int
		code   int
	}{
		"GET retried on 503":            {"GET", nil, 2, 503, 3, 200},
		"GET retried on 502":            {"GET", nil, 1, 502, 2, 200},
		"GET retried on 504":            {"GET", nil, 1, 504, 2, 200},
		"GET retried on 429":            {"GET", nil, 1, 429, 2, 200},
		"GET not retried on 500":        {"GET", nil, 1, 500, 1, 500},
		"GET not retried on 404":        {"GET", nil, 1, 404, 1, 404},
		"POST sent once":                {"POST", []Option{JSONBody(1)}, 1, 503, 1, 503},
		"POST with key retried":         {"POST", []Option{JSONBody(1), IdempotencyKey("k-1")}, 2, 503, 3, 200},
		"PATCH with key retried":        {"PATCH", []Option{IdempotencyKey("k-2")}, 1, 503, 2, 200},
		"DELETE sent once":              {"DELETE", nil, 1, 503, 1, 503},
		"key but NoRetry":               {"POST", []Option{IdempotencyKey("k-3"), NoRetry()}, 1, 503, 1, 503},
		"GET gives up after the budget": {"GET", nil, 99, 503, 3, 503},
	} {
		f := newFake(t, failing(tc.fail, tc.status))
		c, sleeps := newClient(t, f, nil)
		resp, err := c.Do(context.Background(), tc.method, "x", tc.opts...)
		if err != nil || resp.Status != tc.code || f.count() != tc.calls || resp.Attempts != tc.calls {
			t.Errorf("%s: status=%v err=%v calls=%d attempts=%v", name, resp, err, f.count(), tc.calls)
		}
		if len(*sleeps) != tc.calls-1 {
			t.Errorf("%s: slept %d times for %d calls", name, len(*sleeps), tc.calls)
		}
	}

	// A repeat sends exactly the same body and headers (including the idempotency key).
	f := newFake(t, failing(1, 503))
	c, _ := newClient(t, f, nil)
	if _, err := c.Do(context.Background(), "POST", "x", JSONBody(map[string]string{"a": "b"}), IdempotencyKey("same")); err != nil {
		t.Fatal(err)
	}
	if f.reqs[0].body != f.reqs[1].body || f.reqs[0].body != `{"a":"b"}` || f.reqs[1].header.Get("X-Idempotency-Key") != "same" || f.reqs[0].header.Get("X-Request-Id") != f.reqs[1].header.Get("X-Request-Id") {
		t.Errorf("retry differs from the first attempt: %+v", f.reqs)
	}
	// Retries can be disabled for the whole client, and a custom budget is respected.
	f2 := newFake(t, failing(5, 503))
	c2, _ := newClient(t, f2, func(cfg *Config) { cfg.NoRetries = true })
	if resp, _ := c2.Do(context.Background(), "GET", "x"); resp.Status != 503 || f2.count() != 1 {
		t.Errorf("NoRetries: %d calls", f2.count())
	}
	f3 := newFake(t, failing(99, 503))
	c3, _ := newClient(t, f3, func(cfg *Config) { cfg.MaxRetries = 4 })
	c3.Do(context.Background(), "GET", "x")
	if f3.count() != 5 {
		t.Errorf("MaxRetries=4 made %d calls", f3.count())
	}
}

func TestBackoffGrowsAndHonorsRetryAfter(t *testing.T) {
	f := newFake(t, func(n int, r *http.Request) (int, string, map[string]string) {
		switch n {
		case 1:
			return 429, "", map[string]string{"Retry-After": "3"}
		case 2:
			return 503, "", map[string]string{"Retry-After": "9999"}
		}
		return 200, "{}", nil
	})
	c, sleeps := newClient(t, f, func(cfg *Config) { cfg.MaxRetries = 3; cfg.Backoff = 10 * time.Millisecond })
	if resp, err := c.Do(context.Background(), "GET", "x"); err != nil || resp.Status != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	s := *sleeps
	if len(s) != 2 || s[0] != 3*time.Second || s[1] != 5*time.Second {
		t.Errorf("sleeps = %v; want Retry-After honored (3s) and capped (5s)", s)
	}

	// Without Retry-After, delays grow and are jittered within 50-100% of the step.
	f2 := newFake(t, func(int, *http.Request) (int, string, map[string]string) { return 503, "", nil })
	c2, sleeps2 := newClient(t, f2, func(cfg *Config) { cfg.MaxRetries = 6; cfg.Backoff = 100 * time.Millisecond })
	c2.Do(context.Background(), "GET", "x")
	for i, d := range *sleeps2 {
		step := 100 * time.Millisecond << i
		if step > 2*time.Second {
			step = 2 * time.Second
		}
		if d < step/2 || d > step {
			t.Errorf("sleep %d = %v, want between %v and %v", i, d, step/2, step)
		}
	}
	if got := retryAfter(http.Header{"Retry-After": {time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)}}); got <= 0 || got > 3*time.Second {
		t.Errorf("Retry-After as a date = %v", got)
	}
	if retryAfter(http.Header{"Retry-After": {"soon"}}) != 0 || retryAfter(http.Header{"Retry-After": {"-5"}}) != 0 {
		t.Error("an invalid Retry-After must be ignored")
	}
}

func TestTransportFailuresTimeoutsAndCancellation(t *testing.T) {
	// Nothing listening: unavailable, retried for a GET, sent once for a write; the address is not echoed.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	c, err := New(Config{BaseURL: "http://" + addr})
	if err != nil {
		t.Fatal(err)
	}
	var sleeps int
	c.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }
	_, err = c.Do(context.Background(), "GET", "x")
	var ce *Error
	if !errors.Is(err, ErrUnavailable) || !errors.As(err, &ce) || ce.Attempts != 3 || sleeps != 2 || strings.Contains(err.Error(), addr) {
		t.Errorf("GET: %v (attempts %v, sleeps %d)", err, ce, sleeps)
	}
	_, err = c.Do(context.Background(), "POST", "x")
	if !errors.As(err, &ce) || ce.Attempts != 1 {
		t.Errorf("POST: %v", err)
	}

	// An attempt that takes too long is a timeout; a safe call retries it.
	slow := newFake(t, func(n int, r *http.Request) (int, string, map[string]string) {
		if n == 1 {
			time.Sleep(300 * time.Millisecond)
		}
		return 200, "{}", nil
	})
	c2, _ := newClient(t, slow, func(cfg *Config) { cfg.Timeout = 80 * time.Millisecond })
	resp, err := c2.Do(context.Background(), "GET", "x")
	if err != nil || resp.Attempts != 2 {
		t.Errorf("timeout then success: %+v %v", resp, err)
	}
	always := newFake(t, func(int, *http.Request) (int, string, map[string]string) {
		time.Sleep(300 * time.Millisecond)
		return 200, "{}", nil
	})
	c3, _ := newClient(t, always, func(cfg *Config) { cfg.Timeout = 50 * time.Millisecond })
	if _, err := c3.Do(context.Background(), "POST", "x"); !errors.Is(err, ErrTimeout) {
		t.Errorf("a write that times out must say so, not pretend: %v", err)
	}
	if _, err := c3.Do(context.Background(), "POST", "x", Timeout(2*time.Second)); err != nil {
		t.Errorf("a per-call timeout override: %v", err)
	}

	// The caller's context ends the call, and the error says why.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	if _, err := c3.Do(ctx, "GET", "x", Timeout(time.Second)); !errors.Is(err, ErrCanceled) {
		t.Errorf("cancelled: %v", err)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer dcancel()
	if _, err := c3.Do(dctx, "GET", "x", Timeout(time.Second)); !errors.Is(err, ErrTimeout) {
		t.Errorf("deadline: %v", err)
	}
	// An already-cancelled context sends nothing.
	cc, cancel2 := context.WithCancel(context.Background())
	cancel2()
	before := always.count()
	if _, err := c3.Do(cc, "GET", "x"); !errors.Is(err, ErrCanceled) || always.count() != before {
		t.Errorf("pre-cancelled: %v", err)
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	target := newFake(t, okScript())
	redirector := httptest.NewServer(http.RedirectHandler(target.URL+"/elsewhere", http.StatusFound))
	defer redirector.Close()
	c, _ := New(Config{BaseURL: redirector.URL, Headers: map[string]string{"X-Secret-Ish": "v"}})
	resp, err := c.Do(context.Background(), "GET", "x")
	if err != nil || resp.Status != 302 || target.count() != 0 {
		t.Errorf("a redirect must be returned, not followed (and headers must not travel): %+v %v hits=%d", resp, err, target.count())
	}
	// A caller-supplied client is copied: its other settings are kept for the copy, but redirects stay off.
	hc := &http.Client{Timeout: time.Hour}
	c2, _ := New(Config{BaseURL: redirector.URL, HTTPClient: hc})
	if resp, _ := c2.Do(context.Background(), "GET", "x"); resp.Status != 302 {
		t.Errorf("custom client followed a redirect: %d", resp.Status)
	}
	if hc.CheckRedirect != nil || hc.Timeout != time.Hour {
		t.Error("the caller's http.Client must not be modified")
	}
}

func TestResponseSizeLimit(t *testing.T) {
	f := newFake(t, func(int, *http.Request) (int, string, map[string]string) { return 200, strings.Repeat("x", 1000), nil })
	c, _ := newClient(t, f, func(cfg *Config) { cfg.MaxResponseSize = 100 })
	_, err := c.Do(context.Background(), "GET", "x")
	if !errors.Is(err, ErrResponseTooLarge) || f.count() != 1 {
		t.Errorf("err=%v calls=%d (an oversized response is not retried)", err, f.count())
	}
}

func TestJSONHelper(t *testing.T) {
	f := newFake(t, func(n int, r *http.Request) (int, string, map[string]string) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/created"):
			return 201, `{"id":"o1","extra":"ignored"}`, nil
		case strings.HasSuffix(r.URL.Path, "/empty"):
			return 204, "", nil
		case strings.HasSuffix(r.URL.Path, "/missing"):
			return 404, `{"error":"no such order","detail":"` + strings.Repeat("d", 6000) + `"}`, nil
		case strings.HasSuffix(r.URL.Path, "/html"):
			return 200, `<html>`, nil
		}
		return 200, `{"id":"o9"}`, nil
	})
	c, _ := newClient(t, f, nil)
	var out struct{ ID string }
	if err := c.JSON(context.Background(), "POST", "created", map[string]string{"a": "b"}, &out); err != nil || out.ID != "o1" {
		t.Errorf("%+v %v", out, err)
	}
	if f.last().body != `{"a":"b"}` || f.last().header.Get("Content-Type") != "application/json" {
		t.Errorf("request body = %q", f.last().body)
	}
	if err := c.JSON(context.Background(), "DELETE", "empty", nil, &out); err != nil {
		t.Errorf("204: %v", err)
	}
	if err := c.JSON(context.Background(), "GET", "x", nil, nil); err != nil {
		t.Errorf("no out: %v", err)
	}
	err := c.JSON(context.Background(), "GET", "missing", nil, &out)
	var se *StatusError
	if !errors.Is(err, ErrStatus) || !errors.As(err, &se) || se.Status != 404 || len(se.Body) != 4096 || !strings.Contains(string(se.Body), "no such order") {
		t.Errorf("status error: %v %+v", err, se)
	}
	if strings.Contains(err.Error(), "no such order") || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), se.RequestID) || se.Target != "/gw/orders" {
		t.Errorf("the error text must not include the body: %q", err.Error())
	}
	err = c.JSON(context.Background(), "GET", "html", nil, &out)
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "<html>") {
		t.Errorf("a response that is not JSON: %v", err)
	}
	// Options passed to JSON are kept, and the caller's slice is not modified.
	opts := []Option{IdempotencyKey("k")}
	c.JSON(context.Background(), "POST", "x", 1, nil, opts...)
	if len(opts) != 1 || f.last().header.Get("X-Idempotency-Key") != "k" {
		t.Errorf("opts=%d key=%q", len(opts), f.last().header.Get("X-Idempotency-Key"))
	}
	// A 5xx after retries is an error from JSON but a Response from Do.
	f5 := newFake(t, func(int, *http.Request) (int, string, map[string]string) { return 503, `{"error":"down"}`, nil })
	c5, _ := newClient(t, f5, nil)
	if err := c5.JSON(context.Background(), "GET", "x", nil, nil); !errors.As(err, &se) || se.Status != 503 {
		t.Errorf("503: %v", err)
	}
}

func TestSignHookRunsOnEveryAttempt(t *testing.T) {
	f := newFake(t, func(n int, r *http.Request) (int, string, map[string]string) {
		if n == 1 {
			return 503, "", nil
		}
		return 200, "{}", nil
	})
	var bodies []string
	c, _ := newClient(t, f, func(cfg *Config) {
		cfg.Sign = func(req *http.Request, body []byte) error {
			bodies = append(bodies, string(body))
			req.Header.Set("X-Signature", "sig-for-"+string(body))
			return nil
		}
	})
	if _, err := c.Do(context.Background(), "PUT", "x", JSONBody(7), IdempotencyKey("k")); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || bodies[0] != "7" || f.last().header.Get("X-Signature") != "sig-for-7" {
		t.Errorf("bodies=%v sig=%q", bodies, f.last().header.Get("X-Signature"))
	}
	bad, _ := newClient(t, f, func(cfg *Config) {
		cfg.Sign = func(*http.Request, []byte) error { return errors.New("key store password hunter2") }
	})
	before := f.count()
	_, err := bad.Do(context.Background(), "GET", "x")
	if !errors.Is(err, ErrInvalidRequest) || strings.Contains(err.Error(), "hunter2") || f.count() != before {
		t.Errorf("a failing signer must stop the call without leaking: %v", err)
	}
}

func TestErrorTextNeverContainsTheURLOrHeaders(t *testing.T) {
	c, _ := New(Config{BaseURL: "http://127.0.0.1:1", PathPrefix: "/gw"})
	c.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := c.Do(reqctx.WithRequestID(context.Background(), "rid-9"), "GET", Path("secret-id-123"), Header("X-Token", "hunter2"), Query(url.Values{"k": {"secret-q"}}))
	if err == nil || strings.Contains(err.Error(), "secret-id-123") || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "secret-q") || !strings.Contains(err.Error(), "rid-9") {
		t.Errorf("err = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"empty":           {},
		"no scheme":       {BaseURL: "gateway.internal"},
		"ftp":             {BaseURL: "ftp://gw"},
		"no host":         {BaseURL: "http://"},
		"credentials":     {BaseURL: "https://u:secretpw@gw"},
		"query":           {BaseURL: "https://gw?x=1"},
		"fragment":        {BaseURL: "https://gw#x"},
		"bad segment":     {BaseURL: "https://gw", AllowedPrefixes: []string{"Pay"}},
		"negative":        {BaseURL: "https://gw", Timeout: -1},
		"prefix no slash": {BaseURL: "https://gw", PathPrefix: "api"},
		"prefix dots":     {BaseURL: "https://gw", PathPrefix: "/a/../b"},
		"prefix trailing": {BaseURL: "https://gw", PathPrefix: "/api/"},
		"prefix query":    {BaseURL: "https://gw", PathPrefix: "/p?x"},
	} {
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if strings.Contains(err.Error(), "secretpw") {
			t.Errorf("%s: leaks the password", name)
		}
		if _, nerr := New(cfg); nerr == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	for _, good := range []Config{{BaseURL: "https://gw.internal"}, {BaseURL: "http://localhost:8080/base"}, {BaseURL: "https://p", PathPrefix: "/api/v1/gw", AllowedPrefixes: []string{"a", "b-2"}}} {
		if err := good.Validate(); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
}

func TestConcurrentCalls(t *testing.T) {
	f := newFake(t, okScript())
	c, _ := New(Config{BaseURL: f.URL})
	pay, _ := c.WithPrefix("payments")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := pay.Do(context.Background(), "GET", Path("x", "y")); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.count() != 20 {
		t.Errorf("calls = %d", f.count())
	}
}
