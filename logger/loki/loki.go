// Package loki is a log/slog handler that ships records to Grafana Loki over its
// HTTP push API. It needs nothing but the standard library.
//
// Records are queued, batched and sent from a background goroutine, so logging
// never waits for the network. If Loki is slow or down and the queue fills up,
// new records are dropped (and counted) rather than blocking the application.
// Call Close on shutdown to flush what is queued.
//
//	h, err := loki.New(loki.Config{
//		URL:    "http://loki.internal:3100",
//		Labels: map[string]string{"service": "api", "env": "prod"},
//	})
//	if err != nil {
//		return err
//	}
//	defer h.Close(ctx)
//	log, _ := logger.New(logger.Config{Extra: []slog.Handler{h}}) // console + Loki
//
// Each record becomes one Loki log line holding the message and attributes as
// JSON, in a stream labelled with Config.Labels plus "level". Keep labels few
// and stable (Loki indexes them); put everything else in attributes, where
// LogQL's "| json" reads them.
//
// Sensitive attribute values are hidden by this handler itself, using the same
// rule as package logger.
package loki

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wetech-Code/go-common-utilities/logger"
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultBatchSize  = 100
	DefaultBatchWait  = time.Second
	DefaultQueueSize  = 10000
	DefaultTimeout    = 10 * time.Second
	DefaultMaxRetries = 3
	pushPath          = "/loki/api/v1/push"
)

// Config describes the connection to Loki.
type Config struct {
	// URL is the base URL of Loki, such as "http://loki.internal:3100" or
	// "https://logs-prod.grafana.net". The push path is added for you. It must
	// not contain credentials.
	URL string
	// Labels are the static stream labels sent with every record, such as
	// service and env. At least one is required. Names must match
	// [a-zA-Z_][a-zA-Z0-9_]* and values must not be empty. "level" is added per
	// record and must not be set here.
	Labels map[string]string
	// TenantID is sent as X-Scope-OrgID for multi-tenant Loki.
	TenantID string
	// Username and Password enable HTTP basic auth (Grafana Cloud uses the user
	// ID and an access policy token). BearerToken sets an Authorization: Bearer
	// header instead. Use at most one of the two. Prefer https:// when sending
	// credentials.
	Username, Password string
	BearerToken        string
	// Level is the lowest level sent. Default info.
	Level slog.Leveler
	// AddSource adds the calling file and line to each log line.
	AddSource bool
	// BatchSize is the number of records that triggers a send. BatchWait is the
	// longest a record waits before being sent. QueueSize bounds the records
	// held while sending; beyond it, records are dropped. Timeout bounds each
	// request. MaxRetries is the number of retries (not counting the first
	// attempt) after a network error, 429 or 5xx response.
	BatchSize  int
	BatchWait  time.Duration
	QueueSize  int
	Timeout    time.Duration
	MaxRetries int
	// ContextAttrs, if set, adds attributes taken from the record's context to
	// each log line, for example otlp.TraceAttrs so Grafana can link a log to
	// its trace.
	ContextAttrs func(context.Context) []slog.Attr
	// HTTPClient sends the requests. Default: a private client with the
	// standard certificate checks. A client you supply is not closed.
	HTTPClient *http.Client
	// OnError is told about batches that could not be delivered. Default: print
	// to standard error.
	OnError func(error)
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// Validate reports problems with the configuration without connecting.
// Errors never include the credentials.
func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("loki: URL must be an http:// or https:// URL")
	}
	if u.User != nil {
		return errors.New("loki: URL must not contain credentials; use Username and Password")
	}
	if len(c.Labels) == 0 {
		return errors.New("loki: at least one label is required, for example service")
	}
	for k, v := range c.Labels {
		switch {
		case !labelName.MatchString(k):
			return fmt.Errorf("loki: invalid label name %q", k)
		case k == "level":
			return errors.New(`loki: label "level" is set per record; remove it from Labels`)
		case v == "":
			return fmt.Errorf("loki: label %q has an empty value", k)
		}
	}
	if c.BearerToken != "" && (c.Username != "" || c.Password != "") {
		return errors.New("loki: use either BearerToken or Username and Password, not both")
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("loki: Username and Password must be set together")
	}
	if c.BatchSize < 0 || c.BatchWait < 0 || c.QueueSize < 0 || c.Timeout < 0 || c.MaxRetries < 0 {
		return errors.New("loki: BatchSize, BatchWait, QueueSize, Timeout and MaxRetries must not be negative")
	}
	return nil
}

type entry struct {
	ts    time.Time
	level string
	line  string
}

// core is shared by a Handler and every handler derived from it by WithAttrs or
// WithGroup.
type core struct {
	cfg      Config
	endpoint string
	client   *http.Client
	ownTr    *http.Transport // non-nil when we created the client

	// fmtMu serializes formatting: the JSON handlers of all clones write into buf.
	fmtMu sync.Mutex
	buf   bytes.Buffer

	mu      sync.RWMutex // guards closed and the send on queue
	closed  bool
	queue   chan entry
	done    chan struct{}
	dropped atomic.Uint64
	sent    atomic.Uint64
}

func (c *core) Write(p []byte) (int, error) { return c.buf.Write(p) }

// Handler is a slog.Handler that ships records to Loki.
type Handler struct {
	core  *core
	json  slog.Handler
	level slog.Leveler
	ctxFn func(context.Context) []slog.Attr
}

// New validates cfg, starts the background sender and returns the handler. It
// does not contact Loki; delivery problems are reported through OnError. The
// caller must call Close.
func New(cfg Config) (*Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	if cfg.BatchWait == 0 {
		cfg.BatchWait = DefaultBatchWait
	}
	if cfg.QueueSize == 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.OnError == nil {
		cfg.OnError = func(err error) { fmt.Fprintln(os.Stderr, err) }
	}
	if cfg.Level == nil {
		cfg.Level = slog.LevelInfo
	}

	c := &core{
		cfg:      cfg,
		endpoint: strings.TrimRight(cfg.URL, "/") + pushPath,
		queue:    make(chan entry, cfg.QueueSize),
		done:     make(chan struct{}),
	}
	if cfg.HTTPClient != nil {
		c.client = cfg.HTTPClient
	} else {
		c.ownTr = http.DefaultTransport.(*http.Transport).Clone()
		c.client = &http.Client{Transport: c.ownTr, Timeout: cfg.Timeout}
	}
	go c.run()

	// The time is sent as the entry's timestamp, so it is left out of the line.
	opts := &slog.HandlerOptions{
		Level:     slog.Level(-1 << 20),
		AddSource: cfg.AddSource,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}
	return &Handler{core: c, json: logger.Redact(slog.NewJSONHandler(c, opts)), level: cfg.Level, ctxFn: cfg.ContextAttrs}, nil
}

// Enabled reports whether level is at or above Config.Level.
func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

// Handle queues the record. It never blocks on the network, and returns an
// error only when the record was dropped because the handler is closed or the
// queue is full.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if h.ctxFn != nil {
		if attrs := h.ctxFn(ctx); len(attrs) > 0 {
			r = r.Clone()
			r.AddAttrs(attrs...)
		}
	}
	c := h.core
	c.fmtMu.Lock()
	c.buf.Reset()
	err := h.json.Handle(ctx, r)
	line := strings.TrimSuffix(c.buf.String(), "\n")
	c.fmtMu.Unlock()
	if err != nil {
		return fmt.Errorf("loki: format record: %w", err)
	}

	ts := r.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	e := entry{ts: ts, level: levelName(r.Level), line: line}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		c.dropped.Add(1)
		return errors.New("loki: handler is closed")
	}
	select {
	case c.queue <- e:
		return nil
	default:
		c.dropped.Add(1)
		return errors.New("loki: queue full, record dropped")
	}
}

// WithAttrs returns a handler whose records carry attrs.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.json = h.json.WithAttrs(attrs)
	return &n
}

// WithGroup returns a handler that nests later attributes under name.
func (h *Handler) WithGroup(name string) slog.Handler {
	n := *h
	n.json = h.json.WithGroup(name)
	return &n
}

// Dropped is the number of records discarded because the queue was full or the
// handler was closed.
func (h *Handler) Dropped() uint64 { return h.core.dropped.Load() }

// Sent is the number of records Loki has accepted.
func (h *Handler) Sent() uint64 { return h.core.sent.Load() }

// Close stops accepting records, sends everything queued and waits for the
// sender to finish or ctx to end, whichever comes first. It is safe to call
// more than once. Records still queued when ctx ends are lost.
func (h *Handler) Close(ctx context.Context) error {
	c := h.core
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
	c.mu.Unlock()
	select {
	case <-c.done:
		if c.ownTr != nil {
			c.ownTr.CloseIdleConnections()
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("loki: close: %w", ctx.Err())
	}
}

func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	}
	return "error"
}

// run batches queued entries and sends them until the queue is closed.
func (c *core) run() {
	defer close(c.done)
	batch := make([]entry, 0, c.cfg.BatchSize)
	timer := time.NewTimer(c.cfg.BatchWait)
	defer timer.Stop()
	flush := func() {
		if len(batch) > 0 {
			c.send(batch)
			batch = batch[:0]
		}
	}
	for {
		select {
		case e, ok := <-c.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, e)
			if len(batch) >= c.cfg.BatchSize {
				flush()
				resetTimer(timer, c.cfg.BatchWait)
			}
		case <-timer.C:
			flush()
			timer.Reset(c.cfg.BatchWait)
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

func (c *core) payload(batch []entry) ([]byte, error) {
	byLevel := map[string]*stream{}
	var order []string
	for _, e := range batch {
		s, ok := byLevel[e.level]
		if !ok {
			labels := make(map[string]string, len(c.cfg.Labels)+1)
			for k, v := range c.cfg.Labels {
				labels[k] = v
			}
			labels["level"] = e.level
			s = &stream{Stream: labels}
			byLevel[e.level] = s
			order = append(order, e.level)
		}
		s.Values = append(s.Values, [2]string{strconv.FormatInt(e.ts.UnixNano(), 10), e.line})
	}
	streams := make([]*stream, 0, len(order))
	for _, l := range order {
		streams = append(streams, byLevel[l])
	}
	var raw bytes.Buffer
	zw := gzip.NewWriter(&raw)
	if err := json.NewEncoder(zw).Encode(map[string]any{"streams": streams}); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

func (c *core) send(batch []entry) {
	body, err := c.payload(batch)
	if err != nil {
		c.cfg.OnError(fmt.Errorf("loki: encode batch of %d records: %w", len(batch), err))
		return
	}
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(backoff(attempt))
		}
		retry, err := c.post(body)
		if err == nil {
			c.sent.Add(uint64(len(batch)))
			return
		}
		lastErr = err
		if !retry {
			break
		}
	}
	c.dropped.Add(uint64(len(batch)))
	c.cfg.OnError(fmt.Errorf("loki: dropped %d records: %w", len(batch), lastErr))
}

func backoff(attempt int) time.Duration {
	d := 200 * time.Millisecond << (attempt - 1)
	return min(d, 5*time.Second)
}

// post sends one request. retry reports whether the failure may be transient.
func (c *core) post(body []byte) (retry bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	if c.cfg.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", c.cfg.TenantID)
	}
	switch {
	case c.cfg.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+c.cfg.BearerToken)
	case c.cfg.Username != "":
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// url.Error would repeat the URL, which holds no credentials, so it is safe.
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		io.Copy(io.Discard, resp.Body)
		return false, nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	err = fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, err
}
