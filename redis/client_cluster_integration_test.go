//go:build integration

package redis_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Cluster commands against a single cluster-enabled throwaway node that owns all
// 16384 slots.
func TestIntegrationClusterCommands(t *testing.T) {
	ctx, c := newClient(t, startServer(t, "--cluster-enabled", "yes"))
	if err := e(c.ClusterAddSlotsRange(ctx, 0, 16383)); err != nil {
		t.Fatal(err)
	}
	// Wait for the node to report the cluster as ok before using keys.
	for i := 0; i < 50; i++ {
		if info, _ := c.ClusterInfo(ctx); strings.Contains(info, "cluster_state:ok") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	id, err := c.ClusterMyID(ctx)
	if err != nil || id == "" {
		t.Fatalf("ClusterMyID = %q, %v", id, err)
	}
	slot, err := c.ClusterKeySlot(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Set(ctx, "foo", "bar", 0); err != nil {
		t.Fatal(err)
	}

	runSmoke(t, []smoke{
		{"ClusterInfo", func() error {
			info, err := c.ClusterInfo(ctx)
			if err == nil && !strings.Contains(info, "cluster_state:ok") {
				err = fmt.Errorf("cluster not ok: %.80s", info)
			}
			return err
		}},
		{"ClusterMyShardID", func() error { return e(c.ClusterMyShardID(ctx)) }},
		{"ClusterNodes", func() error { return e(c.ClusterNodes(ctx)) }},
		{"ClusterSlots", func() error { return e(c.ClusterSlots(ctx)) }},
		{"ClusterShards", func() error { return e(c.ClusterShards(ctx)) }},
		{"ClusterLinks", func() error { return e(c.ClusterLinks(ctx)) }},
		{"ClusterCountKeysInSlot", func() error {
			n, err := c.ClusterCountKeysInSlot(ctx, int(slot))
			if err == nil && n != 1 {
				err = fmt.Errorf("ClusterCountKeysInSlot = %d", n)
			}
			return err
		}},
		{"ClusterGetKeysInSlot", func() error {
			keys, err := c.ClusterGetKeysInSlot(ctx, int(slot), 10)
			if err == nil && (len(keys) != 1 || keys[0] != "foo") {
				err = fmt.Errorf("ClusterGetKeysInSlot = %v", keys)
			}
			return err
		}},
		{"ClusterCountFailureReports", func() error { return e(c.ClusterCountFailureReports(ctx, id)) }},
		{"ClusterSlaves", func() error { return e(c.ClusterSlaves(ctx, id)) }},
		{"ClusterSaveConfig", func() error { return e(c.ClusterSaveConfig(ctx)) }},
		{"ClusterForget", func() error {
			// A node cannot forget itself; the refusal proves the command reached the server.
			return okOrErr(e(c.ClusterForget(ctx, id)), "forget myself")
		}},
		{"ReadOnly", func() error { return e(c.ReadOnly(ctx)) }},
		{"ReadWrite", func() error { return e(c.ReadWrite(ctx)) }},
		{"ClusterDelSlots", func() error { return e(c.ClusterDelSlots(ctx, 1, 2, 3)) }},
		{"ClusterAddSlots", func() error { return e(c.ClusterAddSlots(ctx, 1, 2, 3)) }},
		{"ClusterDelSlotsRange", func() error {
			if err := e(c.ClusterDelSlotsRange(ctx, 100, 200)); err != nil {
				return err
			}
			return e(c.ClusterAddSlotsRange(ctx, 100, 200)) // restore, or the cluster stays down
		}},
		{"ClusterReset", func() error {
			// RESET needs an empty node: drop the key and give up the slots first.
			for i := 0; i < 50; i++ {
				if info, _ := c.ClusterInfo(ctx); strings.Contains(info, "cluster_state:ok") {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if _, err := c.Del(ctx, "foo"); err != nil {
				return err
			}
			if err := e(c.ClusterDelSlotsRange(ctx, 0, 16383)); err != nil {
				return err
			}
			if err := e(c.ClusterResetSoft(ctx)); err != nil {
				return err
			}
			return e(c.ClusterResetHard(ctx))
		}},
	})
}

// MEET, REPLICATE, FAILOVER and FORGET need a second node, so this test starts two
// cluster nodes as local processes. It needs GCU_TEST_REDIS_SERVER because the
// Docker fallback cannot join nodes across the host port mapping.
func TestIntegrationClusterMultiNode(t *testing.T) {
	if os.Getenv("GCU_TEST_REDIS_SERVER") == "" {
		t.Skip("MultiNode: set GCU_TEST_REDIS_SERVER to a redis-server binary to run the two-node cluster test")
	}
	urlA := startServer(t, "--cluster-enabled", "yes")
	urlB := startServer(t, "--cluster-enabled", "yes")
	ctxA, a := newClient(t, urlA)
	ctxB, b := newClient(t, urlB)

	urlC := startServer(t, "--cluster-enabled", "yes")
	ctxC, cc := newClient(t, urlC)
	idC, _ := cc.ClusterMyID(ctxC)

	if _, err := a.ClusterAddSlotsRange(ctxA, 0, 16383); err != nil {
		t.Fatal(err)
	}
	idA, _ := a.ClusterMyID(ctxA)
	idB, _ := b.ClusterMyID(ctxB)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if cond() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	// FORGET: A meets a spare node C, then drops it again.
	if _, err := a.ClusterMeet(ctxA, "127.0.0.1", portOf(t, urlC)); err != nil {
		t.Fatalf("ClusterMeet (C): %v", err)
	}
	waitFor("A to know C", func() bool { n, _ := a.ClusterNodes(ctxA); return strings.Contains(n, idC) })
	if _, err := a.ClusterForget(ctxA, idC); err != nil {
		t.Fatalf("ClusterForget: %v", err)
	}
	if n, _ := a.ClusterNodes(ctxA); strings.Contains(n, idC) {
		t.Fatal("A still lists C after ClusterForget")
	}

	if _, err := b.ClusterMeet(ctxB, "127.0.0.1", portOf(t, urlA)); err != nil {
		t.Fatalf("ClusterMeet: %v", err)
	}
	waitFor("B to know A", func() bool { n, _ := b.ClusterNodes(ctxB); return strings.Contains(n, idA) })
	waitFor("A to know B", func() bool { n, _ := a.ClusterNodes(ctxA); return strings.Contains(n, idB) })

	if _, err := b.ClusterReplicate(ctxB, idA); err != nil {
		t.Fatalf("ClusterReplicate: %v", err)
	}
	waitFor("B to be a replica of A", func() bool {
		n, _ := b.ClusterNodes(ctxB)
		for _, l := range strings.Split(n, "\n") {
			if strings.Contains(l, "myself,slave") && strings.Contains(l, idA) {
				return true
			}
		}
		return false
	})

	if _, err := b.ClusterFailover(ctxB); err != nil {
		t.Fatalf("ClusterFailover: %v", err)
	}
	waitFor("B to become the master", func() bool {
		n, _ := b.ClusterNodes(ctxB)
		for _, l := range strings.Split(n, "\n") {
			if strings.Contains(l, "myself,master") {
				return true
			}
		}
		return false
	})
}
