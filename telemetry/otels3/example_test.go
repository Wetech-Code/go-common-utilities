package otels3_test

import (
	"context"

	"github.com/Wetech-Code/go-common-utilities/s3"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otels3"
)

func Example() {
	ctx := context.Background()
	tel, _ := telemetry.New(ctx, telemetry.Config{ServiceName: "api"})
	store, err := s3.New(ctx, s3.Config{
		Endpoint: "https://seaweed.internal:8333", AccessKeyID: "id", SecretAccessKey: "secret",
		Bucket: "uploads", UsePathStyle: true,
		APIOptions: otels3.APIOptions(tel.TracerProvider(), tel.MeterProvider()), // a span per S3 operation
	})
	if err != nil {
		return
	}
	defer store.Close()
}
