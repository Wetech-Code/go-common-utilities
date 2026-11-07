package redis_test

import (
	"context"
	"errors"
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

// Wrap gives plain methods for the common operations; Native is still there.
func ExampleWrap() {
	ctx := context.Background()
	rdb, err := redis.New(ctx, redis.Config{URL: os.Getenv("REDIS_URL")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	c := redis.Wrap(rdb)
	defer c.Close()

	if err := c.Set(ctx, "greeting", "hello", time.Minute); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	v, err := c.Get(ctx, "greeting")
	if errors.Is(err, redis.ErrNotFound) {
		fmt.Println("no such key")
		return
	}
	fmt.Println(v, err)

	n, _ := c.Incr(ctx, "visits")
	_, _ = c.ZAdd(ctx, "scores", redis.Z{Score: float64(n), Member: "ann"})
	_ = c.Native.XLen(ctx, "events") // anything not wrapped: use the native client
}
