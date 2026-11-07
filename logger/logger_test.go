package logger_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/Wetech-Code/go-common-utilities/logger"
)

func TestJSONOutputAndLevel(t *testing.T) {
	var buf bytes.Buffer
	log, err := logger.New(logger.Config{Level: "WARN", Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown", "port", 8080)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("output is not one JSON object: %q: %v", buf.String(), err)
	}
	if rec["msg"] != "shown" || rec["level"] != "WARN" || rec["port"] != float64(8080) {
		t.Errorf("unexpected record %v", rec)
	}
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	log, err := logger.New(logger.Config{Format: "text", Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello")
	if !strings.Contains(buf.String(), "msg=hello") {
		t.Errorf("got %q", buf.String())
	}
}

func TestRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	log, _ := logger.New(logger.Config{Output: &buf})
	log.Info("login", "user", "ann", "password", "hunter2", "Authorization", "Bearer abc",
		slog.Group("db", slog.String("api_key", "k-123")))
	log.Info("more", "API-Key", "k-456", "private.key", "pem-789", "DB_DSN", "postgres://u:dsn-pw@h/d", "clientSecret", "cs-000")
	out := buf.String()
	for _, leaked := range []string{"hunter2", "Bearer abc", "k-123", "k-456", "pem-789", "dsn-pw", "cs-000"} {
		if strings.Contains(out, leaked) {
			t.Errorf("secret %q leaked: %s", leaked, out)
		}
	}
	if !strings.Contains(out, `"user":"ann"`) || !strings.Contains(out, "[REDACTED]") {
		t.Errorf("unexpected output %s", out)
	}
}

func TestInvalidConfig(t *testing.T) {
	if _, err := logger.New(logger.Config{Level: "loud"}); err == nil {
		t.Error("want error for bad level")
	}
	if _, err := logger.New(logger.Config{Format: "xml"}); err == nil {
		t.Error("want error for bad format")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelInfo, "Debug": slog.LevelDebug, "warning": slog.LevelWarn, "error": slog.LevelError} {
		got, err := logger.ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
}
