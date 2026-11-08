// Package otelredis adds OpenTelemetry spans and metrics to a Redis client.
// Pass the result in redis.Config.Hooks:
//
//	rdb, err := redis.New(ctx, redis.Config{
//		URL:   url,
//		Hooks: []goredis.Hook{otelredis.New(tel.TracerProvider(), tel.MeterProvider())},
//	})
//
// Each command becomes a client span named after the command (GET, HSET, ...)
// and each pipeline one span named PIPELINE; both are counted in the
// db.client.operation.duration histogram. Keys, arguments and values are never
// recorded. A missing key (redis.Nil) is a normal result, not an error.
package otelredis

import (
	"context"
	"errors"
	"net"
	"strings"

	goredis "github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
)

// ScopeName identifies this package as the instrumentation scope.
const ScopeName = "github.com/Wetech-Code/go-common-utilities/telemetry/otelredis"

// Hook implements go-redis's Hook interface.
type Hook struct{ in *otelkit.Instrument }

// New returns a hook. Nil providers mean no-op.
func New(tp trace.TracerProvider, mp metric.MeterProvider) *Hook {
	return &Hook{in: otelkit.New(ScopeName, "redis", tp, mp)}
}

// DialHook passes dialing through unchanged.
func (h *Hook) DialHook(next goredis.DialHook) goredis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

// ProcessHook records one span per command.
func (h *Hook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if isConnectionSetup(cmd) {
			return next(ctx, cmd)
		}
		name := strings.ToUpper(cmd.Name())
		op := []attribute.KeyValue{attribute.String("db.operation.name", name)}
		ctx, o := h.in.Start(ctx, name, op, op)
		err := next(ctx, cmd)
		o.End(errType(err))
		return err
	}
}

// ProcessPipelineHook records one span per pipeline.
func (h *Hook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		met := []attribute.KeyValue{attribute.String("db.operation.name", "PIPELINE")}
		ctx, o := h.in.Start(ctx, "PIPELINE", append([]attribute.KeyValue{attribute.Int("db.operation.batch.size", len(cmds))}, met...), met)
		err := next(ctx, cmds)
		// The span fails if the pipeline or any of its commands failed, but the
		// hook returns exactly what go-redis returned.
		failure := err
		if failure == nil {
			for _, c := range cmds {
				if cerr := c.Err(); cerr != nil && !errors.Is(cerr, goredis.Nil) {
					failure = cerr
					break
				}
			}
		}
		o.End(errType(failure))
		return err
	}
}

// isConnectionSetup is true for the commands go-redis sends by itself when it
// opens a connection (HELLO, AUTH, CLIENT SETINFO, CLIENT MAINT_NOTIFICATIONS); they are noise, not work done
// for the application.
func isConnectionSetup(cmd goredis.Cmder) bool {
	switch cmd.Name() {
	case "hello", "auth":
		return true
	case "client":
		args := cmd.Args()
		if len(args) > 1 {
			if sub, ok := args[1].(string); ok {
				return strings.EqualFold(sub, "setinfo") || strings.EqualFold(sub, "setname") || strings.EqualFold(sub, "maint_notifications")
			}
		}
	}
	return false
}

var _ goredis.Hook = (*Hook)(nil)

// errType is the Redis error prefix (WRONGTYPE, NOAUTH, ...) for server errors
// and a generic marker otherwise. redis.Nil is not an error.
func errType(err error) string {
	switch {
	case err == nil || errors.Is(err, goredis.Nil):
		return ""
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	}
	var re goredis.Error
	if errors.As(err, &re) {
		if f := strings.Fields(err.Error()); len(f) > 0 {
			return strings.ToUpper(strings.TrimSuffix(f[0], ":"))
		}
	}
	return "error"
}
