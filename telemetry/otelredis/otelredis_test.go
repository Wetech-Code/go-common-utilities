package otelredis_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otelredis"
)

type redisErr string

func (e redisErr) Error() string { return string(e) }
func (e redisErr) RedisError()   {}

func TestCommandSpansAndMetrics(t *testing.T) {
	env := oteltest.New(t)
	h := otelredis.New(env.TP, env.MP)
	ctx := context.Background()

	run := func(args []any, result error) {
		cmd := goredis.NewCmd(ctx, args...)
		err := h.ProcessHook(func(context.Context, goredis.Cmder) error { return result })(ctx, cmd)
		if err != result {
			t.Fatalf("the hook must return the command's own error: got %v want %v", err, result)
		}
	}
	run([]any{"get", "session:alice-secret-token"}, nil)
	run([]any{"get", "missing"}, goredis.Nil)
	run([]any{"incr", "name"}, redisErr("WRONGTYPE Operation against a key holding the wrong kind of value"))
	run([]any{"set", "k", "v"}, context.DeadlineExceeded)
	run([]any{"set", "k", "v"}, errors.New("EOF"))

	s := env.Spans()
	if len(s) != 5 {
		t.Fatalf("spans = %+v", s)
	}
	if s[0].Name != "GET" || s[0].Kind != "client" || s[0].Failed || s[0].Attrs["db.system.name"].AsString() != "redis" || s[0].Attrs["db.operation.name"].AsString() != "GET" {
		t.Errorf("get span = %+v", s[0])
	}
	if s[1].Failed {
		t.Errorf("redis.Nil is a normal result, not an error: %+v", s[1])
	}
	if !s[2].Failed || s[2].Attrs["error.type"].AsString() != "WRONGTYPE" {
		t.Errorf("wrongtype span = %+v", s[2])
	}
	if s[3].Attrs["error.type"].AsString() != "context.DeadlineExceeded" || s[4].Attrs["error.type"].AsString() != "error" {
		t.Errorf("error kinds = %+v %+v", s[3], s[4])
	}
	for _, sp := range s {
		for k, v := range sp.Attrs {
			if strings.Contains(v.Emit(), "alice-secret-token") {
				t.Errorf("key leaked in %s", k)
			}
		}
	}
	if len(env.Histogram(t, otelkit.DurationMetric)) < 4 {
		t.Errorf("histogram points = %v", env.Histogram(t, otelkit.DurationMetric))
	}
}

func TestPipelineSpan(t *testing.T) {
	env := oteltest.New(t)
	h := otelredis.New(env.TP, env.MP)
	ctx := context.Background()
	good := []goredis.Cmder{goredis.NewCmd(ctx, "set", "a", 1), goredis.NewCmd(ctx, "get", "a")}
	if err := h.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { return nil })(ctx, good); err != nil {
		t.Fatal(err)
	}
	// A failing command inside an otherwise successful pipeline fails the span.
	bad := []goredis.Cmder{goredis.NewCmd(ctx, "get", "a"), goredis.NewCmd(ctx, "incr", "a")}
	bad[1].SetErr(redisErr("ERR value is not an integer"))
	if err := h.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { return nil })(ctx, bad); err != nil {
		t.Fatal(err)
	}
	// Nil from one command is not a failure.
	nilCmds := []goredis.Cmder{goredis.NewCmd(ctx, "get", "x")}
	nilCmds[0].SetErr(goredis.Nil)
	h.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { return nil })(ctx, nilCmds)
	// The pipeline's own error is returned as is.
	want := errors.New("connection reset")
	if err := h.ProcessPipelineHook(func(context.Context, []goredis.Cmder) error { return want })(ctx, good); err != want {
		t.Errorf("err = %v", err)
	}

	s := env.Spans()
	if len(s) != 4 || s[0].Name != "PIPELINE" || s[0].Attrs["db.operation.batch.size"].AsInt64() != 2 || s[0].Failed {
		t.Fatalf("spans = %+v", s)
	}
	if !s[1].Failed || s[1].Attrs["error.type"].AsString() != "ERR" || s[2].Failed || !s[3].Failed {
		t.Errorf("failure marking: %+v", s[1:])
	}
}

func TestDialHookPassesThroughAndNilProvidersWork(t *testing.T) {
	h := otelredis.New(nil, nil)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	conn, err := h.DialHook(func(context.Context, string, string) (net.Conn, error) { return c1, nil })(context.Background(), "tcp", "x:1")
	if err != nil || conn != c1 {
		t.Errorf("DialHook = %v, %v", conn, err)
	}
	cmd := goredis.NewCmd(context.Background(), "ping")
	if err := h.ProcessHook(func(context.Context, goredis.Cmder) error { return nil })(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionSetupCommandsAreNotTraced(t *testing.T) {
	env := oteltest.New(t)
	h := otelredis.New(env.TP, env.MP)
	ctx := context.Background()
	called := 0
	next := func(context.Context, goredis.Cmder) error { called++; return redisErr("ERR whatever") }
	for _, args := range [][]any{{"hello", 3}, {"auth", "pw"}, {"client", "setinfo", "lib-name", "x"}, {"CLIENT", "SETNAME", "n"}, {"client", "maint_notifications", "on"}} {
		h.ProcessHook(next)(ctx, goredis.NewCmd(ctx, args...))
	}
	if called != 5 || len(env.Spans()) != 0 {
		t.Errorf("next called %d times, %d spans", called, len(env.Spans()))
	}
	// Other CLIENT subcommands, and CLIENT with no argument, are traced as usual.
	h.ProcessHook(next)(ctx, goredis.NewCmd(ctx, "client", "list"))
	h.ProcessHook(next)(ctx, goredis.NewCmd(ctx, "client"))
	if len(env.Spans()) != 2 {
		t.Errorf("spans = %+v", env.Spans())
	}
}
