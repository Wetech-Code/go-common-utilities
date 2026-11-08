// Package otels3 adds OpenTelemetry spans and metrics to an S3 client. Pass the
// result as s3.Config.APIOptions:
//
//	store, err := s3.New(ctx, s3.Config{
//		Endpoint: endpoint, /* ... */
//		APIOptions: otels3.APIOptions(tel.TracerProvider(), tel.MeterProvider()),
//	})
//
// Each S3 operation (PutObject, GetObject, ...) becomes a client span named
// "S3 <operation>" and is counted in the db.client.operation.duration
// histogram. Bucket names, object keys and headers are never recorded. The span
// covers the whole operation including the SDK's retries.
package otels3

import (
	"context"
	"errors"

	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
)

// ScopeName identifies this package as the instrumentation scope.
const ScopeName = "github.com/Wetech-Code/go-common-utilities/telemetry/otels3"

// APIOptions returns the middleware to add to an S3 client. Nil providers mean no-op.
func APIOptions(tp trace.TracerProvider, mp metric.MeterProvider) []func(*middleware.Stack) error {
	in := otelkit.New(ScopeName, "aws.s3", tp, mp)
	mw := middleware.InitializeMiddlewareFunc("GCUOpenTelemetry",
		func(ctx context.Context, input middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
			opName := middleware.GetOperationName(ctx)
			if opName == "" {
				opName = "unknown"
			}
			met := []attribute.KeyValue{attribute.String("db.operation.name", opName)}
			span := append([]attribute.KeyValue{attribute.String("rpc.system", "aws-api"), attribute.String("rpc.service", "S3"), attribute.String("rpc.method", opName)}, met...)
			ctx, op := in.Start(ctx, "S3 "+opName, span, met)
			out, md, err := next.HandleInitialize(ctx, input)
			op.End(errType(err))
			return out, md, err
		})
	return []func(*middleware.Stack) error{
		func(s *middleware.Stack) error { return s.Initialize.Add(mw, middleware.Before) },
	}
}

// errType is the S3 error code (NoSuchKey, AccessDenied, ...) when there is one.
func errType(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return "error"
}
