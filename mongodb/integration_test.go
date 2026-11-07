//go:build integration

package mongodb_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// Run with: GCU_TEST_MONGODB_URI=mongodb://... go test -tags integration ./mongodb
func TestIntegrationLifecycle(t *testing.T) {
	uri := os.Getenv("GCU_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("GCU_TEST_MONGODB_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mongodb.New(ctx, mongodb.Config{URI: uri, AppName: "gcu-integration-test"})
	if err != nil {
		t.Fatal(err)
	}

	coll := client.Database("gcu_test").Collection(fmt.Sprintf("items_%d", time.Now().UnixNano()))
	defer coll.Drop(ctx)

	if _, err := coll.InsertOne(ctx, bson.M{"name": "widget", "qty": 3}); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Name string `bson:"name"`
		Qty  int    `bson:"qty"`
	}
	if err := coll.FindOne(ctx, bson.M{"name": "widget"}).Decode(&doc); err != nil || doc.Qty != 3 {
		t.Fatalf("FindOne = %+v, %v", doc, err)
	}

	if err := client.Disconnect(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err == nil {
		t.Error("Ping after Disconnect should fail")
	}
}
