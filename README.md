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
| [`s3`](s3) | S3 / SeaweedFS client with Put, Get, Delete, presigned URLs | `*s3.Client` (AWS SDK v2) via `Client.S3` |

Not included, on purpose: ORMs, migrations, repositories, queues, a generic `Database` interface, global clients,
retries around writes, authorization, anything specific to a product. See the
[requirements](https://github.com/Wetech-Code/tech-blueprints/blob/main/shared/requirements/2026-11-07-minimal-published-go-common-utilities.md)
and [ADR-0004](https://github.com/Wetech-Code/tech-blueprints/blob/main/shared/decisions/ADR-0004-simple-published-go-common-utilities-module.md).

## Install

```sh
go get github.com/Wetech-Code/go-common-utilities@latest   # or pin: @v0.1.0
```

The repository is **private**, so Go needs to know not to use the public proxy and needs GitHub credentials:

```sh
go env -w GOPRIVATE=github.com/Wetech-Code/*

# Pick one way to authenticate git over HTTPS:
gh auth setup-git                      # uses your GitHub CLI login
# or a token with read access to this repo (fine-grained PAT, "Contents: read"):
git config --global url."https://<user>:<token>@github.com/".insteadOf "https://github.com/"
```

In CI, give the job a token that can read this repository (a GitHub App token or fine-grained PAT stored as a secret;
the default `GITHUB_TOKEN` of another repo cannot) and run the same two commands before `go build`. In Docker builds,
pass the token as a build secret rather than an `ARG`. Never commit the token.

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

### MongoDB

`URI` starts with `mongodb://` or `mongodb+srv://`; add `tls=true` (implicit for `+srv`) for TLS.
Optional: `AppName`, `MaxPoolSize`, `ConnectTimeout`. This is the official driver **v2**
(`go.mongodb.org/mongo-driver/v2`); use `client.Database("x").Collection("y")` as usual.

```sh
MONGODB_URI=mongodb://app:<password>@mongo.internal:27017/?tls=true&replicaSet=rs0
```

### Redis

`URL` is `redis://[user:password@]host:port/db`, or `rediss://` for TLS (certificates are verified).
Optional: `PoolSize`, `DialTimeout`, `ReadTimeout`, `WriteTimeout`. You get a go-redis v9 client.

```sh
REDIS_URL=rediss://:<password>@redis.internal:6380/0
```

### S3 / SeaweedFS

Point `Endpoint` at the SeaweedFS S3 gateway (default port 8333) and set `UsePathStyle: true`. Credentials are the
access/secret key of an identity in the gateway's `-s3.config`. The bucket must already exist; this package never
creates or deletes buckets. `Put`/`Get`/`Delete` are the helpers; `PresignGet`/`PresignPut` give time-limited URLs
(at most 7 days); anything else is available on `store.S3` (the AWS SDK client).

```sh
S3_ENDPOINT=https://seaweed.internal:8333
S3_ACCESS_KEY_ID=<from the gateway's s3 config>
S3_SECRET_ACCESS_KEY=<from the gateway's s3 config>
```

Choices worth knowing: checksums are only added when an operation requires them (the SDK's default of always adding
trailing CRC32 trips many S3-compatible gateways); `Put` takes an `io.ReadSeeker` and sends one `PutObject`, so objects
are limited to 5 GiB and streams must be buffered by the caller; an `http://` endpoint is accepted for in-cluster
traffic, but credentials are then sent unencrypted, so prefer `https://`. There is no option to skip TLS verification
anywhere in this module.

## Run the tests

```sh
go vet ./...
go test -race ./...                      # unit tests, no services needed
scripts/consumer-check.sh                # builds a separate throwaway module that imports all six packages
scripts/check.sh                         # everything above plus gofmt, go mod tidy/verify and govulncheck
scripts/seaweedfs-test.sh                # SeaweedFS in Docker: s3 integration test + consumer check
```

Nothing runs automatically. `scripts/check.sh` is the gate, run by the author and reviewer before merging. The same
checks, plus the PostgreSQL, MongoDB, Redis and SeaweedFS integration tests in containers, are available as a GitHub
Actions workflow that only starts when you trigger it by hand: Actions tab, "ci", "Run workflow", or
`gh workflow run ci.yml --ref <branch>` (GitHub only offers it once the file is on the default branch).

Integration tests are opt-in: they need the `integration` build tag and a disposable service, and each one is skipped
(and reported as skipped with `-v`) when its variable is unset.

| Package | Variables |
| --- | --- |
| postgres | `GCU_TEST_POSTGRES_URL` |
| mongodb | `GCU_TEST_MONGODB_URI` |
| redis | `GCU_TEST_REDIS_URL` |
| s3 | `GCU_TEST_S3_ENDPOINT`, `GCU_TEST_S3_ACCESS_KEY`, `GCU_TEST_S3_SECRET_KEY`, optional `GCU_TEST_S3_BUCKET` (default `gcu-test`, created if missing) |

```sh
# Example with Docker (all disposable; use throwaway credentials only)
docker run -d -p 5432:5432 -e POSTGRES_HOST_AUTH_METHOD=trust postgres:18
docker run -d -p 27017:27017 mongo:8.0
docker run -d -p 6379:6379 redis:8
# SeaweedFS needs an S3 identity file (or `brew install seaweedfs` and `weed server -s3 -s3.config=...`); scripts/seaweedfs-test.sh starts one in Docker and runs the s3 test for you
# (the manual "ci" workflow does the same).

GCU_TEST_POSTGRES_URL='postgres://postgres@localhost:5432/postgres?sslmode=disable' \
GCU_TEST_MONGODB_URI='mongodb://localhost:27017/?directConnection=true' \
GCU_TEST_REDIS_URL='redis://localhost:6379/0' \
go test -tags integration -race -count=1 -v -run Integration ./...
```

Never point integration tests at a shared or production system: they create and delete their own uniquely named
collections, keys and objects, but they are still tests.


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

Integration tests were run on 2026-11-07 against PostgreSQL 16.14, MongoDB 8.3.4, Redis 8.8.0 and SeaweedFS 4.52
(all installed locally with Homebrew, no Docker), and all passed. Compatibility is only claimed for what those tests
ran; other server versions are untested. The SeaweedFS test covers single-object Put/Get/Delete, presigned GET/PUT and credential
rejection. Multipart upload, listing and tagging are not covered.

## Releases and upgrades

Releases are ordinary Git tags following [semantic versioning](https://semver.org): `v0.1.0`, `v0.1.1`, `v0.2.0`, ...
One tag covers all six packages. There is no registry or package server; `go get` fetches the tag from GitHub.

- **Release:** after a pull request is reviewed and merged, and `scripts/check.sh` passes on `main`, a maintainer
  publishes from the Actions tab: run the "ci" workflow on `main` and choose `release`: `patch`, `minor` or `major`
  (`none` only runs the checks). The workflow works out the next version from the latest tag (the first release is
  always `v0.1.0`), re-runs every check and integration test, and only then creates the tag and the GitHub release and
  installs the new tag from a clean consumer module. By hand, the equivalent is
  `git tag -a v0.1.0 -m "v0.1.0" && git push origin v0.1.0` plus `gh release create v0.1.0`. Tags are never
  moved or deleted; a bad release is fixed by publishing the next patch version.
- **Before 1.0**, a minor bump (`v0.1` → `v0.2`) may change an API; patch bumps (`v0.1.0` → `v0.1.1`) will not. Release
  notes list every breaking change.
- **Upgrade a consumer:** `go get github.com/Wetech-Code/go-common-utilities@v0.2.0 && go mod tidy`, then run your tests.
  See which versions exist with `go list -m -versions github.com/Wetech-Code/go-common-utilities`.
- **Dependencies:** update them in a pull request (`go get -u ./... && go mod tidy`, then `scripts/check.sh`, which
  includes `govulncheck`). Raising the minimum Go version is treated as a minor release.
- **Verify a published tag** from a clean module: `CONSUMER_VERSION=v0.1.0 scripts/consumer-check.sh`
  (needs the private-repo access described above).

## Contributing

Open a pull request from a branch; don't push to `main`. Keep the scope small: a new helper needs a concrete consumer
and a review before it is added (requirement GCU-14). A new package needs a documented API, unit tests, an example and,
for a network service, an opt-in integration test. Run the commands under "Run the tests" and `gofmt -l .` before pushing.
No licence has been chosen yet, so this repository is not open source.
