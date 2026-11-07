//go:build integration

package redis_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
	goredis "github.com/redis/go-redis/v9"
)

// e keeps only the error of a (value, error) call.
func e[T any](_ T, err error) error { return err }

// e3 is e for calls that return two values and an error (the SCAN family).
func e3[A, B any](_ A, _ B, err error) error { return err }

// Keys, strings, scripting and introspection commands that are safe to run, on a
// private server started with the DEBUG command and LFU eviction enabled.
func TestIntegrationMiscCommands(t *testing.T) {
	url := startServer(t, "--enable-debug-command", "yes", "--maxmemory-policy", "allkeys-lfu")
	ctx, c := newClient(t, url)

	for _, k := range []string{"a", "b", "c"} {
		if err := c.Set(ctx, k, "v-"+k, 0); err != nil {
			t.Fatal(err)
		}
	}
	dump, err := c.Dump(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	tm := time.Now().Add(time.Hour)

	runSmoke(t, []smoke{
		{"ExpireAt", func() error { return e(c.ExpireAt(ctx, "a", tm)) }},
		{"ExpireTime", func() error {
			d, err := c.ExpireTime(ctx, "a")
			if err == nil && d <= 0 {
				err = fmt.Errorf("ExpireTime = %v", d)
			}
			return err
		}},
		{"ExpireNX", func() error { return e(c.ExpireNX(ctx, "b", time.Hour)) }},
		{"ExpireGT", func() error { return e(c.ExpireGT(ctx, "b", 2*time.Hour)) }},
		{"ExpireLT", func() error { return e(c.ExpireLT(ctx, "b", time.Minute)) }},
		{"ExpireXX", func() error { return e(c.ExpireXX(ctx, "b", time.Minute)) }},
		{"PExpireAt", func() error { return e(c.PExpireAt(ctx, "c", tm)) }},
		{"PExpireTime", func() error { return e(c.PExpireTime(ctx, "c")) }},
		{"GetSet", func() error { return e(c.GetSet(ctx, "c", "v-c2")) }},
		{"Keys", func() error {
			ks, err := c.Keys(ctx, "*")
			if err == nil && len(ks) != 3 {
				err = fmt.Errorf("Keys = %v", ks)
			}
			return err
		}},
		{"RandomKey", func() error { return e(c.RandomKey(ctx)) }},
		{"RenameNX", func() error { return e(c.RenameNX(ctx, "c", "c2")) }},
		{"Move", func() error { return e(c.Move(ctx, "c2", 1)) }},
		{"Restore", func() error { return e(c.Restore(ctx, "restored", 0, dump)) }},
		{"MemoryUsage", func() error { return e(c.MemoryUsage(ctx, "a")) }},
		{"ObjectFreq", func() error { return e(c.ObjectFreq(ctx, "a")) }},
		{"ObjectIdleTime", func() error {
			// Idle time is tracked only under an LRU policy (ObjectFreq needs LFU).
			if _, err := c.ConfigSet(ctx, "maxmemory-policy", "allkeys-lru"); err != nil {
				return err
			}
			return e(c.ObjectIdleTime(ctx, "a"))
		}},
		{"ObjectRefCount", func() error { return e(c.ObjectRefCount(ctx, "a")) }},
		{"DebugObject", func() error { return e(c.DebugObject(ctx, "a")) }},
		{"Command", func() error {
			m, err := c.Command(ctx)
			if err == nil && m["get"] == nil {
				err = errors.New("COMMAND lacks get")
			}
			return err
		}},
		{"CommandGetKeys", func() error {
			ks, err := c.CommandGetKeys(ctx, "SET", "mykey", "val")
			if err == nil && len(ks) != 1 {
				err = fmt.Errorf("keys = %v", ks)
			}
			return err
		}},
		{"CommandGetKeysAndFlags", func() error { return e(c.CommandGetKeysAndFlags(ctx, "SET", "mykey", "val")) }},
		{"CommandList", func() error {
			l, err := c.CommandList(ctx, &goredis.FilterBy{Pattern: "get*"})
			if err == nil && len(l) == 0 {
				err = errors.New("empty command list")
			}
			return err
		}},
		{"InfoMap", func() error {
			m, err := c.InfoMap(ctx, "server")
			if err == nil && m["Server"]["redis_version"] == "" {
				err = fmt.Errorf("InfoMap = %v", m)
			}
			return err
		}},
		{"Digest", func() error { return e(c.Digest(ctx, "a")) }},
		{"GetToBuffer", func() error {
			buf := make([]byte, 16)
			n, err := c.GetToBuffer(ctx, "a", buf)
			if err == nil && string(buf[:n]) != "v-a" {
				err = fmt.Errorf("GetToBuffer = %q", buf[:n])
			}
			return err
		}},
		{"SetFromBuffer", func() error { return e(c.SetFromBuffer(ctx, "buf", []byte("raw"))) }},
		{"DoRaw", func() error {
			b, err := c.DoRaw(ctx, "GET", "buf")
			if err == nil && string(b) != "$3\r\nraw\r\n" { // DoRaw returns the raw RESP reply
				err = fmt.Errorf("DoRaw = %q", b)
			}
			return err
		}},
		{"DoRawWriteTo", func() error {
			var w bytes.Buffer
			n, err := c.DoRawWriteTo(ctx, &w, "GET", "buf")
			if err == nil && (n == 0 || w.Len() == 0) {
				err = errors.New("nothing written")
			}
			return err
		}},
		{"SetArgs", func() error {
			return e(c.SetArgs(ctx, "sa", "1", goredis.SetArgs{Mode: "NX", TTL: time.Minute}))
		}},
		{"RPushX", func() error {
			if _, err := c.RPush(ctx, "list", "a", "b", "c", "d"); err != nil {
				return err
			}
			return e(c.RPushX(ctx, "list", "e"))
		}},
		{"RPopCount", func() error { return e(c.RPopCount(ctx, "list", 2)) }},
		{"EvalRO", func() error { return e(c.EvalRO(ctx, "return 1", nil)) }},
		{"EvalShaRO", func() error {
			sha, err := c.ScriptLoad(ctx, "return ARGV[1]")
			if err != nil {
				return err
			}
			return e(c.EvalShaRO(ctx, sha, nil, "x"))
		}},
		{"FunctionLoad", func() error {
			return e(c.FunctionLoad(ctx, "#!lua name=misclib\nredis.register_function{function_name='ro', callback=function() return 7 end, flags={'no-writes'}}"))
		}},
		{"FCallRO", func() error { return e(c.FCallRO(ctx, "ro", nil)) }},
		{"FCallRo", func() error { return e(c.FCallRo(ctx, "ro", nil)) }},
		{"FunctionList", func() error {
			libs, err := c.FunctionList(ctx, goredis.FunctionListQuery{LibraryNamePattern: "misclib"})
			if err == nil && len(libs) != 1 {
				err = fmt.Errorf("FunctionList = %d", len(libs))
			}
			return err
		}},
		{"FunctionStats", func() error { return e(c.FunctionStats(ctx)) }},
		{"FunctionDump", func() error {
			d, err := c.FunctionDump(ctx)
			if err != nil {
				return err
			}
			if _, err := c.FunctionFlush(ctx); err != nil {
				return err
			}
			return e(c.FunctionRestore(ctx, d))
		}},
		{"FunctionFlush", func() error { return e(c.FunctionFlush(ctx)) }},
		{"FunctionFlushAsync", func() error { return e(c.FunctionFlushAsync(ctx)) }},
		{"FunctionKill", func() error {
			// Nothing is running, so the server answers NOTBUSY; seeing that error
			// proves the command reached the server.
			_, err := c.FunctionKill(ctx)
			if err != nil && containsAny(err.Error(), "NOTBUSY", "No scripts") {
				return nil
			}
			return fmt.Errorf("FunctionKill: want NOTBUSY, got %v", err)
		}},
		{"ScriptKill", func() error {
			_, err := c.ScriptKill(ctx)
			if err != nil && containsAny(err.Error(), "NOTBUSY", "No scripts") {
				return nil
			}
			return fmt.Errorf("ScriptKill: want NOTBUSY, got %v", err)
		}},
		{"ScriptFlush", func() error { return e(c.ScriptFlush(ctx)) }},
		{"PubSubChannels", func() error { return e(c.PubSubChannels(ctx, "*")) }},
		{"PubSubNumPat", func() error { return e(c.PubSubNumPat(ctx)) }},
		{"PubSubShardChannels", func() error { return e(c.PubSubShardChannels(ctx, "*")) }},
		{"PubSubShardNumSub", func() error { return e(c.PubSubShardNumSub(ctx, "ch")) }},
		{"SPublish", func() error {
			// Sharded pub/sub works on a standalone server too.
			return e(c.SPublish(ctx, "ch", "hello"))
		}},
		{"Wait", func() error { return e(c.Wait(ctx, 0, 100*time.Millisecond)) }},
		{"WaitAOF", func() error { return e(c.WaitAOF(ctx, 0, 0, 100*time.Millisecond)) }},
		{"LastSave", func() error { return e(c.LastSave(ctx)) }},
		{"SlowLogLen", func() error { return e(c.SlowLogLen(ctx)) }},
		{"SlowLogGet", func() error { return e(c.SlowLogGet(ctx, 5)) }},
		{"SlowLogReset", func() error { return e(c.SlowLogReset(ctx)) }},
		{"Latency", func() error { return e(c.Latency(ctx)) }},
		{"LatencyReset", func() error { return e(c.LatencyReset(ctx)) }},
	})
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && bytes.Contains([]byte(s), []byte(sub)) {
			return true
		}
	}
	return false
}

var _ = redis.ErrNotFound
