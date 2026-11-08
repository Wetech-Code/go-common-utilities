package jwt_test

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	gjwt "github.com/golang-jwt/jwt/v5"

	"github.com/Wetech-Code/go-common-utilities/jwt"
)

var now = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func fixed() time.Time { return now }

func hmacSecret(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

var (
	rsaKey2048  = mustRSA(2048)
	rsaKey1024  = mustRSA(1024)
	p256        = mustEC(elliptic.P256())
	p384        = mustEC(elliptic.P384())
	p521        = mustEC(elliptic.P521())
	_, edKey, _ = ed25519.GenerateKey(rand.Reader)
)

func mustRSA(bits int) *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		panic(err)
	}
	return k
}

func mustEC(c elliptic.Curve) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(c, rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

func base(alg string, key jwt.Key) jwt.Config {
	return jwt.Config{Algorithm: alg, Key: key, Issuer: "api.example.com", Audience: []string{"web"}, TTL: 15 * time.Minute, Now: fixed}
}

func TestRoundTripForEveryAlgorithm(t *testing.T) {
	for alg, key := range map[string]jwt.Key{
		"HS256": {Secret: hmacSecret(32)}, "HS384": {Secret: hmacSecret(48)}, "HS512": {Secret: hmacSecret(64)},
		"RS256": {Private: rsaKey2048}, "RS384": {Private: rsaKey2048}, "RS512": {Private: rsaKey2048},
		"PS256": {Private: rsaKey2048}, "PS384": {Private: rsaKey2048}, "PS512": {Private: rsaKey2048},
		"ES256": {Private: p256}, "ES384": {Private: p384}, "ES512": {Private: p521},
		"EdDSA": {Private: edKey},
	} {
		t.Run(alg, func(t *testing.T) {
			c, err := jwt.New(base(alg, key))
			if err != nil {
				t.Fatal(err)
			}
			tok, err := c.Sign(jwt.Claims{Subject: "user-42", ID: "t-1", Extra: map[string]any{"role": "admin", "n": 5, "nested": map[string]any{"a": true}}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Verify(tok)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.Subject != "user-42" || got.ID != "t-1" || got.Issuer != "api.example.com" || !reflect.DeepEqual(got.Audience, []string{"web"}) ||
				!got.IssuedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(15*time.Minute)) {
				t.Errorf("claims = %+v", got)
			}
			if got.Extra["role"] != "admin" || got.Extra["n"] != float64(5) || got.Extra["nested"].(map[string]any)["a"] != true {
				t.Errorf("extra = %v", got.Extra)
			}
			if len(got.Extra) != 3 {
				t.Errorf("registered claims must not appear in Extra: %v", got.Extra)
			}
		})
	}
}

func TestVerifyOnlyWithPublicKeys(t *testing.T) {
	for alg, tc := range map[string]struct{ private, public crypto.PrivateKey }{
		"RS256": {rsaKey2048, &rsaKey2048.PublicKey},
		"ES256": {p256, &p256.PublicKey},
		"EdDSA": {edKey, edKey.Public()},
	} {
		signer, err := jwt.New(base(alg, jwt.Key{Private: tc.private}))
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := signer.Sign(jwt.Claims{Subject: "s"})
		cfg := base(alg, jwt.Key{Public: tc.public})
		cfg.TTL = 0
		verifier, err := jwt.New(cfg)
		if err != nil {
			t.Fatalf("%s: %v", alg, err)
		}
		if _, err := verifier.Verify(tok); err != nil {
			t.Errorf("%s: %v", alg, err)
		}
		if _, err := verifier.Sign(jwt.Claims{}); !errors.Is(err, jwt.ErrNoSigningKey) {
			t.Errorf("%s: verify-only Sign = %v", alg, err)
		}
	}
}

// RFC 7515 appendix A.1: a token produced by another implementation.
func TestInteroperatesWithTheRFC7515Example(t *testing.T) {
	const token = "eyJ0eXAiOiJKV1QiLA0KICJhbGciOiJIUzI1NiJ9." +
		"eyJpc3MiOiJqb2UiLA0KICJleHAiOjEzMDA4MTkzODAsDQogImh0dHA6Ly9leGFtcGxlLmNvbS9pc19yb290Ijp0cnVlfQ." +
		"dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	secret, err := base64.RawURLEncoding.DecodeString("AyM1SysPpbyDfgZld3umj1qzKObwVMkoqQ-EstJQLr_T-1qS0gZH75aKtMN3Yj0iPS4hcgUuTwjAzZr1Z9CAow")
	if err != nil {
		t.Fatal(err)
	}
	c, err := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: secret}, Issuer: "joe",
		Now: func() time.Time { return time.Unix(1300819370, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Verify(token)
	if err != nil {
		t.Fatalf("the RFC example must verify: %v", err)
	}
	if got.Issuer != "joe" || got.ExpiresAt.Unix() != 1300819380 || got.Extra["http://example.com/is_root"] != true {
		t.Errorf("claims = %+v", got)
	}
	// One second after exp it is expired.
	late, _ := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: secret}, Now: func() time.Time { return time.Unix(1300819381, 0) }})
	if _, err := late.Verify(token); !errors.Is(err, jwt.ErrExpired) {
		t.Errorf("late = %v", err)
	}
	// A flipped bit in the signature fails, and so does a non-canonical encoding of
	// the same signature (the last character only carries four bits).
	for name, tampered := range map[string]string{
		"changed signature":     token[:len(token)-10] + "AAAAAAAAAA",
		"non-canonical padding": token[:len(token)-1] + "l",
	} {
		if _, err := c.Verify(tampered); !errors.Is(err, jwt.ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func hs(t *testing.T, mut func(*jwt.Config)) *jwt.Codec {
	t.Helper()
	cfg := base("HS256", jwt.Key{Secret: hmacSecret(32)})
	if mut != nil {
		mut(&cfg)
	}
	c, err := jwt.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExpiryNotBeforeAndLeeway(t *testing.T) {
	clock := now
	signer := hs(t, func(c *jwt.Config) { c.Now = func() time.Time { return clock } })
	secretCfg := base("HS256", jwt.Key{Secret: nil})
	_ = secretCfg
	tok, _ := signer.Sign(jwt.Claims{Subject: "s", NotBefore: now.Add(time.Minute)})

	clock = now
	if _, err := signer.Verify(tok); !errors.Is(err, jwt.ErrNotYetValid) {
		t.Errorf("before nbf: %v", err)
	}
	clock = now.Add(2 * time.Minute)
	if _, err := signer.Verify(tok); err != nil {
		t.Errorf("inside the window: %v", err)
	}
	clock = now.Add(15*time.Minute + time.Second)
	err := func() error { _, err := signer.Verify(tok); return err }()
	if !errors.Is(err, jwt.ErrExpired) || errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("after exp: %v", err)
	}
}

func TestLeewayToleratesClockSkew(t *testing.T) {
	key := hmacSecret(32)
	mk := func(leeway time.Duration, at time.Time) *jwt.Codec {
		c, err := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: key}, TTL: time.Minute, Leeway: leeway, Now: func() time.Time { return at }})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	tok, _ := mk(0, now).Sign(jwt.Claims{})
	// The verifier's clock is 20 s ahead of exp, so only leeway lets it through.
	if _, err := mk(0, now.Add(80*time.Second)).Verify(tok); !errors.Is(err, jwt.ErrExpired) {
		t.Errorf("no leeway: %v", err)
	}
	if _, err := mk(30*time.Second, now.Add(80*time.Second)).Verify(tok); err != nil {
		t.Errorf("with leeway: %v", err)
	}
	// A token issued 20 s in the verifier's future (clock skew) needs leeway too.
	future, _ := mk(0, now.Add(20*time.Second)).Sign(jwt.Claims{})
	if _, err := mk(0, now).Verify(future); !errors.Is(err, jwt.ErrNotYetValid) {
		t.Errorf("iat in the future: %v", err)
	}
	if _, err := mk(30*time.Second, now).Verify(future); err != nil {
		t.Errorf("iat within leeway: %v", err)
	}
}

func TestAlgorithmConfusionAndNoneAreRejected(t *testing.T) {
	// Attacker signs HS256 using the RSA public key's bytes as the secret.
	pubDER, _ := x509.MarshalPKIXPublicKey(&rsaKey2048.PublicKey)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	forged, err := gjwt.NewWithClaims(gjwt.SigningMethodHS256, gjwt.MapClaims{"sub": "admin", "exp": now.Add(time.Hour).Unix()}).SignedString(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	rsaVerifier, _ := jwt.New(jwt.Config{Algorithm: "RS256", Key: jwt.Key{Public: &rsaKey2048.PublicKey}, Now: fixed})
	if _, err := rsaVerifier.Verify(forged); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("algorithm confusion accepted: %v", err)
	}

	// alg "none", unsigned.
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin","exp":` + "4102444800" + `}`))
	c := hs(t, nil)
	for _, tok := range []string{hdr + "." + body + ".", hdr + "." + body, hdr + "." + body + ".AAAA"} {
		if _, err := c.Verify(tok); !errors.Is(err, jwt.ErrInvalid) {
			t.Errorf("alg none accepted: %q -> %v", tok, err)
		}
	}

	// A valid token for a different HMAC algorithm is not accepted either.
	secret := hmacSecret(64)
	other, _ := jwt.New(jwt.Config{Algorithm: "HS512", Key: jwt.Key{Secret: secret}, TTL: time.Minute, Now: fixed})
	tok512, _ := other.Sign(jwt.Claims{})
	same256, _ := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: secret}, Now: fixed})
	if _, err := same256.Verify(tok512); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("a token with another algorithm was accepted: %v", err)
	}
}

func TestMissingExpiryIsRejected(t *testing.T) {
	secret := hmacSecret(32)
	c, _ := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: secret}, TTL: time.Minute, Now: fixed})
	noExp, _ := gjwt.NewWithClaims(gjwt.SigningMethodHS256, gjwt.MapClaims{"sub": "x", "iat": now.Unix()}).SignedString(secret)
	if _, err := c.Verify(noExp); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("a token without exp was accepted: %v", err)
	}
}

func TestIssuerAudienceAndTamperingAreChecked(t *testing.T) {
	secret := hmacSecret(32)
	mk := func(iss string, aud ...string) *jwt.Codec {
		c, err := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: secret}, Issuer: iss, Audience: aud, TTL: time.Minute, Now: fixed})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	tok, _ := mk("a.example", "web", "mobile").Sign(jwt.Claims{Subject: "s"})
	for name, c := range map[string]*jwt.Codec{
		"wrong issuer":   mk("b.example", "web"),
		"wrong audience": mk("a.example", "admin"),
	} {
		if _, err := c.Verify(tok); !errors.Is(err, jwt.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := mk("a.example", "mobile").Verify(tok); err != nil {
		t.Errorf("any matching audience should do: %v", err)
	}
	// A verifier that checks neither accepts it.
	if _, err := mk("").Verify(tok); err != nil {
		t.Errorf("no issuer/audience configured: %v", err)
	}
	// A token without aud fails an audience-checking verifier.
	noAud, _ := mk("a.example").Sign(jwt.Claims{})
	if _, err := mk("a.example", "web").Verify(noAud); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("missing aud: %v", err)
	}
	// Per-call audience overrides the default.
	custom, _ := mk("a.example", "web").Sign(jwt.Claims{Audience: []string{"batch"}})
	if _, err := mk("a.example", "batch").Verify(custom); err != nil {
		t.Errorf("claim audience: %v", err)
	}
	if _, err := mk("a.example", "web").Verify(custom); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("default audience matched an overridden token: %v", err)
	}

	// Tamper with the payload: swap in another subject but keep the signature.
	parts := strings.Split(tok, ".")
	var payload map[string]any
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	json.Unmarshal(raw, &payload)
	payload["sub"] = "admin"
	forged, _ := json.Marshal(payload)
	parts[1] = base64.RawURLEncoding.EncodeToString(forged)
	if _, err := mk("a.example", "web").Verify(strings.Join(parts, ".")); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("tampered payload accepted: %v", err)
	}
	// Another key does not verify.
	if _, err := hs(t, nil).Verify(tok); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("wrong key accepted: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	oldSecret, newSecret := hmacSecret(32), hmacSecret(32)
	oldCodec, _ := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{ID: "2025", Secret: oldSecret}, TTL: time.Hour, Now: fixed})
	oldTok, _ := oldCodec.Sign(jwt.Claims{Subject: "old"})

	rotated, err := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{ID: "2026", Secret: newSecret},
		OldKeys: []jwt.Key{{ID: "2025", Secret: oldSecret}}, TTL: time.Hour, Now: fixed})
	if err != nil {
		t.Fatal(err)
	}
	newTok, _ := rotated.Sign(jwt.Claims{Subject: "new"})
	for _, tok := range []string{oldTok, newTok} {
		if _, err := rotated.Verify(tok); err != nil {
			t.Errorf("rotation: %v", err)
		}
	}
	if h, _ := jwt.Inspect(newTok); h.Header["kid"] != "2026" {
		t.Errorf("kid header = %v", h.Header)
	}
	// After the old key is dropped, its tokens stop verifying.
	retired, _ := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{ID: "2026", Secret: newSecret}, TTL: time.Hour, Now: fixed})
	if _, err := retired.Verify(oldTok); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("retired key accepted: %v", err)
	}
	// A token naming a kid we do not know is rejected, even if signed with a known secret.
	unknown, _ := gjwt.NewWithClaims(gjwt.SigningMethodHS256, gjwt.MapClaims{"exp": now.Add(time.Hour).Unix()}).SignedString(newSecret)
	_ = unknown
	forgedKid := gjwt.NewWithClaims(gjwt.SigningMethodHS256, gjwt.MapClaims{"exp": now.Add(time.Hour).Unix()})
	forgedKid.Header["kid"] = "attacker"
	s, _ := forgedKid.SignedString(newSecret)
	if _, err := rotated.Verify(s); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("unknown kid: %v", err)
	}
	// A kid that is not a string is rejected.
	badKid := gjwt.NewWithClaims(gjwt.SigningMethodHS256, gjwt.MapClaims{"exp": now.Add(time.Hour).Unix()})
	badKid.Header["kid"] = 7
	s, _ = badKid.SignedString(newSecret)
	if _, err := rotated.Verify(s); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("numeric kid: %v", err)
	}
	// A token with no kid verifies with the primary key.
	noKid, _ := gjwt.NewWithClaims(gjwt.SigningMethodHS256, gjwt.MapClaims{"exp": now.Add(time.Hour).Unix()}).SignedString(newSecret)
	if _, err := rotated.Verify(noKid); err != nil {
		t.Errorf("no kid: %v", err)
	}
}

func TestTokenSizeLimitAndMalformedInput(t *testing.T) {
	c := hs(t, func(cfg *jwt.Config) { cfg.MaxTokenSize = 200 })
	big, _ := c.Sign(jwt.Claims{Extra: map[string]any{"pad": strings.Repeat("x", 500)}})
	if _, err := c.Verify(big); !errors.Is(err, jwt.ErrInvalid) || !strings.Contains(err.Error(), "larger than 200") {
		t.Errorf("oversized token: %v", err)
	}
	for _, bad := range []string{"", "a", "a.b", "a.b.c", "....", "e30.e30.", strings.Repeat("a.", 100), "\x00\x01.\x02.\x03"} {
		if _, err := hs(t, nil).Verify(bad); !errors.Is(err, jwt.ErrInvalid) {
			t.Errorf("Verify(%q) = %v", bad, err)
		}
		if _, err := jwt.Inspect(bad); err == nil {
			t.Errorf("Inspect(%q) should fail", bad)
		}
	}
	if _, err := jwt.Inspect(strings.Repeat("a", 9000)); !errors.Is(err, jwt.ErrInvalid) {
		t.Errorf("Inspect oversized: %v", err)
	}
}

func TestSignRejectsBadClaimsAndTTL(t *testing.T) {
	c := hs(t, nil)
	for _, name := range []string{"iss", "sub", "aud", "exp", "nbf", "iat", "jti"} {
		if _, err := c.Sign(jwt.Claims{Extra: map[string]any{name: "x"}}); !errors.Is(err, jwt.ErrConfig) {
			t.Errorf("reserved %q in Extra: %v", name, err)
		}
	}
	if _, err := c.Sign(jwt.Claims{Extra: map[string]any{"bad": make(chan int)}}); err == nil || strings.Contains(err.Error(), "chan") {
		t.Errorf("unencodable claim: %v", err)
	}
	if _, err := c.SignWithTTL(jwt.Claims{}, 0); !errors.Is(err, jwt.ErrConfig) {
		t.Errorf("zero ttl: %v", err)
	}
	noTTL := hs(t, func(cfg *jwt.Config) { cfg.TTL = 0 })
	if _, err := noTTL.Sign(jwt.Claims{}); !errors.Is(err, jwt.ErrConfig) {
		t.Errorf("Sign without a TTL: %v", err)
	}
	if tok, err := noTTL.SignWithTTL(jwt.Claims{}, time.Minute); err != nil {
		t.Errorf("SignWithTTL needs no Config.TTL: %v", err)
	} else if _, err := noTTL.Verify(tok); err != nil {
		t.Error(err)
	}
	short, err := c.SignWithTTL(jwt.Claims{Subject: "s"}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Verify(short)
	if err != nil || !got.ExpiresAt.Equal(now.Add(time.Second)) {
		t.Errorf("short-lived token: %+v %v", got, err)
	}
}

func TestConfigValidation(t *testing.T) {
	good := hmacSecret(32)
	for name, cfg := range map[string]jwt.Config{
		"no algorithm":      {Key: jwt.Key{Secret: good}, TTL: time.Minute},
		"unknown algorithm": {Algorithm: "none", Key: jwt.Key{Secret: good}, TTL: time.Minute},
		"lowercase":         {Algorithm: "hs256", Key: jwt.Key{Secret: good}, TTL: time.Minute},
		"short HS256":       {Algorithm: "HS256", Key: jwt.Key{Secret: hmacSecret(31)}, TTL: time.Minute},
		"short HS384":       {Algorithm: "HS384", Key: jwt.Key{Secret: hmacSecret(47)}, TTL: time.Minute},
		"short HS512":       {Algorithm: "HS512", Key: jwt.Key{Secret: hmacSecret(63)}, TTL: time.Minute},
		"no key":            {Algorithm: "HS256", TTL: time.Minute},
		"hmac with private": {Algorithm: "HS256", Key: jwt.Key{Secret: good, Private: rsaKey2048}, TTL: time.Minute},
		"rsa with secret":   {Algorithm: "RS256", Key: jwt.Key{Secret: good}, TTL: time.Minute},
		"small rsa":         {Algorithm: "RS256", Key: jwt.Key{Private: rsaKey1024}, TTL: time.Minute},
		"small rsa public":  {Algorithm: "PS256", Key: jwt.Key{Public: &rsaKey1024.PublicKey}},
		"wrong key type":    {Algorithm: "RS256", Key: jwt.Key{Private: p256}, TTL: time.Minute},
		"wrong public type": {Algorithm: "ES256", Key: jwt.Key{Public: &rsaKey2048.PublicKey}},
		"wrong curve":       {Algorithm: "ES256", Key: jwt.Key{Private: p384}, TTL: time.Minute},
		"ES384 on P-256":    {Algorithm: "ES384", Key: jwt.Key{Private: p256}, TTL: time.Minute},
		"ed with rsa":       {Algorithm: "EdDSA", Key: jwt.Key{Private: rsaKey2048}, TTL: time.Minute},
		"ed public wrong":   {Algorithm: "EdDSA", Key: jwt.Key{Public: p256.Public()}},
		"public mismatch":   {Algorithm: "RS256", Key: jwt.Key{Private: rsaKey2048, Public: &mustRSA(2048).PublicKey}, TTL: time.Minute},
		"ec mismatch":       {Algorithm: "ES256", Key: jwt.Key{Private: p256, Public: &mustEC(elliptic.P256()).PublicKey}, TTL: time.Minute},
		"ed mismatch":       {Algorithm: "EdDSA", Key: jwt.Key{Private: edKey, Public: func() crypto.PublicKey { p, _, _ := ed25519.GenerateKey(rand.Reader); return p }()}, TTL: time.Minute},
		"negative ttl":      {Algorithm: "HS256", Key: jwt.Key{Secret: good}, TTL: -1},
		"negative leeway":   {Algorithm: "HS256", Key: jwt.Key{Secret: good}, TTL: time.Minute, Leeway: -1},
		"negative size":     {Algorithm: "HS256", Key: jwt.Key{Secret: good}, TTL: time.Minute, MaxTokenSize: -1},
		"empty audience":    {Algorithm: "HS256", Key: jwt.Key{Secret: good}, TTL: time.Minute, Audience: []string{""}},
		"old key no id":     {Algorithm: "HS256", Key: jwt.Key{Secret: good}, TTL: time.Minute, OldKeys: []jwt.Key{{Secret: good}}},
		"old key dup id":    {Algorithm: "HS256", Key: jwt.Key{ID: "a", Secret: good}, TTL: time.Minute, OldKeys: []jwt.Key{{ID: "a", Secret: good}}},
		"old key bad":       {Algorithm: "HS256", Key: jwt.Key{Secret: good}, TTL: time.Minute, OldKeys: []jwt.Key{{ID: "o", Secret: hmacSecret(8)}}},
	} {
		err := cfg.Validate()
		if !errors.Is(err, jwt.ErrConfig) {
			t.Errorf("%s: Validate = %v, want ErrConfig", name, err)
			continue
		}
		if _, nerr := jwt.New(cfg); nerr == nil {
			t.Errorf("%s: New accepted it", name)
		}
		if strings.Contains(err.Error(), string(good)) {
			t.Errorf("%s: error leaks key material", name)
		}
	}
}

func TestErrorsNeverContainTheTokenOrClaims(t *testing.T) {
	c := hs(t, nil)
	tok, _ := c.Sign(jwt.Claims{Subject: "sensitive-subject", Extra: map[string]any{"ssn": "123-45-6789"}})
	other := hs(t, nil)
	_, err := other.Verify(tok)
	for _, leak := range []string{tok, "sensitive-subject", "123-45-6789", strings.Split(tok, ".")[1], strings.Split(tok, ".")[2]} {
		if err != nil && strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}
	// Junk with recognizable fragments is not echoed either.
	_, err = c.Verify("not-a-token-hunter2.payload-hunter2.sig-hunter2")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("malformed input echoed: %v", err)
	}
}

func TestInspectReadsWithoutVerifying(t *testing.T) {
	c := hs(t, nil)
	tok, _ := c.Sign(jwt.Claims{Subject: "who"})
	u, err := jwt.Inspect(tok)
	if err != nil {
		t.Fatal(err)
	}
	if u.Header["alg"] != "HS256" || u.Claims["sub"] != "who" || u.Claims["iss"] != "api.example.com" {
		t.Errorf("Inspect = %+v", u)
	}
	// It accepts what Verify refuses; that is its purpose and its danger.
	forged := tok[:strings.LastIndex(tok, ".")+1] + "AAAA"
	if _, err := c.Verify(forged); err == nil {
		t.Fatal("Verify accepted a forged signature")
	}
	if _, err := jwt.Inspect(forged); err != nil {
		t.Errorf("Inspect should not check signatures: %v", err)
	}
}

func TestConcurrentUse(t *testing.T) {
	c := hs(t, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				tok, err := c.Sign(jwt.Claims{Subject: "s", Extra: map[string]any{"i": i}})
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := c.Verify(tok); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestSecretIsCopiedAtConstruction(t *testing.T) {
	secret := hmacSecret(32)
	c, _ := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: secret}, TTL: time.Minute, Now: fixed})
	tok, _ := c.Sign(jwt.Claims{})
	for i := range secret {
		secret[i] = 0 // the caller wipes its copy
	}
	if _, err := c.Verify(tok); err != nil {
		t.Errorf("the Codec must keep its own copy of the secret: %v", err)
	}
}

func pemOf(t *testing.T, typ string, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func TestPEMParsing(t *testing.T) {
	pkcs8 := func(k crypto.PrivateKey) []byte { b, err := x509.MarshalPKCS8PrivateKey(k); must(t, err); return b }
	ecDER, err := x509.MarshalECPrivateKey(p256)
	must(t, err)

	for name, tc := range map[string]struct {
		pem  []byte
		alg  string
		want any
	}{
		"pkcs8 rsa": {pemOf(t, "PRIVATE KEY", pkcs8(rsaKey2048)), "RS256", &rsa.PrivateKey{}},
		"pkcs8 ec":  {pemOf(t, "PRIVATE KEY", pkcs8(p256)), "ES256", &ecdsa.PrivateKey{}},
		"pkcs8 ed":  {pemOf(t, "PRIVATE KEY", pkcs8(edKey)), "EdDSA", ed25519.PrivateKey{}},
		"pkcs1":     {pemOf(t, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey2048)), "RS256", &rsa.PrivateKey{}},
		"sec1":      {pemOf(t, "EC PRIVATE KEY", ecDER), "ES256", &ecdsa.PrivateKey{}},
	} {
		k, err := jwt.ParsePrivateKeyPEM(tc.pem)
		if err != nil || reflect.TypeOf(k) != reflect.TypeOf(tc.want) {
			t.Errorf("%s: %T, %v", name, k, err)
			continue
		}
		if _, err := jwt.New(jwt.Config{Algorithm: tc.alg, Key: jwt.Key{Private: k}, TTL: time.Minute}); err != nil {
			t.Errorf("%s: parsed key unusable: %v", name, err)
		}
	}

	rsaPubDER, _ := x509.MarshalPKIXPublicKey(&rsaKey2048.PublicKey)
	edPubDER, _ := x509.MarshalPKIXPublicKey(edKey.Public())
	for name, tc := range map[string]struct {
		pem  []byte
		want any
	}{
		"pkix rsa":  {pemOf(t, "PUBLIC KEY", rsaPubDER), &rsa.PublicKey{}},
		"pkix ed":   {pemOf(t, "PUBLIC KEY", edPubDER), ed25519.PublicKey{}},
		"pkcs1 pub": {pemOf(t, "RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(&rsaKey2048.PublicKey)), &rsa.PublicKey{}},
	} {
		k, err := jwt.ParsePublicKeyPEM(tc.pem)
		if err != nil || reflect.TypeOf(k) != reflect.TypeOf(tc.want) {
			t.Errorf("%s: %T, %v", name, k, err)
		}
	}

	for name, data := range map[string][]byte{
		"not pem":    []byte("hello"),
		"empty":      nil,
		"wrong type": pemOf(t, "CERTIFICATE REQUEST", []byte{1}),
		"bad pkcs8":  pemOf(t, "PRIVATE KEY", []byte{1, 2, 3}),
		"bad pkcs1":  pemOf(t, "RSA PRIVATE KEY", []byte{1, 2, 3}),
		"bad ec":     pemOf(t, "EC PRIVATE KEY", []byte{1, 2, 3}),
		"encrypted":  []byte("-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00\n\nAAAA\n-----END RSA PRIVATE KEY-----\n"),
		"enc pkcs8":  pemOf(t, "ENCRYPTED PRIVATE KEY", []byte{1}),
	} {
		if _, err := jwt.ParsePrivateKeyPEM(data); !errors.Is(err, jwt.ErrConfig) {
			t.Errorf("private %s: %v", name, err)
		}
	}
	for name, data := range map[string][]byte{
		"not pem":    []byte("hello"),
		"wrong type": pemOf(t, "PRIVATE KEY", []byte{1}),
		"bad pkix":   pemOf(t, "PUBLIC KEY", []byte{1, 2}),
		"bad pkcs1":  pemOf(t, "RSA PUBLIC KEY", []byte{1, 2}),
		"bad cert":   pemOf(t, "CERTIFICATE", []byte{1, 2}),
	} {
		if _, err := jwt.ParsePublicKeyPEM(data); !errors.Is(err, jwt.ErrConfig) {
			t.Errorf("public %s: %v", name, err)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPublicKeyFromACertificate(t *testing.T) {
	tmpl := &x509.Certificate{SerialNumber: bigOne(), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	for name, tc := range map[string]struct {
		priv crypto.Signer
		want any
	}{"rsa": {rsaKey2048, &rsa.PublicKey{}}, "ec": {p256, &ecdsa.PublicKey{}}, "ed": {edKey, ed25519.PublicKey{}}} {
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, tc.priv.Public(), tc.priv)
		must(t, err)
		k, err := jwt.ParsePublicKeyPEM(pemOf(t, "CERTIFICATE", der))
		if err != nil || reflect.TypeOf(k) != reflect.TypeOf(tc.want) {
			t.Errorf("%s: %T, %v", name, k, err)
		}
	}
}

func TestMissingKeyMaterialForAsymmetricAlgorithms(t *testing.T) {
	for _, alg := range []string{"RS256", "ES256", "EdDSA"} {
		if _, err := jwt.New(jwt.Config{Algorithm: alg}); !errors.Is(err, jwt.ErrConfig) {
			t.Errorf("%s with no key: %v", alg, err)
		}
	}
}

// FuzzVerify checks that arbitrary input never panics and never verifies.
func FuzzVerify(f *testing.F) {
	c, err := jwt.New(jwt.Config{Algorithm: "HS256", Key: jwt.Key{Secret: hmacSecret(32)}, TTL: time.Minute, Now: fixed})
	if err != nil {
		f.Fatal(err)
	}
	good, _ := c.Sign(jwt.Claims{Subject: "s"})
	for _, seed := range []string{"", "a.b.c", good, good[:len(good)-2], strings.ToUpper(good), good + ".", "e30.e30."} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, tok string) {
		if tok == good {
			return
		}
		if _, err := c.Verify(tok); err == nil {
			t.Fatalf("a token other than the one we signed verified: %q", tok)
		}
		jwt.Inspect(tok)
	})
}

func bigOne() *big.Int { return big.NewInt(1) }
