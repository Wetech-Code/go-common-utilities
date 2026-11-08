#!/usr/bin/env bash
# Builds and runs a throwaway module, outside this repo, that imports all six
# packages (and the logger/loki and logger/otlp sinks) the way a real consumer would.
#
#   scripts/consumer-check.sh                    # against this checkout (replace directive)
#   CONSUMER_VERSION=v0.1.0 scripts/consumer-check.sh   # against a published tag
#
# If GCU_TEST_POSTGRES_URL / GCU_TEST_MONGODB_URI / GCU_TEST_REDIS_URL /
# GCU_TEST_S3_* / GCU_TEST_LOKI_URL are set, the consumer also connects to each service and closes
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
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Wetech-Code/go-common-utilities/config"
	"github.com/Wetech-Code/go-common-utilities/jwt"
	"github.com/Wetech-Code/go-common-utilities/logger"
	"github.com/Wetech-Code/go-common-utilities/logger/loki"
	"github.com/Wetech-Code/go-common-utilities/logger/otlp"
	"github.com/Wetech-Code/go-common-utilities/mongodb"
	"github.com/Wetech-Code/go-common-utilities/postgres"
	"github.com/Wetech-Code/go-common-utilities/redis"
	"github.com/Wetech-Code/go-common-utilities/s3"
	"github.com/Wetech-Code/go-common-utilities/telemetry"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelmongo"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelpgx"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelredis"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otels3"
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
	lokiURL := r.String("GCU_TEST_LOKI_URL", "")
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

	if lokiURL != "" {
		lh, err := loki.New(loki.Config{URL: lokiURL, Labels: map[string]string{"service": "consumer-check"}})
		if err != nil {
			return err
		}
		oh, err := otlp.New(ctx, otlp.Config{Endpoint: lokiURL + "/otlp", ServiceName: "consumer-check"})
		if err != nil {
			return err
		}
		both, err := logger.New(logger.Config{Output: os.Stdout, Extra: []slog.Handler{lh, oh}, ContextAttrs: otlp.TraceAttrs})
		if err != nil {
			return err
		}
		both.Info("loki and otlp ok", "password", "not-shown")
		if err := lh.Close(ctx); err != nil {
			return err
		}
		if err := oh.Shutdown(ctx); err != nil {
			return err
		}
		if lh.Dropped() != 0 {
			return fmt.Errorf("loki dropped %d records", lh.Dropped())
		}
	}

	// Telemetry and the client instrumentation build and shut down without a receiver.
	tel, err := telemetry.New(ctx, telemetry.Config{Endpoint: "http://127.0.0.1:1", ServiceName: "consumer-check", Timeout: time.Second})
	if err != nil {
		return err
	}
	_ = otelpgx.New(tel.TracerProvider(), tel.MeterProvider())
	_ = otelmongo.New(tel.TracerProvider(), tel.MeterProvider())
	_ = otelredis.New(tel.TracerProvider(), tel.MeterProvider())
	_ = otels3.APIOptions(tel.TracerProvider(), tel.MeterProvider())
	_, span := tel.Tracer("consumer").Start(ctx, "check")
	span.End()
	stop, cancelStop := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStop()
	_ = tel.Shutdown(stop) // the receiver is unreachable on purpose; only the call returning matters

	// Tokens need no server either.
	jc, err := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: []byte(strings.Repeat("k", 32))}, TTL: time.Minute})
	if err != nil {
		return err
	}
	tok, err := jc.Sign(jwt.Claims{Subject: "consumer"})
	if err != nil {
		return err
	}
	if _, err := jc.Verify(tok); err != nil {
		return err
	}

	// Config types must stay usable without any server.
	for name, err := range map[string]error{
		"postgres": postgres.Config{}.Validate(),
		"mongodb":  mongodb.Config{}.Validate(),
		"redis":    redis.Config{}.Validate(),
		"s3":       s3.Config{}.Validate(),
		"loki":     loki.Config{}.Validate(),
		"telemetry": telemetry.Config{SampleRatio: 2}.Validate(),
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
