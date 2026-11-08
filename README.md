# go-common-utilities

Small shared Go packages for Wetech backends: **config, logger, postgres, mongodb, redis, s3**.
One repository, one Go module, one version number. Import only the packages you need; each provider package
returns the driver's own client, so nothing is hidden behind a custom abstraction.

Module: `github.com/Wetech-Code/go-common-utilities` · Go 1.26.10 or newer

| Package | What it gives you | Native type you get back |
| --- | --- | --- |
| [`config`](config) | Read env vars (and `.env` files, Docker/Kubernetes secret files) as typed values, load a whole struct from tags, collect every error at once | — |
| [`jwt`](jwt) | Sign and verify JSON Web Tokens (HS/RS/PS/ES/EdDSA) with safe defaults: fixed algorithm, required expiry, key checks, rotation | token strings, `jwt.Claims` |
| [`logger`](logger) | `*slog.Logger` from a small config; JSON or text; redacts password/token/secret keys; fans out to extra sinks | `*slog.Logger` |
| [`logger/loki`](logger/loki) | Handler that pushes logs to Grafana Loki (stdlib only) | `slog.Handler` |
| [`telemetry`](telemetry) | One setup for logs, traces and metrics over OTLP (Grafana Loki/Tempo/Prometheus, Grafana Cloud, Collector, ...); HTTP server and client instrumentation | `trace.Tracer`, `metric.Meter`, `slog.Handler` |
| [`telemetry/otelpgx`](telemetry/otelpgx), [`otelmongo`](telemetry/otelmongo), [`otelredis`](telemetry/otelredis), [`otels3`](telemetry/otels3) | Spans and duration metrics for the Postgres, MongoDB, Redis and S3 clients | hook values for each `Config` |
| [`logger/otlp`](logger/otlp) | Handler that exports logs with OpenTelemetry OTLP: Grafana Cloud, Loki, Collector, Datadog, Elastic, ... | `slog.Handler` |
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
	"github.com/Wetech-Code/go-common-utilities/jwt"
	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/logger/loki" // optional: Grafana Loki
	"github.com/Wetech-Code/go-common-utilities/logger/otlp" // optional: OpenTelemetry logs (adds the OTel SDK to your build)
	"github.com/Wetech-Code/go-common-utilities/telemetry"   // optional: logs + traces + metrics, and telemetry/otelpgx etc.
	"github.com/Wetech-Code/go-common-utilities/postgres"
	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"github.com/Wetech-Code/go-common-utilities/redis"
	"github.com/Wetech-Code/go-common-utilities/s3"
)
```

Importing `postgres` does not pull in the MongoDB, Redis or AWS libraries, and vice versa. Likewise `logger` and
`logger/loki` use only the standard library; only a program that imports `logger/otlp` or `telemetry` compiles the OpenTelemetry SDK.

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

### Config

`config.New()` reads environment variables into typed values, remembers every problem, and reports them all at once with
`Err()`. An empty variable counts as unset, and error messages name the variable but never its value (it may be a secret).

```go
r := config.New()
dsn := r.URL("DATABASE_URL", "", "postgres", "postgresql") // absolute URL with an allowed scheme
port := r.IntRange("PORT", 8080, 1, 65535)
level := r.Enum("LOG_LEVEL", "info", "debug", "info", "warn", "error")
limit := r.Bytes("MAX_UPLOAD", 10<<20)                      // "10MiB", "512KB", "1048576"
labels := r.Map("LABELS", nil)                              // "env=prod,team=ops"
if r.Bool("TLS", false) && !r.Has("TLS_CERT") {
	r.Invalid("TLS_CERT", "set when TLS is enabled")        // your own cross-field checks
}
if err := r.Err(); err != nil { return err }               // every missing or invalid setting, together
```

| Group | Methods |
| --- | --- |
| Basic | `Require` `String` `Int` `Bool` `Duration` `Strings` `Has` |
| More types | `Int64` `Uint` `Float64` `Bytes` `URL` `Map` `Enum` |
| Bounds | `IntRange` `DurationRange` |
| Your own checks | `Invalid` |
| Sources | `New` (environment), `FromMap` (tests), `FromFile` (a `.env` file under the real environment), `WithPrefix("APP_")`, `WithFileSecrets()` |
| Struct | `Load(&cfg)` |

Load a whole struct from tags; every problem still lands in the same `Err()`:

```go
type Settings struct {
	Port    int           `env:"PORT" default:"8080" min:"1" max:"65535"`
	DSN     string        `env:"DATABASE_URL" required:"true"`
	Level   string        `env:"LOG_LEVEL" default:"info" enum:"debug,info,warn,error"`
	Timeout time.Duration `env:"TIMEOUT" default:"30s" max:"5m"`
	Redis   struct {
		URL string `env:"URL" required:"true"` // read as REDIS_URL
	} `envPrefix:"REDIS_"`
}
var s Settings
r := config.New().WithPrefix("APP_").WithFileSecrets() // APP_PORT, and APP_DATABASE_URL_FILE=/run/secrets/db
r.Load(&s)
```

Supported field types: string, bool, int and uint of every size, floats, `time.Duration`, `[]string`, `map[string]string` and
nested structs. `WithFileSecrets` makes an unset `KEY` fall back to the file named by `KEY_FILE`
(Docker and Kubernetes secrets; the variable wins if both are set; files over 1 MiB are refused). `FromFile` is for local
development: `KEY=VALUE` lines, `#` comments, `export`, single quotes (literal) and double quotes (`\n`, `\t`, `\"`, `\\`
escapes), no variable expansion; real environment variables always override the file, and nothing reads a file unless you call it.
Not included, on purpose: JSON, YAML or TOML files, hot reload, and remote configuration servers.

### JWT

`jwt` signs and verifies JSON Web Tokens on top of [`golang-jwt/jwt/v5`](https://github.com/golang-jwt/jwt); it adds
safe defaults instead of its own cryptography. A token is signed, not encrypted: anyone can read its claims, so do not put
secrets in them.

```go
c, err := jwt.New(jwt.Config{
	Algorithm: "HS256",                       // required; only this algorithm is ever accepted
	Key:       jwt.Key{ID: "2026-11", Secret: secret}, // at least 32 random bytes for HS256
	OldKeys:   []jwt.Key{{ID: "2026-05", Secret: oldSecret}}, // still verify during a rotation
	Issuer:    "api.example.com",
	Audience:  []string{"web"},
	TTL:       15 * time.Minute,
	Leeway:    30 * time.Second,              // clock skew between services
})
token, err := c.Sign(jwt.Claims{Subject: "user-42", Extra: map[string]any{"role": "admin"}})
claims, err := c.Verify(token)               // errors.Is(err, jwt.ErrExpired / jwt.ErrNotYetValid / jwt.ErrInvalid)
```

For a service that only checks tokens, give it the public key (`jwt.Key{Public: pub}`, read with `jwt.ParsePublicKeyPEM`)
and keep the private key with the issuer. `jwt.ParsePrivateKeyPEM` reads PKCS#8, PKCS#1 and SEC 1 keys.

| | |
| --- | --- |
| Algorithms | HS256 HS384 HS512, RS256 RS384 RS512, PS256 PS384 PS512, ES256 ES384 ES512, EdDSA |
| Functions | `New` `Sign` `SignWithTTL` `Verify` `VerifyContext` `Inspect` `ParsePrivateKeyPEM` `ParsePublicKeyPEM` |
| Provider keys | `jwt/jwks`: `New` (fetch and cache an identity provider's key set), `FromJSON`, `Parse`, `Refresh` |
| Errors | `ErrExpired` `ErrNotYetValid` `ErrInvalid` `ErrNoSigningKey` `ErrConfig` |

What it enforces: the algorithm comes from your `Config`, never from the token, so `alg: none` and algorithm-confusion
tokens (such as an HS256 token signed with an RSA public key) are rejected; tokens must have an `exp`, and `Sign` always sets
one; issuer and audience are checked when configured; tokens issued in the future are refused; HMAC secrets must be at least
as long as the hash output, RSA keys at least 2048 bits, and ECDSA keys must match their curve; base64 padding bits must be
canonical; tokens over `MaxTokenSize` (8 KiB) are refused before parsing; errors name a category but never contain the token,
keys or claim values. `Inspect` decodes a token **without** checking it (to read a `kid` or for debugging) and is marked
dangerous: never authorize anything from it.
Not included: encrypted tokens (JWE), refresh-token storage or revocation lists, and login or session handling. Whether a token's holder may do something is the caller's decision.

To verify tokens issued by an identity provider (Auth0, Keycloak, Cognito, Google, ...), point `jwt/jwks` at its key set and
use it as the `KeySource`. Keys are fetched on first use, cached, and fetched again when a token names a key you do not have
(the provider rotated), at most once per `MinRefreshInterval`, so forged key IDs cannot make you hammer the provider; if a
refresh fails the old keys keep working. The URL must be https (plain http only for localhost), certificates are always
verified, redirects and oversized responses are refused, only public RSA, EC and Ed25519 keys are read, and a key set that
contains private key material or a shared secret is rejected. The algorithm you configure still decides what is accepted, and
every key the provider publishes is checked against it (type, RSA size, curve).

```go
set, err := jwks.New(jwks.Config{URL: "https://idp.example.com/.well-known/jwks.json"})
if err := set.Refresh(ctx); err != nil { return err } // optional: fail at startup if the provider is unreachable
c, err := jwt.New(jwt.Config{Algorithm: "RS256", KeySource: set, Issuer: "https://idp.example.com/", Audience: []string{"my-api"}})
claims, err := c.VerifyContext(ctx, bearerToken)       // the context bounds the key fetch
```

### Logger: console, Grafana Loki, OpenTelemetry

`logger.New` returns a plain `*slog.Logger` writing JSON (or text) to the console. To also ship records elsewhere, build
a handler from `logger/loki` or `logger/otlp` and pass it in `Config.Extra`. Any `slog.Handler` works there, so the same
mechanism covers other sinks. `Config.Level` and the secret redaction (`password`, `token`, `api_key`, `dsn`, ... even
inside groups) apply to every destination before a record leaves the process.

```go
lokiH, err := loki.New(loki.Config{
	URL:    "http://loki.internal:3100",                      // or https://logs-prod-xx.grafana.net
	Labels: map[string]string{"service": "api", "env": "prod"}, // keep labels few and stable
	// Username/Password (Grafana Cloud), BearerToken or TenantID for secured Loki
	ContextAttrs: otlp.TraceAttrs,                            // optional: trace_id / span_id in each line
})
otelH, err := otlp.New(ctx, otlp.Config{
	Endpoint:    "http://collector.internal:4318",          // base URL; /v1/logs is added
	Headers:     map[string]string{"Authorization": authHeader},
	ServiceName: "api", ServiceVersion: "1.4.2", Environment: "prod",
})
log, err := logger.New(logger.Config{
	Level:        "info",
	Extra:        []slog.Handler{lokiH, otelH},              // console + Loki + OTLP
	ContextAttrs: otlp.TraceAttrs,                           // console lines get trace_id / span_id
})
defer func() { // on shutdown, after the last log call
	lokiH.Close(ctx)
	otelH.Shutdown(ctx)
}()

log.InfoContext(ctx, "order placed", "order", 42, "password", "hunter2") // password is [REDACTED] everywhere
```

| | `logger/loki` | `logger/otlp` |
| --- | --- | --- |
| Protocol | Loki push API (JSON, gzip) | OTLP/HTTP (protobuf), official OpenTelemetry Go SDK |
| Reaches | Grafana Loki, Grafana Cloud Logs | Grafana Cloud/Alloy, Loki's `/otlp` endpoint, OpenTelemetry Collector, Datadog, Elastic, Honeycomb, Splunk, ... |
| Fields | one JSON line per record; labels = `Config.Labels` + `level` | body = message, attributes = fields, resource = `service.name` etc. |
| Trace link | add `ContextAttrs: otlp.TraceAttrs` | automatic from the context's span |
| Dependencies | standard library | OpenTelemetry SDK, protobuf, gRPC (compiled only if imported) |
| Flush on exit | `Close(ctx)` | `Shutdown(ctx)` (`ForceFlush(ctx)` any time) |

Grafana only displays logs; point its Loki data source at the Loki you push to, or use Grafana Cloud's OTLP gateway.
Both handlers batch and send in the background, so logging never waits for the network. The Loki handler retries
network errors, 429 and 5xx a few times, then drops the batch and calls `OnError` (default: standard error); when its
queue is full it drops new records and counts them (`Dropped()`). Delivery is best effort: use the console output (collected
by Alloy, Promtail or your platform) when you must not lose a line. For traces and metrics too, use `telemetry` below.

### Telemetry: logs, traces and metrics

`telemetry.New` starts all three OpenTelemetry signals with one service identity (`service.name`, version, environment),
exports them over OTLP/HTTP, and hands you the pieces. Nothing is global unless you set `SetGlobal`, and instrumented code
never waits on the network (everything is batched in the background; delivery is best effort).

```go
tel, err := telemetry.New(ctx, telemetry.Config{
	Endpoint:       "http://collector.internal:4318",   // /v1/logs, /v1/traces, /v1/metrics are added
	Headers:        map[string]string{"Authorization": authHeader},
	ServiceName:    "api", ServiceVersion: "1.4.2", Environment: "prod",
	RuntimeMetrics: true,                               // Go memory, goroutines, GC
	SampleRatio:    0.1,                                // keep 10% of new traces (whole traces, never half)
})
defer tel.Shutdown(ctx)                                 // after the last log, span and measurement

log, _ := logger.New(logger.Config{Extra: []slog.Handler{tel.LogHandler()}, ContextAttrs: telemetry.TraceAttrs})

ctx, span := tel.Tracer("orders").Start(ctx, "place order")  // traces
defer span.End()
log.InfoContext(ctx, "order placed")                         // logs, linked to the span
orders, _ := tel.Meter("orders").Int64Counter("orders.placed")
orders.Add(ctx, 1)                                           // metrics

srv := &http.Server{Handler: tel.HTTPHandler(mux, "api")}   // a span and http.server.* metrics per request
client := &http.Client{Transport: tel.HTTPTransport(nil)}   // client spans, http.client.* metrics, trace headers forwarded
```

Where each signal goes: set `Endpoint` for one receiver (a Collector, Alloy, Grafana Cloud's OTLP gateway), or
`LogsEndpoint`, `TracesEndpoint` and `MetricsEndpoint` for separate backends: Loki's `.../otlp`, Tempo's OTLP port, and
Prometheus's `.../api/v1/otlp` (start it with `--web.enable-otlp-receiver`). Leave everything empty to use the standard
`OTEL_EXPORTER_OTLP_*` environment variables. `DisableLogs`, `DisableTraces` and `DisableMetrics` turn a signal off;
its accessors then return working no-ops. A trace continues from the incoming `traceparent` header, and an unsampled or
sampled decision from the caller is respected.

Instrument the clients by passing the providers in each package's `Config`; spans never contain statements, keys, bucket
names, command arguments or error messages, only operation names and error types or codes (such as the SQLSTATE):

```go
pool, _ := postgres.New(ctx, postgres.Config{URL: dsn, Tracer: otelpgx.New(tel.TracerProvider(), tel.MeterProvider())})
mdb, _  := mongodb.New(ctx, mongodb.Config{URI: uri, Monitor: otelmongo.New(tel.TracerProvider(), tel.MeterProvider())})
rdb, _  := redis.New(ctx, redis.Config{URL: url, Hooks: []goredis.Hook{otelredis.New(tel.TracerProvider(), tel.MeterProvider())}})
st, _   := s3.New(ctx, s3.Config{ /* ... */ APIOptions: otels3.APIOptions(tel.TracerProvider(), tel.MeterProvider())})
```

Each records one client span per operation (Postgres: query, batch, COPY, prepare, connect; MongoDB: every command except the
handshake; Redis: commands and pipelines; S3: every operation) and the `db.client.operation.duration` histogram, so database
latency appears in Tempo next to the request that caused it and in Prometheus as a metric. `otelpgx.WithStatement()` adds the
SQL text if your statements never contain data. The `telemetry` packages are separate imports, so a program that does not
import them does not compile the OpenTelemetry SDK or HTTP instrumentation.

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
scripts/integration.sh    # Docker: real PostgreSQL, MongoDB, Redis, SeaweedFS, Loki, Tempo, Prometheus and Grafana, then the consumer check
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
| logger, telemetry | `GCU_TEST_LOKI_URL`, `GCU_TEST_GRAFANA_URL`, `GCU_TEST_GRAFANA_PASSWORD`, and for telemetry also `GCU_TEST_TEMPO_OTLP_URL`, `GCU_TEST_PROMETHEUS_URL` (Loki, Tempo, Prometheus and Grafana; everything is read back through Grafana's data sources) |
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
| `github.com/golang-jwt/jwt/v5` | v5.3.1 |
| `github.com/aws/aws-sdk-go-v2` / `service/s3` / `credentials` | v1.47.8 / v1.114.9 / v1.20.16 |
| `go.opentelemetry.io/otel` / `sdk/log` / `otlploghttp` / `otelslog` | v1.47.0 / v1.47.0 / v0.23.0 / v0.21.0 |
| `otelhttp` / OTel runtime metrics (contrib) | v0.72.0 / v0.72.0 |
| SeaweedFS / Loki / Grafana / Tempo / Prometheus (integration tested) | 4.52 / 3.7.9 / 12.4.14 / 2.9.5 / 3.9.1 |

`go.mod` has a `toolchain go1.27.3` line, so inside this repo Go downloads and uses 1.27.3 automatically (consumers only
need the 1.26.10 minimum). Use Go 1.27.3+ or 1.26.10+ for your own builds: 1.27.1 has standard-library advisories, and
`govulncheck` run with it reports them even though the library code is fine.

Integration tests were last run on 2026-11-08 in Docker against PostgreSQL 18, MongoDB 8.3.13, Redis 8.10.3,
SeaweedFS 4.52, Loki 3.7.9, Tempo 2.9.5, Prometheus 3.9.1 and Grafana 12.4.14, and all passed. Compatibility is only claimed for what those tests ran;
other server versions are untested. See the S3 section for exactly which operations SeaweedFS accepts. The OTLP handler
is tested against Loki's own OTLP endpoint and a fake receiver, not against Datadog, Elastic, Honeycomb or Splunk.

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
