//go:build integration

package mongodb_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// startReplicaSet starts a private single-node replica set (transactions and change
// streams need one) and returns its URI. It uses the mongod binary named by
// GCU_TEST_MONGOD if set, otherwise a mongo:8.3 Docker container, and skips the test
// if neither exists. It never touches a shared server.
func startReplicaSet(t *testing.T) string {
	t.Helper()
	var uri, memberHost string
	if bin := os.Getenv("GCU_TEST_MONGOD"); bin != "" {
		port := freePort(t)
		cmd := exec.Command(bin, "--replSet", "rs0", "--port", port, "--bind_ip", "127.0.0.1",
			"--nounixsocket", "--dbpath", t.TempDir())
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", bin, err)
		}
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		t.Cleanup(func() { cmd.Process.Kill(); <-done })
		uri, memberHost = "mongodb://127.0.0.1:"+port+"/?directConnection=true", "127.0.0.1:"+port
	} else if _, err := exec.LookPath("docker"); err == nil && exec.Command("docker", "info").Run() == nil {
		id, err := exec.Command("docker", "run", "-d", "--rm", "-p", "127.0.0.1::27017",
			"mongo:8.3", "mongod", "--replSet", "rs0", "--bind_ip_all").Output()
		if err != nil {
			t.Fatalf("docker run: %v", err)
		}
		cid := strings.TrimSpace(string(id))
		t.Cleanup(func() { exec.Command("docker", "rm", "-f", cid).Run() })
		mapped, err := exec.Command("docker", "port", cid, "27017/tcp").Output()
		if err != nil {
			t.Fatalf("docker port: %v", err)
		}
		addr := strings.TrimSpace(strings.Split(string(mapped), "\n")[0])
		uri, memberHost = "mongodb://"+addr+"/?directConnection=true", "localhost:27017"
	} else {
		t.Skip("needs a disposable MongoDB: set GCU_TEST_MONGOD to a mongod binary or start Docker")
	}

	// Initiate the replica set with the raw driver (mongodb.New would wait for a
	// primary that does not exist yet), then wait for a primary.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var raw *mongo.Client
	for {
		var err error
		raw, err = mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(2 * time.Second))
		if err == nil {
			err = raw.Database("admin").RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err()
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("mongod did not start: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	defer raw.Disconnect(context.Background())
	init := bson.D{{Key: "replSetInitiate", Value: bson.D{
		{Key: "_id", Value: "rs0"},
		{Key: "members", Value: bson.A{bson.D{{Key: "_id", Value: 0}, {Key: "host", Value: memberHost}}}},
	}}}
	if err := raw.Database("admin").RunCommand(ctx, init).Err(); err != nil && !strings.Contains(err.Error(), "already initialized") {
		t.Fatalf("replSetInitiate: %v", err)
	}
	for {
		var hello struct {
			Primary bool `bson:"isWritablePrimary"`
		}
		if err := raw.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err == nil && hello.Primary {
			return uri
		}
		if ctx.Err() != nil {
			t.Fatal("no primary elected within 60s")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// newClient connects a wrapped client through mongodb.New and closes it when the test ends.
func newClient(t *testing.T, uri string) (context.Context, *mongodb.Client) {
	t.Helper()
	lastURI = uri
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	mc, err := mongodb.New(ctx, mongodb.Config{URI: uri, AppName: "gcu-client-test"})
	if err != nil {
		t.Fatal(err)
	}
	c := mongodb.Wrap(mc)
	t.Cleanup(func() { c.Close(context.Background()) })
	return ctx, c
}

// lastURI is the URI newClient last connected to; tests that need a second
// connection to the same deployment reuse it.
var lastURI string

var _ = fmt.Sprint
