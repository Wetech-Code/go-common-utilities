// Package logger builds *slog.Logger values with sensible defaults.
//
// It does not keep a global logger: New returns a logger that the caller owns
// and passes around. Attributes whose key looks sensitive (password, secret,
// token, authorization, cookie, api key, credential, dsn) are replaced with "[REDACTED]".
//
//	log, err := logger.New(logger.Config{Level: "debug", Format: "json"})
//	if err != nil {
//		return err
//	}
//	log.Info("started", "port", 8080)
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config selects how a logger is built. The zero value is valid: JSON lines at
// info level written to standard error.
type Config struct {
	// Level is "debug", "info", "warn" or "error" (any case). Default "info".
	Level string
	// Format is "json" or "text". Default "json".
	Format string
	// Output is the destination. Default os.Stderr.
	Output io.Writer
	// AddSource adds the calling file and line to every record.
	AddSource bool
}

// New returns a structured logger built from cfg. It returns an error for an
// unknown Level or Format.
func New(cfg Config) (*slog.Logger, error) {
	level, err := ParseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	out := cfg.Output
	if out == nil {
		out = os.Stderr
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: cfg.AddSource, ReplaceAttr: redact}

	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "", "json":
		return slog.New(slog.NewJSONHandler(out, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(out, opts)), nil
	default:
		return nil, fmt.Errorf("logger: unknown format %q (want json or text)", cfg.Format)
	}
}

// ParseLevel converts "debug", "info", "warn"/"warning" or "error" (any case)
// to a slog.Level. The empty string means info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("logger: unknown level %q (want debug, info, warn or error)", s)
}

// sensitive lists key fragments that cause a value to be hidden. Keys are
// lower-cased and stripped of "-", "_", "." and spaces before matching, so
// "API-Key", "api_key" and "apiKey" are all caught.
var sensitive = []string{
	"password", "passwd", "secret", "token", "authorization", "cookie",
	"apikey", "privatekey", "credential", "connectionstring", "dsn",
}

var keyNormalizer = strings.NewReplacer("-", "", "_", "", ".", "", " ", "")

func redact(_ []string, a slog.Attr) slog.Attr {
	key := keyNormalizer.Replace(strings.ToLower(a.Key))
	for _, s := range sensitive {
		if strings.Contains(key, s) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}
