package mongodb_test

import (
	"context"
	"fmt"
	"os"

	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Connect at startup, use the native driver API, and disconnect on shutdown.
func Example() {
	ctx := context.Background()
	client, err := mongodb.New(ctx, mongodb.Config{
		URI:     os.Getenv("MONGODB_URI"),
		AppName: "my-service",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer client.Disconnect(context.Background()) // the caller owns the client

	coll := client.Database("app").Collection("notes")
	if _, err := coll.InsertOne(ctx, bson.M{"text": "hello"}); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
