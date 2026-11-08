package otelpgx_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelpgx"
)

func TestVerb(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                      "SELECT",
		"  select * from t":             "SELECT",
		"\n\tInsert INTO t VALUES (1)":  "INSERT",
		"-- note\nUPDATE t SET a=1":     "UPDATE",
		"/* hint */ DELETE FROM t":      "DELETE",
		"/* a */ -- b\nWITH x AS (...)": "WITH",
		"":                              "QUERY",
		"(select 1)":                    "QUERY",
		"-- only a comment":             "QUERY",
		"/* unterminated":               "QUERY",
		"COPY t FROM STDIN":             "COPY",
	} {
		if got := otelpgx.Verb(in); got != want {
			t.Errorf("Verb(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuerySpanAndMetric(t *testing.T) {
	env := oteltest.New(t)
	tr := otelpgx.New(env.TP, env.MP)
	ctx := context.Background()

	c := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT secret FROM users WHERE email = 'a@x.io'", Args: []any{"hunter2"}})
	tr.TraceQueryEnd(c, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 3")})

	c = tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "INSERT INTO users VALUES ($1)"})
	tr.TraceQueryEnd(c, nil, pgx.TraceQueryEndData{Err: &pgconn.PgError{Code: "23505", Message: "duplicate key value (a@x.io)"}})

	spans := env.Spans()
	if len(spans) != 2 {
		t.Fatalf("spans = %v", spans)
	}
	ok, bad := spans[0], spans[1]
	if ok.Name != "SELECT" || ok.Kind != "client" || ok.Failed || ok.Attrs["db.system.name"].AsString() != "postgresql" ||
		ok.Attrs["db.operation.name"].AsString() != "SELECT" || ok.Attrs["db.response.returned_rows"].AsInt64() != 3 {
		t.Errorf("ok span = %+v", ok)
	}
	if bad.Name != "INSERT" || !bad.Failed || bad.Attrs["error.type"].AsString() != "23505" {
		t.Errorf("failed span = %+v", bad)
	}
	for _, s := range spans {
		for k, v := range s.Attrs {
			if strings.Contains(v.Emit(), "hunter2") || strings.Contains(v.Emit(), "a@x.io") || k == "db.query.text" {
				t.Errorf("span leaks statement or parameter data: %s=%s", k, v.Emit())
			}
		}
	}

	pts := env.Histogram(t, otelkit.DurationMetric)
	if len(pts) != 2 {
		t.Fatalf("histogram points = %v", pts)
	}
	var sawErr bool
	for _, p := range pts {
		if p.Attrs["error.type"].AsString() == "23505" {
			sawErr = true
		}
		if _, has := p.Attrs["db.query.text"]; has {
			t.Error("the statement must not be a metric attribute")
		}
	}
	if !sawErr {
		t.Errorf("no failed-operation measurement: %v", pts)
	}
}

func TestWithStatementRecordsSQLText(t *testing.T) {
	env := oteltest.New(t)
	tr := otelpgx.New(env.TP, env.MP, otelpgx.WithStatement())
	c := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT $1"})
	tr.TraceQueryEnd(c, nil, pgx.TraceQueryEndData{})
	c = tr.TracePrepareStart(context.Background(), nil, pgx.TracePrepareStartData{SQL: "SELECT $1"})
	tr.TracePrepareEnd(c, nil, pgx.TracePrepareEndData{})
	spans := env.Spans()
	if spans[0].Attrs["db.query.text"].AsString() != "SELECT $1" || spans[1].Attrs["db.query.text"].AsString() != "SELECT $1" {
		t.Errorf("spans = %+v", spans)
	}
}

func TestBatchCopyPrepareConnect(t *testing.T) {
	env := oteltest.New(t)
	tr := otelpgx.New(env.TP, env.MP)
	ctx := context.Background()

	batch := &pgx.Batch{}
	batch.Queue("INSERT INTO t VALUES (1)")
	batch.Queue("UPDATE t SET a = 2")
	c := tr.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{Batch: batch})
	tr.TraceBatchQuery(c, nil, pgx.TraceBatchQueryData{SQL: "INSERT INTO t VALUES (1)"})
	tr.TraceBatchQuery(c, nil, pgx.TraceBatchQueryData{SQL: "UPDATE t SET a = 2", Err: errors.New("boom")})
	tr.TraceBatchEnd(c, nil, pgx.TraceBatchEndData{Err: errors.New("boom")})

	c = tr.TraceCopyFromStart(ctx, nil, pgx.TraceCopyFromStartData{TableName: pgx.Identifier{"app", "events"}})
	tr.TraceCopyFromEnd(c, nil, pgx.TraceCopyFromEndData{})

	c = tr.TracePrepareStart(ctx, nil, pgx.TracePrepareStartData{Name: "s1", SQL: "SELECT 1"})
	tr.TracePrepareEnd(c, nil, pgx.TracePrepareEndData{Err: context.Canceled})

	cfg, _ := pgx.ParseConfig("postgres://u:pw@db.internal:5433/app")
	c = tr.TraceConnectStart(ctx, pgx.TraceConnectStartData{ConnConfig: cfg})
	tr.TraceConnectEnd(c, pgx.TraceConnectEndData{})
	c = tr.TraceConnectStart(ctx, pgx.TraceConnectStartData{})
	tr.TraceConnectEnd(c, pgx.TraceConnectEndData{Err: context.DeadlineExceeded})

	s := env.Spans()
	if len(s) != 5 {
		t.Fatalf("spans = %+v", s)
	}
	if s[0].Name != "BATCH" || s[0].Attrs["db.operation.batch.size"].AsInt64() != 2 || !s[0].Failed || len(s[0].Events) != 2 {
		t.Errorf("batch span = %+v", s[0])
	}
	if s[1].Name != "COPY" || s[1].Attrs["db.collection.name"].AsString() != "app.events" {
		t.Errorf("copy span = %+v", s[1])
	}
	if s[2].Name != "PREPARE" || s[2].Attrs["error.type"].AsString() != "context.Canceled" {
		t.Errorf("prepare span = %+v", s[2])
	}
	if s[3].Name != "CONNECT" || s[3].Attrs["server.address"].AsString() != "db.internal" || s[3].Attrs["server.port"].AsInt64() != 5433 || s[3].Attrs["db.namespace"].AsString() != "app" {
		t.Errorf("connect span = %+v", s[3])
	}
	for _, sp := range s {
		for _, v := range sp.Attrs {
			if strings.Contains(v.Emit(), "pw") && v.Emit() != "app.events" {
				t.Errorf("password-like data in %v", sp.Attrs)
			}
		}
	}
	if s[4].Attrs["error.type"].AsString() != "context.DeadlineExceeded" {
		t.Errorf("connect failure span = %+v", s[4])
	}
	// A batch with no Batch value and an end without a start must not panic.
	tr.TraceBatchStart(ctx, nil, pgx.TraceBatchStartData{})
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: pgx.ErrNoRows})
	tr.TraceBatchQuery(ctx, nil, pgx.TraceBatchQueryData{SQL: "SELECT 1"})
}

func TestNilProvidersAreNoOps(t *testing.T) {
	tr := otelpgx.New(nil, nil)
	c := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	tr.TraceQueryEnd(c, nil, pgx.TraceQueryEndData{})
}

func TestErrorTypes(t *testing.T) {
	env := oteltest.New(t)
	tr := otelpgx.New(env.TP, env.MP)
	for _, err := range []error{pgx.ErrNoRows, errors.New("some failure")} {
		c := tr.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
		tr.TraceQueryEnd(c, nil, pgx.TraceQueryEndData{Err: err})
	}
	s := env.Spans()
	if s[0].Attrs["error.type"].AsString() != "no_rows" || s[1].Attrs["error.type"].AsString() != "error" {
		t.Errorf("spans = %+v", s)
	}
	for _, sp := range s {
		if strings.Contains(sp.Attrs["error.type"].Emit(), "some failure") {
			t.Error("error messages must not be recorded")
		}
	}
}
