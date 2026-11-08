// Package telemetry sets up OpenTelemetry for a service in one call: logs,
// traces and metrics, exported over OTLP/HTTP with the same service identity.
// The result works with Grafana (Loki for logs, Tempo for traces, Prometheus or
// Mimir for metrics), Grafana Cloud, an OpenTelemetry Collector, and other OTLP
// backends such as Datadog, Elastic, Honeycomb and Splunk.
//
//	tel, err := telemetry.New(ctx, telemetry.Config{
//		Endpoint:    "http://collector.internal:4318",
//		ServiceName: "api",
//		Environment: "prod",
//	})
//	if err != nil {
//		return err
//	}
//	defer tel.Shutdown(ctx) // flushes logs, spans and metrics
//
//	log, _ := logger.New(logger.Config{
//		Extra:        []slog.Handler{tel.LogHandler()},
//		ContextAttrs: telemetry.TraceAttrs, // trace_id and span_id on console lines
//	})
//
//	ctx, span := tel.Tracer("orders").Start(ctx, "place order")
//	defer span.End()
//	log.InfoContext(ctx, "order placed") // linked to the span
//
//	orders, _ := tel.Meter("orders").Int64Counter("orders.placed")
//	orders.Add(ctx, 1)
//
//	mux := http.NewServeMux()
//	http.ListenAndServe(":8080", tel.HTTPHandler(mux, "api")) // a span and request metrics per request
//
// There are no global providers unless Config.SetGlobal is true, and nothing
// is exported until a signal is used. Records, spans and measurements are
// batched and sent in the background, so instrumented code never waits for the
// network. Delivery is best effort.
//
// Instrumentation for the database and storage clients lives in the
// telemetry/otelpgx, otelmongo, otelredis and otels3 packages, which you import
// only when you want them.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	goruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/Wetech-Code/go-common-utilities/internal/otelres"
	"github.com/Wetech-Code/go-common-utilities/logger/otlp"
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultTimeout        = 10 * time.Second
	DefaultMetricInterval = 15 * time.Second
	DefaultBatchInterval  = time.Second
)

// Config describes where telemetry goes and which service it comes from.
type Config struct {
	// Endpoint is the base URL of an OTLP/HTTP receiver, such as
	// "http://collector.internal:4318" or Grafana Cloud's
	// "https://otlp-gateway-prod-us-central-0.grafana.net/otlp". "/v1/logs",
	// "/v1/traces" and "/v1/metrics" are added for you. If empty, the standard
	// OTEL_EXPORTER_OTLP_* environment variables are used. Certificates are
	// always verified; use https:// for anything beyond a private network.
	Endpoint string
	// LogsEndpoint, TracesEndpoint and MetricsEndpoint override Endpoint for one
	// signal, for setups where each goes to a different backend (for example
	// Loki's ".../otlp", Tempo, and Prometheus's ".../api/v1/otlp"). Same format
	// as Endpoint.
	LogsEndpoint, TracesEndpoint, MetricsEndpoint string
	// Headers are sent with every request, typically for authentication. They are
	// never written to errors.
	Headers map[string]string
	// ServiceName, ServiceVersion and Environment become the resource attributes
	// service.name, service.version and deployment.environment on all signals.
	// ServiceName is strongly recommended.
	ServiceName    string
	ServiceVersion string
	Environment    string
	// Attributes are extra resource attributes, such as {"k8s.namespace.name": "prod"}.
	Attributes map[string]string
	// Gzip compresses request bodies.
	Gzip bool
	// Timeout bounds each export. Default DefaultTimeout.
	Timeout time.Duration

	// Level is the lowest log level exported. Default info. AddSource adds the
	// calling file and line to exported log records.
	Level     slog.Leveler
	AddSource bool
	// SampleRatio is the fraction of new traces recorded, from 0 to 1. Zero means
	// the default, 1 (all). A trace that already has a sampled parent, for example
	// from an incoming request, is always kept so traces stay whole.
	SampleRatio float64
	// MetricInterval is how often metrics are exported. Default DefaultMetricInterval.
	MetricInterval time.Duration
	// BatchInterval is how often logs and spans are exported. Default DefaultBatchInterval.
	BatchInterval time.Duration
	// RuntimeMetrics also records Go runtime metrics (memory, goroutines, GC).
	RuntimeMetrics bool

	// DisableLogs, DisableTraces and DisableMetrics turn a signal off. Its
	// accessors then return working no-op implementations.
	DisableLogs, DisableTraces, DisableMetrics bool

	// SetGlobal also installs the trace and meter providers and the propagator as
	// OpenTelemetry's process-wide defaults, so third-party instrumentation
	// libraries that read the globals work. Do this once per process.
	SetGlobal bool
}

// Validate reports problems with the configuration without connecting.
// Errors never include header values.
func (c Config) Validate() error {
	for name, ep := range map[string]string{
		"Endpoint": c.Endpoint, "LogsEndpoint": c.LogsEndpoint,
		"TracesEndpoint": c.TracesEndpoint, "MetricsEndpoint": c.MetricsEndpoint,
	} {
		if ep == "" {
			continue
		}
		u, err := url.Parse(ep)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("telemetry: %s must be an http:// or https:// URL", name)
		}
		if u.User != nil {
			return fmt.Errorf("telemetry: %s must not contain credentials; use Headers", name)
		}
	}
	switch {
	case c.SampleRatio < 0 || c.SampleRatio > 1:
		return errors.New("telemetry: SampleRatio must be between 0 and 1")
	case c.Timeout < 0 || c.MetricInterval < 0 || c.BatchInterval < 0:
		return errors.New("telemetry: Timeout, MetricInterval and BatchInterval must not be negative")
	}
	for k := range c.Headers {
		if strings.TrimSpace(k) == "" {
			return errors.New("telemetry: header names must not be empty")
		}
	}
	return nil
}

// Telemetry holds the configured providers. The caller must call Shutdown.
type Telemetry struct {
	logs *otlp.Handler
	tp   trace.TracerProvider
	mp   metric.MeterProvider
	prop propagation.TextMapPropagator

	sdkTP *sdktrace.TracerProvider
	sdkMP *sdkmetric.MeterProvider
}

// New validates cfg and starts the exporters. It does not contact the receivers;
// export problems are reported through OpenTelemetry's error handler (printed
// to standard error unless you set one with otel.SetErrorHandler).
func New(ctx context.Context, cfg Config) (*Telemetry, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MetricInterval == 0 {
		cfg.MetricInterval = DefaultMetricInterval
	}
	if cfg.BatchInterval == 0 {
		cfg.BatchInterval = DefaultBatchInterval
	}
	if cfg.SampleRatio == 0 {
		cfg.SampleRatio = 1
	}
	pick := func(specific string) string {
		if specific != "" {
			return specific
		}
		return cfg.Endpoint
	}

	res, err := otelres.Build(cfg.ServiceName, cfg.ServiceVersion, cfg.Environment, cfg.Attributes)
	if err != nil {
		return nil, fmt.Errorf("telemetry: build resource: %w", err)
	}

	t := &Telemetry{
		tp:   tracenoop.NewTracerProvider(),
		mp:   metricnoop.NewMeterProvider(),
		prop: propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}),
	}
	fail := func(err error) (*Telemetry, error) {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		t.Shutdown(sctx)
		return nil, err
	}

	if !cfg.DisableLogs {
		h, err := otlp.New(ctx, otlp.Config{
			Endpoint: pick(cfg.LogsEndpoint), Headers: cfg.Headers,
			ServiceName: cfg.ServiceName, ServiceVersion: cfg.ServiceVersion, Environment: cfg.Environment,
			Attributes: cfg.Attributes, Level: cfg.Level, AddSource: cfg.AddSource,
			Gzip: cfg.Gzip, BatchInterval: cfg.BatchInterval, Timeout: cfg.Timeout,
		})
		if err != nil {
			return fail(err)
		}
		t.logs = h
	}

	if !cfg.DisableTraces {
		opts := []otlptracehttp.Option{otlptracehttp.WithTimeout(cfg.Timeout)}
		if ep := pick(cfg.TracesEndpoint); ep != "" {
			opts = append(opts, otlptracehttp.WithEndpointURL(signalURL(ep, "/v1/traces")))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
		}
		if cfg.Gzip {
			opts = append(opts, otlptracehttp.WithCompression(otlptracehttp.GzipCompression))
		}
		exp, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return fail(fmt.Errorf("telemetry: create trace exporter: %w", err))
		}
		t.sdkTP = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(cfg.BatchInterval), sdktrace.WithExportTimeout(cfg.Timeout)),
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
		)
		t.tp = t.sdkTP
	}

	if !cfg.DisableMetrics {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithTimeout(cfg.Timeout)}
		if ep := pick(cfg.MetricsEndpoint); ep != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(signalURL(ep, "/v1/metrics")))
		}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
		}
		if cfg.Gzip {
			opts = append(opts, otlpmetrichttp.WithCompression(otlpmetrichttp.GzipCompression))
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return fail(fmt.Errorf("telemetry: create metric exporter: %w", err))
		}
		t.sdkMP = sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp,
				sdkmetric.WithInterval(cfg.MetricInterval), sdkmetric.WithTimeout(cfg.Timeout))),
			sdkmetric.WithResource(res),
		)
		t.mp = t.sdkMP
		if cfg.RuntimeMetrics {
			if err := goruntime.Start(goruntime.WithMeterProvider(t.sdkMP)); err != nil {
				return fail(fmt.Errorf("telemetry: start runtime metrics: %w", err))
			}
		}
	}

	if cfg.SetGlobal {
		otel.SetTracerProvider(t.tp)
		otel.SetMeterProvider(t.mp)
		otel.SetTextMapPropagator(t.prop)
	}
	return t, nil
}

// Tracer returns a tracer for the named instrumentation scope, usually the
// package or component name.
func (t *Telemetry) Tracer(name string) trace.Tracer { return t.tp.Tracer(name) }

// Meter returns a meter for the named instrumentation scope.
func (t *Telemetry) Meter(name string) metric.Meter { return t.mp.Meter(name) }

// TracerProvider returns the trace provider, for passing to instrumentation
// libraries and to the telemetry/otel* packages.
func (t *Telemetry) TracerProvider() trace.TracerProvider { return t.tp }

// MeterProvider returns the meter provider.
func (t *Telemetry) MeterProvider() metric.MeterProvider { return t.mp }

// Propagator returns the W3C Trace Context and Baggage propagator that
// HTTPHandler and HTTPTransport use to carry traces between services.
func (t *Telemetry) Propagator() propagation.TextMapPropagator { return t.prop }

// LogHandler returns the slog handler that exports logs, to pass in
// logger.Config.Extra. If logs are disabled it returns a handler that discards.
func (t *Telemetry) LogHandler() slog.Handler {
	if t.logs == nil {
		return slog.DiscardHandler
	}
	return t.logs
}

// HTTPHandler wraps next so that every request gets a server span (continuing
// the trace in the incoming headers) and the standard http.server.* metrics.
// operation names the handler and appears in the span name.
func (t *Telemetry) HTTPHandler(next http.Handler, operation string) http.Handler {
	return otelhttp.NewHandler(next, operation,
		otelhttp.WithTracerProvider(t.tp), otelhttp.WithMeterProvider(t.mp), otelhttp.WithPropagators(t.prop))
}

// HTTPTransport wraps base (http.DefaultTransport if nil) so that outgoing
// requests get a client span, carry the trace headers to the next service, and
// record the standard http.client.* metrics. Use it as an http.Client's Transport.
func (t *Telemetry) HTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base,
		otelhttp.WithTracerProvider(t.tp), otelhttp.WithMeterProvider(t.mp), otelhttp.WithPropagators(t.prop))
}

// ForceFlush exports everything queued now, without shutting down.
func (t *Telemetry) ForceFlush(ctx context.Context) error {
	var errs []error
	if t.logs != nil {
		errs = append(errs, t.logs.ForceFlush(ctx))
	}
	if t.sdkTP != nil {
		errs = append(errs, t.sdkTP.ForceFlush(ctx))
	}
	if t.sdkMP != nil {
		errs = append(errs, t.sdkMP.ForceFlush(ctx))
	}
	return errors.Join(errs...)
}

// Shutdown exports everything queued and releases the exporters. Call it once
// on exit, after the last log, span and measurement. It returns when the export
// finishes or ctx ends.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	if t.sdkTP != nil {
		errs = append(errs, t.sdkTP.Shutdown(ctx))
	}
	if t.sdkMP != nil {
		errs = append(errs, t.sdkMP.Shutdown(ctx))
	}
	if t.logs != nil {
		errs = append(errs, t.logs.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// TraceAttrs returns trace_id and span_id attributes for the span in ctx, or
// nil if there is none. Pass it as logger.Config.ContextAttrs so console lines
// carry the IDs Grafana uses to jump from a log line to its trace.
func TraceAttrs(ctx context.Context) []slog.Attr { return otlp.TraceAttrs(ctx) }

func signalURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, path) {
		return base
	}
	return base + path
}
