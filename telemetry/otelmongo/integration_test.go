//go:build integration

package otelmongo_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelmongo"
)

func TestIntegrationCommandSpans(t *testing.T) {
	uri := os.Getenv("GCU_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("GCU_TEST_MONGODB_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env := oteltest.New(t)
	client, err := mongodb.New(ctx, mongodb.Config{URI: uri, Monitor: otelmongo.New(env.TP, env.MP)})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(context.Background())

	c := mongodb.Wrap(client)
	dbName := fmt.Sprintf("gcu_otel_%d", time.Now().UnixNano())
	coll := c.Database(dbName).Collection("orders")
	defer c.Database(dbName).Drop(context.Background())

	if _, err := coll.InsertOne(ctx, bson.D{{Key: "n", Value: 1}, {Key: "password", Value: "hunter2"}}); err != nil {
		t.Fatal(err)
	}
	if n, err := coll.Count(ctx, bson.D{}); err != nil || n != 1 {
		t.Fatalf("count: %d %v", n, err)
	}
	var found []bson.M
	if err := coll.Find(ctx, bson.D{{Key: "n", Value: 1}}, &found); err != nil || len(found) != 1 {
		t.Fatalf("find: %v %v", found, err)
	}
	// A command the server rejects fails the span with its error code (59: CommandNotFound).
	if err := c.Database(dbName).RunCommand(ctx, bson.D{{Key: "noSuchCommand", Value: 1}}, nil); err == nil {
		t.Fatal("want an error")
	}

	names := map[string]bool{}
	codes := map[string]bool{}
	for _, s := range env.Spans() {
		names[s.Name] = true
		if s.Failed {
			codes[s.Attrs["error.type"].AsString()] = true
		}
		for _, v := range s.Attrs {
			if v.Emit() == "hunter2" {
				t.Errorf("document data recorded: %+v", s)
			}
		}
	}
	for _, want := range []string{"insert orders", "find orders"} {
		if !names[want] {
			t.Errorf("no %q span; got %v", want, names)
		}
	}
	if !names["noSuchCommand"] || !codes["59"] {
		t.Errorf("failed command not recorded: names=%v codes=%v", names, codes)
	}
}
