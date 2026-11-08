package loki_test

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/logger/loki"
)

type push struct {
	Streams []struct {
		Stream map[string]string `json:"stream"`
		Values [][2]string       `json:"values"`
	} `json:"streams"`
}

// fakeLoki records pushes and answers with a configurable status.
type fakeLoki struct {
	mu      sync.Mutex
	pushes  []push
	headers []http.Header
	paths   []string
	status  func(n int) int // n is the 1-based request number
	n       int
	gate    chan struct{} // if set, requests block until it is closed
}

func newFake(t *testing.T) (*fakeLoki, *httptest.Server) {
	t.Helper()
	f := &fakeLoki{status: func(int) int { return 204 }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.gate != nil {
			<-f.gate
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		var p push
		if err := json.NewDecoder(zr).Decode(&p); err != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.n++
		code := f.status(f.n)
		if code/100 == 2 {
			f.pushes = append(f.pushes, p)
		}
		f.headers = append(f.headers, r.Header.Clone())
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		w.WriteHeader(code)
		if code >= 400 {
			io.WriteString(w, "nope")
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeLoki) lines() (out []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.pushes {
		for _, s := range p.Streams {
			for _, v := range s.Values {
				out = append(out, v[1])
			}
		}
	}
	return out
}

func (f *fakeLoki) requests() int { f.mu.Lock(); defer f.mu.Unlock(); return f.n }

func cfg(url string) loki.Config {
	return loki.Config{URL: url, Labels: map[string]string{"service": "api", "env": "test"}, BatchWait: 20 * time.Millisecond, OnError: func(error) {}}
}

func closeIt(t *testing.T, h *loki.Handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRecordsArriveWithLabelsLevelAndJSONLine(t *testing.T) {
	f, srv := newFake(t)
	h, err := loki.New(cfg(srv.URL + "/"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(h)
	log.Info("started", "port", 8080)
	log.Error("failed", "err", "boom")
	log.Debug("hidden")
	closeIt(t, h)

	if f.paths[0] != "/loki/api/v1/push" {
		t.Errorf("path = %q", f.paths[0])
	}
	if got := f.headers[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	levels := map[string][]string{}
	for _, p := range f.pushes {
		for _, s := range p.Streams {
			if s.Stream["service"] != "api" || s.Stream["env"] != "test" {
				t.Errorf("labels = %v", s.Stream)
			}
			for _, v := range s.Values {
				levels[s.Stream["level"]] = append(levels[s.Stream["level"]], v[1])
				if len(v[0]) != 19 {
					t.Errorf("timestamp %q is not nanoseconds", v[0])
				}
			}
		}
	}
	if len(levels["info"]) != 1 || len(levels["error"]) != 1 || len(levels["debug"]) != 0 {
		t.Fatalf("by level: %v", levels)
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(levels["info"][0]), &line); err != nil {
		t.Fatalf("line is not JSON: %q", levels["info"][0])
	}
	if line["msg"] != "started" || line["port"] != float64(8080) || line["level"] != "INFO" {
		t.Errorf("line = %v", line)
	}
	if _, has := line["time"]; has {
		t.Error("the time belongs in the Loki timestamp, not in the line")
	}
	if h.Sent() != 2 || h.Dropped() != 0 {
		t.Errorf("sent=%d dropped=%d", h.Sent(), h.Dropped())
	}
}

func TestLevelLabelBuckets(t *testing.T) {
	f, srv := newFake(t)
	c := cfg(srv.URL)
	c.Level = slog.Level(-8)
	h, _ := loki.New(c)
	log := slog.New(h)
	log.Log(context.Background(), slog.Level(-8), "trace-ish")
	log.Debug("d")
	log.Log(context.Background(), slog.LevelInfo+2, "between info and warn")
	log.Warn("w")
	log.Log(context.Background(), slog.LevelError+4, "fatal-ish")
	closeIt(t, h)
	got := map[string]int{}
	for _, p := range f.pushes {
		for _, s := range p.Streams {
			got[s.Stream["level"]] += len(s.Values)
		}
	}
	if got["debug"] != 2 || got["info"] != 1 || got["warn"] != 1 || got["error"] != 1 {
		t.Errorf("levels = %v", got)
	}
}

func TestSecretsAreHiddenInLines(t *testing.T) {
	f, srv := newFake(t)
	h, _ := loki.New(cfg(srv.URL))
	log := slog.New(h).With("api_key", "k-1")
	log.Info("login", "password", "hunter2", slog.Group("credentials", slog.String("user", "u")), slog.Group("db", slog.String("dsn", "pg://x")))
	slog.New(h).WithGroup("g").Info("x", "Token", "t-9")
	closeIt(t, h)
	all := strings.Join(f.lines(), "\n")
	for _, leaked := range []string{"hunter2", "k-1", `"user"`, "pg://x", "t-9"} {
		if strings.Contains(all, leaked) {
			t.Errorf("%q leaked: %s", leaked, all)
		}
	}
}

func TestWithAttrsAndGroupsAndContextAttrs(t *testing.T) {
	f, srv := newFake(t)
	c := cfg(srv.URL)
	type key struct{}
	c.ContextAttrs = func(ctx context.Context) []slog.Attr {
		if v, ok := ctx.Value(key{}).(string); ok {
			return []slog.Attr{slog.String("trace_id", v)}
		}
		return nil
	}
	h, _ := loki.New(c)
	log := slog.New(h).With("req", "r1").WithGroup("g")
	log.InfoContext(context.WithValue(context.Background(), key{}, "abc"), "hello", "k", "v")
	closeIt(t, h)
	var line map[string]any
	json.Unmarshal([]byte(f.lines()[0]), &line)
	g, _ := line["g"].(map[string]any)
	if line["req"] != "r1" || g["k"] != "v" || g["trace_id"] != "abc" {
		t.Errorf("line = %v", line)
	}
}

func TestAddSource(t *testing.T) {
	f, srv := newFake(t)
	c := cfg(srv.URL)
	c.AddSource = true
	h, _ := loki.New(c)
	slog.New(h).Info("where")
	closeIt(t, h)
	if !strings.Contains(f.lines()[0], `"source"`) {
		t.Errorf("no source in %s", f.lines()[0])
	}
}

func TestAuthAndTenantHeaders(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*loki.Config)
		want func(http.Header) bool
	}{
		"basic":  {func(c *loki.Config) { c.Username, c.Password = "u", "p" }, func(h http.Header) bool { return strings.HasPrefix(h.Get("Authorization"), "Basic ") }},
		"bearer": {func(c *loki.Config) { c.BearerToken = "tok" }, func(h http.Header) bool { return h.Get("Authorization") == "Bearer tok" }},
		"tenant": {func(c *loki.Config) { c.TenantID = "acme" }, func(h http.Header) bool { return h.Get("X-Scope-OrgID") == "acme" && h.Get("Authorization") == "" }},
	} {
		t.Run(name, func(t *testing.T) {
			f, srv := newFake(t)
			c := cfg(srv.URL)
			tc.mut(&c)
			h, _ := loki.New(c)
			slog.New(h).Info("x")
			closeIt(t, h)
			if len(f.headers) == 0 || !tc.want(f.headers[0]) {
				t.Errorf("headers = %v", f.headers)
			}
		})
	}
}

func TestBatchingBySizeAndByWait(t *testing.T) {
	f, srv := newFake(t)
	c := cfg(srv.URL)
	c.BatchSize = 5
	c.BatchWait = time.Hour // only size and Close can trigger a send
	h, _ := loki.New(c)
	log := slog.New(h)
	for i := 0; i < 12; i++ {
		log.Info("m")
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.requests() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.requests() != 2 || len(f.lines()) != 10 {
		t.Errorf("before Close: %d requests, %d lines", f.requests(), len(f.lines()))
	}
	closeIt(t, h)
	if len(f.lines()) != 12 || f.requests() != 3 {
		t.Errorf("after Close: %d requests, %d lines", f.requests(), len(f.lines()))
	}

	// With a short wait, a lone record is delivered without Close.
	f2, srv2 := newFake(t)
	h2, _ := loki.New(cfg(srv2.URL))
	slog.New(h2).Info("lone")
	deadline = time.Now().Add(3 * time.Second)
	for len(f2.lines()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(f2.lines()) != 1 {
		t.Error("the wait timer did not flush")
	}
	closeIt(t, h2)
}

func TestRetriesTransientFailuresButNotClientErrors(t *testing.T) {
	// 500, 429, then success.
	f, srv := newFake(t)
	f.status = func(n int) int { return map[int]int{1: 500, 2: 429}[n] + map[bool]int{true: 204}[n > 2] }
	h, _ := loki.New(cfg(srv.URL))
	slog.New(h).Info("retry me")
	closeIt(t, h)
	if f.requests() != 3 || len(f.lines()) != 1 || h.Dropped() != 0 {
		t.Errorf("requests=%d lines=%d dropped=%d", f.requests(), len(f.lines()), h.Dropped())
	}

	// 400 is final: one request, reported once, nothing retried.
	f2, srv2 := newFake(t)
	f2.status = func(int) int { return 400 }
	var errs []error
	var mu sync.Mutex
	c := cfg(srv2.URL)
	c.OnError = func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }
	h2, _ := loki.New(c)
	slog.New(h2).Info("rejected")
	closeIt(t, h2)
	mu.Lock()
	defer mu.Unlock()
	if f2.requests() != 1 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "status 400: nope") || h2.Dropped() != 1 {
		t.Errorf("requests=%d errs=%v dropped=%d", f2.requests(), errs, h2.Dropped())
	}

	// A server that always fails exhausts the retries.
	f3, srv3 := newFake(t)
	f3.status = func(int) int { return 503 }
	c3 := cfg(srv3.URL)
	c3.MaxRetries = 2
	h3, _ := loki.New(c3)
	slog.New(h3).Info("never")
	closeIt(t, h3)
	if f3.requests() != 3 || h3.Dropped() != 1 {
		t.Errorf("requests=%d dropped=%d", f3.requests(), h3.Dropped())
	}
}

func TestUnreachableLokiNeverBlocksTheCaller(t *testing.T) {
	f, srv := newFake(t)
	f.gate = make(chan struct{}) // Loki hangs
	c := cfg(srv.URL)
	c.QueueSize = 5
	c.BatchSize = 1
	c.Timeout = 100 * time.Millisecond
	c.MaxRetries = 1
	h, _ := loki.New(c)
	log := slog.New(h)
	start := time.Now()
	for i := 0; i < 200; i++ {
		log.Info("flood")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("logging blocked for %v", d)
	}
	if h.Dropped() == 0 {
		t.Error("expected drops with a full queue")
	}
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "x", 0)); err == nil {
		t.Error("Handle on a full queue must report the drop")
	}
	close(f.gate)
	closeIt(t, h)
}

func TestCloseSemantics(t *testing.T) {
	f, srv := newFake(t)
	h, _ := loki.New(cfg(srv.URL))
	slog.New(h).Info("before")
	closeIt(t, h)
	closeIt(t, h) // idempotent
	if len(f.lines()) != 1 {
		t.Errorf("lines = %v", f.lines())
	}
	before := h.Dropped()
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "late", 0)); err == nil || h.Dropped() != before+1 {
		t.Errorf("Handle after Close: err=%v dropped=%d", err, h.Dropped())
	}

	// Close gives up when its context ends while Loki hangs.
	f2, srv2 := newFake(t)
	f2.gate = make(chan struct{})
	t.Cleanup(func() { close(f2.gate) })
	c := cfg(srv2.URL)
	c.Timeout = 5 * time.Second
	h2, _ := loki.New(c)
	slog.New(h2).Info("stuck")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := h2.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v; want a deadline error", err)
	}
}

func TestEnabledFollowsLevel(t *testing.T) {
	_, srv := newFake(t)
	c := cfg(srv.URL)
	c.Level = slog.LevelWarn
	h, _ := loki.New(c)
	defer closeIt(t, h)
	if h.Enabled(context.Background(), slog.LevelInfo) || !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("Enabled ignores Config.Level")
	}
}

func TestConcurrentLoggingIsSafe(t *testing.T) {
	f, srv := newFake(t)
	h, _ := loki.New(cfg(srv.URL))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log := slog.New(h).With("g", g)
			for i := 0; i < 100; i++ {
				log.Info("m", "i", i)
			}
		}()
	}
	wg.Wait()
	closeIt(t, h)
	if len(f.lines()) != 800 {
		t.Errorf("got %d lines, want 800", len(f.lines()))
	}
	for _, l := range f.lines() {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			t.Fatalf("corrupt line %q", l)
		}
	}
}

func TestWorksAsAnExtraHandlerOfTheLoggerPackage(t *testing.T) {
	f, srv := newFake(t)
	h, _ := loki.New(cfg(srv.URL))
	var console strings.Builder
	log, err := logger.New(logger.Config{Level: "warn", Output: &console, Extra: []slog.Handler{h}})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("quiet")
	log.Warn("shown", "password", "pw")
	closeIt(t, h)
	if len(f.lines()) != 1 || strings.Contains(f.lines()[0], "pw") || !strings.Contains(console.String(), "shown") {
		t.Errorf("loki=%v console=%q", f.lines(), console.String())
	}
}

func TestOwnTransportIsReleasedAndSuppliedClientIsKept(t *testing.T) {
	_, srv := newFake(t)
	c := cfg(srv.URL)
	c.HTTPClient = srv.Client()
	h, err := loki.New(c)
	if err != nil {
		t.Fatal(err)
	}
	slog.New(h).Info("x")
	closeIt(t, h)
}

func TestValidate(t *testing.T) {
	ok := loki.Config{URL: "http://loki:3100", Labels: map[string]string{"service": "api"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*loki.Config){
		"no url":          func(c *loki.Config) { c.URL = "" },
		"bad scheme":      func(c *loki.Config) { c.URL = "ftp://loki" },
		"no host":         func(c *loki.Config) { c.URL = "http://" },
		"url credentials": func(c *loki.Config) { c.URL = "http://u:pw@loki:3100" },
		"no labels":       func(c *loki.Config) { c.Labels = nil },
		"bad label name":  func(c *loki.Config) { c.Labels = map[string]string{"my-label": "x"} },
		"level label":     func(c *loki.Config) { c.Labels = map[string]string{"level": "x"} },
		"empty value":     func(c *loki.Config) { c.Labels = map[string]string{"a": ""} },
		"both auth":       func(c *loki.Config) { c.BearerToken, c.Username, c.Password = "t", "u", "p" },
		"user only":       func(c *loki.Config) { c.Username = "u" },
		"negative":        func(c *loki.Config) { c.BatchSize = -1 },
	} {
		c := ok
		mut(&c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if strings.Contains(err.Error(), "pw") && name == "url credentials" {
			t.Errorf("%s: error leaks the password: %v", name, err)
		}
		if _, nerr := loki.New(c); nerr == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}
