//go:build integration

package redis_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
	goredis "github.com/redis/go-redis/v9"
)

// okOrErr passes when err is nil or its text contains one of the expected
// server replies. Used where a bare test server legitimately refuses (for
// example CONFIG REWRITE without a config file): the refusal proves the command
// reached the server and was decoded.
func okOrErr(err error, expected ...string) error {
	if err == nil {
		return nil
	}
	for _, x := range expected {
		if strings.Contains(err.Error(), x) {
			return nil
		}
	}
	return err
}

func portOf(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Port()
}

// serverGone reports whether nothing answers at rawURL any more.
func serverGone(rawURL string) bool {
	u, _ := url.Parse(rawURL)
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err == nil {
		conn.Close()
		return false
	}
	return true
}

// ACL, CLIENT, CONFIG, FLUSH, persistence, replication, hot keys, HIMPORT,
// MIGRATE and MODULE commands. These change server state, so they run on private
// throwaway servers only.
func TestIntegrationAdminCommands(t *testing.T) {
	url := startServer(t, "--enable-module-command", "yes")
	ctx, c := newClient(t, url)
	other := startServer(t) // MIGRATE and REPLICAOF target
	ctx2, c2 := newClient(t, other)
	_ = ctx2
	otherPort := portOf(t, other)
	otherID, err := c2.ClientID(ctx2)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "mk", "v", 0); err != nil {
		t.Fatal(err)
	}

	runSmoke(t, []smoke{
		// ACL (the user has no password and only read access)
		{"ACLSetUser", func() error {
			return e(c.ACLSetUser(ctx, "tester", "on", "nopass", "~*", "+@read"))
		}},
		{"ACLList", func() error { return e(c.ACLList(ctx)) }},
		{"ACLUsers", func() error { return e(c.ACLUsers(ctx)) }},
		{"ACLWhoAmI", func() error { return e(c.ACLWhoAmI(ctx)) }},
		{"ACLCat", func() error { return e(c.ACLCat(ctx)) }},
		{"ACLCatArgs", func() error { return e(c.ACLCatArgs(ctx, &goredis.ACLCatArgs{Category: "read"})) }},
		{"ACLGenPass", func() error { return e(c.ACLGenPass(ctx, 128)) }},
		{"ACLDryRun", func() error { return e(c.ACLDryRun(ctx, "tester", "GET", "mk")) }},
		{"ACLLog", func() error { return e(c.ACLLog(ctx, 10)) }},
		{"ACLLogReset", func() error { return e(c.ACLLogReset(ctx)) }},
		{"ACLDelUser", func() error { return e(c.ACLDelUser(ctx, "tester")) }},

		// CLIENT
		{"ClientGetName", func() error {
			// No name is set on this connection, so the server replies nil.
			if _, err := c.ClientGetName(ctx); !errors.Is(err, redis.ErrNotFound) {
				return fmt.Errorf("want ErrNotFound for an unnamed connection, got %v", err)
			}
			return nil
		}},
		{"ClientInfo", func() error {
			info, err := c.ClientInfo(ctx)
			if err == nil && info.ID == 0 {
				err = errors.New("empty ClientInfo")
			}
			return err
		}},
		{"ClientList", func() error { return e(c.ClientList(ctx)) }},
		{"ClientKill", func() error {
			info, err := c2.ClientInfo(ctx2)
			if err != nil {
				return err
			}
			return e(c2.ClientKill(ctx2, info.Addr)) // kills a pooled connection of c2; the pool reconnects
		}},
		{"ClientKillByFilter", func() error { return e(c.ClientKillByFilter(ctx, "TYPE", "pubsub")) }},
		{"ClientPause", func() error { return e(c.ClientPause(ctx, 50*time.Millisecond)) }},
		{"ClientUnpause", func() error { return e(c.ClientUnpause(ctx)) }},
		{"ClientUnblock", func() error { return e(c.ClientUnblock(ctx, 999999)) }},
		{"ClientUnblockWithError", func() error { return e(c.ClientUnblockWithError(ctx, 999999)) }},
		{"ClientTrackingOn", func() error { return e(c.ClientTrackingOn(ctx, &goredis.ClientTrackingOptions{Redirect: otherID})) }},
		{"ClientTrackingOff", func() error { return e(c.ClientTrackingOff(ctx)) }},
		{"ClientTracking", func() error { return e(c.ClientTracking(ctx, false, nil)) }},
		{"ClientMaintNotifications", func() error {
			// Open-source Redis does not implement CLIENT MAINT_NOTIFICATIONS (Redis
			// Cloud/Enterprise only), so the most this can show is the server's refusal.
			return okOrErr(e(c.ClientMaintNotifications(ctx, false, "none")), "unknown subcommand")
		}},

		// CONFIG
		{"ConfigSet", func() error { return e(c.ConfigSet(ctx, "maxmemory-policy", "noeviction")) }},
		{"ConfigResetStat", func() error { return e(c.ConfigResetStat(ctx)) }},
		{"ConfigRewrite", func() error {
			// The test server has no config file, which the server reports.
			return okOrErr(e(c.ConfigRewrite(ctx)), "without a config file")
		}},

		// persistence
		{"Save", func() error { return e(c.Save(ctx)) }},
		{"BgSave", func() error { return e(c.BgSave(ctx)) }},
		{"BgRewriteAOF", func() error {
			time.Sleep(500 * time.Millisecond) // let BGSAVE finish
			return okOrErr(e(c.BgRewriteAOF(ctx)), "already in progress", "scheduled")
		}},

		// replication: become a replica of the other server, then stop
		{"ReplicaOf", func() error { return e(c.ReplicaOf(ctx, "127.0.0.1", otherPort)) }},
		{"SlaveOf", func() error { return e(c.SlaveOf(ctx, "NO", "ONE")) }},

		// MIGRATE moves a key to the other server

		// MODULE LOADEX with a path that does not exist: the server refuses
		{"ModuleLoadex", func() error {
			return okOrErr(e(c.ModuleLoadex(ctx, &goredis.ModuleLoadexConfig{Path: "/nonexistent/module.so"})), "Error loading the extension", "loading")
		}},

		// HOTKEYS and HIMPORT are Redis 8.x features
		{"HotKeysStart", func() error {
			return e(c.HotKeysStart(ctx, &goredis.HotKeysStartArgs{Metrics: []goredis.HotKeysMetric{goredis.HotKeysMetricCPU}}))
		}},
		{"HotKeysGet", func() error { return e(c.HotKeysGet(ctx)) }},
		{"HotKeysStop", func() error { return e(c.HotKeysStop(ctx)) }},
		{"HotKeysReset", func() error { return e(c.HotKeysReset(ctx)) }},
		{"HImportPrepare", func() error { return e(c.HImportPrepare(ctx, "fs", "f1", "f2")) }},
		{"HImportSet", func() error { return e(c.HImportSet(ctx, "imported", "fs", "v1", "v2")) }},
		{"HImportDiscard", func() error { return e(c.HImportDiscard(ctx, "fs")) }},
		{"HImportDiscardAll", func() error { return e(c.HImportDiscardAll(ctx)) }},

		// FLUSH last: it removes everything
		{"FlushDB", func() error { return e(c.FlushDB(ctx)) }},
		{"FlushDBAsync", func() error { return e(c.FlushDBAsync(ctx)) }},
		{"FlushAll", func() error { return e(c.FlushAll(ctx)) }},
		{"FlushAllAsync", func() error { return e(c.FlushAllAsync(ctx)) }},
	})
}

// Each SHUTDOWN variant stops its own private server, then the test checks the
// port is closed.
func TestIntegrationShutdownCommands(t *testing.T) {
	variants := []struct {
		name string
		run  func(context.Context, *redis.Client) error
	}{
		{"Shutdown", func(ctx context.Context, c *redis.Client) error { return e(c.Shutdown(ctx)) }},
		{"ShutdownSave", func(ctx context.Context, c *redis.Client) error { return e(c.ShutdownSave(ctx)) }},
		{"ShutdownNoSave", func(ctx context.Context, c *redis.Client) error { return e(c.ShutdownNoSave(ctx)) }},
	}
	for _, v := range variants {
		v := v
		t.Run(v.name, func(t *testing.T) {
			url := startServer(t)
			ctx, c := newClient(t, url)
			if err := v.run(ctx, c); err != nil && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") &&
				!strings.Contains(err.Error(), "closed") && !strings.Contains(err.Error(), "reset") &&
				!strings.Contains(err.Error(), "refused") { // go-redis retries the dropped command against the stopped server
				t.Fatalf("%s returned %v", v.name, err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for !serverGone(url) {
				if time.Now().After(deadline) {
					t.Fatalf("%s: server still answering after 10s", v.name)
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

// MIGRATE moves a key to a different server that the source can reach.
func TestIntegrationMigrate(t *testing.T) {
	src, host, port := startPair(t)
	ctx, c := newClient(t, src)
	if err := c.Set(ctx, "mk", "v", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Migrate(ctx, host, port, "mk", 0, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if n, _ := c.Exists(ctx, "mk"); n != 0 {
		t.Fatal("key still on the source after MIGRATE")
	}
}
