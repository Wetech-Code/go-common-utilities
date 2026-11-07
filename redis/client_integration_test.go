//go:build integration

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
)

// Exercises every Client method against a real server. Run with:
// GCU_TEST_REDIS_URL=redis://... go test -tags integration -run IntegrationClient ./redis
func TestIntegrationClientOperations(t *testing.T) {
	url := envOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rdb, err := redis.New(ctx, redis.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	c := redis.Wrap(rdb)
	defer c.Close()

	p := fmt.Sprintf("gcu_test:%d:", time.Now().UnixNano())
	k := func(name string) string { return p + name }
	defer func() { // remove everything this test created
		keys, _ := c.Scan(ctx, p+"*", 100)
		if len(keys) > 0 {
			c.Del(ctx, keys...)
		}
	}()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	eq := func(got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}

	must(c.Ping(ctx))

	t.Run("strings", func(t *testing.T) {
		must(c.Set(ctx, k("s"), "hello", time.Minute))
		v, err := c.Get(ctx, k("s"))
		must(err)
		eq(v, "hello")
		if _, err := c.Get(ctx, k("absent")); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("Get missing: want ErrNotFound, got %v", err)
		}
		ok, err := c.SetNX(ctx, k("s"), "other", 0)
		must(err)
		eq(ok, false)
		ok, err = c.SetNX(ctx, k("nx"), "first", 0)
		must(err)
		eq(ok, true)
		must(c.MSet(ctx, map[string]any{k("m1"): "a", k("m2"): "b"}))
		m, err := c.MGet(ctx, k("m1"), k("absent"), k("m2"))
		must(err)
		eq(m, map[string]string{k("m1"): "a", k("m2"): "b"})
		n, err := c.Append(ctx, k("m1"), "z")
		must(err)
		eq(n, int64(2))
		v, err = c.GetDel(ctx, k("m1"))
		must(err)
		eq(v, "az")
		if _, err := c.GetDel(ctx, k("m1")); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("GetDel again: want ErrNotFound, got %v", err)
		}
	})

	t.Run("counters", func(t *testing.T) {
		for _, step := range []struct {
			f    func() (int64, error)
			want int64
		}{
			{func() (int64, error) { return c.Incr(ctx, k("n")) }, 1},
			{func() (int64, error) { return c.IncrBy(ctx, k("n"), 9) }, 10},
			{func() (int64, error) { return c.Decr(ctx, k("n")) }, 9},
			{func() (int64, error) { return c.DecrBy(ctx, k("n"), 4) }, 5},
		} {
			got, err := step.f()
			must(err)
			eq(got, step.want)
		}
		f, err := c.IncrByFloat(ctx, k("f"), 1.5)
		must(err)
		eq(f, 1.5)
	})

	t.Run("keys", func(t *testing.T) {
		must(c.Set(ctx, k("key"), "v", 0))
		n, err := c.Exists(ctx, k("key"), k("absent"))
		must(err)
		eq(n, int64(1))
		ttl, err := c.TTL(ctx, k("key"))
		must(err)
		eq(ttl, time.Duration(-1))
		ok, err := c.Expire(ctx, k("key"), time.Minute)
		must(err)
		eq(ok, true)
		ttl, _ = c.TTL(ctx, k("key"))
		if ttl <= 0 || ttl > time.Minute {
			t.Fatalf("TTL after Expire = %v", ttl)
		}
		ok, err = c.Persist(ctx, k("key"))
		must(err)
		eq(ok, true)
		ttl, _ = c.TTL(ctx, k("absent"))
		eq(ttl, time.Duration(-2))
		must(c.Rename(ctx, k("key"), k("renamed")))
		typ, err := c.Type(ctx, k("renamed"))
		must(err)
		eq(typ, "string")
		keys, err := c.Scan(ctx, p+"re*", 10)
		must(err)
		eq(keys, []string{k("renamed")})
		n, err = c.Del(ctx, k("renamed"), k("absent"))
		must(err)
		eq(n, int64(1))
	})

	t.Run("hashes", func(t *testing.T) {
		must(c.HSet(ctx, k("h"), map[string]any{"a": "1", "b": "2"}))
		v, err := c.HGet(ctx, k("h"), "a")
		must(err)
		eq(v, "1")
		if _, err := c.HGet(ctx, k("h"), "zz"); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("HGet missing: want ErrNotFound, got %v", err)
		}
		all, err := c.HGetAll(ctx, k("h"))
		must(err)
		eq(all, map[string]string{"a": "1", "b": "2"})
		miss, err := c.HGetAll(ctx, k("nohash"))
		must(err)
		eq(len(miss), 0)
		ok, _ := c.HExists(ctx, k("h"), "b")
		eq(ok, true)
		n, err := c.HIncrBy(ctx, k("h"), "a", 5)
		must(err)
		eq(n, int64(6))
		l, _ := c.HLen(ctx, k("h"))
		eq(l, int64(2))
		d, err := c.HDel(ctx, k("h"), "a", "nope")
		must(err)
		eq(d, int64(1))
	})

	t.Run("lists", func(t *testing.T) {
		n, err := c.RPush(ctx, k("l"), "b", "c")
		must(err)
		eq(n, int64(2))
		_, err = c.LPush(ctx, k("l"), "a")
		must(err)
		r, err := c.LRange(ctx, k("l"), 0, -1)
		must(err)
		eq(r, []string{"a", "b", "c"})
		ll, _ := c.LLen(ctx, k("l"))
		eq(ll, int64(3))
		v, err := c.LPop(ctx, k("l"))
		must(err)
		eq(v, "a")
		v, err = c.RPop(ctx, k("l"))
		must(err)
		eq(v, "c")
		_, _ = c.RPush(ctx, k("l"), "b", "x")
		rem, err := c.LRem(ctx, k("l"), 0, "b")
		must(err)
		eq(rem, int64(2))
		must(c.LTrim(ctx, k("l"), 0, 0))
		must(func() error { _, e := c.LPop(ctx, k("l")); return e }())
		if _, err := c.LPop(ctx, k("l")); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("LPop empty: want ErrNotFound, got %v", err)
		}
		// BLPop: returns an element, then times out with ErrNotFound.
		_, _ = c.RPush(ctx, k("bl"), "job")
		key, val, err := c.BLPop(ctx, time.Second, k("bl"))
		must(err)
		eq([]string{key, val}, []string{k("bl"), "job"})
		if _, _, err := c.BLPop(ctx, time.Second, k("bl")); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("BLPop timeout: want ErrNotFound, got %v", err)
		}
	})

	t.Run("sets", func(t *testing.T) {
		n, err := c.SAdd(ctx, k("set"), "x", "y", "z")
		must(err)
		eq(n, int64(3))
		m, err := c.SMembers(ctx, k("set"))
		must(err)
		sort.Strings(m)
		eq(m, []string{"x", "y", "z"})
		ok, _ := c.SIsMember(ctx, k("set"), "y")
		eq(ok, true)
		card, _ := c.SCard(ctx, k("set"))
		eq(card, int64(3))
		rem, _ := c.SRem(ctx, k("set"), "x", "nope")
		eq(rem, int64(1))
		_, err = c.SPop(ctx, k("set"))
		must(err)
		_, _ = c.SPop(ctx, k("set"))
		if _, err := c.SPop(ctx, k("set")); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("SPop empty: want ErrNotFound, got %v", err)
		}
	})

	t.Run("sorted sets", func(t *testing.T) {
		n, err := c.ZAdd(ctx, k("z"), redis.Z{Score: 1, Member: "one"}, redis.Z{Score: 2, Member: "two"}, redis.Z{Score: 3, Member: "three"})
		must(err)
		eq(n, int64(3))
		r, _ := c.ZRange(ctx, k("z"), 0, -1)
		eq(r, []string{"one", "two", "three"})
		rev, _ := c.ZRevRange(ctx, k("z"), 0, 0)
		eq(rev, []string{"three"})
		ws, err := c.ZRangeWithScores(ctx, k("z"), 0, 0)
		must(err)
		eq(ws, []redis.Z{{Score: 1, Member: "one"}})
		s, err := c.ZScore(ctx, k("z"), "two")
		must(err)
		eq(s, 2.0)
		if _, err := c.ZScore(ctx, k("z"), "nope"); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("ZScore missing: want ErrNotFound, got %v", err)
		}
		rank, err := c.ZRank(ctx, k("z"), "three")
		must(err)
		eq(rank, int64(2))
		ns, _ := c.ZIncrBy(ctx, k("z"), 10, "one")
		eq(ns, 11.0)
		cnt, _ := c.ZCount(ctx, k("z"), "2", "3")
		eq(cnt, int64(2))
		card, _ := c.ZCard(ctx, k("z"))
		eq(card, int64(3))
		rem, _ := c.ZRem(ctx, k("z"), "one", "nope")
		eq(rem, int64(1))
	})

	t.Run("pubsub", func(t *testing.T) {
		sub := c.Subscribe(ctx, k("chan"))
		defer sub.Close()
		_, err := sub.Receive(ctx) // wait for the subscribe confirmation
		must(err)
		n, err := c.Publish(ctx, k("chan"), "ping")
		must(err)
		eq(n, int64(1))
		msg, err := sub.ReceiveMessage(ctx)
		must(err)
		eq(msg.Payload, "ping")
	})

	t.Run("pipeline, transaction, watch, script", func(t *testing.T) {
		var incr interface{ Val() int64 }
		must(c.Pipelined(ctx, func(p redis.Pipeliner) error {
			p.Set(ctx, k("p1"), "1", 0)
			incr = p.Incr(ctx, k("p2"))
			return nil
		}))
		eq(incr.Val(), int64(1))

		must(c.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Set(ctx, k("t1"), "a", 0)
			p.Set(ctx, k("t2"), "b", 0)
			return nil
		}))
		m, _ := c.MGet(ctx, k("t1"), k("t2"))
		eq(len(m), 2)

		must(c.Watch(ctx, func(tx *redis.Tx) error {
			_, err := tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Set(ctx, k("w"), "done", 0)
				return nil
			})
			return err
		}, k("w")))
		v, _ := c.Get(ctx, k("w"))
		eq(v, "done")

		res, err := c.Eval(ctx, "return redis.call('SET', KEYS[1], ARGV[1])", []string{k("lua")}, "ok")
		must(err)
		eq(res, "OK")
		if _, err := c.Eval(ctx, "return nil", nil); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("Eval nil: want ErrNotFound, got %v", err)
		}
	})
}
