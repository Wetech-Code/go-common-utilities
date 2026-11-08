// Package otelmongo adds OpenTelemetry spans and metrics to a MongoDB client.
// Pass the result as mongodb.Config.Monitor:
//
//	client, err := mongodb.New(ctx, mongodb.Config{
//		URI:     uri,
//		Monitor: otelmongo.New(tel.TracerProvider(), tel.MeterProvider()),
//	})
//
// Each command sent to the server becomes a client span named "<command>
// <collection>" and is counted in the db.client.operation.duration histogram.
// Command documents, filters and values are never recorded. The handshake,
// authentication and session-cleanup commands are skipped as noise.
package otelmongo

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
)

// ScopeName identifies this package as the instrumentation scope.
const ScopeName = "github.com/Wetech-Code/go-common-utilities/telemetry/otelmongo"

var skipped = map[string]bool{
	"hello": true, "isMaster": true, "ismaster": true, "saslStart": true,
	"saslContinue": true, "endSessions": true, "buildInfo": true,
}

// New returns a command monitor. Nil providers mean no-op.
func New(tp trace.TracerProvider, mp metric.MeterProvider) *event.CommandMonitor {
	in := otelkit.New(ScopeName, "mongodb", tp, mp)
	var inflight sync.Map // request ID -> *otelkit.Op

	return &event.CommandMonitor{
		Started: func(ctx context.Context, e *event.CommandStartedEvent) {
			if skipped[e.CommandName] {
				return
			}
			name := e.CommandName
			span := []attribute.KeyValue{
				attribute.String("db.operation.name", e.CommandName),
				attribute.String("db.namespace", e.DatabaseName),
			}
			// For most commands the first field is the command name and its value
			// is the collection name, as in {find: "users", filter: {...}}.
			if first, err := e.Command.IndexErr(0); err == nil {
				if coll, ok := first.Value().StringValueOK(); ok && first.Key() == e.CommandName {
					name += " " + coll
					span = append(span, attribute.String("db.collection.name", coll))
				}
			}
			_, op := in.Start(ctx, name, span, []attribute.KeyValue{attribute.String("db.operation.name", e.CommandName)})
			inflight.Store(e.RequestID, op)
		},
		Succeeded: func(_ context.Context, e *event.CommandSucceededEvent) {
			if op, ok := inflight.LoadAndDelete(e.RequestID); ok {
				op.(*otelkit.Op).End("")
			}
		},
		Failed: func(_ context.Context, e *event.CommandFailedEvent) {
			if op, ok := inflight.LoadAndDelete(e.RequestID); ok {
				op.(*otelkit.Op).End(errType(e.Failure))
			}
		},
	}
}

// errType is the server error code for command errors and a generic marker otherwise.
func errType(err error) string {
	// The driver reports a failed command as a driver.Error; mongo.CommandError is
	// what the same failure looks like to callers after the driver converts it.
	var de driver.Error
	if errors.As(err, &de) {
		return fmt.Sprintf("%d", de.Code)
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		return fmt.Sprintf("%d", ce.Code)
	}
	if errors.Is(err, context.Canceled) {
		return "context.Canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context.DeadlineExceeded"
	}
	return "error"
}
