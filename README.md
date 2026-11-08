# go-common-utilities

Small shared Go packages for Wetech backends: **config, logger, postgres, mongodb, redis, s3**.
One repository, one Go module, one version number. Import only the packages you need; each provider package
returns the driver's own client, so nothing is hidden behind a custom abstraction.

Module: `github.com/Wetech-Code/go-common-utilities` · Go 1.26.10 or newer

| Package | What it gives you | Native type you get back |
| --- | --- | --- |
| [`config`](config) | Read env vars as string/int/bool/duration/list, collect all errors, require values | — |
| [`logger`](logger) | `*slog.Logger` from a small config; JSON or text; redacts password/token/secret keys | `*slog.Logger` |
| [`postgres`](postgres) | Validated pgx pool, startup ping | `*pgxpool.Pool` |
| [`mongodb`](mongodb) | Validated MongoDB client (driver v2), startup ping | `*mongo.Client` |
| [`redis`](redis) | Validated go-redis client, startup ping | `*redis.Client` |
| [`s3`](s3) | S3 / SeaweedFS client: bucket-bound helpers, every S3 operation, presigned URLs | `*s3.Client` (AWS SDK v2) via `Client.S3` |

Not included, on purpose: ORMs, migrations, repositories, queues, a generic `Database` interface, global clients,
retries around writes, authorization, anything specific to a product. Adding a helper needs a concrete consumer and a review.

## Install

```sh
go get github.com/Wetech-Code/go-common-utilities@latest   # or pin a version, e.g. @v0.1.1
```

API documentation: https://pkg.go.dev/github.com/Wetech-Code/go-common-utilities

## Import

```go
import (
	"github.com/Wetech-Code/go-common-utilities/config"
	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/postgres"
	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"github.com/Wetech-Code/go-common-utilities/redis"
	"github.com/Wetech-Code/go-common-utilities/s3"
)
```

Importing `postgres` does not pull in the MongoDB, Redis or AWS libraries, and vice versa.

## Configure, open, close

Every provider package follows the same shape: a `Config` struct, `New(ctx, cfg)` that validates, connects and pings
(so a wrong address or password fails at startup), and **you own the result and must close it**. Errors never contain
passwords or secret keys.

```go
r := config.New()
dsn := r.Require("DATABASE_URL")
mongoURI := r.Require("MONGODB_URI")
redisURL := r.Require("REDIS_URL")
s3Endpoint := r.Require("S3_ENDPOINT")
s3Key, s3Secret := r.Require("S3_ACCESS_KEY_ID"), r.Require("S3_SECRET_ACCESS_KEY")
level := r.String("LOG_LEVEL", "info")
if err := r.Err(); err != nil { // reports every missing or invalid setting at once
	return err
}

log, err := logger.New(logger.Config{Level: level})
if err != nil { return err }

pool, err := postgres.New(ctx, postgres.Config{URL: dsn, MaxConns: 10})
if err != nil { return err }
defer pool.Close()

mdb, err := mongodb.New(ctx, mongodb.Config{URI: mongoURI})
if err != nil { return err }
defer mdb.Disconnect(context.Background())

rdb, err := redis.New(ctx, redis.Config{URL: redisURL})
if err != nil { return err }
defer rdb.Close()

store, err := s3.New(ctx, s3.Config{
	Endpoint: s3Endpoint, AccessKeyID: s3Key, SecretAccessKey: s3Secret,
	Bucket: "uploads", UsePathStyle: true,
})
if err != nil { return err }
defer store.Close()

log.Info("connected")
```

Each package has a runnable example in its `example_test.go` and full documentation via `go doc`, e.g. `go doc -all ./s3`.

### PostgreSQL

`URL` is a normal connection string. TLS is chosen by the URL, so use `sslmode=verify-full` outside local development.
Optional: `MaxConns`, `MinConns`, `MaxConnLifetime`, `MaxConnIdleTime`, `HealthCheckPeriod`, `ConnectTimeout`, `AppName`.
Use the returned pool exactly as you would use pgx (`pool.Query`, `pool.Begin`, `pool.CopyFrom`, ...).

```sh
DATABASE_URL=postgres://app:<password>@db.internal:5432/app?sslmode=verify-full
```

For plain methods and the usual conveniences, wrap the pool with `postgres.Wrap`. You still write the SQL; nothing
here builds queries or knows your schema, so it is not an ORM. A missing row is `postgres.ErrNotFound` (use `errors.Is`):

```go
c := postgres.Wrap(pool)                          // pool from postgres.New; c.Close() closes it

n, err := c.Exec(ctx, "UPDATE users SET name = $1 WHERE id = $2", "Ann", 7) // rows affected

type User struct{ ID int64; Email string; OwnerID *int64 } // filled by column name, see below
u, err := postgres.Get[User](ctx, c, "SELECT id, email, owner_id FROM users WHERE id = $1", 7)
all, err := postgres.Select[User](ctx, c, "SELECT id, email FROM users")      // or Select[*User]
count, err := postgres.Get[int64](ctx, c, "SELECT count(*) FROM users")

var user User                                     // or the sqlx style, filling a destination you pass in
err = c.Get(ctx, &user, "SELECT id, email FROM users WHERE id = $1", 7)
var users []User                                  // also *[]*User, *[]string, *[]int64, ...
err = c.Select(ctx, &users, "SELECT id, email FROM users ORDER BY id")

err = c.WithTx(ctx, func(tx postgres.Tx) error {  // commits on nil, rolls back on error or panic
	_, err := tx.Exec(ctx, "INSERT INTO users (id, email) VALUES ($1, $2)", 8, "b@x.io")
	return err
})
if postgres.IsUniqueViolation(err) { /* duplicate key */ }
```

| Group | Methods |
| --- | --- |
| Run SQL | `Exec` `ExecTag` `Query` `QueryRow` `Exists`; scanning: methods `Get` `Select` (into a destination you pass) and package functions `Get[T]` `Select[T]` `SelectMaps` (these also work inside a `Tx`) |
| Transactions | `WithTx` `WithTxOptions` `Begin` `BeginTx` (inside, `Tx` is the full pgx transaction: `Exec`, `Query`, `CopyFrom`, `SendBatch`, savepoints) |
| Bulk and batches | `CopyFrom` `CopyFromRows` `SendBatch` `RunBatch` |
| Dedicated connections | `Acquire` `AcquireFunc` `AcquireAllIdle` |
| LISTEN / NOTIFY | `Notify` `Listen` (then `Wait`, `Close` on the `Listener`) |
| Pool | `Ping` `Stat` `Config` `Reset` `Close` |
| Errors | `ErrNotFound` `ErrTooManyRows` `ErrorCode` `ConstraintName` `IsUniqueViolation` `IsForeignKeyViolation` `IsNotNullViolation` `IsCheckViolation` `IsSerializationFailure` `IsDeadlock` |

**Struct scanning.** `Get`, `Select` and their generic forms fill a struct, a pointer to a struct, a `map[string]any`, or a single
value (`int64`, `string`, `time.Time`, `[]byte`, `pgtype.Numeric`, `netip.Addr`, a pointer for a nullable column, ...).
Field types are whatever pgx can scan: arrays into slices, `jsonb`/`json` into maps, structs or `json.RawMessage`, enums into
string types, `uuid`, `inet`, `interval`, `numeric` and so on.

- **Matching:** a field matches a column by its `db:"name"` tag, else by its Go name ignoring case (`ID` ↔ `id`) or in snake_case
  (`UserID` ↔ `user_id`, `HTTPServer` ↔ `http_server`). Quoted mixed-case column names match too.
- **Lax:** columns with no field are ignored, and fields with no column keep their value. `db:"-"` skips a field and unexported
  fields are ignored.
- **Embedded structs** are flattened, including embedded pointers (allocated only when a column needs them, even when the pointer's
  type is unexported). An outer field wins over an embedded one of the same name.
- **One JSON column:** if the query returns a single `json`/`jsonb` column that matches no field, the whole value is decoded into
  the struct with `encoding/json`, so `SELECT to_jsonb(u) FROM users u` fills a struct that has `json` tags.
- **Safety:** two columns that map to the same field (a join with two `id` columns) is an error, so one never silently overwrites
  the other; alias one in the query.
- **NULL:** a `NULL` in a column mapped to a plain (non-pointer) field is an error by default, because a silent zero hides bugs. Use
  a pointer (`*string`), a `sql.Null*` type, or opt in per field with `db:"nick,nullzero"` to get the zero value instead.
- **Custom Postgres types:** list them in `Config.Types` and they are registered on every connection, with their array types:
  `postgres.Config{URL: url, Types: []string{"hstore", "app.mood", "app.address"}}`. This covers `hstore` (scan into
  `pgtype.Hstore`), enums, domains, ranges and composites. A composite scans into a struct (or `*struct`, nil for `NULL`) by
  attribute name, using the same matching rules as columns. List a type's dependencies first, and `CREATE EXTENSION hstore` first.
  A type that does not exist makes `New` fail at startup. Not supported: arrays of composite values into `[]Struct` (scan those
  into a `pgtype` array, or select the elements as rows).
- **Results:** `Get` returns `ErrNotFound` for no rows and `ErrTooManyRows` for several; `Select` returns an empty, non-nil slice
  for none. A single-value type needs a one-column query.

`WithTx` never retries by itself; on `IsSerializationFailure` or `IsDeadlock` you can run the whole transaction again.
Every method is called by at least one test, and a test fails if a pgx pool method has no wrapper.

### MongoDB

`URI` starts with `mongodb://` or `mongodb+srv://`; add `tls=true` (implicit for `+srv`) for TLS.
Optional: `AppName`, `MaxPoolSize`, `ConnectTimeout`. This is the official driver **v2**
(`go.mongodb.org/mongo-driver/v2`); use `client.Database("x").Collection("y")` as usual.

```sh
MONGODB_URI=mongodb://app:<password>@mongo.internal:27017/?tls=true&replicaSet=rs0
```

For plain methods and the usual conveniences, wrap the client with `mongodb.Wrap`. It adds no schema or repository layer.
A missing document is `mongodb.ErrNotFound` (use `errors.Is`); documents are decoded with the driver's own `bson` tags:

```go
c := mongodb.Wrap(mc)                                  // mc from mongodb.New; defer c.Close(ctx)
users := c.Database("app").Collection("users")

type User struct {
	ID    bson.ObjectID `bson:"_id,omitempty"`
	Email string        `bson:"email"`
	Age   int           `bson:"age"`
}
id, err := users.InsertOne(ctx, User{Email: "a@x.io", Age: 30})

var u User
err = users.Get(ctx, bson.D{{Key: "email", Value: "a@x.io"}}, &u)   // ErrNotFound if none
var adults []User
err = users.Find(ctx, bson.D{{Key: "age", Value: bson.D{{Key: "$gte", Value: 18}}}}, &adults,
	options.Find().SetSort(bson.D{{Key: "age", Value: 1}}))               // or mongodb.Find[User](...)

err = c.WithTx(ctx, func(tx context.Context) error {  // commits on nil, aborts on error
	_, err := users.UpdateOne(tx, bson.D{{Key: "email", Value: "a@x.io"}}, bson.D{{Key: "$inc", Value: bson.D{{Key: "age", Value: 1}}}})
	return err                                         // use tx, not ctx, for every operation inside
})
if mongodb.IsDuplicateKey(err) { /* unique index violated */ }
```

| Group | Methods |
| --- | --- |
| Client | `Ping` `Close` `Database` `ListDatabaseNames` `ListDatabases` `Watch` `BulkWrite` `StartSession` `UseSession` `UseSessionWithOptions` `NumberSessionsInProgress` `AppendDriverInfo` `WithTx` `WithTxOptions` |
| Database | `Name` `Collection` `Bucket` `Drop` `CreateCollection` `CreateView` `ListCollectionNames` `ListCollectionSpecifications` `ListCollections` `RunCommand` `RunCommandCursor` `Aggregate` `AggregateCursor` `Watch` |
| Read | `Get` `Find` `FindCursor` `Exists` `Count` `EstimatedCount` `Distinct` `Aggregate` `AggregateCursor`; generics `mongodb.Get[T]` `Find[T]` `Aggregate[T]` |
| Write | `InsertOne` `InsertMany` `UpdateOne` `UpdateByID` `UpdateMany` `ReplaceOne` `DeleteOne` `DeleteMany` `FindOneAndUpdate` `FindOneAndReplace` `FindOneAndDelete` `BulkWrite` |
| Indexes | `CreateIndex` `CreateIndexes` `ListIndexes` `ListIndexSpecifications` `DropIndex` `DropIndexWithKey` `DropAllIndexes` |
| Change streams | `Watch` on the client, a database or a collection |
| GridFS files | `db.Bucket()` then `Upload` `UploadWithID` `Download` `DownloadByName` `Rename` `Find` `Delete` `Drop` (streaming and the files and chunks collections via `Bucket.Native`) |
| Errors | `ErrNotFound` `IsDuplicateKey` `IsTimeout` `IsNetworkError` `IsTransient` `IsWriteConflict` `IsNamespaceNotFound` `HasCode` `HasLabel` |

Notes: transactions and change streams need a replica set or sharded cluster. `WithTx` follows the driver, which retries the
whole function on transient errors and retries an uncertain commit for up to about two minutes, so the function must be safe to
run more than once. `DeleteMany` with a nil filter deletes every document, and `Drop` removes data; both are marked "Dangerous"
in their docs. Atlas Search indexes, `Collection.Clone`, and streaming GridFS uploads and downloads are reachable through the
`Native` field. Every driver method has a wrapper or a documented reason it does not, a test enforces that, and every wrapper
is called by at least one test.

### Redis

`URL` is `redis://[user:password@]host:port/db`, or `rediss://` for TLS (certificates are verified).
Optional: `PoolSize`, `DialTimeout`, `ReadTimeout`, `WriteTimeout`. You get a go-redis v9 client.

For plain `(value, error)` methods instead of go-redis command objects, wrap it with `redis.Wrap`. `Client` has a
method for **every command go-redis implements (620 as of v9.23.0)**, so `c.Set`, `c.Get`, `c.HSet`, `c.LPush`, `c.ZAdd`, `c.XAdd`,
`c.GeoAdd`, `c.JSONSet`, `c.TSAdd` and the rest are all there. A key, field or member that does not exist gives
`redis.ErrNotFound` (use `errors.Is`), not go-redis's `redis.Nil`:

```go
c := redis.Wrap(rdb)             // rdb from redis.New; c.Close() closes it
err := c.Set(ctx, "greeting", "hello", time.Minute)
v, err := c.Get(ctx, "greeting") // errors.Is(err, redis.ErrNotFound) when missing
n, err := c.Incr(ctx, "visits")
```

| Family | Examples |
| --- | --- |
| Strings, counters, keys | `Set` `SetNX` `Get` `GetEx` `MGet` `Incr` `Del` `Unlink` `Expire` `TTL` `Scan` `Copy` `Dump` |
| Hashes, lists, sets | `HSet` `HGetAll` `HScan` `LPush` `LMove` `BLPop` `SAdd` `SInter` `SMembersMap` |
| Sorted sets | `ZAdd` `ZRange` `ZRangeByScore` `ZPopMin` `ZUnionStore` `ZScore` |
| Streams | `XAdd` `XRead` `XReadGroup` `XAck` `XPending` `XInfoStream` |
| Bitmaps, HyperLogLog, geo | `SetBit` `BitCount` `PFAdd` `PFCount` `GeoAdd` `GeoSearch` |
| Scripts, functions, batches | `Eval` `EvalSha` `FCall` `Pipelined` `TxPipelined` `Watch` |
| Pub/sub | `Publish` `Subscribe` `PubSubChannels` |
| Server and cluster | `Info` `DBSize` `ConfigGet` `ClientList` `Cluster*` |
| Modules (need a server with them) | `JSONSet` `TSAdd` `BFAdd` `FTSearch` `VAdd` |

About 60 common methods are written by hand in `redis/client.go`; the rest are generated from go-redis's own source into
`redis/client_gen.go` by `internal/genredis`. After changing the go-redis version run `go generate ./redis` and review
the diff; a unit test fails if go-redis gains a command that has not been generated. Arguments such as `XAddArgs` and
`GeoLocation` are go-redis types. Not wrapped: `Monitor` (a streaming command; use `c.Native`), and `Quit`, `Sync` and `JSONDebugMemory`, which go-redis itself
implements as panics.

**Dangerous commands are included.** `FlushAll`, `FlushDB`, `Shutdown`, `ConfigSet`, ACL changes, `Keys`, `Migrate`,
cluster failover and similar methods are present and marked "Dangerous" in their documentation. Nothing in this library
calls them, and the tests never do. Business queues, locks and key conventions are still left to the caller.

### S3 / SeaweedFS

Point `Endpoint` at the SeaweedFS S3 gateway (default port 8333) and set `UsePathStyle: true`. Credentials are the
access/secret key of an identity in the gateway's `-s3.config`. `s3.New` checks that the bucket exists; to use a client you
built yourself, or to work with buckets that may not exist yet, use `s3.Wrap(native, bucket)`, which contacts nothing.

```sh
S3_ENDPOINT=https://seaweed.internal:8333
S3_ACCESS_KEY_ID=<from the gateway's s3 config>
S3_SECRET_ACCESS_KEY=<from the gateway's s3 config>
```

```go
store, err := s3.New(ctx, s3.Config{Endpoint: endpoint, AccessKeyID: id, SecretAccessKey: secret, Bucket: "uploads", UsePathStyle: true})
defer store.Close()

err = store.Upload(ctx, "videos/a.mp4", file, "video/mp4", 0)   // streams, multipart, any size, aborts on failure
info, err := store.Head(ctx, "videos/a.mp4")                      // s3.ErrNotFound if missing
ok, err := store.Exists(ctx, "videos/a.mp4")
objs, err := store.List(ctx, "videos/")                           // all pages; Walk for huge buckets
err = store.Copy(ctx, "videos/a.mp4", "archive/a.mp4")
err = store.DeleteMany(ctx, []string{"tmp/1", "tmp/2"})           // batches of 1000
n, err := store.Download(ctx, "videos/a.mp4", w)
err = store.SetTags(ctx, "videos/a.mp4", map[string]string{"team": "ops"})
u, err := store.PresignGet(ctx, "videos/a.mp4", 15*time.Minute)

// Everything else in the S3 API: the SDK input, the bucket filled in for you.
out, err := store.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
	VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
```

| Group | Methods |
| --- | --- |
| Helpers | `Put` `Get` `Delete` `Head` `Exists` `List` `Walk` `Copy` `DeleteMany` `Download` `Upload` `GetTags` `SetTags` `Ping` `Close` |
| Presign | `PresignGet` `PresignPut` `PresignHead` `PresignDelete` `PresignUploadPart` `PresignPost` `PresignHeadBucket` `PresignDeleteBucket` (at most 7 days) |
| Every S3 operation | all 112 methods of the SDK client (`GetObject`, `PutBucketPolicy`, `ListObjectVersions`, `CreateMultipartUpload`, `PutObjectRetention`, ...), generated by `go generate ./s3` |

The generated methods take the SDK's own input struct and return its output. They fill in `Bucket` when you leave it empty
(the input you pass is never modified), prefix errors with the operation name, and make object-level "no such key" errors
match `s3.ErrNotFound` (use `errors.Is`; the SDK error stays reachable with `errors.As`). `GetObject`'s `Body` must be closed.
Bucket deletion and policy, lifecycle, ACL and lock changes are marked "Dangerous" in their documentation. A test fails when
the SDK gains an operation that has not been generated.

Choices worth knowing: checksums are only added when an operation requires them (the SDK's default of always adding
trailing CRC32 trips many S3-compatible gateways); `Put` takes an `io.ReadSeeker` and sends one `PutObject` (up to 5 GiB),
while `Upload` streams any `io.Reader` as a multipart upload with 8 MiB parts by default (at most 10000 parts, so about
78 GiB at that size; raise `partSize` for more); `Copy` is one request (up to 5 GiB); an `http://` endpoint is accepted
for in-cluster traffic, but credentials are then sent unencrypted, so prefer `https://`. There is no option to skip TLS
verification anywhere in this module.

**SeaweedFS 4.52 coverage.** All 112 operations are called against SeaweedFS 4.52 by the integration tests. 64 work and have
their results checked. 42 are refused by SeaweedFS (not implemented, or the AWS-only feature does not exist there): Public Access Block,
bucket website, replication, logging, notifications, accelerate, analytics/inventory/metrics/intelligent-tiering
configurations, S3 Metadata, ABAC, `RenameObject`, `RestoreObject`, `SelectObjectContent`, `CreateSession`,
`ListDirectoryBuckets`, `WriteGetObjectResponse`. Six more are accepted but not honoured: SeaweedFS does not know the
sub-resources of `GetObjectTorrent`, `UpdateObjectEncryption` and the object annotation operations and treats the request as
a plain object request, so **`DeleteObjectAnnotation` deletes the object** and `PutObjectAnnotation` overwrites it. Do not
call those six against SeaweedFS. On AWS S3 these refused operations exist; they are untested there. Also observed: with
versioning enabled, `UploadPartCopy` from an object that predates versioning returned NoSuchKey on SeaweedFS 4.52 (the
cause was not isolated); and every new bucket reserves several volumes, so a test server needs `-volume.max` raised.

## Run the tests

Tests run on your machine; the GitHub workflow only publishes releases and runs no tests.

```sh
scripts/check.sh          # gofmt, go mod tidy/verify, go vet, go test -race, consumer build, govulncheck
scripts/integration.sh    # Docker: real PostgreSQL, MongoDB, Redis and SeaweedFS, then the consumer check
```

`scripts/check.sh` needs only Go. `scripts/integration.sh` needs Docker with the compose plugin. It creates random
throwaway S3 credentials, starts the containers in [`docker-compose.yml`](docker-compose.yml) (bound to `127.0.0.1` on
unusual ports so they do not clash with servers you already run), runs the tests, prints the server versions, and removes
the containers afterwards. A skipped integration test counts as a failure. To keep the servers up while you work, use
`docker compose` yourself (`GCU_S3_CONFIG` must point to an S3 identity file, see the script).

Integration tests are opt-in: they need the `integration` build tag, and each one is skipped (and reported as skipped
with `-v`) when its variable is unset. You can point them at servers you already run instead of Docker:

| Package | Variables |
| --- | --- |
| postgres | `GCU_TEST_POSTGRES_URL` |
| mongodb | `GCU_TEST_MONGODB_URI` |
| redis | `GCU_TEST_REDIS_URL` |
| s3 | `GCU_TEST_S3_ENDPOINT`, `GCU_TEST_S3_ACCESS_KEY`, `GCU_TEST_S3_SECRET_KEY`, optional `GCU_TEST_S3_BUCKET` (default `gcu-test`, created if missing; the client tests create and delete their own uniquely named buckets) |

```sh
GCU_TEST_POSTGRES_URL='postgres://postgres@localhost:5432/postgres?sslmode=disable' \
GCU_TEST_MONGODB_URI='mongodb://localhost:27017/?directConnection=true' \
GCU_TEST_REDIS_URL='redis://localhost:6379/0' \
go test -tags integration -race -count=1 -v -run Integration ./...
```

Never point integration tests at a shared or production system: they create and delete their own uniquely named
collections, keys and objects, but they are still tests.

### Redis wrapper coverage

Every `redis.Client` method is called by at least one test; `TestEveryWrapperIsCalledByATest` fails otherwise, so adding
a go-redis command without a test breaks the build. The Redis tests that change server state (FLUSHALL, CONFIG SET,
ACL, SHUTDOWN, cluster, MIGRATE, ...) start their own throwaway servers, never the ones above: a `redis:8` container
by default (it bundles the JSON, Search, TimeSeries and Bloom-family modules), or the binary named by
`GCU_TEST_REDIS_SERVER`. The two-node cluster test (`ClusterMeet`, `ClusterReplicate`, `ClusterFailover`,
`ClusterForget`) needs that local binary, because Docker port mappings cannot join cluster nodes; `scripts/integration.sh`
runs it when `redis-server` is installed and prints a warning when it is not.

Most tests check that the server accepts the command and the reply decodes (a smoke test), with value checks on the
common ones. A few commands can only be exercised as far as the server's own refusal, because a bare test server has
no config file or the feature is not in open-source Redis: `ConfigRewrite`, `ModuleLoadex`, `ClientMaintNotifications`
(Redis Cloud/Enterprise only), `FunctionKill` and `ScriptKill` (nothing running to kill), `FTCursorRead`/`FTCursorDel`
(go-redis does not expose a cursor id), and `JSONArrTrim` (go-redis omits the required start and stop). `Shutdown*` is
checked by the server's port closing.

## Versions

Chosen and verified on 2026-11-07.

| Item | Version |
| --- | --- |
| Go (module minimum / newest tested) | 1.26.10 / 1.27.3 |
| `github.com/jackc/pgx/v5` | v5.12.0 |
| `go.mongodb.org/mongo-driver/v2` | v2.10.0 |
| `github.com/redis/go-redis/v9` | v9.23.0 |
| `github.com/aws/aws-sdk-go-v2` / `service/s3` / `credentials` | v1.47.8 / v1.114.9 / v1.20.16 |
| SeaweedFS (integration tested) | 4.52 |

`go.mod` has a `toolchain go1.27.3` line, so inside this repo Go downloads and uses 1.27.3 automatically (consumers only
need the 1.26.10 minimum). Use Go 1.27.3+ or 1.26.10+ for your own builds: 1.27.1 has standard-library advisories, and
`govulncheck` run with it reports them even though the library code is fine.

Integration tests were run on 2026-11-07 against PostgreSQL 16.14, MongoDB 8.3.4, Redis 8.8.0 and SeaweedFS 4.52
(installed locally with Homebrew), and all passed. Compatibility is only claimed for what those tests
ran; other server versions are untested. The SeaweedFS test covers single-object Put/Get/Delete, presigned GET/PUT and credential
rejection. Multipart upload, listing and tagging are not covered.

## Releases and upgrades

Releases are ordinary Git tags following [semantic versioning](https://semver.org): `v0.1.0`, `v0.1.1`, `v0.2.0`, ...
One tag covers all six packages. There is no registry or package server; `go get` fetches the tag from GitHub.

- **Release:** first run `scripts/check.sh` and `scripts/integration.sh` on `main` (nothing in GitHub runs tests).
  Then publish from the Actions tab: run the "release" workflow on `main` and choose `auto`, `patch`, `minor` or
  `major`. `auto` reads the commit messages since the last tag: `type!:` or a `BREAKING CHANGE` footer means major,
  `feat:` means minor, anything else patch. Before 1.0 a breaking change gives a minor bump, and `v1.0.0` must be chosen
  explicitly with `major`. `auto` fails if nothing changed since the last tag. The workflow works out the version from
  the latest tag (the first release is always `v0.1.0`), creates the tag and the GitHub release with generated notes,
  and does nothing else. By hand, the equivalent is
  `git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0` plus `gh release create v0.1.0`. Then confirm the tag
  installs: `CONSUMER_VERSION=v0.1.0 scripts/consumer-check.sh`. Tags are never moved or deleted; a bad release is
  fixed by publishing the next patch version.
- **Before 1.0**, a minor bump (`v0.1` → `v0.2`) may change an API; patch bumps (`v0.1.0` → `v0.1.1`) will not. Release
  notes list every breaking change.
- **Upgrade a consumer:** `go get github.com/Wetech-Code/go-common-utilities@v0.2.0 && go mod tidy`, then run your tests.
  See which versions exist with `go list -m -versions github.com/Wetech-Code/go-common-utilities`.
- **Dependencies:** update them in a pull request (`go get -u ./... && go mod tidy`, then `scripts/check.sh`, which
  includes `govulncheck`). Raising the minimum Go version is treated as a minor release.
- **Verify a published tag** from a clean module: `CONSUMER_VERSION=v0.1.0 scripts/consumer-check.sh`.

## Contributing

Enable the pre-commit hook once per clone:

```sh
git config core.hooksPath .githooks
```

When Go or module files are staged it runs `gofmt`, `go mod tidy` consistency (if `go.mod`/`go.sum` changed),
`go vet` and the unit tests, and blocks the commit if any fail. It skips commits that touch no Go files, and
`git commit --no-verify` bypasses it for one commit. The full gate is still `scripts/check.sh`.

Open a pull request from a branch; don't push to `main`. Keep the scope small: a new helper needs a concrete consumer
and a review before it is added (requirement GCU-14). A new package needs a documented API, unit tests, an example and,
for a network service, an opt-in integration test. Run the commands under "Run the tests" and `gofmt -l .` before pushing.
Released under the [MIT licence](LICENSE).
