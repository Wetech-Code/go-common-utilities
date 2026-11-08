//go:build integration

package otelredis_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/redis"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelredis"
)

func TestIntegrationCommandSpans(t *testing.T) {
	url := os.Getenv("GCU_TEST_REDIS_URL")
	if url == "" {
		t.Skip("GCU_TEST_REDIS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env := oteltest.New(t)
	rdb, err := redis.New(ctx, redis.Config{URL: url, Hooks: []goredis.Hook{otelredis.New(env.TP, env.MP)}})
	if err != nil {
		t.Fatal(err)
	}
	defer rdb.Close()

	key := fmt.Sprintf("gcu:otel:%d", time.Now().UnixNano())
	defer rdb.Del(context.Background(), key, key+":list")
	if err := rdb.Set(ctx, key, "v", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if v, err := rdb.Get(ctx, key).Result(); err != nil || v != "v" {
		t.Fatalf("get: %q %v", v, err)
	}
	if err := rdb.Get(ctx, key+":missing").Err(); err != goredis.Nil {
		t.Fatalf("missing key: %v", err)
	}
	rdb.LPush(ctx, key+":list", "a")
	if err := rdb.Get(ctx, key+":list").Err(); err == nil {
		t.Fatal("want WRONGTYPE")
	}
	pipe := rdb.Pipeline()
	pipe.Incr(ctx, key+":n")
	pipe.Incr(ctx, key+":n")
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatal(err)
	}
	rdb.Del(ctx, key+":n")

	seen := map[string]int{}
	codes := map[string]bool{}
	for _, s := range env.Spans() {
		seen[s.Name]++
		if s.Failed {
			codes[s.Attrs["error.type"].AsString()] = true
		}
	}
	for _, name := range []string{"SET", "GET", "LPUSH", "PIPELINE"} {
		if seen[name] == 0 {
			t.Errorf("no %s span; got %v", name, seen)
		}
	}
	if !codes["WRONGTYPE"] || len(codes) != 1 {
		t.Errorf("failures = %v; want only WRONGTYPE (a missing key is not an error)", codes)
	}
}
