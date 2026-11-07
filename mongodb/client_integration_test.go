//go:build integration

package mongodb_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

type user struct {
	ID    bson.ObjectID `bson:"_id,omitempty"`
	Email string        `bson:"email"`
	Name  string        `bson:"name"`
	Age   int           `bson:"age"`
	Tags  []string      `bson:"tags,omitempty"`
}

func eq(t *testing.T, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// Exercises every Client, Database, Collection and Bucket method against a private
// single-node replica set. Run with Docker, or point GCU_TEST_MONGOD at a mongod.
func TestIntegrationClient(t *testing.T) {
	ctx, c := newClient(t, startReplicaSet(t))
	db := c.Database(fmt.Sprintf("gcu_%d", time.Now().UnixNano()))
	coll := db.Collection("users")
	defer db.Drop(context.Background())

	t.Run("client and database", func(t *testing.T) {
		must(t, c.Ping(ctx))
		eq(t, db.Name() != "", true)
		_, err := coll.InsertOne(ctx, user{Email: "seed@x.io"})
		must(t, err)
		names, err := c.ListDatabaseNames(ctx, nil)
		must(t, err)
		if !contains(names, db.Name()) {
			t.Fatalf("ListDatabaseNames lacks %s: %v", db.Name(), names)
		}
		only, err := c.ListDatabaseNames(ctx, bson.D{{Key: "name", Value: db.Name()}})
		must(t, err)
		eq(t, len(only), 1)
		res, err := c.ListDatabases(ctx, nil)
		must(t, err)
		eq(t, len(res.Databases) >= 2, true)
		c.AppendDriverInfo(options.DriverInfo{Name: "gcu-test", Version: "0"})

		must(t, db.CreateCollection(ctx, "capped", options.CreateCollection().SetCapped(true).SetSizeInBytes(1<<20)))
		must(t, db.CreateView(ctx, "adults", "users", bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 18}}}}}}}))
		collNames, err := db.ListCollectionNames(ctx, nil)
		must(t, err)
		for _, want := range []string{"users", "capped", "adults"} {
			if !contains(collNames, want) {
				t.Fatalf("ListCollectionNames lacks %s: %v", want, collNames)
			}
		}
		specs, err := db.ListCollectionSpecifications(ctx, bson.D{{Key: "name", Value: "capped"}})
		must(t, err)
		eq(t, len(specs), 1)
		cur, err := db.ListCollections(ctx, nil)
		must(t, err)
		var all []bson.M
		must(t, cur.All(ctx, &all))
		eq(t, len(all) >= 3, true)

		var pong bson.M
		must(t, db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}, &pong))
		eq(t, pong["ok"], 1.0)
		must(t, db.RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}, nil))
		err = db.RunCommand(ctx, bson.D{{Key: "collMod", Value: "no_such_collection"}}, nil)
		if !mongodb.IsNamespaceNotFound(err) {
			t.Fatalf("collMod on a missing collection: want namespace-not-found, got %v", err)
		}
		cc, err := db.RunCommandCursor(ctx, bson.D{{Key: "listCollections", Value: 1}})
		must(t, err)
		var viaCmd []bson.M
		must(t, cc.All(ctx, &viaCmd))
		eq(t, len(viaCmd) >= 3, true)

		admin := c.Database("admin")
		var sessions []bson.M
		must(t, admin.Aggregate(ctx, bson.A{bson.D{{Key: "$listLocalSessions", Value: bson.D{}}}}, &sessions))
		ac, err := admin.AggregateCursor(ctx, bson.A{bson.D{{Key: "$listLocalSessions", Value: bson.D{}}}})
		must(t, err)
		ac.Close(ctx)
	})

	t.Run("insert get find count", func(t *testing.T) {
		id, err := coll.InsertOne(ctx, user{Email: "ann@x.io", Name: "Ann", Age: 30, Tags: []string{"a", "b"}})
		must(t, err)
		if _, ok := id.(bson.ObjectID); !ok {
			t.Fatalf("InsertOne id = %T", id)
		}
		ids, err := coll.InsertMany(ctx, []user{
			{Email: "bo@x.io", Name: "Bo", Age: 25},
			{Email: "cy@x.io", Name: "Cy", Age: 41},
			{Email: "di@x.io", Name: "Di", Age: 17},
		})
		must(t, err)
		eq(t, len(ids), 3)

		var u user
		must(t, coll.Get(ctx, bson.D{{Key: "email", Value: "ann@x.io"}}, &u))
		eq(t, u.Name, "Ann")
		eq(t, u.Tags, []string{"a", "b"})
		err = coll.Get(ctx, bson.D{{Key: "email", Value: "nobody@x.io"}}, &u)
		if !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("Get missing: want ErrNotFound, got %v", err)
		}
		oldest, err := mongodb.Get[user](ctx, coll, nil, options.FindOne().SetSort(bson.D{{Key: "age", Value: -1}}))
		must(t, err)
		eq(t, oldest.Name, "Cy")
		if _, err := mongodb.Get[user](ctx, coll, bson.D{{Key: "email", Value: "nobody@x.io"}}); !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("generic Get missing: got %v", err)
		}

		var users []user
		must(t, coll.Find(ctx, bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 18}}}}, &users, options.Find().SetSort(bson.D{{Key: "age", Value: 1}})))
		eq(t, len(users), 3)
		eq(t, users[0].Name, "Bo")
		var none []user
		must(t, coll.Find(ctx, bson.D{{Key: "age", Value: -1}}, &none))
		if none == nil || len(none) != 0 {
			t.Fatalf("no match should give an empty non-nil slice, got %#v", none)
		}
		limited, err := mongodb.Find[user](ctx, coll, nil, options.Find().SetLimit(2))
		must(t, err)
		eq(t, len(limited), 2)
		fc, err := coll.FindCursor(ctx, bson.D{{Key: "name", Value: "Ann"}})
		must(t, err)
		eq(t, fc.Next(ctx), true)
		fc.Close(ctx)

		ok, err := coll.Exists(ctx, bson.D{{Key: "name", Value: "Cy"}})
		must(t, err)
		eq(t, ok, true)
		ok, err = coll.Exists(ctx, bson.D{{Key: "name", Value: "Zed"}})
		must(t, err)
		eq(t, ok, false)
		n, err := coll.Count(ctx, nil)
		must(t, err)
		eq(t, n, 5) // the seed document plus four
		est, err := coll.EstimatedCount(ctx)
		must(t, err)
		eq(t, est, 5)
		var emails []string
		must(t, coll.Distinct(ctx, "email", bson.D{{Key: "age", Value: bson.D{{Key: "$gt", Value: 20}}}}, &emails))
		eq(t, len(emails), 3) // Ann 30, Bo 25, Cy 41
	})

	t.Run("update replace delete findOneAnd", func(t *testing.T) {
		res, err := coll.UpdateOne(ctx, bson.D{{Key: "email", Value: "bo@x.io"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 26}}}})
		must(t, err)
		eq(t, res.MatchedCount, 1)
		eq(t, res.ModifiedCount, 1)
		res, err = coll.UpdateOne(ctx, bson.D{{Key: "email", Value: "none@x.io"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 1}}}})
		must(t, err)
		eq(t, res.MatchedCount, 0)
		res, err = coll.UpdateOne(ctx, bson.D{{Key: "email", Value: "up@x.io"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "name", Value: "Up"}}}}, options.UpdateOne().SetUpsert(true))
		must(t, err)
		if res.UpsertedID == nil {
			t.Fatal("upsert should report the new _id")
		}
		var up user
		must(t, coll.Get(ctx, bson.D{{Key: "email", Value: "up@x.io"}}, &up))
		res, err = coll.UpdateByID(ctx, up.ID, bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 50}}}})
		must(t, err)
		eq(t, res.ModifiedCount, 1)
		res, err = coll.UpdateMany(ctx, bson.D{{Key: "age", Value: bson.D{{Key: "$lt", Value: 18}}}}, bson.D{{Key: "$set", Value: bson.D{{Key: "minor", Value: true}}}})
		must(t, err)
		eq(t, res.ModifiedCount, 2) // Di (17) and the seed document (age 0)
		res, err = coll.ReplaceOne(ctx, bson.D{{Key: "email", Value: "di@x.io"}}, user{Email: "di@x.io", Name: "Dee", Age: 18})
		must(t, err)
		eq(t, res.ModifiedCount, 1)

		var before user
		must(t, coll.FindOneAndUpdate(ctx, bson.D{{Key: "email", Value: "cy@x.io"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "age", Value: 1}}}}, &before))
		eq(t, before.Age, 41)
		var after user
		must(t, coll.FindOneAndUpdate(ctx, bson.D{{Key: "email", Value: "cy@x.io"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "age", Value: 1}}}}, &after,
			options.FindOneAndUpdate().SetReturnDocument(options.After)))
		eq(t, after.Age, 43)
		if err := coll.FindOneAndUpdate(ctx, bson.D{{Key: "email", Value: "none@x.io"}}, bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: 1}}}}, &after); !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("FindOneAndUpdate nothing matched: want ErrNotFound, got %v", err)
		}
		var replaced user
		must(t, coll.FindOneAndReplace(ctx, bson.D{{Key: "email", Value: "di@x.io"}}, user{Email: "di@x.io", Name: "Dee2", Age: 19}, &replaced))
		eq(t, replaced.Name, "Dee")
		var deleted user
		must(t, coll.FindOneAndDelete(ctx, bson.D{{Key: "email", Value: "up@x.io"}}, &deleted))
		eq(t, deleted.Name, "Up")
		if err := coll.FindOneAndDelete(ctx, bson.D{{Key: "email", Value: "up@x.io"}}, &deleted); !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("FindOneAndDelete again: want ErrNotFound, got %v", err)
		}

		n, err := coll.DeleteOne(ctx, bson.D{{Key: "email", Value: "seed@x.io"}})
		must(t, err)
		eq(t, n, 1)
		n, err = coll.DeleteOne(ctx, bson.D{{Key: "email", Value: "seed@x.io"}})
		must(t, err)
		eq(t, n, 0)
		_, err = coll.InsertMany(ctx, []bson.M{{"tmp": 1}, {"tmp": 2}, {"tmp": 3}})
		must(t, err)
		n, err = coll.DeleteMany(ctx, bson.D{{Key: "tmp", Value: bson.D{{Key: "$exists", Value: true}}}})
		must(t, err)
		eq(t, n, 3)
	})

	t.Run("aggregate and bulk write", func(t *testing.T) {
		pipeline := bson.A{
			bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: nil}, {Key: "total", Value: bson.D{{Key: "$sum", Value: "$age"}}}}}},
		}
		var sums []bson.M
		must(t, coll.Aggregate(ctx, pipeline, &sums))
		eq(t, len(sums), 1)
		typed, err := mongodb.Aggregate[struct {
			Total int `bson:"total"`
		}](ctx, coll, pipeline)
		must(t, err)
		eq(t, len(typed), 1)
		eq(t, typed[0].Total > 0, true)
		ac, err := coll.AggregateCursor(ctx, pipeline)
		must(t, err)
		eq(t, ac.Next(ctx), true)
		ac.Close(ctx)

		bw, err := coll.BulkWrite(ctx, []mongodb.WriteModel{
			mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "email", Value: "bulk1@x.io"}, {Key: "age", Value: 1}}),
			mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "email", Value: "bulk2@x.io"}, {Key: "age", Value: 2}}),
			mongo.NewUpdateOneModel().SetFilter(bson.D{{Key: "email", Value: "bulk1@x.io"}}).SetUpdate(bson.D{{Key: "$set", Value: bson.D{{Key: "age", Value: 3}}}}),
			mongo.NewDeleteOneModel().SetFilter(bson.D{{Key: "email", Value: "bulk2@x.io"}}),
		})
		must(t, err)
		eq(t, bw.InsertedCount, 2)
		eq(t, bw.ModifiedCount, 1)
		eq(t, bw.DeletedCount, 1)

		cbw, err := c.BulkWrite(ctx, []mongo.ClientBulkWrite{
			{Database: db.Name(), Collection: "users", Model: mongo.NewClientInsertOneModel().SetDocument(bson.D{{Key: "email", Value: "cbulk@x.io"}})},
		})
		must(t, err)
		eq(t, cbw.InsertedCount, 1)
	})

	t.Run("indexes and duplicate keys", func(t *testing.T) {
		name, err := coll.CreateIndex(ctx, mongodb.IndexModel{Keys: bson.D{{Key: "email", Value: 1}}, Options: options.Index().SetUnique(true).SetName("email_unique")})
		must(t, err)
		eq(t, name, "email_unique")
		names, err := coll.CreateIndexes(ctx, []mongodb.IndexModel{
			{Keys: bson.D{{Key: "age", Value: 1}}},
			{Keys: bson.D{{Key: "name", Value: 1}, {Key: "age", Value: -1}}, Options: options.Index().SetName("name_age")},
		})
		must(t, err)
		eq(t, len(names), 2)
		var idx []bson.M
		must(t, coll.ListIndexes(ctx, &idx))
		eq(t, len(idx), 4) // _id plus three
		specs, err := coll.ListIndexSpecifications(ctx)
		must(t, err)
		eq(t, len(specs), 4)

		_, err = coll.InsertOne(ctx, user{Email: "ann@x.io"})
		if !mongodb.IsDuplicateKey(err) {
			t.Fatalf("duplicate insert: want IsDuplicateKey, got %v", err)
		}
		_, err = coll.InsertMany(ctx, []user{{Email: "ok1@x.io"}, {Email: "ann@x.io"}})
		if !mongodb.IsDuplicateKey(err) {
			t.Fatalf("duplicate InsertMany: want IsDuplicateKey, got %v", err)
		}
		_, err = coll.BulkWrite(ctx, []mongodb.WriteModel{mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "email", Value: "ann@x.io"}})})
		if !mongodb.IsDuplicateKey(err) || !mongodb.HasCode(err, mongodb.CodeDuplicateKey) {
			t.Fatalf("duplicate BulkWrite: got %v", err)
		}

		must(t, coll.DropIndex(ctx, "email_unique"))
		must(t, coll.DropIndexWithKey(ctx, bson.D{{Key: "age", Value: 1}}))
		must(t, coll.DropAllIndexes(ctx))
		must(t, coll.ListIndexes(ctx, &idx))
		eq(t, len(idx), 1)
	})

	t.Run("transactions and sessions", func(t *testing.T) {
		counters := db.Collection("counters")
		_, err := counters.InsertOne(ctx, bson.D{{Key: "_id", Value: "hits"}, {Key: "n", Value: 0}})
		must(t, err)

		must(t, c.WithTx(ctx, func(tx context.Context) error {
			_, err := counters.InsertOne(tx, bson.D{{Key: "_id", Value: "committed"}})
			return err
		}))
		ok, err := counters.Exists(ctx, bson.D{{Key: "_id", Value: "committed"}})
		must(t, err)
		eq(t, ok, true)

		boom := errors.New("boom")
		err = c.WithTx(ctx, func(tx context.Context) error {
			if _, err := counters.InsertOne(tx, bson.D{{Key: "_id", Value: "aborted"}}); err != nil {
				return err
			}
			// Inside the transaction the write is visible; outside it is not yet.
			inside, _ := counters.Exists(tx, bson.D{{Key: "_id", Value: "aborted"}})
			outside, _ := counters.Exists(ctx, bson.D{{Key: "_id", Value: "aborted"}})
			eq(t, inside, true)
			eq(t, outside, false)
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("WithTx should return fn's error, got %v", err)
		}
		ok, _ = counters.Exists(ctx, bson.D{{Key: "_id", Value: "aborted"}})
		eq(t, ok, false)

		must(t, c.WithTxOptions(ctx, options.Transaction().SetWriteConcern(writeconcern.Majority()), func(tx context.Context) error {
			_, err := counters.InsertOne(tx, bson.D{{Key: "_id", Value: "majority"}})
			return err
		}))

		// 10 transactions fight over one document; the driver retries write conflicts,
		// so every increment lands exactly once.
		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := c.WithTx(ctx, func(tx context.Context) error {
					_, err := counters.UpdateOne(tx, bson.D{{Key: "_id", Value: "hits"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "n", Value: 1}}}})
					return err
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		var hits struct {
			N int `bson:"n"`
		}
		must(t, counters.Get(ctx, bson.D{{Key: "_id", Value: "hits"}}, &hits))
		eq(t, hits.N, 10)

		must(t, c.UseSession(ctx, func(sc context.Context) error {
			_, err := counters.InsertOne(sc, bson.D{{Key: "_id", Value: "session"}})
			return err
		}))
		must(t, c.UseSessionWithOptions(ctx, options.Session().SetCausalConsistency(true), func(sc context.Context) error {
			return counters.Get(sc, bson.D{{Key: "_id", Value: "session"}}, &bson.M{})
		}))
		sess, err := c.StartSession()
		must(t, err)
		eq(t, c.NumberSessionsInProgress() >= 1, true)
		sess.EndSession(ctx)

		// transient and conflict classification of a real write conflict is covered by
		// the retry above; here the helpers must be false for an unrelated failure.
		if mongodb.IsTransient(boom) || mongodb.IsWriteConflict(boom) {
			t.Fatal("a plain error is neither transient nor a write conflict")
		}
	})

	t.Run("change streams", func(t *testing.T) {
		events := db.Collection("events")
		_, err := events.InsertOne(ctx, bson.D{{Key: "seed", Value: true}}) // create the collection
		must(t, err)

		next := func(cs *mongodb.ChangeStream, label string) bson.M {
			t.Helper()
			wctx, stop := context.WithTimeout(ctx, 10*time.Second)
			defer stop()
			if !cs.Next(wctx) {
				t.Fatalf("%s: no change event: %v", label, cs.Err())
			}
			var ev bson.M
			must(t, cs.Decode(&ev))
			return ev
		}
		insert := func(name string) {
			_, err := events.InsertOne(ctx, bson.D{{Key: "name", Value: name}})
			must(t, err)
		}

		cs, err := events.Watch(ctx, nil)
		must(t, err)
		defer cs.Close(ctx)
		insert("one")
		ev := next(cs, "collection")
		eq(t, ev["operationType"], "insert")

		dbs, err := db.Watch(ctx, nil)
		must(t, err)
		defer dbs.Close(ctx)
		insert("two")
		eq(t, next(dbs, "database")["operationType"], "insert")

		cls, err := c.Watch(ctx, nil)
		must(t, err)
		defer cls.Close(ctx)
		insert("three")
		eq(t, next(cls, "client")["operationType"], "insert")
	})

	t.Run("gridfs", func(t *testing.T) {
		bk := db.Bucket()
		data := make([]byte, 3<<20+123) // several 255 KiB chunks
		_, _ = rand.Read(data)
		want := sha256.Sum256(data)

		id, err := bk.Upload(ctx, "big.bin", bytes.NewReader(data))
		must(t, err)
		var out bytes.Buffer
		n, err := bk.Download(ctx, id, &out)
		must(t, err)
		eq(t, n, len(data))
		eq(t, sha256.Sum256(out.Bytes()) == want, true)

		must(t, bk.UploadWithID(ctx, "fixed-id", "named.txt", strings.NewReader("hello")))
		out.Reset()
		_, err = bk.DownloadByName(ctx, "named.txt", &out)
		must(t, err)
		eq(t, out.String(), "hello")
		must(t, bk.Rename(ctx, "fixed-id", "renamed.txt"))
		var files []bson.M
		must(t, bk.Find(ctx, bson.D{{Key: "filename", Value: "renamed.txt"}}, &files))
		eq(t, len(files), 1)
		var noFiles []bson.M
		must(t, bk.Find(ctx, bson.D{{Key: "filename", Value: "absent"}}, &noFiles))
		if noFiles == nil || len(noFiles) != 0 {
			t.Fatalf("no files should give an empty non-nil slice, got %#v", noFiles)
		}

		must(t, bk.Delete(ctx, "fixed-id"))
		if _, err := bk.Download(ctx, "fixed-id", &out); !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("Download deleted: want ErrNotFound, got %v", err)
		}
		if err := bk.Delete(ctx, "fixed-id"); !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("Delete missing: want ErrNotFound, got %v", err)
		}
		if err := bk.Rename(ctx, "fixed-id", "x"); !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("Rename missing: want ErrNotFound, got %v", err)
		}
		if _, err := bk.DownloadByName(ctx, "never-uploaded", &out); !errors.Is(err, mongodb.ErrNotFound) {
			t.Fatalf("DownloadByName missing: want ErrNotFound, got %v", err)
		}
		must(t, bk.Drop(ctx))
	})

	t.Run("errors, cancellation, drop and close", func(t *testing.T) {
		cctx, stop := context.WithCancel(ctx)
		stop()
		if _, err := coll.Count(cctx, nil); err == nil {
			t.Fatal("Count with a cancelled context should fail")
		}
		tctx, stop2 := context.WithTimeout(ctx, time.Nanosecond)
		defer stop2()
		time.Sleep(time.Millisecond)
		if _, err := coll.Count(tctx, nil); !mongodb.IsTimeout(err) {
			t.Fatalf("an expired deadline should be a timeout, got %v", err)
		}

		must(t, coll.Drop(ctx))
		eq(t, mustCount(t, ctx, coll), 0)
		must(t, db.Drop(ctx))

		extra := mongodb.Wrap(mustNative(t, ctx, c))
		must(t, extra.Ping(ctx))
		must(t, extra.Close(ctx))
		if err := extra.Ping(ctx); err == nil {
			t.Fatal("Ping after Close should fail")
		}
	})
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func mustCount(t *testing.T, ctx context.Context, coll *mongodb.Collection) int64 {
	t.Helper()
	n, err := coll.Count(ctx, nil)
	must(t, err)
	return n
}

// mustNative opens a second connection to the same deployment.
func mustNative(t *testing.T, ctx context.Context, c *mongodb.Client) *mongo.Client {
	t.Helper()
	uri := c.Native.Database("admin")
	_ = uri
	mc, err := mongodb.New(ctx, mongodb.Config{URI: lastURI})
	must(t, err)
	return mc
}

// Every method must return the driver's error, never panic, when the operation
// fails. A cancelled context makes each one fail the same way without a server fault.
func TestIntegrationErrorPropagation(t *testing.T) {
	ctx, c := newClient(t, startReplicaSet(t))
	db := c.Database(fmt.Sprintf("gcu_err_%d", time.Now().UnixNano()))
	coll := db.Collection("c")
	bk := db.Bucket()
	defer db.Drop(context.Background())
	_, err := coll.InsertMany(ctx, []bson.M{{"n": 1}, {"n": 2}})
	must(t, err)

	dead, cancel := context.WithCancel(ctx)
	cancel()
	var docs []bson.M
	var one bson.M
	var names []string
	model := mongo.NewInsertOneModel().SetDocument(bson.D{{Key: "a", Value: 1}})
	pipe := bson.A{bson.D{{Key: "$match", Value: bson.D{}}}}

	failing := []smoke{
		{"Client.Ping", func() error { return c.Ping(dead) }},
		{"Client.ListDatabaseNames", func() error { _, err := c.ListDatabaseNames(dead, nil); return err }},
		{"Client.ListDatabases", func() error { _, err := c.ListDatabases(dead, nil); return err }},
		{"Client.Watch", func() error { _, err := c.Watch(dead, nil); return err }},
		{"Client.BulkWrite", func() error {
			_, err := c.BulkWrite(dead, []mongo.ClientBulkWrite{{Database: "d", Collection: "c", Model: mongo.NewClientInsertOneModel().SetDocument(bson.D{})}})
			return err
		}},
		{"Client.WithTx", func() error { return c.WithTx(dead, func(context.Context) error { return nil }) }},
		{"Client.WithTxOptions", func() error {
			return c.WithTxOptions(dead, options.Transaction(), func(context.Context) error { return nil })
		}},
		{"Database.Drop", func() error { return db.Drop(dead) }},
		{"Database.CreateCollection", func() error { return db.CreateCollection(dead, "x") }},
		{"Database.CreateView", func() error { return db.CreateView(dead, "v", "c", pipe) }},
		{"Database.ListCollectionNames", func() error { _, err := db.ListCollectionNames(dead, nil); return err }},
		{"Database.ListCollectionSpecifications", func() error { _, err := db.ListCollectionSpecifications(dead, nil); return err }},
		{"Database.ListCollections", func() error { _, err := db.ListCollections(dead, nil); return err }},
		{"Database.RunCommand", func() error { return db.RunCommand(dead, bson.D{{Key: "ping", Value: 1}}, &one) }},
		{"Database.RunCommand (discard)", func() error { return db.RunCommand(dead, bson.D{{Key: "ping", Value: 1}}, nil) }},
		{"Database.RunCommandCursor", func() error {
			_, err := db.RunCommandCursor(dead, bson.D{{Key: "listCollections", Value: 1}})
			return err
		}},
		{"Database.Aggregate", func() error { return db.Aggregate(dead, pipe, &docs) }},
		{"Database.AggregateCursor", func() error { _, err := db.AggregateCursor(dead, pipe); return err }},
		{"Database.Watch", func() error { _, err := db.Watch(dead, pipe); return err }},
		{"Collection.Drop", func() error { return coll.Drop(dead) }},
		{"Collection.InsertOne", func() error { _, err := coll.InsertOne(dead, bson.D{}); return err }},
		{"Collection.InsertMany", func() error { _, err := coll.InsertMany(dead, []bson.M{{}}); return err }},
		{"Collection.Get", func() error { return coll.Get(dead, nil, &one) }},
		{"Collection.Find", func() error { return coll.Find(dead, nil, &docs) }},
		{"Collection.FindCursor", func() error { _, err := coll.FindCursor(dead, nil); return err }},
		{"Collection.Exists", func() error { _, err := coll.Exists(dead, nil); return err }},
		{"Collection.Count", func() error { _, err := coll.Count(dead, nil); return err }},
		{"Collection.EstimatedCount", func() error { _, err := coll.EstimatedCount(dead); return err }},
		{"Collection.UpdateOne", func() error {
			_, err := coll.UpdateOne(dead, bson.D{}, bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: 1}}}})
			return err
		}},
		{"Collection.UpdateByID", func() error {
			_, err := coll.UpdateByID(dead, 1, bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: 1}}}})
			return err
		}},
		{"Collection.UpdateMany", func() error {
			_, err := coll.UpdateMany(dead, bson.D{}, bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: 1}}}})
			return err
		}},
		{"Collection.ReplaceOne", func() error { _, err := coll.ReplaceOne(dead, bson.D{}, bson.D{{Key: "a", Value: 1}}); return err }},
		{"Collection.DeleteOne", func() error { _, err := coll.DeleteOne(dead, bson.D{}); return err }},
		{"Collection.DeleteMany", func() error { _, err := coll.DeleteMany(dead, bson.D{{Key: "n", Value: 1}}); return err }},
		{"Collection.FindOneAndUpdate", func() error {
			return coll.FindOneAndUpdate(dead, bson.D{}, bson.D{{Key: "$set", Value: bson.D{{Key: "a", Value: 1}}}}, &one)
		}},
		{"Collection.FindOneAndReplace", func() error { return coll.FindOneAndReplace(dead, bson.D{}, bson.D{{Key: "a", Value: 1}}, &one) }},
		{"Collection.FindOneAndDelete", func() error { return coll.FindOneAndDelete(dead, bson.D{}, &one) }},
		{"Collection.Distinct", func() error { return coll.Distinct(dead, "n", nil, &names) }},
		{"Collection.Aggregate", func() error { return coll.Aggregate(dead, pipe, &docs) }},
		{"Collection.AggregateCursor", func() error { _, err := coll.AggregateCursor(dead, pipe); return err }},
		{"Collection.BulkWrite", func() error { _, err := coll.BulkWrite(dead, []mongodb.WriteModel{model}); return err }},
		{"Collection.Watch", func() error { _, err := coll.Watch(dead, pipe); return err }},
		{"Collection.CreateIndex", func() error {
			_, err := coll.CreateIndex(dead, mongodb.IndexModel{Keys: bson.D{{Key: "n", Value: 1}}})
			return err
		}},
		{"Collection.CreateIndexes", func() error {
			_, err := coll.CreateIndexes(dead, []mongodb.IndexModel{{Keys: bson.D{{Key: "n", Value: 1}}}})
			return err
		}},
		{"Collection.ListIndexes", func() error { return coll.ListIndexes(dead, &docs) }},
		{"Collection.ListIndexSpecifications", func() error { _, err := coll.ListIndexSpecifications(dead); return err }},
		{"Collection.DropIndex", func() error { return coll.DropIndex(dead, "n_1") }},
		{"Collection.DropIndexWithKey", func() error { return coll.DropIndexWithKey(dead, bson.D{{Key: "n", Value: 1}}) }},
		{"Collection.DropAllIndexes", func() error { return coll.DropAllIndexes(dead) }},
		{"Bucket.Upload", func() error { _, err := bk.Upload(dead, "f", strings.NewReader("x")); return err }},
		{"Bucket.UploadWithID", func() error { return bk.UploadWithID(dead, "id", "f", strings.NewReader("x")) }},
		{"Bucket.Download", func() error { _, err := bk.Download(dead, "id", &bytes.Buffer{}); return err }},
		{"Bucket.DownloadByName", func() error { _, err := bk.DownloadByName(dead, "f", &bytes.Buffer{}); return err }},
		{"Bucket.Delete", func() error { return bk.Delete(dead, "id") }},
		{"Bucket.Rename", func() error { return bk.Rename(dead, "id", "g") }},
		{"Bucket.Find", func() error { return bk.Find(dead, nil, &docs) }},
		{"Bucket.Drop", func() error { return bk.Drop(dead) }},
		{"generic Get", func() error { _, err := mongodb.Get[bson.M](dead, coll, nil); return err }},
		{"generic Find", func() error { _, err := mongodb.Find[bson.M](dead, coll, nil); return err }},
		{"generic Aggregate", func() error { _, err := mongodb.Aggregate[bson.M](dead, coll, pipe); return err }},
	}
	for _, tc := range failing {
		tc := tc
		t.Run("fails: "+tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil {
				t.Fatal("want an error from a cancelled context, got nil")
			}
		})
	}

	t.Run("a result that cannot be decoded is an error", func(t *testing.T) {
		var wrong []int // the documents are not integers
		if err := coll.Find(ctx, nil, &wrong); err == nil {
			t.Fatal("decoding documents into []int should fail")
		}
	})
	t.Run("a pipeline can be passed to Watch", func(t *testing.T) {
		cs, err := coll.Watch(ctx, bson.A{bson.D{{Key: "$match", Value: bson.D{{Key: "operationType", Value: "insert"}}}}})
		must(t, err)
		cs.Close(ctx)
	})
	t.Run("work inside a transaction on a closed client fails", func(t *testing.T) {
		mc, err := mongodb.New(ctx, mongodb.Config{URI: lastURI})
		must(t, err)
		closed := mongodb.Wrap(mc)
		must(t, closed.Close(ctx))
		cc := closed.Database("gcu_closed").Collection("c")
		// StartSession never contacts the server, so an empty transaction still
		// succeeds; any operation inside it fails because the client is closed.
		if err := closed.WithTx(ctx, func(tx context.Context) error { _, err := cc.InsertOne(tx, bson.D{}); return err }); err == nil {
			t.Fatal("WithTx with an operation on a closed client should fail")
		}
		if err := closed.WithTxOptions(ctx, options.Transaction(), func(tx context.Context) error { _, err := cc.InsertOne(tx, bson.D{}); return err }); err == nil {
			t.Fatal("WithTxOptions with an operation on a closed client should fail")
		}
	})
}

type smoke struct {
	name string
	run  func() error
}
