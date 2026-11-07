//go:build integration

package redis_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/redis"
)

// The module wrappers (JSON, TimeSeries, Bloom family, Search) need a server
// with those modules loaded, such as the redis:8 image. Each group skips, and is
// reported as skipped, when its module is missing. The Homebrew Redis does not
// ship them, so run this against scripts/integration.sh's containers.
func TestIntegrationModuleWrappers(t *testing.T) {
	url := envOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rdb, err := redis.New(ctx, redis.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	c := redis.Wrap(rdb)
	defer c.Close()

	p := fmt.Sprintf("gcu_test:mod:%d:", time.Now().UnixNano())
	k := func(n string) string { return p + n }
	defer func() {
		keys, _ := c.Scan(ctx, p+"*", 100)
		if len(keys) > 0 {
			c.Unlink(ctx, keys...)
		}
	}()
	missing := func(err error) bool { return err != nil && strings.Contains(err.Error(), "unknown command") }
	eq := func(got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}

	t.Run("JSON", func(t *testing.T) {
		_, err := c.JSONSet(ctx, k("doc"), "$", `{"name":"ann","n":1}`)
		if missing(err) {
			t.Skip("JSON module not loaded")
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.JSONGet(ctx, k("doc"), "$.name")
		if err != nil {
			t.Fatal(err)
		}
		eq(got, `["ann"]`)
		n, err := c.JSONDel(ctx, k("doc"), "$.n")
		if err != nil {
			t.Fatal(err)
		}
		eq(n, int64(1))
	})

	t.Run("TimeSeries", func(t *testing.T) {
		_, err := c.TSCreate(ctx, k("ts"))
		if missing(err) {
			t.Skip("TimeSeries module not loaded")
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.TSAdd(ctx, k("ts"), 1000, 21.5); err != nil {
			t.Fatal(err)
		}
		last, err := c.TSGet(ctx, k("ts"))
		if err != nil {
			t.Fatal(err)
		}
		eq(last.Value, 21.5)
	})

	t.Run("Bloom, TopK, CountMin, TDigest", func(t *testing.T) {
		_, err := c.BFReserve(ctx, k("bf"), 0.01, 100)
		if missing(err) {
			t.Skip("probabilistic module not loaded")
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.BFAdd(ctx, k("bf"), "x"); err != nil {
			t.Fatal(err)
		}
		in, err := c.BFExists(ctx, k("bf"), "x")
		if err != nil {
			t.Fatal(err)
		}
		eq(in, true)
		out, err := c.BFExists(ctx, k("bf"), "never-added")
		if err != nil {
			t.Fatal(err)
		}
		eq(out, false)

		if _, err := c.TopKReserve(ctx, k("topk"), 3); err != nil {
			t.Fatal(err)
		}
		if _, err := c.CMSInitByDim(ctx, k("cms"), 100, 5); err != nil {
			t.Fatal(err)
		}
		if _, err := c.TDigestCreate(ctx, k("td")); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("Search", func(t *testing.T) {
		idx, err := c.FT_List(ctx)
		if missing(err) {
			t.Skip("Search module not loaded")
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = idx // listing works; creating indexes is left to the caller's own tests
	})
}
