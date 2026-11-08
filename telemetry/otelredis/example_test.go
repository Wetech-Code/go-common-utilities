package otelredis_test

import (
	"context"

	goredis "github.com/redis/go-redis/v9"

	"github.com/Wetech-Code/go-common-utilities/redis"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelredis"
)

func Example() {
	ctx := context.Background()
	tel, _ := telemetry.New(ctx, telemetry.Config{ServiceName: "api"})
	rdb, err := redis.New(ctx, redis.Config{
		URL:   "redis://cache.internal:6379/0",
		Hooks: []goredis.Hook{otelredis.New(tel.TracerProvider(), tel.MeterProvider())}, // a span per command
	})
	if err != nil {
		return
	}
	defer rdb.Close()
}
