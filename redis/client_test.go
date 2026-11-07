package redis

import (
	"errors"
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestNotFoundMapsNil(t *testing.T) {
	if !errors.Is(notFound(goredis.Nil), ErrNotFound) {
		t.Error("redis.Nil should map to ErrNotFound")
	}
	boom := errors.New("boom")
	if notFound(boom) != boom {
		t.Error("other errors must pass through unchanged")
	}
	if notFound(nil) != nil {
		t.Error("nil must stay nil")
	}
}

func TestPipelineIgnoresNilReply(t *testing.T) {
	// A Nil reply to one command inside a pipeline is read from that command,
	// it does not fail the whole batch.
	if notFoundUnlessReal(goredis.Nil) != nil {
		t.Error("Nil in a pipeline should not be an error for the batch")
	}
	boom := errors.New("boom")
	if notFoundUnlessReal(boom) != boom {
		t.Error("real errors must pass through")
	}
}

func TestWrapKeepsNativeClient(t *testing.T) {
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	c := Wrap(rdb)
	if c.Native != rdb {
		t.Fatal("Native should be the wrapped client")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}
