package otelmongo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelmongo"
)

func started(t *testing.T, id int64, name, db string, doc bson.D) *event.CommandStartedEvent {
	t.Helper()
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &event.CommandStartedEvent{Command: raw, DatabaseName: db, CommandName: name, RequestID: id}
}

func TestCommandSpansAndMetrics(t *testing.T) {
	env := oteltest.New(t)
	m := otelmongo.New(env.TP, env.MP)
	ctx := context.Background()

	m.Started(ctx, started(t, 1, "find", "shop", bson.D{{Key: "find", Value: "orders"}, {Key: "filter", Value: bson.D{{Key: "email", Value: "a@x.io"}, {Key: "password", Value: "hunter2"}}}}))
	m.Succeeded(ctx, &event.CommandSucceededEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 1}})

	m.Started(ctx, started(t, 2, "insert", "shop", bson.D{{Key: "insert", Value: "orders"}}))
	m.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 2}, Failure: mongo.CommandError{Code: 11000, Message: "E11000 duplicate key a@x.io"}})

	spans := env.Spans()
	if len(spans) != 2 {
		t.Fatalf("spans = %+v", spans)
	}
	if spans[0].Name != "find orders" || spans[0].Kind != "client" || spans[0].Failed ||
		spans[0].Attrs["db.system.name"].AsString() != "mongodb" || spans[0].Attrs["db.collection.name"].AsString() != "orders" ||
		spans[0].Attrs["db.namespace"].AsString() != "shop" || spans[0].Attrs["db.operation.name"].AsString() != "find" {
		t.Errorf("find span = %+v", spans[0])
	}
	if spans[1].Name != "insert orders" || !spans[1].Failed || spans[1].Attrs["error.type"].AsString() != "11000" {
		t.Errorf("insert span = %+v", spans[1])
	}
	for _, s := range spans {
		for k, v := range s.Attrs {
			if strings.Contains(v.Emit(), "hunter2") || strings.Contains(v.Emit(), "a@x.io") {
				t.Errorf("span attribute %s leaks request data: %s", k, v.Emit())
			}
		}
	}
	pts := env.Histogram(t, otelkit.DurationMetric)
	if len(pts) != 2 {
		t.Errorf("points = %v", pts)
	}
	for _, p := range pts {
		if _, has := p.Attrs["db.collection.name"]; has {
			t.Error("collection names must not be metric attributes")
		}
	}
}

func TestSkippedCommandsUnknownRequestsAndNonCollectionCommands(t *testing.T) {
	env := oteltest.New(t)
	m := otelmongo.New(env.TP, env.MP)
	ctx := context.Background()

	for i, name := range []string{"hello", "isMaster", "saslStart", "endSessions", "buildInfo"} {
		id := int64(100 + i)
		m.Started(ctx, started(t, id, name, "admin", bson.D{{Key: name, Value: 1}}))
		m.Succeeded(ctx, &event.CommandSucceededEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: id}})
	}
	// Events for a request that was never started are ignored.
	m.Succeeded(ctx, &event.CommandSucceededEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 999}})
	m.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 998}, Failure: errors.New("x")})
	if len(env.Spans()) != 0 {
		t.Errorf("skipped commands produced spans: %+v", env.Spans())
	}

	// A command whose first field is not its name (or is not a string) has no collection.
	m.Started(ctx, started(t, 1, "listDatabases", "admin", bson.D{{Key: "listDatabases", Value: 1}}))
	m.Succeeded(ctx, &event.CommandSucceededEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 1}})
	m.Started(ctx, &event.CommandStartedEvent{Command: bson.Raw{}, DatabaseName: "d", CommandName: "ping", RequestID: 2})
	m.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 2}, Failure: context.DeadlineExceeded})
	s := env.Spans()
	if len(s) != 2 || s[0].Name != "listDatabases" || s[1].Name != "ping" || s[1].Attrs["error.type"].AsString() != "context.DeadlineExceeded" {
		t.Errorf("spans = %+v", s)
	}
	if _, has := s[0].Attrs["db.collection.name"]; has {
		t.Errorf("no collection expected: %+v", s[0])
	}
}

func TestDriverErrorCodesAreReported(t *testing.T) {
	env := oteltest.New(t)
	m := otelmongo.New(env.TP, env.MP)
	ctx := context.Background()
	// This is the error value the driver itself puts in a failed-command event.
	m.Started(ctx, started(t, 1, "noSuchCommand", "d", bson.D{{Key: "noSuchCommand", Value: 1}}))
	m.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 1},
		Failure: fmt.Errorf("wrapped: %w", driver.Error{Code: 59, Message: "no such command: 'noSuchCommand'"})})
	s := env.Spans()
	if len(s) != 1 || s[0].Attrs["error.type"].AsString() != "59" {
		t.Errorf("spans = %+v", s)
	}
}

func TestErrorKindsAndNilProviders(t *testing.T) {
	env := oteltest.New(t)
	m := otelmongo.New(env.TP, env.MP)
	ctx := context.Background()
	for i, err := range []error{context.Canceled, errors.New("socket closed")} {
		id := int64(i + 1)
		m.Started(ctx, started(t, id, "find", "d", bson.D{{Key: "find", Value: "c"}}))
		m.Failed(ctx, &event.CommandFailedEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: id}, Failure: err})
	}
	s := env.Spans()
	if s[0].Attrs["error.type"].AsString() != "context.Canceled" || s[1].Attrs["error.type"].AsString() != "error" {
		t.Errorf("spans = %+v", s)
	}

	n := otelmongo.New(nil, nil)
	n.Started(ctx, started(t, 1, "find", "d", bson.D{{Key: "find", Value: "c"}}))
	n.Succeeded(ctx, &event.CommandSucceededEvent{CommandFinishedEvent: event.CommandFinishedEvent{RequestID: 1}})
}
