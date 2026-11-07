package mongodb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestNotFoundMapping(t *testing.T) {
	if !errors.Is(notFound(mongo.ErrNoDocuments), ErrNotFound) {
		t.Error("ErrNoDocuments should map to ErrNotFound")
	}
	if !errors.Is(notFound(fmt.Errorf("wrapped: %w", mongo.ErrFileNotFound)), ErrNotFound) {
		t.Error("ErrFileNotFound should map to ErrNotFound")
	}
	boom := errors.New("boom")
	if notFound(boom) != boom || notFound(nil) != nil {
		t.Error("other errors and nil must pass through unchanged")
	}
}

func TestErrorHelpers(t *testing.T) {
	dup := mongo.WriteException{WriteErrors: []mongo.WriteError{{Code: CodeDuplicateKey, Message: "E11000 duplicate key"}}}
	conflict := mongo.CommandError{Code: CodeWriteConflict, Labels: []string{"TransientTransactionError"}, Name: "WriteConflict"}
	noNS := mongo.CommandError{Code: CodeNamespaceNotFound, Name: "NamespaceNotFound"}
	plain := errors.New("not a server error")

	if !IsDuplicateKey(fmt.Errorf("wrapped: %w", dup)) || IsDuplicateKey(conflict) || IsDuplicateKey(plain) {
		t.Error("IsDuplicateKey")
	}
	if !IsWriteConflict(conflict) || IsWriteConflict(noNS) || IsWriteConflict(plain) {
		t.Error("IsWriteConflict")
	}
	if !IsTransient(conflict) || IsTransient(noNS) || IsTransient(plain) {
		t.Error("IsTransient")
	}
	if !IsNamespaceNotFound(noNS) || IsNamespaceNotFound(conflict) {
		t.Error("IsNamespaceNotFound")
	}
	if !HasCode(noNS, 26) || HasCode(noNS, 27) || !HasLabel(conflict, "TransientTransactionError") || HasLabel(conflict, "x") {
		t.Error("HasCode/HasLabel")
	}
	if !IsTimeout(context.DeadlineExceeded) || IsTimeout(plain) {
		t.Error("IsTimeout")
	}
	if IsNetworkError(plain) || IsNetworkError(nil) {
		t.Error("IsNetworkError must be false for non-network errors")
	}
}

func TestFilterAndPipelineDefaults(t *testing.T) {
	if got := filterOrEmpty(nil); !reflect.DeepEqual(got, struct{}{}) {
		t.Errorf("filterOrEmpty(nil) = %#v", got)
	}
	f := bson.D{{Key: "a", Value: 1}}
	if got := filterOrEmpty(f); !reflect.DeepEqual(got, f) {
		t.Errorf("a given filter must pass through, got %#v", got)
	}
	if got := pipelineOrEmpty(nil); reflect.ValueOf(got).Len() != 0 {
		t.Errorf("pipelineOrEmpty(nil) = %#v", got)
	}
}

func TestDecodeAll(t *testing.T) {
	type doc struct {
		N int `bson:"n"`
	}
	cursorOf := func(docs ...any) *Cursor {
		c, err := mongo.NewCursorFromDocuments(docs, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	var got []doc
	if err := decodeAll(context.Background(), cursorOf(bson.D{{Key: "n", Value: 1}}, bson.D{{Key: "n", Value: 2}}), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].N != 2 {
		t.Errorf("decoded %#v", got)
	}
	var none []doc
	if err := decodeAll(context.Background(), cursorOf(), &none); err != nil {
		t.Fatal(err)
	}
	if none == nil || len(none) != 0 {
		t.Errorf("no documents should give an empty non-nil slice, got %#v", none)
	}
}

func TestWrapAndHandles(t *testing.T) {
	// Connecting is lazy, so handles can be built without a server.
	mc, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	defer mc.Disconnect(context.Background())
	c := Wrap(mc)
	if c.Native != mc {
		t.Fatal("Native should be the wrapped client")
	}
	db := c.Database("app")
	coll := db.Collection("users")
	if db.Name() != "app" || coll.Name() != "users" || coll.Database().Name() != "app" {
		t.Errorf("handle names: %q %q %q", db.Name(), coll.Name(), coll.Database().Name())
	}
	if db.Bucket().Native == nil {
		t.Error("Bucket should return a GridFS bucket")
	}
	if c.NumberSessionsInProgress() != 0 {
		t.Error("no sessions should be open")
	}
}

// native methods deliberately absent from a wrapper, with the reason.
var skipped = map[string]map[string]string{
	"Client": {},
	"Database": {
		"Client":       "use db.Native.Client()",
		"GridFSBucket": "wrapped as Bucket",
	},
	"Collection": {
		"Clone":                  "use coll.Native.Clone(opts...)",
		"Indexes":                "wrapped as CreateIndex, CreateIndexes, ListIndexes, DropIndex, ...",
		"SearchIndexes":          "Atlas Search only; use coll.Native.SearchIndexes()",
		"CountDocuments":         "wrapped as Count",
		"EstimatedDocumentCount": "wrapped as EstimatedCount",
		"FindOne":                "wrapped as Get",
		"Find":                   "wrapped as Find (decoding) and FindCursor",
		"Aggregate":              "wrapped as Aggregate (decoding) and AggregateCursor",
	},
	"Session": {},
	"GridFSBucket": {
		"GetChunksCollection":      "use bucket.Native",
		"GetFilesCollection":       "use bucket.Native",
		"OpenDownloadStream":       "streaming; use bucket.Native",
		"OpenDownloadStreamByName": "streaming; use bucket.Native",
		"OpenUploadStream":         "streaming; use bucket.Native",
		"OpenUploadStreamWithID":   "streaming; use bucket.Native",
		"UploadFromStream":         "wrapped as Upload",
		"UploadFromStreamWithID":   "wrapped as UploadWithID",
		"DownloadToStream":         "wrapped as Download",
		"DownloadToStreamByName":   "wrapped as DownloadByName",
	},
}

// Every method of the driver's Client, Database, Collection and GridFSBucket has a
// wrapper of the same name, or is listed above with the reason. If this fails after
// upgrading the driver, add a wrapper or an entry.
func TestEveryDriverMethodIsWrappedOrExplained(t *testing.T) {
	pairs := []struct {
		name   string
		native reflect.Type
		ours   reflect.Type
	}{
		{"Client", reflect.TypeFor[*mongo.Client](), reflect.TypeFor[*Client]()},
		{"Database", reflect.TypeFor[*mongo.Database](), reflect.TypeFor[*Database]()},
		{"Collection", reflect.TypeFor[*mongo.Collection](), reflect.TypeFor[*Collection]()},
		{"GridFSBucket", reflect.TypeFor[*mongo.GridFSBucket](), reflect.TypeFor[*Bucket]()},
	}
	var missing []string
	for _, p := range pairs {
		for i := 0; i < p.native.NumMethod(); i++ {
			name := p.native.Method(i).Name
			if _, ok := p.ours.MethodByName(name); ok {
				continue
			}
			if p.name == "Client" && name == "Disconnect" {
				continue // wrapped as Close
			}
			if skipped[p.name][name] != "" {
				continue
			}
			missing = append(missing, p.name+"."+name)
		}
	}
	// Index and session operations live on other driver types.
	for _, m := range []struct {
		native reflect.Type
		names  []string
	}{
		{reflect.TypeFor[mongo.IndexView](), []string{"CreateOne", "CreateMany", "DropAll", "DropOne", "DropWithKey", "List", "ListSpecifications"}},
	} {
		for _, n := range m.names {
			if _, ok := m.native.MethodByName(n); !ok {
				missing = append(missing, "IndexView."+n+" (no longer exists; update the wrapper)")
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("driver methods with no wrapper and no explanation: %v", missing)
	}
}

var _ = time.Second
