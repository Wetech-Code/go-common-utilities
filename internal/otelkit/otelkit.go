// Package otelkit holds what the telemetry/otel* client instrumentations share:
// one client span and one duration measurement per operation.
//
// Errors are reported by type or code only, never by message, because database
// and storage error messages can contain data from the failing request.
package otelkit

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// DurationMetric is the name of the histogram every client instrumentation records.
const DurationMetric = "db.client.operation.duration"

// Instrument starts spans and records durations for one client library.
type Instrument struct {
	tracer trace.Tracer
	hist   metric.Float64Histogram
	system string
}

// New returns an Instrument for the named scope and system (for example
// "postgresql"). Nil providers mean no-op.
func New(scope, system string, tp trace.TracerProvider, mp metric.MeterProvider) *Instrument {
	if tp == nil {
		tp = tracenoop.NewTracerProvider()
	}
	if mp == nil {
		mp = metricnoop.NewMeterProvider()
	}
	hist, _ := mp.Meter(scope).Float64Histogram(DurationMetric,
		metric.WithUnit("s"), metric.WithDescription("Duration of client operations."))
	return &Instrument{tracer: tp.Tracer(scope), hist: hist, system: system}
}

// Op is one in-flight operation.
type Op struct {
	in    *Instrument
	span  trace.Span
	start time.Time
	attrs []attribute.KeyValue
	ctx   context.Context
}

// Start begins a client span called name. attrs go on the span; metricAttrs go
// on the duration measurement and must have low cardinality (no table keys, no
// IDs). db.system.name is added to both.
func (i *Instrument) Start(ctx context.Context, name string, attrs, metricAttrs []attribute.KeyValue) (context.Context, *Op) {
	sys := attribute.String("db.system.name", i.system)
	ctx, span := i.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append([]attribute.KeyValue{sys}, attrs...)...))
	return ctx, &Op{in: i, span: span, start: time.Now(), attrs: append([]attribute.KeyValue{sys}, metricAttrs...), ctx: ctx}
}

// SpanEvent adds a named event to the span.
func (o *Op) SpanEvent(name string, attrs ...attribute.KeyValue) {
	o.span.AddEvent(name, trace.WithAttributes(attrs...))
}

// SetAttributes adds attributes to the span only.
func (o *Op) SetAttributes(attrs ...attribute.KeyValue) { o.span.SetAttributes(attrs...) }

// End finishes the span and records the duration. errType is empty on success;
// otherwise it is the error's type or code, and the span is marked failed.
func (o *Op) End(errType string) {
	if errType != "" {
		o.attrs = append(o.attrs, attribute.String("error.type", errType))
		o.span.SetAttributes(attribute.String("error.type", errType))
		o.span.SetStatus(codes.Error, errType)
	}
	o.in.hist.Record(context.WithoutCancel(o.ctx), time.Since(o.start).Seconds(), metric.WithAttributes(o.attrs...))
	o.span.End()
}
