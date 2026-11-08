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
//
// To ship logs elsewhere, build a handler from logger/loki (Grafana Loki) or
// logger/otlp (OpenTelemetry: Grafana Cloud, Loki, Datadog, Elastic, ...) and
// pass it in Config.Extra. The packages are separate so that code which only
// logs to the console does not compile the OpenTelemetry SDK.
package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
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
	// Extra are additional destinations, such as the handlers from the
	// logger/loki and logger/otlp packages. Every record goes to the console
	// output and to each of them. Level applies to all of them, and sensitive
	// values are hidden before a record reaches any of them. The caller keeps
	// ownership: flush and close those handlers on shutdown (Close or Shutdown
	// on the handler), after the last log call.
	Extra []slog.Handler
	// ContextAttrs, if set, adds attributes taken from the record's context to
	// the console output, for example otlp.TraceAttrs to print trace_id and
	// span_id. Handlers in Extra do their own context handling.
	ContextAttrs func(context.Context) []slog.Attr
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

	var console slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "", "json":
		console = slog.NewJSONHandler(out, opts)
	case "text":
		console = slog.NewTextHandler(out, opts)
	default:
		return nil, fmt.Errorf("logger: unknown format %q (want json or text)", cfg.Format)
	}
	console = WithContextAttrs(console, cfg.ContextAttrs)
	if len(cfg.Extra) == 0 {
		return slog.New(console), nil
	}

	all := make([]slog.Handler, 0, 1+len(cfg.Extra))
	all = append(all, console)
	for i, h := range cfg.Extra {
		if h == nil {
			return nil, fmt.Errorf("logger: Extra[%d] is nil", i)
		}
		all = append(all, levelHandler{Redact(h), level})
	}
	return slog.New(slog.NewMultiHandler(all...)), nil
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

const redacted = "[REDACTED]"

func isSensitive(key string) bool {
	key = keyNormalizer.Replace(strings.ToLower(key))
	for _, s := range sensitive {
		if strings.Contains(key, s) {
			return true
		}
	}
	return false
}

// redact hides a leaf attribute whose key is sensitive, or that sits inside a
// group (from WithGroup or slog.Group) whose name is sensitive.
func redact(groups []string, a slog.Attr) slog.Attr {
	if isSensitive(a.Key) || slices.ContainsFunc(groups, isSensitive) {
		return slog.String(a.Key, redacted)
	}
	return a
}
