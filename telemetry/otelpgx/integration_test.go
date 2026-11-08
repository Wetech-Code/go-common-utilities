//go:build integration

package otelpgx_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/postgres"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelpgx"
)

func TestIntegrationPoolSpans(t *testing.T) {
	url := os.Getenv("GCU_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GCU_TEST_POSTGRES_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env := oteltest.New(t)
	pool, err := postgres.New(ctx, postgres.Config{URL: url, Tracer: otelpgx.New(env.TP, env.MP)})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	table := fmt.Sprintf("gcu_otel_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id int PRIMARY KEY, name text)"); err != nil {
		t.Fatal(err)
	}
	defer pool.Exec(context.Background(), "DROP TABLE "+table)

	if _, err := pool.Exec(ctx, "INSERT INTO "+table+" VALUES ($1, $2)", 1, "ann"); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM "+table+" WHERE id = $1", 1).Scan(&name); err != nil || name != "ann" {
		t.Fatalf("select: %q %v", name, err)
	}
	// A server error carries its SQLSTATE: 23505 unique_violation.
	if _, err := pool.Exec(ctx, "INSERT INTO "+table+" VALUES ($1, $2)", 1, "dup"); err == nil {
		t.Fatal("want a unique violation")
	}
	// 42601 syntax_error.
	if _, err := pool.Exec(ctx, "SELEKT 1"); err == nil {
		t.Fatal("want a syntax error")
	}
	b := &pgx.Batch{}
	b.Queue("INSERT INTO " + table + " VALUES (2, 'bob')")
	b.Queue("INSERT INTO " + table + " VALUES (3, 'cy')")
	br := pool.SendBatch(ctx, b)
	if err := br.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := pool.CopyFrom(ctx, pgx.Identifier{table}, []string{"id", "name"}, pgx.CopyFromRows([][]any{{10, "x"}, {11, "y"}})); err != nil || n != 2 {
		t.Fatalf("copy: %d %v", n, err)
	}

	seen := map[string]int{}
	codes := map[string]bool{}
	for _, s := range env.Spans() {
		seen[s.Name]++
		if s.Failed {
			codes[s.Attrs["error.type"].AsString()] = true
		}
		if _, has := s.Attrs["db.query.text"]; has {
			t.Errorf("statement text recorded without WithStatement: %+v", s)
		}
	}
	for _, name := range []string{"CONNECT", "CREATE", "INSERT", "SELECT", "BATCH", "COPY", "SELEKT"} {
		if seen[name] == 0 {
			t.Errorf("no %s span; got %v", name, seen)
		}
	}
	if !codes["23505"] || !codes["42601"] {
		t.Errorf("SQLSTATE error types = %v", codes)
	}
	if len(env.Histogram(t, "db.client.operation.duration")) == 0 {
		t.Error("no duration measurements")
	}
}
