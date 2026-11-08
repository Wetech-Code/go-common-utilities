// Package otelpgx adds OpenTelemetry spans and metrics to a PostgreSQL pool.
// Pass the result as postgres.Config.Tracer:
//
//	pool, err := postgres.New(ctx, postgres.Config{
//		URL:    dsn,
//		Tracer: otelpgx.New(tel.TracerProvider(), tel.MeterProvider()),
//	})
//
// Each query, batch, COPY, prepare and new connection becomes a client span
// named after the SQL verb (SELECT, INSERT, ...), and each is counted in the
// db.client.operation.duration histogram. Parameters are never recorded. The SQL
// text is left out unless you ask for it with WithStatement, because statements
// can contain literal values.
package otelpgx

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
)

// ScopeName identifies this package as the instrumentation scope.
const ScopeName = "github.com/Wetech-Code/go-common-utilities/telemetry/otelpgx"

// Option changes what is recorded.
type Option func(*Tracer)

// WithStatement records the SQL text as db.query.text on query spans. Only do
// this if your statements never contain secrets or personal data; use
// placeholders ($1) for values.
func WithStatement() Option { return func(t *Tracer) { t.statement = true } }

// Tracer implements pgx.QueryTracer, BatchTracer, CopyFromTracer, PrepareTracer
// and ConnectTracer.
type Tracer struct {
	in        *otelkit.Instrument
	statement bool
}

// New returns a tracer. Nil providers mean no-op.
func New(tp trace.TracerProvider, mp metric.MeterProvider, opts ...Option) *Tracer {
	t := &Tracer{in: otelkit.New(ScopeName, "postgresql", tp, mp)}
	for _, o := range opts {
		o(t)
	}
	return t
}

type opKey struct{}

func start(ctx context.Context, in *otelkit.Instrument, name string, span, metric []attribute.KeyValue) context.Context {
	ctx, op := in.Start(ctx, name, span, metric)
	return context.WithValue(ctx, opKey{}, op)
}

func finish(ctx context.Context, err error) {
	if op, ok := ctx.Value(opKey{}).(*otelkit.Op); ok {
		op.End(errType(err))
	}
}

// errType is the SQLSTATE for server errors and the Go type otherwise.
func errType(err error) string {
	if err == nil {
		return ""
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	if errors.Is(err, context.Canceled) {
		return "context.Canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context.DeadlineExceeded"
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "no_rows"
	}
	return "error"
}

// Verb returns the first SQL keyword in upper case, skipping leading comments,
// or "QUERY" if there is none.
func Verb(sql string) string {
	s := strings.TrimSpace(sql)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = strings.TrimSpace(s[i+1:])
				continue
			}
			return "QUERY"
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = strings.TrimSpace(s[i+2:])
				continue
			}
			return "QUERY"
		}
		break
	}
	end := strings.IndexFunc(s, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') })
	if end < 0 {
		end = len(s)
	}
	if end == 0 {
		return "QUERY"
	}
	return strings.ToUpper(s[:end])
}

func (t *Tracer) sqlAttrs(sql string) (span, met []attribute.KeyValue) {
	verb := Verb(sql)
	met = []attribute.KeyValue{attribute.String("db.operation.name", verb)}
	span = append(span, met...)
	if t.statement {
		span = append(span, attribute.String("db.query.text", sql))
	}
	return span, met
}

// TraceQueryStart implements pgx.QueryTracer.
func (t *Tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	span, met := t.sqlAttrs(data.SQL)
	return start(ctx, t.in, Verb(data.SQL), span, met)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (t *Tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if op, ok := ctx.Value(opKey{}).(*otelkit.Op); ok && data.Err == nil {
		op.SetAttributes(attribute.Int64("db.response.returned_rows", data.CommandTag.RowsAffected()))
	}
	finish(ctx, data.Err)
}

// TraceBatchStart implements pgx.BatchTracer.
func (t *Tracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	n := 0
	if data.Batch != nil {
		n = data.Batch.Len()
	}
	return start(ctx, t.in, "BATCH",
		[]attribute.KeyValue{attribute.String("db.operation.name", "BATCH"), attribute.Int("db.operation.batch.size", n)},
		[]attribute.KeyValue{attribute.String("db.operation.name", "BATCH")})
}

// TraceBatchQuery implements pgx.BatchTracer; each statement is a span event.
func (t *Tracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if op, ok := ctx.Value(opKey{}).(*otelkit.Op); ok {
		span, _ := t.sqlAttrs(data.SQL)
		if data.Err != nil {
			span = append(span, attribute.String("error.type", errType(data.Err)))
		}
		op.SpanEvent("db.query", span...)
	}
}

// TraceBatchEnd implements pgx.BatchTracer.
func (t *Tracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	finish(ctx, data.Err)
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (t *Tracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	return start(ctx, t.in, "COPY",
		[]attribute.KeyValue{attribute.String("db.operation.name", "COPY"), attribute.String("db.collection.name", strings.Join(data.TableName, "."))},
		[]attribute.KeyValue{attribute.String("db.operation.name", "COPY")})
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (t *Tracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	finish(ctx, data.Err)
}

// TracePrepareStart implements pgx.PrepareTracer.
func (t *Tracer) TracePrepareStart(ctx context.Context, _ *pgx.Conn, data pgx.TracePrepareStartData) context.Context {
	met := []attribute.KeyValue{attribute.String("db.operation.name", "PREPARE")}
	span := met
	if t.statement {
		span = append(append([]attribute.KeyValue{}, met...), attribute.String("db.query.text", data.SQL))
	}
	return start(ctx, t.in, "PREPARE", span, met)
}

// TracePrepareEnd implements pgx.PrepareTracer.
func (t *Tracer) TracePrepareEnd(ctx context.Context, _ *pgx.Conn, data pgx.TracePrepareEndData) {
	finish(ctx, data.Err)
}

// TraceConnectStart implements pgx.ConnectTracer.
func (t *Tracer) TraceConnectStart(ctx context.Context, data pgx.TraceConnectStartData) context.Context {
	span := []attribute.KeyValue{attribute.String("db.operation.name", "CONNECT")}
	if data.ConnConfig != nil {
		span = append(span, attribute.String("server.address", data.ConnConfig.Host), attribute.Int("server.port", int(data.ConnConfig.Port)),
			attribute.String("db.namespace", data.ConnConfig.Database))
	}
	return start(ctx, t.in, "CONNECT", span, []attribute.KeyValue{attribute.String("db.operation.name", "CONNECT")})
}

// TraceConnectEnd implements pgx.ConnectTracer.
func (t *Tracer) TraceConnectEnd(ctx context.Context, data pgx.TraceConnectEndData) {
	finish(ctx, data.Err)
}

var (
	_ pgx.QueryTracer    = (*Tracer)(nil)
	_ pgx.BatchTracer    = (*Tracer)(nil)
	_ pgx.CopyFromTracer = (*Tracer)(nil)
	_ pgx.PrepareTracer  = (*Tracer)(nil)
	_ pgx.ConnectTracer  = (*Tracer)(nil)
)
