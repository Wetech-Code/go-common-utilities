// Package oteltest gives tests an in-memory OpenTelemetry setup: spans and
// metrics are kept in memory so a test can inspect them.
package oteltest

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Env holds the providers to hand to the code under test.
type Env struct {
	TP     *sdktrace.TracerProvider
	MP     *sdkmetric.MeterProvider
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
}

// New returns an Env; its providers are shut down when the test ends.
func New(t testing.TB) *Env {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	e := &Env{
		TP:     sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)),
		MP:     sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
		spans:  rec,
		reader: reader,
	}
	t.Cleanup(func() {
		e.TP.Shutdown(context.Background())
		e.MP.Shutdown(context.Background())
	})
	return e
}

// Span is a finished span reduced to what tests check.
type Span struct {
	Name   string
	Kind   string
	Failed bool
	Attrs  map[string]attribute.Value
	Events []string
}

// Spans returns the finished spans in the order they ended.
func (e *Env) Spans() []Span {
	var out []Span
	for _, s := range e.spans.Ended() {
		sp := Span{Name: s.Name(), Kind: s.SpanKind().String(), Failed: s.Status().Code == 1, Attrs: map[string]attribute.Value{}}
		for _, kv := range s.Attributes() {
			sp.Attrs[string(kv.Key)] = kv.Value
		}
		for _, ev := range s.Events() {
			sp.Events = append(sp.Events, ev.Name)
		}
		out = append(out, sp)
	}
	return out
}

// Point is one histogram data point reduced to what tests check.
type Point struct {
	Attrs map[string]attribute.Value
	Count uint64
}

// Histogram returns the data points of the named histogram.
func (e *Env) Histogram(t testing.TB, name string) []Point {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var out []Point
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %s is %T, not a float histogram", name, m.Data)
			}
			for _, dp := range h.DataPoints {
				p := Point{Attrs: map[string]attribute.Value{}, Count: dp.Count}
				for _, kv := range dp.Attributes.ToSlice() {
					p.Attrs[string(kv.Key)] = kv.Value
				}
				out = append(out, p)
			}
		}
	}
	return out
}
