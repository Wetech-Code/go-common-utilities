package otelpgx_test

import (
	"context"

	"github.com/Wetech-Code/go-common-utilities/postgres"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelpgx"
)

func Example() {
	ctx := context.Background()
	tel, _ := telemetry.New(ctx, telemetry.Config{ServiceName: "api"})
	pool, err := postgres.New(ctx, postgres.Config{
		URL:    "postgres://app@db.internal/app?sslmode=verify-full",
		Tracer: otelpgx.New(tel.TracerProvider(), tel.MeterProvider()), // spans and metrics for every query
	})
	if err != nil {
		return
	}
	defer pool.Close()
}
