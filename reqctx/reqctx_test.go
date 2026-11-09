package reqctx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Wetech-Code/go-common-utilities/reqctx"
)

func TestNewRequestIDIsRandomAndValid(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := reqctx.NewRequestID()
		if len(id) != 32 || !reqctx.ValidID(id) || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"a", "0", "abc-123", "user_42", "a.b:c-d", "3f2b8c1e-5a4d-4e8b-9c1a-7d6e5f4a3b2c", strings.Repeat("a", 128)} {
		if !reqctx.ValidID(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", " ", "a b", "a\nb", "a\r\nX-Evil: 1", "-abc", ".abc", "_x", "a\x00", "né", "a/b", `a"b`, "a;b", strings.Repeat("a", 129)} {
		if reqctx.ValidID(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestRequestIDInContext(t *testing.T) {
	ctx := reqctx.WithRequestID(context.Background(), "req-1")
	if reqctx.RequestID(ctx) != "req-1" {
		t.Errorf("RequestID = %q", reqctx.RequestID(ctx))
	}
	if reqctx.RequestID(context.Background()) != "" {
		t.Error("no ID expected")
	}
	// An unsafe ID is replaced, never stored.
	got := reqctx.RequestID(reqctx.WithRequestID(context.Background(), "evil\nX: y"))
	if got == "" || strings.ContainsAny(got, "\n ") || !reqctx.ValidID(got) {
		t.Errorf("unsafe id stored: %q", got)
	}
	attrs := reqctx.Attrs(ctx)
	if len(attrs) != 1 || attrs[0].Key != "request_id" || attrs[0].Value.String() != "req-1" {
		t.Errorf("Attrs = %v", attrs)
	}
	if reqctx.Attrs(context.Background()) != nil {
		t.Error("no attrs without an ID")
	}
}

func TestCapturedHeaders(t *testing.T) {
	ctx := reqctx.WithHeaders(context.Background(), map[string]string{"x-tenant-id": "t1", "X-User-ID": "u1"})
	if reqctx.HeaderValue(ctx, "X-Tenant-ID") != "t1" || reqctx.HeaderValue(ctx, "x-user-id") != "u1" {
		t.Errorf("values = %q %q (names are case-insensitive)", reqctx.HeaderValue(ctx, "X-Tenant-ID"), reqctx.HeaderValue(ctx, "x-user-id"))
	}
	if reqctx.HeaderValue(ctx, "X-Other") != "" || reqctx.HeaderValue(context.Background(), "X-User-ID") != "" {
		t.Error("absent values must be empty")
	}
	// Adding more keeps what was there, and a later value replaces an earlier one.
	ctx2 := reqctx.WithHeaders(ctx, map[string]string{"X-Region": "r1", "X-User-ID": "u2"})
	if reqctx.HeaderValue(ctx2, "X-Tenant-ID") != "t1" || reqctx.HeaderValue(ctx2, "X-Region") != "r1" || reqctx.HeaderValue(ctx2, "X-User-ID") != "u2" {
		t.Error("merge")
	}
	if reqctx.HeaderValue(ctx, "X-Region") != "" || reqctx.HeaderValue(ctx, "X-User-ID") != "u1" {
		t.Error("the parent context must not change")
	}
	// Unsafe values are dropped, never stored.
	bad := reqctx.WithHeaders(context.Background(), map[string]string{"X-A": "a\nX-Evil: 1", "X-B": "has space", "X-C": "", "X-D": "ok"})
	if reqctx.HeaderValue(bad, "X-A") != "" || reqctx.HeaderValue(bad, "X-B") != "" || reqctx.HeaderValue(bad, "X-C") != "" || reqctx.HeaderValue(bad, "X-D") != "ok" {
		t.Error("unsafe values must be dropped")
	}
}
