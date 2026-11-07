package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/postgres"
)

func TestValidate(t *testing.T) {
	tests := map[string]postgres.Config{
		"missing URL":        {},
		"negative max":       {URL: "postgres://h/d", MaxConns: -1},
		"min above max":      {URL: "postgres://h/d", MaxConns: 2, MinConns: 3},
		"negative duration":  {URL: "postgres://h/d", ConnectTimeout: -time.Second},
		"negative idle time": {URL: "postgres://h/d", MaxConnIdleTime: -1},
		"empty type name":    {URL: "postgres://h/d", Types: []string{"hstore", " "}},
	}
	for name, cfg := range tests {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if err := (postgres.Config{URL: "postgres://h/d", MaxConns: 4, MinConns: 1}).Validate(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}

func TestNewDoesNotLeakPassword(t *testing.T) {
	// "port" is not numeric, so parsing fails; the password must not appear in the error.
	_, err := postgres.New(context.Background(), postgres.Config{URL: "postgres://app:hunter2@localhost:notaport/db"})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
}

func TestNewFailsFastWhenUnreachable(t *testing.T) {
	start := time.Now()
	pool, err := postgres.New(context.Background(), postgres.Config{
		URL:            "postgres://app:pw@127.0.0.1:1/db?sslmode=disable",
		ConnectTimeout: 2 * time.Second,
	})
	if err == nil {
		pool.Close()
		t.Fatal("want error for unreachable server")
	}
	if strings.Contains(err.Error(), ":pw@") {
		t.Errorf("error leaks password: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestNewHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if pool, err := postgres.New(ctx, postgres.Config{URL: "postgres://app@127.0.0.1:1/db?sslmode=disable"}); err == nil {
		pool.Close()
		t.Fatal("want error for cancelled context")
	}
}
