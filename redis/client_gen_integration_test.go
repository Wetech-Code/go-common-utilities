//go:build integration

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
	goredis "github.com/redis/go-redis/v9"
)

// Exercises the generated wrappers family by family against a real server. Only
// harmless commands run: nothing here flushes, reconfigures or shuts down the
// server, and every key lives under a unique prefix that is removed afterwards.
func TestIntegrationGeneratedWrappers(t *testing.T) {
	url := envOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	rdb, err := redis.New(ctx, redis.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	c := redis.Wrap(rdb)
	defer c.Close()

	p := fmt.Sprintf("gcu_test:gen:%d:", time.Now().UnixNano())
	k := func(n string) string { return p + n }
	defer func() {
		keys, _ := c.Scan(ctx, p+"*", 100)
		if len(keys) > 0 {
			c.Unlink(ctx, keys...)
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

	t.Run("server", func(t *testing.T) {
		s, err := c.Echo(ctx, "hi")
		must(err)
		eq(s, "hi")
		now, err := c.Time(ctx)
		must(err)
		if time.Since(now) > time.Minute || time.Since(now) < -time.Minute {
			t.Fatalf("server time %v is far from local time", now)
		}
		n, err := c.DBSize(ctx)
		must(err)
		if n < 0 {
			t.Fatal("DBSize negative")
		}
		info, err := c.Info(ctx, "server")
		must(err)
		if !strings.Contains(info, "redis_version") {
			t.Fatalf("Info lacks redis_version: %.80s", info)
		}
		cfg, err := c.ConfigGet(ctx, "databases")
		must(err)
		if cfg["databases"] == "" {
			t.Fatalf("ConfigGet = %v", cfg)
		}
		id, err := c.ClientID(ctx)
		must(err)
		if id <= 0 {
			t.Fatal("ClientID")
		}
	})

	t.Run("keys", func(t *testing.T) {
		must(c.Set(ctx, k("src"), "v", 0))
		n, err := c.Copy(ctx, k("src"), k("dst"), 0, false)
		must(err)
		eq(n, int64(1))
		dump, err := c.Dump(ctx, k("src"))
		must(err)
		_, err = c.RestoreReplace(ctx, k("dst"), 0, dump)
		must(err)
		got, _ := c.Get(ctx, k("dst"))
		eq(got, "v")
		t1, err := c.Touch(ctx, k("src"), k("absent"))
		must(err)
		eq(t1, int64(1))
		ok, err := c.PExpire(ctx, k("src"), time.Minute)
		must(err)
		eq(ok, true)
		ttl, _ := c.PTTL(ctx, k("src"))
		if ttl <= 0 {
			t.Fatalf("PTTL = %v", ttl)
		}
		enc, err := c.ObjectEncoding(ctx, k("src"))
		must(err)
		if enc == "" {
			t.Fatal("ObjectEncoding empty")
		}
		u, err := c.Unlink(ctx, k("src"), k("dst"))
		must(err)
		eq(u, int64(2))
	})

	t.Run("strings", func(t *testing.T) {
		_, err := c.SetEx(ctx, k("ex"), "abc", time.Minute)
		must(err)
		ok, err := c.SetXX(ctx, k("ex"), "abcdef", time.Minute)
		must(err)
		eq(ok, true)
		l, _ := c.StrLen(ctx, k("ex"))
		eq(l, int64(6))
		r, _ := c.GetRange(ctx, k("ex"), 1, 2)
		eq(r, "bc")
		_, err = c.SetRange(ctx, k("ex"), 0, "X")
		must(err)
		v, err := c.GetEx(ctx, k("ex"), 0)
		must(err)
		eq(v, "Xbcdef")
		ok, err = c.MSetNX(ctx, k("n1"), "1", k("n2"), "2")
		must(err)
		eq(ok, true)
		ok, err = c.MSetNX(ctx, k("n1"), "x", k("n3"), "3")
		must(err)
		eq(ok, false)
	})

	t.Run("bitmaps and hyperloglog", func(t *testing.T) {
		_, err := c.SetBit(ctx, k("bits"), 7, 1)
		must(err)
		_, err = c.SetBit(ctx, k("bits"), 9, 1)
		must(err)
		b, _ := c.GetBit(ctx, k("bits"), 7)
		eq(b, int64(1))
		cnt, err := c.BitCount(ctx, k("bits"), &goredis.BitCount{Start: 0, End: -1})
		must(err)
		eq(cnt, int64(2))
		pos, err := c.BitPos(ctx, k("bits"), 1)
		must(err)
		eq(pos, int64(7))
		_, err = c.SetBit(ctx, k("bits2"), 7, 1)
		must(err)
		n, err := c.BitOpAnd(ctx, k("bitsand"), k("bits"), k("bits2"))
		must(err)
		eq(n, int64(2))

		_, err = c.PFAdd(ctx, k("hll1"), "a", "b", "c")
		must(err)
		_, err = c.PFAdd(ctx, k("hll2"), "c", "d")
		must(err)
		_, err = c.PFMerge(ctx, k("hll"), k("hll1"), k("hll2"))
		must(err)
		est, err := c.PFCount(ctx, k("hll"))
		must(err)
		eq(est, int64(4))
	})

	t.Run("geo", func(t *testing.T) {
		n, err := c.GeoAdd(ctx, k("geo"),
			&goredis.GeoLocation{Name: "palermo", Longitude: 13.361389, Latitude: 38.115556},
			&goredis.GeoLocation{Name: "catania", Longitude: 15.087269, Latitude: 37.502669})
		must(err)
		eq(n, int64(2))
		d, err := c.GeoDist(ctx, k("geo"), "palermo", "catania", "km")
		must(err)
		if d < 160 || d > 170 {
			t.Fatalf("distance = %v km", d)
		}
		hashes, err := c.GeoHash(ctx, k("geo"), "palermo")
		must(err)
		eq(len(hashes), 1)
		pos, err := c.GeoPos(ctx, k("geo"), "palermo")
		must(err)
		if len(pos) != 1 || pos[0] == nil {
			t.Fatalf("GeoPos = %v", pos)
		}
		near, err := c.GeoSearch(ctx, k("geo"), &goredis.GeoSearchQuery{
			Longitude: 15, Latitude: 37, Radius: 200, RadiusUnit: "km", Sort: "ASC"})
		must(err)
		eq(near, []string{"catania", "palermo"})
	})

	t.Run("hash extras", func(t *testing.T) {
		_, err := c.HMSet(ctx, k("h"), "a", "1", "b", "2")
		must(err)
		vals, err := c.HMGet(ctx, k("h"), "a", "zz")
		must(err)
		eq(vals, []any{"1", nil})
		keys, _ := c.HKeys(ctx, k("h"))
		sort.Strings(keys)
		eq(keys, []string{"a", "b"})
		ok, err := c.HSetNX(ctx, k("h"), "a", "9")
		must(err)
		eq(ok, false)
		f, err := c.HIncrByFloat(ctx, k("h"), "a", 0.5)
		must(err)
		eq(f, 1.5)
		hv, _ := c.HVals(ctx, k("h"))
		eq(len(hv), 2)
		scanKeys, _, err := c.HScan(ctx, k("h"), 0, "*", 10)
		must(err)
		eq(len(scanKeys), 4) // fields and values interleaved
	})

	t.Run("list extras", func(t *testing.T) {
		_, err := c.RPush(ctx, k("l"), "a", "b", "c")
		must(err)
		v, err := c.LIndex(ctx, k("l"), 1)
		must(err)
		eq(v, "b")
		_, err = c.LInsert(ctx, k("l"), "BEFORE", "b", "a2")
		must(err)
		must(func() error { _, e := c.LSet(ctx, k("l"), 0, "A"); return e }())
		pos, err := c.LPos(ctx, k("l"), "b", goredis.LPosArgs{})
		must(err)
		eq(pos, int64(2))
		moved, err := c.LMove(ctx, k("l"), k("l2"), "RIGHT", "LEFT")
		must(err)
		eq(moved, "c")
		back, err := c.RPopLPush(ctx, k("l2"), k("l"))
		must(err)
		eq(back, "c")
		n, err := c.LPushX(ctx, k("nolist"), "x")
		must(err)
		eq(n, int64(0))
	})

	t.Run("set extras", func(t *testing.T) {
		_, err := c.SAdd(ctx, k("s1"), "a", "b", "c")
		must(err)
		_, err = c.SAdd(ctx, k("s2"), "b", "c", "d")
		must(err)
		diff, _ := c.SDiff(ctx, k("s1"), k("s2"))
		eq(diff, []string{"a"})
		inter, _ := c.SInter(ctx, k("s1"), k("s2"))
		sort.Strings(inter)
		eq(inter, []string{"b", "c"})
		union, _ := c.SUnion(ctx, k("s1"), k("s2"))
		eq(len(union), 4)
		n, err := c.SInterStore(ctx, k("s3"), k("s1"), k("s2"))
		must(err)
		eq(n, int64(2))
		ok, err := c.SMove(ctx, k("s1"), k("s2"), "a")
		must(err)
		eq(ok, true)
		mem, err := c.SMIsMember(ctx, k("s2"), "a", "zz")
		must(err)
		eq(mem, []bool{true, false})
		set, err := c.SMembersMap(ctx, k("s2"))
		must(err)
		eq(len(set), 4)
		r, err := c.SRandMember(ctx, k("s2"))
		must(err)
		if _, in := set[r]; !in {
			t.Fatalf("SRandMember %q not in set", r)
		}
	})

	t.Run("sorted set extras", func(t *testing.T) {
		_, err := c.ZAdd(ctx, k("z"), goredis.Z{Score: 1, Member: "a"}, goredis.Z{Score: 2, Member: "b"}, goredis.Z{Score: 3, Member: "c"})
		must(err)
		by, err := c.ZRangeByScore(ctx, k("z"), &goredis.ZRangeBy{Min: "2", Max: "+inf"})
		must(err)
		eq(by, []string{"b", "c"})
		rr, err := c.ZRevRank(ctx, k("z"), "c")
		must(err)
		eq(rr, int64(0))
		ms, err := c.ZMScore(ctx, k("z"), "a", "zz")
		must(err)
		eq(len(ms), 2)
		low, err := c.ZPopMin(ctx, k("z"))
		must(err)
		eq(low, []goredis.Z{{Score: 1, Member: "a"}})
		hi, err := c.ZPopMax(ctx, k("z"))
		must(err)
		eq(hi, []goredis.Z{{Score: 3, Member: "c"}})
		if _, err := c.ZRevRank(ctx, k("z"), "gone"); !errors.Is(err, redis.ErrNotFound) {
			t.Fatalf("ZRevRank missing: want ErrNotFound, got %v", err)
		}
		_, err = c.ZAdd(ctx, k("z2"), goredis.Z{Score: 10, Member: "b"})
		must(err)
		n, err := c.ZUnionStore(ctx, k("zu"), &goredis.ZStore{Keys: []string{k("z"), k("z2")}})
		must(err)
		eq(n, int64(1))
	})

	t.Run("streams", func(t *testing.T) {
		id1, err := c.XAdd(ctx, &goredis.XAddArgs{Stream: k("x"), Values: map[string]any{"n": "1"}})
		must(err)
		_, err = c.XAdd(ctx, &goredis.XAddArgs{Stream: k("x"), Values: map[string]any{"n": "2"}})
		must(err)
		l, _ := c.XLen(ctx, k("x"))
		eq(l, int64(2))
		rng, err := c.XRange(ctx, k("x"), "-", "+")
		must(err)
		eq(len(rng), 2)
		eq(rng[0].ID, id1)

		must(func() error { _, e := c.XGroupCreate(ctx, k("x"), "g", "0"); return e }())
		res, err := c.XReadGroup(ctx, &goredis.XReadGroupArgs{Group: "g", Consumer: "me", Streams: []string{k("x"), ">"}, Count: 10})
		must(err)
		eq(len(res), 1)
		eq(len(res[0].Messages), 2)
		pend, err := c.XPending(ctx, k("x"), "g")
		must(err)
		eq(pend.Count, int64(2))
		acked, err := c.XAck(ctx, k("x"), "g", id1)
		must(err)
		eq(acked, int64(1))
		groups, err := c.XInfoGroups(ctx, k("x"))
		must(err)
		eq(len(groups), 1)
		del, err := c.XDel(ctx, k("x"), id1)
		must(err)
		eq(del, int64(1))
	})

	t.Run("scripts and functions", func(t *testing.T) {
		sha, err := c.ScriptLoad(ctx, "return ARGV[1]")
		must(err)
		out, err := c.EvalSha(ctx, sha, nil, "pong")
		must(err)
		eq(out, "pong")
		exists, err := c.ScriptExists(ctx, sha)
		must(err)
		eq(exists, []bool{true})
		lib := strings.ReplaceAll(p, ":", "_") + "lib"
		_, err = c.FunctionLoadReplace(ctx, fmt.Sprintf("#!lua name=%s\nredis.register_function('hello', function(keys, args) return 'hi' end)", lib))
		must(err)
		fres, err := c.FCall(ctx, "hello", nil)
		must(err)
		eq(fres, "hi")
		_, err = c.FunctionDelete(ctx, lib)
		must(err)
	})

	t.Run("pubsub introspection and Do", func(t *testing.T) {
		sub := c.Subscribe(ctx, k("chan"))
		defer sub.Close()
		_, err := sub.Receive(ctx)
		must(err)
		n, err := c.PubSubNumSub(ctx, k("chan"))
		must(err)
		eq(n[k("chan")], int64(1))
		res, err := c.Do(ctx, "ECHO", "via-do")
		must(err)
		eq(res, "via-do")
	})
}
