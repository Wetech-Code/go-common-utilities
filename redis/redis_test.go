package redis_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
)

func TestValidate(t *testing.T) {
	tests := map[string]redis.Config{
		"missing URL":      {},
		"wrong scheme":     {URL: "http://localhost"},
		"negative pool":    {URL: "redis://h", PoolSize: -1},
		"negative timeout": {URL: "redis://h", ReadTimeout: -1},
	}
	for name, cfg := range tests {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	for _, u := range []string{"redis://h:6379/0", "rediss://h:6380", "unix:///tmp/redis.sock"} {
		if err := (redis.Config{URL: u}).Validate(); err != nil {
			t.Errorf("%s rejected: %v", u, err)
		}
	}
}

func TestNewRejectsBadURLWithoutLeakingPassword(t *testing.T) {
	_, err := redis.New(context.Background(), redis.Config{URL: "redis://:hunter2@localhost:notaport/0"})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
}

func TestNewFailsFastWhenUnreachable(t *testing.T) {
	start := time.Now()
	client, err := redis.New(context.Background(), redis.Config{
		URL:         "redis://:hunter2@127.0.0.1:1/0",
		DialTimeout: time.Second,
	})
	if err == nil {
		_ = client.Close()
		t.Fatal("want error for unreachable server")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestNewHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if client, err := redis.New(ctx, redis.Config{URL: "redis://127.0.0.1:1/0"}); err == nil {
		_ = client.Close()
		t.Fatal("want error for cancelled context")
	}
}
