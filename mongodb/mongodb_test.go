package mongodb_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/mongodb"
)

func TestValidate(t *testing.T) {
	tests := map[string]mongodb.Config{
		"missing URI":      {},
		"wrong scheme":     {URI: "http://localhost"},
		"negative timeout": {URI: "mongodb://h", ConnectTimeout: -1},
	}
	for name, cfg := range tests {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	for _, uri := range []string{"mongodb://h:27017", "mongodb+srv://cluster.example.com"} {
		if err := (mongodb.Config{URI: uri}).Validate(); err != nil {
			t.Errorf("%s rejected: %v", uri, err)
		}
	}
}

func TestNewFailsFastWhenUnreachable(t *testing.T) {
	start := time.Now()
	client, err := mongodb.New(context.Background(), mongodb.Config{
		URI:            "mongodb://app:hunter2@127.0.0.1:1/?directConnection=true",
		ConnectTimeout: time.Second,
	})
	if err == nil {
		_ = client.Disconnect(context.Background())
		t.Fatal("want error for unreachable server")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestNewRejectsBadURIWithoutLeakingPassword(t *testing.T) {
	_, err := mongodb.New(context.Background(), mongodb.Config{URI: "mongodb://app:hunter2@localhost:notaport/"})
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
}

func TestNewHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if client, err := mongodb.New(ctx, mongodb.Config{URI: "mongodb://127.0.0.1:1/?directConnection=true"}); err == nil {
		_ = client.Disconnect(context.Background())
		t.Fatal("want error for cancelled context")
	}
}
