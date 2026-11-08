package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/Wetech-Code/go-common-utilities/logger"
)

// capture is a handler that remembers the records it receives, flattened to JSON.
type capture struct {
	mu   *sync.Mutex
	buf  *bytes.Buffer
	next slog.Handler
}

func newCapture(level slog.Level) *capture {
	buf := &bytes.Buffer{}
	return &capture{mu: &sync.Mutex{}, buf: buf, next: slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level})}
}

func (c *capture) Enabled(ctx context.Context, l slog.Level) bool { return c.next.Enabled(ctx, l) }
func (c *capture) Handle(ctx context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.next.Handle(ctx, r)
}
func (c *capture) WithAttrs(a []slog.Attr) slog.Handler {
	return &capture{c.mu, c.buf, c.next.WithAttrs(a)}
}
func (c *capture) WithGroup(n string) slog.Handler { return &capture{c.mu, c.buf, c.next.WithGroup(n)} }
func (c *capture) String() string                  { c.mu.Lock(); defer c.mu.Unlock(); return c.buf.String() }

func TestExtraReceivesEveryRecordAndConsoleStillWorks(t *testing.T) {
	var console bytes.Buffer
	extra := newCapture(slog.LevelDebug)
	log, err := logger.New(logger.Config{Output: &console, Extra: []slog.Handler{extra}})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "port", 8080)
	log.With("req", "r1").WithGroup("g").Warn("grouped", "k", "v")

	if !strings.Contains(console.String(), `"msg":"hello"`) {
		t.Errorf("console lost the record: %q", console.String())
	}
	lines := strings.Split(strings.TrimSpace(extra.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("extra got %d records: %q", len(lines), extra.String())
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	g, _ := second["g"].(map[string]any)
	if second["req"] != "r1" || g["k"] != "v" || second["level"] != "WARN" {
		t.Errorf("second record = %v", second)
	}
}

func TestLevelAppliesToExtraHandlersToo(t *testing.T) {
	extra := newCapture(slog.LevelDebug) // would accept everything on its own
	log, _ := logger.New(logger.Config{Level: "warn", Output: &bytes.Buffer{}, Extra: []slog.Handler{extra}})
	log.Info("quiet")
	log.Error("loud")
	if strings.Contains(extra.String(), "quiet") || !strings.Contains(extra.String(), "loud") {
		t.Errorf("extra saw %q", extra.String())
	}
}

func TestSecretsNeverReachExtraHandlers(t *testing.T) {
	extra := newCapture(slog.LevelDebug)
	log, _ := logger.New(logger.Config{Output: &bytes.Buffer{}, Extra: []slog.Handler{extra}})
	log.With("api_key", "k-with").Info("login", "password", "hunter2",
		slog.Group("db", slog.String("dsn", "postgres://u:pw@h/d"), slog.Group("deep", slog.String("Token", "t-deep"))),
		slog.Group("credentials", slog.String("user", "x")))
	log.WithGroup("outer").Info("grouped", "client-secret", "cs")
	out := extra.String()
	for _, leaked := range []string{"hunter2", "k-with", "pw@h", "t-deep", `"user":"x"`, `"cs"`} {
		if strings.Contains(out, leaked) {
			t.Errorf("%q leaked to the extra handler: %s", leaked, out)
		}
	}
	if strings.Count(out, "[REDACTED]") < 5 {
		t.Errorf("expected redaction markers: %s", out)
	}
}

func TestRedactIsUsableOnItsOwn(t *testing.T) {
	extra := newCapture(slog.LevelDebug)
	slog.New(logger.Redact(extra)).Info("x", "secret", "s3cr3t", "name", "ann")
	if strings.Contains(extra.String(), "s3cr3t") || !strings.Contains(extra.String(), "ann") {
		t.Errorf("got %s", extra.String())
	}
	// Enabled follows the wrapped handler.
	if logger.Redact(newCapture(slog.LevelError)).Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Redact must not enable levels the wrapped handler disables")
	}
}

func TestRedactAttrMatchesTheConsoleRule(t *testing.T) {
	if got := logger.RedactAttr(nil, slog.String("Password", "x")); got.Value.String() != "[REDACTED]" {
		t.Errorf("got %v", got)
	}
	if got := logger.RedactAttr(nil, slog.String("name", "x")); got.Value.String() != "x" {
		t.Errorf("got %v", got)
	}
}

type ctxKey struct{}

func TestContextAttrsAreAddedToTheConsole(t *testing.T) {
	var console bytes.Buffer
	log, _ := logger.New(logger.Config{Output: &console, ContextAttrs: func(ctx context.Context) []slog.Attr {
		if v, ok := ctx.Value(ctxKey{}).(string); ok {
			return []slog.Attr{slog.String("request_id", v)}
		}
		return nil
	}})
	log.InfoContext(context.WithValue(context.Background(), ctxKey{}, "r-7"), "with")
	log.Info("without")
	log.With("a", 1).WithGroup("g").InfoContext(context.WithValue(context.Background(), ctxKey{}, "r-8"), "derived", "k", "v")
	out := console.String()
	if !strings.Contains(out, `"request_id":"r-7"`) || strings.Count(out, "request_id") != 2 {
		t.Errorf("console = %s", out)
	}
	if h := logger.WithContextAttrs(slog.Default().Handler(), nil); h != slog.Default().Handler() {
		t.Error("a nil function must leave the handler unchanged")
	}
}

func TestNilExtraIsRejected(t *testing.T) {
	if _, err := logger.New(logger.Config{Extra: []slog.Handler{nil}}); err == nil {
		t.Error("want error for a nil Extra handler")
	}
}
