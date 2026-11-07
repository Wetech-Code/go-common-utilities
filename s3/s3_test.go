package s3_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/s3"
)

// fakeS3 is a tiny path-style S3 stand-in that records what it receives.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	types   map[string]string
	auth    []string
}

func newFake(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, types: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))

	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != "bkt" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case key == "" && r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[key], f.types[key] = b, r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet:
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`)
			return
		}
		w.Write(b)
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func cfg(endpoint string) s3.Config {
	return s3.Config{Endpoint: endpoint, AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "unit-test-secret", Bucket: "bkt", UsePathStyle: true}
}

func TestValidate(t *testing.T) {
	good := cfg("http://localhost:8333")
	if err := good.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	mut := map[string]func(*s3.Config){
		"no key id":         func(c *s3.Config) { c.AccessKeyID = "" },
		"no secret":         func(c *s3.Config) { c.SecretAccessKey = "" },
		"no bucket":         func(c *s3.Config) { c.Bucket = "" },
		"bad scheme":        func(c *s3.Config) { c.Endpoint = "ftp://host" },
		"no host":           func(c *s3.Config) { c.Endpoint = "http://" },
		"creds in endpoint": func(c *s3.Config) { c.Endpoint = "https://user:pw@host" },
		"negative timeout":  func(c *s3.Config) { c.ConnectTimeout = -1 },
	}
	for name, f := range mut {
		c := good
		f(&c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: want error", name)
		} else if strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), good.SecretAccessKey) {
			t.Errorf("%s: error leaks a secret: %v", name, err)
		}
	}
}

func TestPutGetDeleteRoundTrip(t *testing.T) {
	f, srv := newFake(t)
	ctx := context.Background()
	c, err := s3.New(ctx, cfg(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Put(ctx, "dir/a.txt", strings.NewReader("hello"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if string(f.objects["dir/a.txt"]) != "hello" || f.types["dir/a.txt"] != "text/plain" {
		t.Errorf("server saw %q (%s)", f.objects["dir/a.txt"], f.types["dir/a.txt"])
	}

	rc, err := c.Get(ctx, "dir/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello" {
		t.Errorf("Get = %q", got)
	}

	if err := c.Delete(ctx, "dir/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "dir/a.txt"); !errors.Is(err, s3.ErrNotFound) {
		t.Errorf("Get after delete: want ErrNotFound, got %v", err)
	}
	for _, a := range f.auth {
		if strings.Contains(a, "unit-test-secret") {
			t.Fatal("secret key was sent on the wire")
		}
	}
}

func TestNewFailsWhenBucketMissing(t *testing.T) {
	_, srv := newFake(t)
	c := cfg(srv.URL)
	c.Bucket = "other"
	if _, err := s3.New(context.Background(), c); err == nil {
		t.Fatal("want error for missing bucket")
	} else if strings.Contains(err.Error(), c.SecretAccessKey) {
		t.Errorf("error leaks secret: %v", err)
	}
}

func TestNewFailsFastWhenUnreachable(t *testing.T) {
	c := cfg("http://127.0.0.1:1")
	c.ConnectTimeout = 2 * time.Second
	start := time.Now()
	if _, err := s3.New(context.Background(), c); err == nil {
		t.Fatal("want error")
	}
	if time.Since(start) > 6*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestPresign(t *testing.T) {
	_, srv := newFake(t)
	ctx := context.Background()
	c, err := s3.New(ctx, cfg(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for name, fn := range map[string]func(context.Context, string, time.Duration) (string, error){"get": c.PresignGet, "put": c.PresignPut} {
		raw, err := fn(ctx, "dir/a b.txt", 15*time.Minute)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if !strings.HasPrefix(u.Path, "/bkt/dir/a%20b.txt") && !strings.HasPrefix(u.Path, "/bkt/dir/a b.txt") {
			t.Errorf("%s: unexpected path %q", name, u.Path)
		}
		if q.Get("X-Amz-Expires") != "900" || q.Get("X-Amz-Signature") == "" {
			t.Errorf("%s: unexpected query %v", name, q)
		}
		if strings.Contains(raw, "unit-test-secret") {
			t.Errorf("%s: URL contains the secret key", name)
		}
	}

	if _, err := c.PresignGet(ctx, "k", 0); err == nil {
		t.Error("want error for zero expiry")
	}
	if _, err := c.PresignPut(ctx, "k", 8*24*time.Hour); err == nil {
		t.Error("want error for expiry over 7 days")
	}
}

func TestRejectsEmptyKey(t *testing.T) {
	_, srv := newFake(t)
	ctx := context.Background()
	c, err := s3.New(ctx, cfg(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Put(ctx, " ", bytes.NewReader(nil), "") == nil || c.Delete(ctx, "") == nil {
		t.Error("want error for empty key")
	}
	if _, err := c.Get(ctx, ""); err == nil {
		t.Error("want error for empty key")
	}
}

func TestRejectsUnsafeKeys(t *testing.T) {
	_, srv := newFake(t)
	ctx := context.Background()
	c, err := s3.New(ctx, cfg(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, key := range []string{"../other/x", "a/../../other/x", "a/./b", "/abs", `a\b`, "a\nb", "..", "a/.."} {
		if c.Delete(ctx, key) == nil {
			t.Errorf("Delete(%q): want error", key)
		}
		if _, err := c.PresignGet(ctx, key, time.Minute); err == nil {
			t.Errorf("PresignGet(%q): want error", key)
		}
	}
	for _, key := range []string{"a/b.txt", "..hidden", "a/b..c", "dir/file name.txt", "ünï/çödé"} {
		if err := c.Delete(ctx, key); err != nil {
			t.Errorf("Delete(%q): %v", key, err)
		}
	}
}
