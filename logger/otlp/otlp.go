// Package otlp is a log/slog handler that exports records with OpenTelemetry's
// OTLP/HTTP protocol, the vendor-neutral way to reach Grafana Cloud or Alloy,
// Loki (its /otlp endpoint), an OpenTelemetry Collector, Datadog, Elastic,
// Honeycomb, Splunk and most other log backends.
//
// It is built on the official OpenTelemetry Go SDK, so records that are logged
// with a context carrying a trace span are linked to that trace automatically.
// Importing this package adds the OpenTelemetry SDK to your build; code that
// only logs to the console or to Loki does not need it.
//
//	h, err := otlp.New(ctx, otlp.Config{
//		Endpoint:    "http://collector.internal:4318",
//		ServiceName: "api",
//	})
//	if err != nil {
//		return err
//	}
//	defer h.Shutdown(ctx) // flushes queued records
//	log, _ := logger.New(logger.Config{Extra: []slog.Handler{h}}) // console + OTLP
//
// Records are batched and sent from a background goroutine, so logging does not
// wait for the network. Sensitive attribute values are hidden by this handler
// itself, using the same rule as package logger.
package otlp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"

	"github.com/Wetech-Code/go-common-utilities/internal/otelres"
	"github.com/Wetech-Code/go-common-utilities/logger"
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultBatchInterval = time.Second
	DefaultBatchSize     = 512
	DefaultQueueSize     = 2048
	DefaultTimeout       = 10 * time.Second
	logsPath             = "/v1/logs"
	// ScopeName identifies this package as the instrumentation scope.
	ScopeName = "github.com/Wetech-Code/go-common-utilities/logger/otlp"
)

// Config describes where to send records and which service they come from.
type Config struct {
	// Endpoint is the base URL of an OTLP/HTTP receiver, such as
	// "http://collector.internal:4318" or Grafana Cloud's
	// "https://otlp-gateway-prod-us-central-0.grafana.net/otlp"; "/v1/logs" is
	// added for you. If empty, the standard OTEL_EXPORTER_OTLP_ENDPOINT and
	// OTEL_EXPORTER_OTLP_LOGS_ENDPOINT environment variables are used. Certificates
	// are always verified; use an https:// URL for anything beyond a private network.
	Endpoint string
	// Headers are sent with every request, typically for authentication, for
	// example {"Authorization": "Basic ..."} for Grafana Cloud. They are never
	// written to errors.
	Headers map[string]string
	// ServiceName, ServiceVersion and Environment become the resource attributes
	// service.name, service.version and deployment.environment. ServiceName is
	// strongly recommended; backends group logs by it.
	ServiceName    string
	ServiceVersion string
	Environment    string
	// Attributes are extra resource attributes, such as {"k8s.namespace.name": "prod"}.
	Attributes map[string]string
	// Level is the lowest level exported. Default info.
	Level slog.Leveler
	// AddSource adds the calling file and line to each record.
	AddSource bool
	// Gzip compresses request bodies.
	Gzip bool
	// BatchInterval is how often queued records are exported, BatchSize the
	// largest batch, QueueSize how many records may wait (beyond that, new records
	// are dropped), and Timeout the limit for one export.
	BatchInterval time.Duration
	BatchSize     int
	QueueSize     int
	Timeout       time.Duration
}

// Validate reports problems with the configuration without connecting.
// Errors never include header values.
func (c Config) Validate() error {
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("otlp: Endpoint must be an http:// or https:// URL")
		}
		if u.User != nil {
			return errors.New("otlp: Endpoint must not contain credentials; use Headers")
		}
	}
	if c.BatchInterval < 0 || c.BatchSize < 0 || c.QueueSize < 0 || c.Timeout < 0 {
		return errors.New("otlp: BatchInterval, BatchSize, QueueSize and Timeout must not be negative")
	}
	if c.BatchSize > 0 && c.QueueSize > 0 && c.BatchSize > c.QueueSize {
		return errors.New("otlp: BatchSize must not exceed QueueSize")
	}
	for k := range c.Headers {
		if strings.TrimSpace(k) == "" {
			return errors.New("otlp: header names must not be empty")
		}
	}
	return nil
}

// Handler is a slog.Handler that exports records over OTLP/HTTP.
type Handler struct {
	slog.Handler
	provider *sdklog.LoggerProvider
}

// New validates cfg and starts the exporter. It does not contact the receiver;
// export problems are reported through the OpenTelemetry error handler (printed
// to standard error unless you set one with otel.SetErrorHandler). The caller
// must call Shutdown.
func New(ctx context.Context, cfg Config) (*Handler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.BatchInterval == 0 {
		cfg.BatchInterval = DefaultBatchInterval
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = min(DefaultBatchSize, cmpOr(cfg.QueueSize, DefaultQueueSize))
	}
	if cfg.QueueSize == 0 {
		cfg.QueueSize = max(DefaultQueueSize, cfg.BatchSize)
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Level == nil {
		cfg.Level = slog.LevelInfo
	}

	opts := []otlploghttp.Option{otlploghttp.WithTimeout(cfg.Timeout)}
	if cfg.Endpoint != "" {
		opts = append(opts, otlploghttp.WithEndpointURL(logsURL(cfg.Endpoint)))
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
	}
	if cfg.Gzip {
		opts = append(opts, otlploghttp.WithCompression(otlploghttp.GzipCompression))
	}
	exp, err := otlploghttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp: create exporter: %w", err)
	}

	res, err := otelres.Build(cfg.ServiceName, cfg.ServiceVersion, cfg.Environment, cfg.Attributes)
	if err != nil {
		return nil, fmt.Errorf("otlp: build resource: %w", err)
	}

	proc := sdklog.NewBatchProcessor(exp,
		sdklog.WithExportInterval(cfg.BatchInterval),
		sdklog.WithExportMaxBatchSize(cfg.BatchSize),
		sdklog.WithMaxQueueSize(cfg.QueueSize),
		sdklog.WithExportTimeout(cfg.Timeout),
	)
	provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc), sdklog.WithResource(res))

	bridge := otelslog.NewHandler(ScopeName, otelslog.WithLoggerProvider(provider), otelslog.WithSource(cfg.AddSource))
	return &Handler{Handler: logger.Redact(levelFilter{bridge, cfg.Level}), provider: provider}, nil
}

// Shutdown exports everything queued and releases the exporter. Call it once on
// exit, after the last log call. It returns when the export finishes or ctx
// ends.
func (h *Handler) Shutdown(ctx context.Context) error { return h.provider.Shutdown(ctx) }

// ForceFlush exports everything queued now, without shutting down.
func (h *Handler) ForceFlush(ctx context.Context) error { return h.provider.ForceFlush(ctx) }

// TraceAttrs returns trace_id and span_id attributes for the span in ctx, or nil
// if there is none. Pass it as logger.Config.ContextAttrs (or to the Loki
// handler's ContextAttrs) so console and Loki lines carry the IDs that Grafana
// uses to link a log to its trace. Records exported through this package carry
// the trace context natively and do not need it.
func TraceAttrs(ctx context.Context) []slog.Attr {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []slog.Attr{slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String())}
}

// logsURL turns a base URL into the logs endpoint unless it already is one.
func logsURL(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, logsPath) {
		return base
	}
	return base + logsPath
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// levelFilter keeps the bridge from exporting records below min.
type levelFilter struct {
	slog.Handler
	min slog.Leveler
}

func (f levelFilter) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= f.min.Level() && f.Handler.Enabled(ctx, l)
}

func (f levelFilter) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < f.min.Level() {
		return nil
	}
	return f.Handler.Handle(ctx, r)
}

func (f levelFilter) WithAttrs(a []slog.Attr) slog.Handler {
	return levelFilter{f.Handler.WithAttrs(a), f.min}
}

func (f levelFilter) WithGroup(n string) slog.Handler {
	return levelFilter{f.Handler.WithGroup(n), f.min}
}
