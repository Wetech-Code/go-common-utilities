package redis_test

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
)

// Connect at startup, use the native go-redis API, and close on shutdown.
func Example() {
	ctx := context.Background()
	rdb, err := redis.New(ctx, redis.Config{URL: os.Getenv("REDIS_URL")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer rdb.Close() // the caller owns the client

	if err := rdb.Set(ctx, "greeting", "hello", time.Minute).Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
