#!/usr/bin/env bash
# Builds and runs a throwaway module, outside this repo, that imports all six
# packages the way a real consumer would.
#
#   scripts/consumer-check.sh                    # against this checkout (replace directive)
#   CONSUMER_VERSION=v0.1.0 scripts/consumer-check.sh   # against a published tag
#
# If GCU_TEST_POSTGRES_URL / GCU_TEST_MONGODB_URI / GCU_TEST_REDIS_URL /
# GCU_TEST_S3_* are set, the consumer also connects to each service and closes
# it again. Without them it only proves the packages import, compile and run.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module=github.com/Wetech-Code/go-common-utilities
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"

gover="$(awk '/^go /{print $2; exit}' "$repo/go.mod")"
cat > go.mod <<MOD
module example.com/consumer

go $gover
MOD

cat > main.go <<'GO'
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Wetech-Code/go-common-utilities/config"
	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"github.com/Wetech-Code/go-common-utilities/postgres"
	"github.com/Wetech-Code/go-common-utilities/redis"
	"github.com/Wetech-Code/go-common-utilities/s3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "consumer check failed:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	r := config.New()
	pgURL := r.String("GCU_TEST_POSTGRES_URL", "")
	mongoURI := r.String("GCU_TEST_MONGODB_URI", "")
	redisURL := r.String("GCU_TEST_REDIS_URL", "")
	s3Endpoint := r.String("GCU_TEST_S3_ENDPOINT", "")
	if err := r.Err(); err != nil {
		return err
	}

	log, err := logger.New(logger.Config{Format: "text", Output: os.Stdout})
	if err != nil {
		return err
	}

	if pgURL != "" {
		pool, err := postgres.New(ctx, postgres.Config{URL: pgURL})
		if err != nil {
			return err
		}
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return err
		}
		pool.Close()
		log.Info("postgres ok")
	}
	if mongoURI != "" {
		client, err := mongodb.New(ctx, mongodb.Config{URI: mongoURI})
		if err != nil {
			return err
		}
		if err := client.Disconnect(ctx); err != nil {
			return err
		}
		log.Info("mongodb ok")
	}
	if redisURL != "" {
		rdb, err := redis.New(ctx, redis.Config{URL: redisURL})
		if err != nil {
			return err
		}
		if err := rdb.Set(ctx, "consumer-check", "1", time.Minute).Err(); err != nil {
			return err
		}
		rdb.Del(ctx, "consumer-check")
		if err := rdb.Close(); err != nil {
			return err
		}
		log.Info("redis ok")
	}
	if s3Endpoint != "" {
		bucket := os.Getenv("GCU_TEST_S3_BUCKET")
		if bucket == "" {
			bucket = "gcu-test"
		}
		c, err := s3.New(ctx, s3.Config{
			Endpoint:        s3Endpoint,
			AccessKeyID:     os.Getenv("GCU_TEST_S3_ACCESS_KEY"),
			SecretAccessKey: os.Getenv("GCU_TEST_S3_SECRET_KEY"),
			Bucket:          bucket,
			UsePathStyle:    true,
		})
		if err != nil {
			return err
		}
		if err := c.Put(ctx, "consumer-check.txt", strings.NewReader("ok"), "text/plain"); err != nil {
			return err
		}
		if err := c.Delete(ctx, "consumer-check.txt"); err != nil {
			return err
		}
		c.Close()
		log.Info("s3 ok")
	}

	// Config types must stay usable without any server.
	for name, err := range map[string]error{
		"postgres": postgres.Config{}.Validate(),
		"mongodb":  mongodb.Config{}.Validate(),
		"redis":    redis.Config{}.Validate(),
		"s3":       s3.Config{}.Validate(),
	} {
		if err == nil {
			return fmt.Errorf("%s: empty config unexpectedly valid", name)
		}
	}
	log.Info("consumer check passed")
	return nil
}
GO

if [ -n "${CONSUMER_VERSION:-}" ]; then
  go get "$module@$CONSUMER_VERSION"
else
  go mod edit -require="$module@v0.0.0" -replace="$module=$repo"
fi
go mod tidy
go vet ./...
go build -o consumer .
./consumer
