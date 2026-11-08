package otelmongo_test

import (
	"context"

	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelmongo"
)

func Example() {
	ctx := context.Background()
	tel, _ := telemetry.New(ctx, telemetry.Config{ServiceName: "api"})
	client, err := mongodb.New(ctx, mongodb.Config{
		URI:     "mongodb://mongo.internal:27017/?tls=true",
		Monitor: otelmongo.New(tel.TracerProvider(), tel.MeterProvider()), // a span per command
	})
	if err != nil {
		return
	}
	defer client.Disconnect(ctx)
}
