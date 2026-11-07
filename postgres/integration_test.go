//go:build integration

package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/postgres"
)

// Run with: GCU_TEST_POSTGRES_URL=postgres://... go test -tags integration ./postgres
func TestIntegrationLifecycle(t *testing.T) {
	url := os.Getenv("GCU_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GCU_TEST_POSTGRES_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := postgres.New(ctx, postgres.Config{URL: url, AppName: "gcu-integration-test", MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}

	var got int
	if err := pool.QueryRow(ctx, "SELECT $1::int + 1", 41).Scan(&got); err != nil || got != 42 {
		t.Fatalf("query = %d, %v", got, err)
	}
	var app string
	if err := pool.QueryRow(ctx, "SELECT current_setting('application_name')").Scan(&app); err != nil || app != "gcu-integration-test" {
		t.Fatalf("application_name = %q, %v", app, err)
	}

	pool.Close()
	if err := pool.Ping(ctx); err == nil {
		t.Error("Ping after Close should fail")
	}
}
