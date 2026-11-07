package postgres_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Wetech-Code/go-common-utilities/postgres"
)

// Connect at startup, use the native pgx API, and close on shutdown.
func Example() {
	ctx := context.Background()
	pool, err := postgres.New(ctx, postgres.Config{
		URL:      os.Getenv("DATABASE_URL"),
		AppName:  "my-service",
		MaxConns: 10,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer pool.Close() // the caller owns the pool

	var now time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
