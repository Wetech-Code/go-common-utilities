//go:build integration

package redis_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
)

// Run with: GCU_TEST_REDIS_URL=redis://... go test -tags integration ./redis
func TestIntegrationLifecycle(t *testing.T) {
	url := os.Getenv("GCU_TEST_REDIS_URL")
	if url == "" {
		t.Skip("GCU_TEST_REDIS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rdb, err := redis.New(ctx, redis.Config{URL: url, PoolSize: 4})
	if err != nil {
		t.Fatal(err)
	}

	key := fmt.Sprintf("gcu_test:%d", time.Now().UnixNano())
	if err := rdb.Set(ctx, key, "hello", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := rdb.Get(ctx, key).Result(); err != nil || got != "hello" {
		t.Fatalf("Get = %q, %v", got, err)
	}

	rdb.Del(ctx, key) // clean up before closing; a deferred Del would run on a closed client
	if err := rdb.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Ping(ctx).Err(); err == nil {
		t.Error("Ping after Close should fail")
	}
}

func envOrSkip(t *testing.T) string {
	t.Helper()
	url := os.Getenv("GCU_TEST_REDIS_URL")
	if url == "" {
		t.Skip("GCU_TEST_REDIS_URL not set")
	}
	return url
}
