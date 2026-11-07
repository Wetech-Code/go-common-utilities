package mongodb

import (
	"context"
	"errors"
	"reflect"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// ErrNotFound is returned when a document, or a GridFS file, that must exist does
// not. It replaces mongo.ErrNoDocuments and mongo.ErrFileNotFound so callers can use
// errors.Is without importing the driver.
var ErrNotFound = errors.New("mongodb: not found")

func notFound(err error) error {
	if errors.Is(err, mongo.ErrNoDocuments) || errors.Is(err, mongo.ErrFileNotFound) {
		return ErrNotFound
	}
	return err
}

// Types from the driver that appear in method signatures, re-exported so ordinary
// code does not need to import mongo directly. Options come from
// go.mongodb.org/mongo-driver/v2/mongo/options and documents from .../v2/bson.
type (
	// Cursor iterates over results. The caller must Close it.
	Cursor = mongo.Cursor
	// ChangeStream delivers change events. The caller must Close it.
	ChangeStream = mongo.ChangeStream
	// Session carries a causally consistent sequence of operations.
	Session = mongo.Session
	// WriteModel is one operation of a BulkWrite (mongo.NewInsertOneModel, ...).
	WriteModel = mongo.WriteModel
	// IndexModel describes an index to create.
	IndexModel = mongo.IndexModel
	// IndexSpecification describes an existing index.
	IndexSpecification = mongo.IndexSpecification
	// CollectionSpecification describes an existing collection.
	CollectionSpecification = mongo.CollectionSpecification
	// InsertOneResult carries the _id of an inserted document.
	InsertOneResult = mongo.InsertOneResult
	// InsertManyResult carries the _ids of inserted documents.
	InsertManyResult = mongo.InsertManyResult
	// UpdateResult reports matched, modified and upserted counts.
	UpdateResult = mongo.UpdateResult
	// DeleteResult reports the number of deleted documents.
	DeleteResult = mongo.DeleteResult
	// BulkWriteResult reports the outcome of a BulkWrite.
	BulkWriteResult = mongo.BulkWriteResult
	// ListDatabasesResult is the result of Client.ListDatabases.
	ListDatabasesResult = mongo.ListDatabasesResult
)

// Client wraps a MongoDB client with plain methods for the operations the driver
// offers: databases, collections, CRUD with decoding into structs, aggregation,
// bulk writes, indexes, change streams, transactions and GridFS file storage. It
// adds no schema or repository layer.
//
// A missing document is ErrNotFound; duplicate-key and other server errors can be
// recognised with IsDuplicateKey and the other Is* functions. Native is the
// underlying client for anything not wrapped. Closing a Client disconnects Native.
type Client struct {
	// Native is the underlying driver client.
	Native *mongo.Client
}

// Wrap returns a Client around client, typically the result of New:
//
//	mc, err := mongodb.New(ctx, mongodb.Config{URI: uri})
//	if err != nil { return err }
//	c := mongodb.Wrap(mc)
//	defer c.Close(context.Background())
func Wrap(client *mongo.Client) *Client { return &Client{Native: client} }

// ---- client ----

// Ping checks that the primary answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.Native.Ping(ctx, readpref.Primary())
}

// Close disconnects from the deployment, waiting for in-flight operations until
// ctx ends.
func (c *Client) Close(ctx context.Context) error { return c.Native.Disconnect(ctx) }

// Database returns a handle to the named database. It does not talk to the server.
func (c *Client) Database(name string, opts ...options.Lister[options.DatabaseOptions]) *Database {
	return &Database{Native: c.Native.Database(name, opts...)}
}

// ListDatabaseNames returns the names of the databases matching filter (nil for all).
func (c *Client) ListDatabaseNames(ctx context.Context, filter any, opts ...options.Lister[options.ListDatabasesOptions]) ([]string, error) {
	return c.Native.ListDatabaseNames(ctx, filterOrEmpty(filter), opts...)
}

// ListDatabases returns details about the databases matching filter (nil for all).
func (c *Client) ListDatabases(ctx context.Context, filter any, opts ...options.Lister[options.ListDatabasesOptions]) (ListDatabasesResult, error) {
	return c.Native.ListDatabases(ctx, filterOrEmpty(filter), opts...)
}

// Watch opens a change stream on the whole deployment. It needs a replica set or
// sharded cluster. The caller must Close the stream.
func (c *Client) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error) {
	return c.Native.Watch(ctx, pipelineOrEmpty(pipeline), opts...)
}

// BulkWrite runs write operations across several collections and databases in one
// request (MongoDB 8.0+).
func (c *Client) BulkWrite(ctx context.Context, writes []mongo.ClientBulkWrite, opts ...options.Lister[options.ClientBulkWriteOptions]) (*mongo.ClientBulkWriteResult, error) {
	return c.Native.BulkWrite(ctx, writes, opts...)
}

// StartSession starts a session. The caller must call EndSession on it.
func (c *Client) StartSession(opts ...options.Lister[options.SessionOptions]) (*Session, error) {
	return c.Native.StartSession(opts...)
}

// UseSession runs fn with a session carried in the context it receives, then ends
// the session. Use that context for every operation that belongs to the session.
func (c *Client) UseSession(ctx context.Context, fn func(context.Context) error) error {
	return c.Native.UseSession(ctx, fn)
}

// UseSessionWithOptions is UseSession with session options.
func (c *Client) UseSessionWithOptions(ctx context.Context, opts *options.SessionOptionsBuilder, fn func(context.Context) error) error {
	return c.Native.UseSessionWithOptions(ctx, opts, fn)
}

// NumberSessionsInProgress returns how many sessions are open, which is useful for
// spotting a missing EndSession.
func (c *Client) NumberSessionsInProgress() int { return c.Native.NumberSessionsInProgress() }

// AppendDriverInfo adds a library name and version to the connection handshake, so
// the server's logs show which wrapper is connecting.
func (c *Client) AppendDriverInfo(info options.DriverInfo) { c.Native.AppendDriverInfo(info) }

// WithTx runs fn in a transaction: it commits if fn returns nil and aborts if fn
// returns an error. Every operation inside must use the context fn receives, which
// carries the transaction. The driver retries the whole of fn on transient errors
// and retries the commit when its outcome is unknown, for up to about two minutes,
// so fn must be safe to run more than once. Transactions need a replica set or
// sharded cluster.
func (c *Client) WithTx(ctx context.Context, fn func(ctx context.Context) error) error {
	return c.WithTxOptions(ctx, nil, fn)
}

// WithTxOptions is WithTx with explicit transaction options (read and write
// concern, read preference).
func (c *Client) WithTxOptions(ctx context.Context, opts *options.TransactionOptionsBuilder, fn func(ctx context.Context) error) error {
	session, err := c.Native.StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(context.WithoutCancel(ctx))
	var txOpts []options.Lister[options.TransactionOptions]
	if opts != nil {
		txOpts = append(txOpts, opts)
	}
	_, err = session.WithTransaction(ctx, func(txCtx context.Context) (any, error) {
		return nil, fn(txCtx)
	}, txOpts...)
	return err
}

func filterOrEmpty(filter any) any {
	if filter == nil {
		return struct{}{}
	}
	return filter
}

func pipelineOrEmpty(pipeline any) any {
	if pipeline == nil {
		return []struct{}{}
	}
	return pipeline
}

// ---- database ----

// Database is a handle to one database.
type Database struct {
	// Native is the underlying driver database.
	Native *mongo.Database
}

// Name returns the database name.
func (d *Database) Name() string { return d.Native.Name() }

// Collection returns a handle to the named collection. It does not talk to the server.
func (d *Database) Collection(name string, opts ...options.Lister[options.CollectionOptions]) *Collection {
	return &Collection{Native: d.Native.Collection(name, opts...)}
}

// Drop deletes the database and everything in it.
//
// Dangerous: this removes all data in the database.
func (d *Database) Drop(ctx context.Context) error { return d.Native.Drop(ctx) }

// CreateCollection creates a collection explicitly, for options such as capped,
// time series or validation. Collections are otherwise created on first write.
func (d *Database) CreateCollection(ctx context.Context, name string, opts ...options.Lister[options.CreateCollectionOptions]) error {
	return d.Native.CreateCollection(ctx, name, opts...)
}

// CreateView creates a read-only view over viewOn defined by an aggregation pipeline.
func (d *Database) CreateView(ctx context.Context, viewName, viewOn string, pipeline any, opts ...options.Lister[options.CreateViewOptions]) error {
	return d.Native.CreateView(ctx, viewName, viewOn, pipeline, opts...)
}

// ListCollectionNames returns the names of the collections matching filter (nil for all).
func (d *Database) ListCollectionNames(ctx context.Context, filter any, opts ...options.Lister[options.ListCollectionsOptions]) ([]string, error) {
	return d.Native.ListCollectionNames(ctx, filterOrEmpty(filter), opts...)
}

// ListCollectionSpecifications returns details about the collections matching filter (nil for all).
func (d *Database) ListCollectionSpecifications(ctx context.Context, filter any, opts ...options.Lister[options.ListCollectionsOptions]) ([]CollectionSpecification, error) {
	return d.Native.ListCollectionSpecifications(ctx, filterOrEmpty(filter), opts...)
}

// RunCommand runs a database command, such as bson.D{{"ping", 1}}, and decodes the
// reply into dest (a pointer, or nil to discard it).
func (d *Database) RunCommand(ctx context.Context, command any, dest any, opts ...options.Lister[options.RunCmdOptions]) error {
	res := d.Native.RunCommand(ctx, command, opts...)
	if dest == nil {
		return res.Err()
	}
	return res.Decode(dest)
}

// RunCommandCursor runs a command that returns a cursor, such as listIndexes. The
// caller must Close the cursor.
func (d *Database) RunCommandCursor(ctx context.Context, command any, opts ...options.Lister[options.RunCmdOptions]) (*Cursor, error) {
	return d.Native.RunCommandCursor(ctx, command, opts...)
}

// Aggregate runs a database-level pipeline (such as $currentOp or $listLocalSessions)
// and decodes all results into dest, a pointer to a slice.
func (d *Database) Aggregate(ctx context.Context, pipeline any, dest any, opts ...options.Lister[options.AggregateOptions]) error {
	cur, err := d.Native.Aggregate(ctx, pipeline, opts...)
	if err != nil {
		return err
	}
	return decodeAll(ctx, cur, dest)
}

// AggregateCursor is Aggregate that returns the cursor. The caller must Close it.
func (d *Database) AggregateCursor(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*Cursor, error) {
	return d.Native.Aggregate(ctx, pipeline, opts...)
}

// Watch opens a change stream on every collection in the database. It needs a
// replica set or sharded cluster. The caller must Close the stream.
func (d *Database) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error) {
	return d.Native.Watch(ctx, pipelineOrEmpty(pipeline), opts...)
}

// ListCollections returns a cursor of collection descriptions. The caller must Close it.
func (d *Database) ListCollections(ctx context.Context, filter any, opts ...options.Lister[options.ListCollectionsOptions]) (*Cursor, error) {
	return d.Native.ListCollections(ctx, filterOrEmpty(filter), opts...)
}

// ---- collection ----

// Collection is a handle to one collection.
type Collection struct {
	// Native is the underlying driver collection.
	Native *mongo.Collection
}

// Name returns the collection name.
func (c *Collection) Name() string { return c.Native.Name() }

// Database returns the database the collection belongs to.
func (c *Collection) Database() *Database { return &Database{Native: c.Native.Database()} }

// Drop deletes the collection and its indexes.
//
// Dangerous: this removes all data in the collection.
func (c *Collection) Drop(ctx context.Context, opts ...options.Lister[options.DropCollectionOptions]) error {
	return c.Native.Drop(ctx, opts...)
}

// InsertOne inserts a document (a struct, bson.D or bson.M) and returns its _id,
// generated if the document has none.
func (c *Collection) InsertOne(ctx context.Context, document any, opts ...options.Lister[options.InsertOneOptions]) (any, error) {
	res, err := c.Native.InsertOne(ctx, document, opts...)
	if err != nil {
		return nil, err
	}
	return res.InsertedID, nil
}

// InsertMany inserts a slice of documents of any one type and returns their _ids.
func (c *Collection) InsertMany(ctx context.Context, documents any, opts ...options.Lister[options.InsertManyOptions]) ([]any, error) {
	res, err := c.Native.InsertMany(ctx, documents, opts...)
	if err != nil {
		return nil, err
	}
	return res.InsertedIDs, nil
}

// Get finds one document matching filter and decodes it into dest, a pointer to a
// struct or bson.M. It returns ErrNotFound if nothing matches. When several match,
// the first in natural order is returned; sort with options to choose.
//
//	var u User
//	err := coll.Get(ctx, bson.D{{"email", "a@x.io"}}, &u)
func (c *Collection) Get(ctx context.Context, filter any, dest any, opts ...options.Lister[options.FindOneOptions]) error {
	return notFound(c.Native.FindOne(ctx, filterOrEmpty(filter), opts...).Decode(dest))
}

// Find decodes every document matching filter (nil for all) into dest, a pointer to
// a slice: *[]User, *[]bson.M, ... No match gives an empty, non-nil slice.
func (c *Collection) Find(ctx context.Context, filter any, dest any, opts ...options.Lister[options.FindOptions]) error {
	cur, err := c.Native.Find(ctx, filterOrEmpty(filter), opts...)
	if err != nil {
		return err
	}
	return decodeAll(ctx, cur, dest)
}

// FindCursor is Find that returns the cursor, for results too large to hold in
// memory. The caller must Close it.
func (c *Collection) FindCursor(ctx context.Context, filter any, opts ...options.Lister[options.FindOptions]) (*Cursor, error) {
	return c.Native.Find(ctx, filterOrEmpty(filter), opts...)
}

// Exists reports whether at least one document matches filter.
func (c *Collection) Exists(ctx context.Context, filter any) (bool, error) {
	n, err := c.Native.CountDocuments(ctx, filterOrEmpty(filter), options.Count().SetLimit(1))
	return n > 0, err
}

// Count returns how many documents match filter (nil for all). It scans, so for a
// rough total of a large collection prefer EstimatedCount.
func (c *Collection) Count(ctx context.Context, filter any, opts ...options.Lister[options.CountOptions]) (int64, error) {
	return c.Native.CountDocuments(ctx, filterOrEmpty(filter), opts...)
}

// EstimatedCount returns the approximate number of documents from collection
// metadata, without scanning.
func (c *Collection) EstimatedCount(ctx context.Context, opts ...options.Lister[options.EstimatedDocumentCountOptions]) (int64, error) {
	return c.Native.EstimatedDocumentCount(ctx, opts...)
}

// UpdateOne updates the first document matching filter. update is an update
// document such as bson.D{{"$set", bson.D{{"name", "x"}}}}. Use
// options.UpdateOne().SetUpsert(true) to insert when nothing matches. No match is
// not an error: check the result's MatchedCount.
func (c *Collection) UpdateOne(ctx context.Context, filter, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	return c.Native.UpdateOne(ctx, filter, update, opts...)
}

// UpdateByID updates the document whose _id is id.
func (c *Collection) UpdateByID(ctx context.Context, id, update any, opts ...options.Lister[options.UpdateOneOptions]) (*UpdateResult, error) {
	return c.Native.UpdateByID(ctx, id, update, opts...)
}

// UpdateMany updates every document matching filter.
func (c *Collection) UpdateMany(ctx context.Context, filter, update any, opts ...options.Lister[options.UpdateManyOptions]) (*UpdateResult, error) {
	return c.Native.UpdateMany(ctx, filter, update, opts...)
}

// ReplaceOne replaces the first document matching filter with replacement, which
// must not contain update operators.
func (c *Collection) ReplaceOne(ctx context.Context, filter, replacement any, opts ...options.Lister[options.ReplaceOptions]) (*UpdateResult, error) {
	return c.Native.ReplaceOne(ctx, filter, replacement, opts...)
}

// DeleteOne deletes the first document matching filter and returns how many were
// deleted (0 or 1).
func (c *Collection) DeleteOne(ctx context.Context, filter any, opts ...options.Lister[options.DeleteOneOptions]) (int64, error) {
	res, err := c.Native.DeleteOne(ctx, filter, opts...)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

// DeleteMany deletes every document matching filter and returns how many. A nil or
// empty filter deletes the whole collection's documents.
//
// Dangerous: an empty filter removes every document.
func (c *Collection) DeleteMany(ctx context.Context, filter any, opts ...options.Lister[options.DeleteManyOptions]) (int64, error) {
	res, err := c.Native.DeleteMany(ctx, filterOrEmpty(filter), opts...)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

// FindOneAndUpdate atomically updates the first document matching filter and
// decodes it into dest: the document before the update, or after it with
// options.FindOneAndUpdate().SetReturnDocument(options.After). It returns
// ErrNotFound if nothing matched and the options do not upsert.
func (c *Collection) FindOneAndUpdate(ctx context.Context, filter, update any, dest any, opts ...options.Lister[options.FindOneAndUpdateOptions]) error {
	return notFound(c.Native.FindOneAndUpdate(ctx, filter, update, opts...).Decode(dest))
}

// FindOneAndReplace atomically replaces the first document matching filter and
// decodes the old (or, with options, the new) document into dest.
func (c *Collection) FindOneAndReplace(ctx context.Context, filter, replacement any, dest any, opts ...options.Lister[options.FindOneAndReplaceOptions]) error {
	return notFound(c.Native.FindOneAndReplace(ctx, filter, replacement, opts...).Decode(dest))
}

// FindOneAndDelete atomically deletes the first document matching filter and
// decodes it into dest. It returns ErrNotFound if nothing matched.
func (c *Collection) FindOneAndDelete(ctx context.Context, filter any, dest any, opts ...options.Lister[options.FindOneAndDeleteOptions]) error {
	return notFound(c.Native.FindOneAndDelete(ctx, filter, opts...).Decode(dest))
}

// Distinct decodes the distinct values of field among documents matching filter
// (nil for all) into dest, a pointer to a slice such as *[]string.
func (c *Collection) Distinct(ctx context.Context, field string, filter any, dest any, opts ...options.Lister[options.DistinctOptions]) error {
	return c.Native.Distinct(ctx, field, filterOrEmpty(filter), opts...).Decode(dest)
}

// Aggregate runs an aggregation pipeline and decodes all results into dest, a
// pointer to a slice.
func (c *Collection) Aggregate(ctx context.Context, pipeline any, dest any, opts ...options.Lister[options.AggregateOptions]) error {
	cur, err := c.Native.Aggregate(ctx, pipeline, opts...)
	if err != nil {
		return err
	}
	return decodeAll(ctx, cur, dest)
}

// AggregateCursor is Aggregate that returns the cursor. The caller must Close it.
func (c *Collection) AggregateCursor(ctx context.Context, pipeline any, opts ...options.Lister[options.AggregateOptions]) (*Cursor, error) {
	return c.Native.Aggregate(ctx, pipeline, opts...)
}

// BulkWrite runs several write operations (mongo.NewInsertOneModel,
// NewUpdateOneModel, NewDeleteOneModel, ...) in one request.
func (c *Collection) BulkWrite(ctx context.Context, models []WriteModel, opts ...options.Lister[options.BulkWriteOptions]) (*BulkWriteResult, error) {
	return c.Native.BulkWrite(ctx, models, opts...)
}

// Watch opens a change stream on the collection. It needs a replica set or sharded
// cluster. The caller must Close the stream.
func (c *Collection) Watch(ctx context.Context, pipeline any, opts ...options.Lister[options.ChangeStreamOptions]) (*ChangeStream, error) {
	return c.Native.Watch(ctx, pipelineOrEmpty(pipeline), opts...)
}

// ---- indexes ----

// CreateIndex creates one index and returns its name.
func (c *Collection) CreateIndex(ctx context.Context, model IndexModel, opts ...options.Lister[options.CreateIndexesOptions]) (string, error) {
	return c.Native.Indexes().CreateOne(ctx, model, opts...)
}

// CreateIndexes creates several indexes and returns their names.
func (c *Collection) CreateIndexes(ctx context.Context, models []IndexModel, opts ...options.Lister[options.CreateIndexesOptions]) ([]string, error) {
	return c.Native.Indexes().CreateMany(ctx, models, opts...)
}

// ListIndexes decodes the collection's indexes into dest, a pointer to a slice
// such as *[]bson.M.
func (c *Collection) ListIndexes(ctx context.Context, dest any, opts ...options.Lister[options.ListIndexesOptions]) error {
	cur, err := c.Native.Indexes().List(ctx, opts...)
	if err != nil {
		return err
	}
	return decodeAll(ctx, cur, dest)
}

// ListIndexSpecifications returns the collection's indexes as typed specifications.
func (c *Collection) ListIndexSpecifications(ctx context.Context, opts ...options.Lister[options.ListIndexesOptions]) ([]IndexSpecification, error) {
	return c.Native.Indexes().ListSpecifications(ctx, opts...)
}

// DropIndex drops the index called name.
func (c *Collection) DropIndex(ctx context.Context, name string, opts ...options.Lister[options.DropIndexesOptions]) error {
	return c.Native.Indexes().DropOne(ctx, name, opts...)
}

// DropIndexWithKey drops the index with the given key specification, such as
// bson.D{{"email", 1}}.
func (c *Collection) DropIndexWithKey(ctx context.Context, keySpec any, opts ...options.Lister[options.DropIndexesOptions]) error {
	return c.Native.Indexes().DropWithKey(ctx, keySpec, opts...)
}

// DropAllIndexes drops every index except the one on _id.
func (c *Collection) DropAllIndexes(ctx context.Context, opts ...options.Lister[options.DropIndexesOptions]) error {
	return c.Native.Indexes().DropAll(ctx, opts...)
}

// ---- generics ----

// Get finds one document matching filter and returns it as a T (a struct or
// bson.M). It returns ErrNotFound if nothing matches. Inside Client.WithTx pass the
// transaction's context.
func Get[T any](ctx context.Context, coll *Collection, filter any, opts ...options.Lister[options.FindOneOptions]) (T, error) {
	var v T
	err := coll.Get(ctx, filter, &v, opts...)
	return v, err
}

// Find returns every document matching filter (nil for all) as a []T. No match
// gives an empty, non-nil slice.
func Find[T any](ctx context.Context, coll *Collection, filter any, opts ...options.Lister[options.FindOptions]) ([]T, error) {
	var out []T
	err := coll.Find(ctx, filter, &out, opts...)
	return out, err
}

// Aggregate runs a pipeline and returns the results as a []T.
func Aggregate[T any](ctx context.Context, coll *Collection, pipeline any, opts ...options.Lister[options.AggregateOptions]) ([]T, error) {
	var out []T
	err := coll.Aggregate(ctx, pipeline, &out, opts...)
	return out, err
}

// decodeAll reads every document from cur into dest, a pointer to a slice, and
// closes the cursor. An empty result leaves an empty, non-nil slice.
func decodeAll(ctx context.Context, cur *Cursor, dest any) error {
	if err := cur.All(ctx, dest); err != nil {
		return err
	}
	if v := reflect.ValueOf(dest); v.Kind() == reflect.Pointer && !v.IsNil() {
		if s := v.Elem(); s.Kind() == reflect.Slice && s.IsNil() {
			s.Set(reflect.MakeSlice(s.Type(), 0, 0))
		}
	}
	return nil
}

// ---- errors ----

// MongoDB server error codes for the errors callers most often handle.
const (
	CodeNamespaceNotFound = 26
	CodeWriteConflict     = 112
	CodeDuplicateKey      = 11000
)

// IsDuplicateKey reports a unique-index violation (code 11000), from a single write,
// a bulk write or a command.
func IsDuplicateKey(err error) bool { return mongo.IsDuplicateKeyError(err) }

// IsTimeout reports a timeout from the server or the driver, including a context
// deadline.
func IsTimeout(err error) bool { return mongo.IsTimeout(err) }

// IsNetworkError reports a network failure talking to the server.
func IsNetworkError(err error) bool { return mongo.IsNetworkError(err) }

// HasCode reports whether err is a server error carrying the given error code.
func HasCode(err error, code int) bool {
	var se mongo.ServerError
	return errors.As(err, &se) && se.HasErrorCode(code)
}

// HasLabel reports whether err carries the given server error label, such as
// "TransientTransactionError".
func HasLabel(err error, label string) bool {
	var se mongo.ServerError
	return errors.As(err, &se) && se.HasErrorLabel(label)
}

// IsTransient reports an error after which the whole transaction can safely be
// retried (label TransientTransactionError). WithTx already retries these.
func IsTransient(err error) bool { return HasLabel(err, "TransientTransactionError") }

// IsWriteConflict reports a write conflict between concurrent transactions (code 112).
func IsWriteConflict(err error) bool { return HasCode(err, CodeWriteConflict) }

// IsNamespaceNotFound reports an operation on a collection or database that does
// not exist, for example dropping a collection twice (code 26).
func IsNamespaceNotFound(err error) bool { return HasCode(err, CodeNamespaceNotFound) }
