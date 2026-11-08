package jwks_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/jwt"
	"github.com/Wetech-Code/go-common-utilities/jwt/jwks"
)

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func rsaJWK(kid string, k *rsa.PublicKey) map[string]any {
	e := make([]byte, 0, 4)
	for v := k.E; v > 0; v >>= 8 {
		e = append([]byte{byte(v)}, e...)
	}
	return map[string]any{"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256", "n": b64(k.N.Bytes()), "e": b64(e)}
}

func ecJWK(kid string, k *ecdsa.PublicKey) map[string]any {
	size := (k.Curve.Params().BitSize + 7) / 8
	pad := func(b []byte) []byte { return append(make([]byte, size-len(b)), b...) }
	return map[string]any{"kty": "EC", "kid": kid, "crv": k.Curve.Params().Name, "x": b64(pad(k.X.Bytes())), "y": b64(pad(k.Y.Bytes()))}
}

func edJWK(kid string, k ed25519.PublicKey) map[string]any {
	return map[string]any{"kty": "OKP", "kid": kid, "crv": "Ed25519", "x": b64(k)}
}

func doc(keys ...map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{"keys": keys})
	return b
}

var (
	rsa1, _          = rsa.GenerateKey(rand.Reader, 2048)
	rsa2, _          = rsa.GenerateKey(rand.Reader, 2048)
	rsaSmall, _      = rsa.GenerateKey(rand.Reader, 1024)
	p256, _          = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _          = ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p521, _          = ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
	edPub, edPriv, _ = ed25519.GenerateKey(rand.Reader)
)

func TestParseAllKeyTypes(t *testing.T) {
	keys, err := jwks.Parse(doc(rsaJWK("r", &rsa1.PublicKey), ecJWK("e256", &p256.PublicKey), ecJWK("e384", &p384.PublicKey),
		ecJWK("e521", &p521.PublicKey), edJWK("ed", edPub)))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 5 {
		t.Fatalf("parsed %d keys", len(keys))
	}
	if !keys["r"].(*rsa.PublicKey).Equal(&rsa1.PublicKey) || !keys["e256"].(*ecdsa.PublicKey).Equal(&p256.PublicKey) ||
		!keys["e384"].(*ecdsa.PublicKey).Equal(&p384.PublicKey) || !keys["e521"].(*ecdsa.PublicKey).Equal(&p521.PublicKey) ||
		!keys["ed"].(ed25519.PublicKey).Equal(edPub) {
		t.Errorf("parsed keys differ from the originals: %v", keys)
	}
}

func TestParseSkipsWhatItCannotUseAndRejectsWhatIsUnsafe(t *testing.T) {
	enc := rsaJWK("enc", &rsa1.PublicKey)
	enc["use"] = "enc"
	x25519 := map[string]any{"kty": "OKP", "crv": "X25519", "kid": "dh", "x": b64(make([]byte, 32))}
	secp := map[string]any{"kty": "EC", "crv": "secp256k1", "kid": "k1", "x": "AA", "y": "AA"}
	unknown := map[string]any{"kty": "FUTURE", "kid": "f"}
	keys, err := jwks.Parse(doc(enc, x25519, secp, unknown, rsaJWK("good", &rsa1.PublicKey)))
	if err != nil || len(keys) != 1 || keys["good"] == nil {
		t.Fatalf("keys = %v, err %v", keys, err)
	}

	withPrivate := rsaJWK("priv", &rsa1.PublicKey)
	withPrivate["d"] = "c2VjcmV0LXByaXZhdGUtZXhwb25lbnQ"
	oct := map[string]any{"kty": "oct", "kid": "hmac", "k": "c2VjcmV0LXNoYXJlZC1rZXk"}
	noKid := rsaJWK("", &rsa1.PublicKey)
	delete(noKid, "kid")
	badE := rsaJWK("badE", &rsa1.PublicKey)
	badE["e"] = "AQ" // exponent 1
	offCurve := ecJWK("off", &p256.PublicKey)
	offCurve["y"] = b64(make([]byte, 32))
	shortEC := ecJWK("short", &p256.PublicKey)
	shortEC["x"] = "AAAA"
	badEd := edJWK("badEd", edPub)
	badEd["x"] = "AAAA"
	nonCanonical := rsaJWK("nc", &rsa1.PublicKey)
	nonCanonical["e"] = "AQAB=" // padding is not allowed in JWK

	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"private rsa":       {doc(withPrivate), "private key material"},
		"symmetric key":     {doc(oct), "private key material"},
		"no kid":            {doc(noKid), "no kid"},
		"duplicate kid":     {doc(rsaJWK("same", &rsa1.PublicKey), rsaJWK("same", &rsa2.PublicKey)), "repeats a kid"},
		"bad exponent":      {doc(badE), "invalid RSA"},
		"off curve":         {doc(offCurve), "not on the curve"},
		"short coordinate":  {doc(shortEC), "invalid EC"},
		"bad ed25519":       {doc(badEd), "invalid Ed25519"},
		"padded base64":     {doc(nonCanonical), "invalid RSA"},
		"empty set":         {doc(), "no usable key"},
		"only skipped":      {doc(enc), "no usable key"},
		"not json":          {[]byte("<html>"), "not a JSON key set"},
		"keys not a list":   {[]byte(`{"keys": 5}`), "not a JSON key set"},
		"key not an object": {[]byte(`{"keys": [5]}`), "not a JWK"},
	} {
		_, err := jwks.Parse(tc.data)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "c2VjcmV0") || strings.Contains(err.Error(), "<html>")) {
			t.Errorf("%s: error echoes input: %v", name, err)
		}
	}

	many := make([]map[string]any, 101)
	for i := range many {
		many[i] = edJWK(fmt.Sprint(i), edPub)
	}
	if _, err := jwks.Parse(doc(many...)); err == nil || !strings.Contains(err.Error(), "more than 100") {
		t.Errorf("too many keys: %v", err)
	}
}

// server serves a changeable key set and counts requests.
type server struct {
	*httptest.Server
	mu     sync.Mutex
	body   []byte
	status int
	hits   atomic.Int64
	header http.Header
}

func newServer(t *testing.T, body []byte) *server {
	t.Helper()
	s := &server{body: body, status: 200}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.header = r.Header.Clone()
		w.WriteHeader(s.status)
		w.Write(s.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) set(body []byte, status int) {
	s.mu.Lock()
	s.body, s.status = body, status
	s.mu.Unlock()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func TestFetchCacheAndRotation(t *testing.T) {
	srv := newServer(t, doc(rsaJWK("k1", &rsa1.PublicKey)))
	clk := &clock{t: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	var errs []error
	var emu sync.Mutex
	set, err := jwks.New(jwks.Config{URL: srv.URL, CacheTTL: time.Hour, MinRefreshInterval: time.Minute, Now: clk.now,
		OnError: func(e error) { emu.Lock(); errs = append(errs, e); emu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k, err := set.PublicKey(ctx, "k1")
	if err != nil || !k.(*rsa.PublicKey).Equal(&rsa1.PublicKey) {
		t.Fatalf("first lookup: %v %v", k, err)
	}
	set.PublicKey(ctx, "k1")
	set.PublicKey(ctx, "k1")
	if srv.hits.Load() != 1 {
		t.Errorf("a cached key must not refetch: %d requests", srv.hits.Load())
	}

	// The provider rotates: k2 appears. An unknown kid inside MinRefreshInterval does not refetch.
	srv.set(doc(rsaJWK("k1", &rsa1.PublicKey), rsaJWK("k2", &rsa2.PublicKey)), 200)
	if _, err := set.PublicKey(ctx, "k2"); !errors.Is(err, jwks.ErrNoKey) || srv.hits.Load() != 1 {
		t.Errorf("too-soon refresh: err=%v hits=%d", err, srv.hits.Load())
	}
	// After the interval the unknown kid triggers one refetch, which finds it.
	clk.add(61 * time.Second)
	k, err = set.PublicKey(ctx, "k2")
	if err != nil || !k.(*rsa.PublicKey).Equal(&rsa2.PublicKey) || srv.hits.Load() != 2 {
		t.Errorf("rotation: %v %v hits=%d", k, err, srv.hits.Load())
	}
	// A kid nobody published refetches at most once per interval, however often it is asked for.
	for i := 0; i < 20; i++ {
		set.PublicKey(ctx, "forged")
	}
	if srv.hits.Load() != 2 {
		t.Errorf("forged key IDs caused %d fetches", srv.hits.Load()-2)
	}
	clk.add(61 * time.Second)
	set.PublicKey(ctx, "forged")
	set.PublicKey(ctx, "forged")
	if srv.hits.Load() != 3 {
		t.Errorf("one more fetch expected after the interval, got %d total", srv.hits.Load())
	}

	// Past the TTL a known key triggers a refetch; a failing provider keeps the old keys working.
	srv.set([]byte("down"), 503)
	clk.add(2 * time.Hour)
	k, err = set.PublicKey(ctx, "k1")
	if err != nil || k == nil {
		t.Errorf("stale keys must stay usable when the provider fails: %v", err)
	}
	emu.Lock()
	defer emu.Unlock()
	if len(errs) == 0 || !strings.Contains(errs[len(errs)-1].Error(), "status 503") {
		t.Errorf("OnError calls = %v", errs)
	}
}

func TestFailuresWithoutACacheAreErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		body   []byte
		status int
		want   string
	}{
		"server error": {[]byte("oops"), 500, "status 500"},
		"not found":    {nil, 404, "status 404"},
		"html":         {[]byte("<html>hunter2</html>"), 200, "not a JSON key set"},
		"empty set":    {doc(), 200, "no usable key"},
	} {
		srv := newServer(t, tc.body)
		srv.set(tc.body, tc.status)
		set, _ := jwks.New(jwks.Config{URL: srv.URL})
		_, err := set.PublicKey(context.Background(), "k")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: response body echoed", name)
		}
		if set.Refresh(context.Background()) == nil {
			t.Errorf("%s: Refresh should report the failure", name)
		}
	}
}

func TestRedirectsOversizeAndHeaders(t *testing.T) {
	target := newServer(t, doc(rsaJWK("k", &rsa1.PublicKey)))
	redirector := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirector.Close()
	set, _ := jwks.New(jwks.Config{URL: redirector.URL})
	if _, err := set.PublicKey(context.Background(), "k"); err == nil || !strings.Contains(err.Error(), "status 302") || target.hits.Load() != 0 {
		t.Errorf("a redirect must not be followed: err=%v hits=%d", err, target.hits.Load())
	}

	big := newServer(t, doc(rsaJWK("k", &rsa1.PublicKey)))
	small, _ := jwks.New(jwks.Config{URL: big.URL, MaxSize: 50})
	if _, err := small.PublicKey(context.Background(), "k"); err == nil || !strings.Contains(err.Error(), "larger than 50") {
		t.Errorf("oversized response: %v", err)
	}

	withHeader := newServer(t, doc(rsaJWK("k", &rsa1.PublicKey)))
	hs, _ := jwks.New(jwks.Config{URL: withHeader.URL, Headers: map[string]string{"Authorization": "Bearer abc"}})
	if _, err := hs.PublicKey(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
	withHeader.mu.Lock()
	defer withHeader.mu.Unlock()
	if withHeader.header.Get("Authorization") != "Bearer abc" || withHeader.header.Get("Accept") != "application/json" {
		t.Errorf("headers = %v", withHeader.header)
	}
}

func TestTimeoutAndUnreachable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	set, _ := jwks.New(jwks.Config{URL: slow.URL, Timeout: 100 * time.Millisecond})
	start := time.Now()
	if _, err := set.PublicKey(context.Background(), "k"); err == nil || time.Since(start) > time.Second {
		t.Errorf("timeout: err=%v after %v", err, time.Since(start))
	}
	dead, _ := jwks.New(jwks.Config{URL: "http://127.0.0.1:1/jwks.json", Timeout: time.Second})
	_, err := dead.PublicKey(context.Background(), "k")
	if err == nil || strings.Contains(err.Error(), "127.0.0.1:1/jwks.json") {
		t.Errorf("unreachable: %v (the URL should not be echoed)", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := dead.PublicKey(cancelled, "k"); err == nil {
		t.Error("a cancelled context must fail the fetch")
	}
}

func TestEmptyKidAndConcurrency(t *testing.T) {
	one := newServer(t, doc(rsaJWK("only", &rsa1.PublicKey)))
	set, _ := jwks.New(jwks.Config{URL: one.URL})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := set.PublicKey(context.Background(), ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if one.hits.Load() != 1 {
		t.Errorf("concurrent callers made %d requests", one.hits.Load())
	}

	two := newServer(t, doc(rsaJWK("a", &rsa1.PublicKey), rsaJWK("b", &rsa2.PublicKey)))
	set2, _ := jwks.New(jwks.Config{URL: two.URL})
	if _, err := set2.PublicKey(context.Background(), ""); !errors.Is(err, jwks.ErrNoKey) {
		t.Errorf("a token without kid against two keys: %v", err)
	}
}

func TestTLSIsRequiredAndVerified(t *testing.T) {
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(doc(rsaJWK("k", &rsa1.PublicKey)))
	}))
	defer tlsSrv.Close()
	// Default client does not trust the test server's certificate: refused, not bypassed.
	untrusted, err := jwks.New(jwks.Config{URL: tlsSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.PublicKey(context.Background(), "k"); err == nil {
		t.Error("an untrusted certificate must fail")
	}
	trusted, _ := jwks.New(jwks.Config{URL: tlsSrv.URL, HTTPClient: tlsSrv.Client()})
	if _, err := trusted.PublicKey(context.Background(), "k"); err != nil {
		t.Errorf("trusted certificate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, ok := range []string{"https://idp.example.com/.well-known/jwks.json", "http://localhost:8080/jwks", "http://127.0.0.1/jwks", "http://[::1]:9/jwks"} {
		if err := (jwks.Config{URL: ok}).Validate(); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for name, c := range map[string]jwks.Config{
		"empty":        {},
		"plain http":   {URL: "http://idp.example.com/jwks"},
		"ftp":          {URL: "ftp://idp.example.com/jwks"},
		"no host":      {URL: "https://"},
		"credentials":  {URL: "https://user:secretpw@idp.example.com/jwks"},
		"negative":     {URL: "https://idp.example.com/jwks", CacheTTL: -1},
		"negative max": {URL: "https://idp.example.com/jwks", MaxSize: -1},
	} {
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: want an error", name)
			continue
		}
		if strings.Contains(err.Error(), "secretpw") {
			t.Errorf("%s: error leaks the password", name)
		}
		if _, nerr := jwks.New(c); nerr == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
}

func TestFromJSON(t *testing.T) {
	set, err := jwks.FromJSON(doc(rsaJWK("a", &rsa1.PublicKey)))
	if err != nil {
		t.Fatal(err)
	}
	if k, err := set.PublicKey(context.Background(), "a"); err != nil || k == nil {
		t.Errorf("static lookup: %v", err)
	}
	if _, err := set.PublicKey(context.Background(), "zzz"); !errors.Is(err, jwks.ErrNoKey) {
		t.Errorf("unknown kid: %v", err)
	}
	if err := set.Refresh(context.Background()); err != nil {
		t.Errorf("Refresh on a static set: %v", err)
	}
	if _, err := jwks.FromJSON([]byte("nope")); err == nil {
		t.Error("bad JSON")
	}
}

// --- through the jwt package ---

func codec(t *testing.T, alg string, src jwt.KeySource, mut func(*jwt.Config)) *jwt.Codec {
	t.Helper()
	cfg := jwt.Config{Algorithm: alg, KeySource: src, Issuer: "https://idp.example.com/", Audience: []string{"my-api"}}
	if mut != nil {
		mut(&cfg)
	}
	c, err := jwt.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func issuer(t *testing.T, alg string, key jwt.Key) *jwt.Codec {
	t.Helper()
	c, err := jwt.New(jwt.Config{Algorithm: alg, Key: key, Issuer: "https://idp.example.com/", Audience: []string{"my-api"}, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestVerifyTokensFromAnIdentityProvider(t *testing.T) {
	srv := newServer(t, doc(rsaJWK("2026-11", &rsa1.PublicKey)))
	set, _ := jwks.New(jwks.Config{URL: srv.URL, MinRefreshInterval: time.Nanosecond})
	verifier := codec(t, "RS256", set, nil)

	idp := issuer(t, "RS256", jwt.Key{ID: "2026-11", Private: rsa1})
	tok, _ := idp.Sign(jwt.Claims{Subject: "user-1"})
	claims, err := verifier.VerifyContext(context.Background(), tok)
	if err != nil || claims.Subject != "user-1" {
		t.Fatalf("verify: %v %+v", claims, err)
	}
	if _, err := verifier.Sign(jwt.Claims{}); !errors.Is(err, jwt.ErrNoSigningKey) {
		t.Errorf("a KeySource codec must not sign: %v", err)
	}

	// Key rotation at the provider: a token signed with a new key verifies after one refetch.
	rotated := issuer(t, "RS256", jwt.Key{ID: "2026-12", Private: rsa2})
	newTok, _ := rotated.Sign(jwt.Claims{Subject: "user-2"})
	if _, err := verifier.VerifyContext(context.Background(), newTok); !errors.Is(err, jwt.ErrInvalid) {
		t.Fatalf("a key nobody published yet: %v", err)
	}
	srv.set(doc(rsaJWK("2026-11", &rsa1.PublicKey), rsaJWK("2026-12", &rsa2.PublicKey)), 200)
	time.Sleep(time.Millisecond)
	if c, err := verifier.VerifyContext(context.Background(), newTok); err != nil || c.Subject != "user-2" {
		t.Errorf("after rotation: %v %v", c, err)
	}

	// Attacks: the kid names a real key but the signature is by another key.
	forger := issuer(t, "RS256", jwt.Key{ID: "2026-11", Private: rsa2})
	forged, _ := forger.Sign(jwt.Claims{Subject: "admin"})
	if _, err := verifier.VerifyContext(context.Background(), forged); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("forged token accepted: %v", err)
	}
	// Wrong audience and issuer are still enforced.
	other, _ := jwt.New(jwt.Config{Algorithm: "RS256", Key: jwt.Key{ID: "2026-11", Private: rsa1}, Issuer: "https://evil.example/", Audience: []string{"my-api"}, TTL: time.Minute})
	badIss, _ := other.Sign(jwt.Claims{})
	if _, err := verifier.VerifyContext(context.Background(), badIss); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("wrong issuer accepted: %v", err)
	}
}

func TestKeySourceKeysAreCheckedAgainstTheAlgorithm(t *testing.T) {
	// The provider publishes an EC key, but we only accept RS256: refused, not coerced.
	ecSet, _ := jwks.FromJSON(doc(ecJWK("e", &p256.PublicKey)))
	ecIdp := issuer(t, "ES256", jwt.Key{ID: "e", Private: p256})
	ecTok, _ := ecIdp.Sign(jwt.Claims{})
	if _, err := codec(t, "RS256", ecSet, nil).Verify(ecTok); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("algorithm mismatch: %v", err)
	}
	if _, err := codec(t, "ES256", ecSet, nil).Verify(ecTok); err != nil {
		t.Errorf("EC through JWKS: %v", err)
	}
	// Ed25519.
	edSet, _ := jwks.FromJSON(doc(edJWK("d", edPub)))
	edTok, _ := issuer(t, "EdDSA", jwt.Key{ID: "d", Private: edPriv}).Sign(jwt.Claims{})
	if _, err := codec(t, "EdDSA", edSet, nil).Verify(edTok); err != nil {
		t.Errorf("EdDSA through JWKS: %v", err)
	}
	// A 1024-bit RSA key in the set is refused by the size rule.
	smallSet, _ := jwks.FromJSON(doc(rsaJWK("s", &rsaSmall.PublicKey)))
	smallTok, err := jwt.New(jwt.Config{Algorithm: "RS256", Key: jwt.Key{ID: "s", Private: rsaSmall}})
	if err == nil {
		t.Fatalf("setup: the jwt package should refuse to sign with a 1024-bit key, got %v", smallTok)
	}
	// Sign one by hand-free means: re-use a 2048 token whose kid points at the small key.
	tok, _ := issuer(t, "RS256", jwt.Key{ID: "s", Private: rsa1}).Sign(jwt.Claims{})
	if _, err := codec(t, "RS256", smallSet, nil).Verify(tok); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("small key from JWKS: %v", err)
	}
	// A token with no kid works when the set has one key.
	single, _ := jwks.FromJSON(doc(rsaJWK("only", &rsa1.PublicKey)))
	noKid, _ := issuer(t, "RS256", jwt.Key{Private: rsa1}).Sign(jwt.Claims{})
	if _, err := codec(t, "RS256", single, nil).Verify(noKid); err != nil {
		t.Errorf("token without kid, single-key set: %v", err)
	}
}

func TestKeySourceConfigurationRules(t *testing.T) {
	set, _ := jwks.FromJSON(doc(rsaJWK("a", &rsa1.PublicKey)))
	for name, cfg := range map[string]jwt.Config{
		"with a key":    {Algorithm: "RS256", KeySource: set, Key: jwt.Key{Public: &rsa1.PublicKey}},
		"with an id":    {Algorithm: "RS256", KeySource: set, Key: jwt.Key{ID: "x"}},
		"with old keys": {Algorithm: "RS256", KeySource: set, OldKeys: []jwt.Key{{ID: "o", Public: &rsa1.PublicKey}}},
		"with hmac":     {Algorithm: "HS256", KeySource: set},
		"unknown alg":   {Algorithm: "none", KeySource: set},
	} {
		if _, err := jwt.New(cfg); !errors.Is(err, jwt.ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestVerifyContextBoundsTheKeyFetch(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(3 * time.Second) }))
	defer slow.Close()
	set, _ := jwks.New(jwks.Config{URL: slow.URL, Timeout: 10 * time.Second})
	tok, _ := issuer(t, "RS256", jwt.Key{ID: "k", Private: rsa1}).Sign(jwt.Claims{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := codec(t, "RS256", set, nil).VerifyContext(ctx, tok)
	if !errors.Is(err, jwt.ErrInvalid) || time.Since(start) > time.Second {
		t.Errorf("err=%v after %v", err, time.Since(start))
	}
	if err != nil && strings.Contains(err.Error(), slow.URL) {
		t.Errorf("error echoes the key set URL: %v", err)
	}
}

var _ crypto.PublicKey = (*rsa.PublicKey)(nil)
