// Package jwks reads JSON Web Key Sets (RFC 7517), the public keys an identity
// provider such as Auth0, Keycloak, Cognito or Google publishes so that anyone can
// verify the tokens it issues. A Set is a jwt.KeySource:
//
//	set, err := jwks.New(jwks.Config{URL: "https://idp.example.com/.well-known/jwks.json"})
//	if err != nil {
//		return err
//	}
//	c, err := jwt.New(jwt.Config{
//		Algorithm: "RS256",
//		KeySource: set,
//		Issuer:    "https://idp.example.com/",
//		Audience:  []string{"my-api"},
//	})
//	claims, err := c.VerifyContext(ctx, token)
//
// The set is fetched on first use and cached. When a token names a key the cache
// does not have (the provider rotated its keys) the set is fetched again, but at
// most once per MinRefreshInterval so a flood of forged key IDs cannot make the
// service hammer the provider. If a refresh fails, the previous keys keep working.
//
// Safety rules: the URL must be https (plain http is accepted only for loopback
// addresses, for tests), certificates are always verified, redirects are refused,
// the response size is capped, only RSA, P-256/384/521 EC and Ed25519 public keys
// are accepted, and keys marked for encryption are ignored. Only public keys are
// ever read: a JWK that contains private key parameters is rejected.
package jwks

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultCacheTTL           = 10 * time.Minute
	DefaultMinRefreshInterval = 30 * time.Second
	DefaultTimeout            = 10 * time.Second
	DefaultMaxSize            = 1 << 20
	maxKeys                   = 100
)

// ErrNoKey is returned when the set has no usable key for a key ID.
var ErrNoKey = errors.New("jwks: no matching key")

// Config describes a remote key set.
type Config struct {
	// URL of the key set, usually ending in /.well-known/jwks.json. It must be
	// https:// (http:// is accepted for localhost only) and must not contain
	// credentials.
	URL string
	// CacheTTL is how long a fetched set is used before the next fetch.
	// Default DefaultCacheTTL.
	CacheTTL time.Duration
	// MinRefreshInterval is the shortest time between two fetches, including
	// fetches caused by unknown key IDs. Default DefaultMinRefreshInterval.
	MinRefreshInterval time.Duration
	// Timeout bounds one fetch. Default DefaultTimeout.
	Timeout time.Duration
	// MaxSize is the largest response accepted, in bytes. Default DefaultMaxSize.
	MaxSize int64
	// Headers are sent with every fetch, for key sets behind authentication.
	// They are never written to errors.
	Headers map[string]string
	// HTTPClient sends the requests. Default: a client with the standard
	// certificate checks. Redirects are refused whichever client is used.
	HTTPClient *http.Client
	// OnError is told about failed refreshes, which are otherwise silent because
	// the previous keys keep working. Default: ignore.
	OnError func(error)
	// Now returns the current time. Default time.Now; set it in tests.
	Now func() time.Time
}

// Validate reports problems with the configuration without fetching. Errors never
// include header values.
func (c Config) Validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("jwks: URL must be an https:// URL")
	}
	if u.User != nil {
		return errors.New("jwks: URL must not contain credentials; use Headers")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return errors.New("jwks: plain http is only allowed for localhost; key sets must be fetched over https")
	}
	if c.CacheTTL < 0 || c.MinRefreshInterval < 0 || c.Timeout < 0 || c.MaxSize < 0 {
		return errors.New("jwks: CacheTTL, MinRefreshInterval, Timeout and MaxSize must not be negative")
	}
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Set is a cached remote key set. It is safe for concurrent use.
type Set struct {
	cfg    Config
	client *http.Client

	mu        sync.RWMutex
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time // time of the last fetch attempt
	loadedAt  time.Time // time of the last successful fetch
	refresh   sync.Mutex
}

// New validates cfg and returns a Set. It does not fetch; the first PublicKey
// call does, or call Refresh to fail early at startup.
func New(cfg Config) (*Set, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.CacheTTL == 0 {
		cfg.CacheTTL = DefaultCacheTTL
	}
	if cfg.MinRefreshInterval == 0 {
		cfg.MinRefreshInterval = DefaultMinRefreshInterval
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxSize == 0 {
		cfg.MaxSize = DefaultMaxSize
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Set{cfg: cfg}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		s.client = &c
	} else {
		s.client = &http.Client{}
	}
	s.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return s, nil
}

// FromJSON returns a Set made from a key set you already have, for example one
// read from a file. It never fetches anything and never expires.
func FromJSON(data []byte) (*Set, error) {
	keys, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return &Set{keys: keys, cfg: Config{CacheTTL: time.Duration(1<<63 - 1), Now: time.Now}}, nil
}

// PublicKey implements jwt.KeySource. kid is the token's kid header; if it is
// empty the set must contain exactly one key.
func (s *Set) PublicKey(ctx context.Context, kid string) (crypto.PublicKey, error) {
	if key, ok := s.lookup(kid); ok && !s.stale() {
		return key, nil
	}
	if s.cfg.URL != "" {
		if err := s.refreshIfAllowed(ctx, kid); err != nil {
			if s.cfg.OnError != nil {
				s.cfg.OnError(err)
			}
			if _, have := s.snapshot(); !have {
				return nil, err // nothing cached to fall back on
			}
		}
	}
	if key, ok := s.lookup(kid); ok {
		return key, nil
	}
	return nil, ErrNoKey
}

func (s *Set) snapshot() (map[string]crypto.PublicKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keys, s.keys != nil
}

func (s *Set) lookup(kid string) (crypto.PublicKey, bool) {
	keys, ok := s.snapshot()
	if !ok {
		return nil, false
	}
	if kid == "" {
		if len(keys) == 1 {
			for _, k := range keys {
				return k, true
			}
		}
		return nil, false
	}
	k, ok := keys[kid]
	return k, ok
}

func (s *Set) stale() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.URL != "" && s.cfg.Now().Sub(s.loadedAt) >= s.cfg.CacheTTL
}

// refreshIfAllowed fetches unless a fetch was attempted less than
// MinRefreshInterval ago. Concurrent callers share one fetch.
func (s *Set) refreshIfAllowed(ctx context.Context, kid string) error {
	s.refresh.Lock()
	defer s.refresh.Unlock()

	s.mu.RLock()
	last, haveKeys := s.fetchedAt, s.keys != nil
	s.mu.RUnlock()
	if haveKeys {
		// Another caller may have just refreshed.
		if _, ok := s.lookup(kid); ok && !s.stale() {
			return nil
		}
		if s.cfg.Now().Sub(last) < s.cfg.MinRefreshInterval {
			return nil // too soon; let the lookup report what is cached
		}
	}
	return s.fetch(ctx)
}

// Refresh fetches the key set now, ignoring MinRefreshInterval. Call it at
// startup to fail early if the provider is unreachable. It does nothing for a Set
// made with FromJSON.
func (s *Set) Refresh(ctx context.Context) error {
	if s.cfg.URL == "" {
		return nil
	}
	s.refresh.Lock()
	defer s.refresh.Unlock()
	return s.fetch(ctx)
}

func (s *Set) fetch(ctx context.Context) error {
	s.mu.Lock()
	s.fetchedAt = s.cfg.Now()
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return errors.New("jwks: cannot build the request")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range s.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL, which the caller already knows
		}
		return fmt.Errorf("jwks: fetch failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: fetch failed: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.MaxSize+1))
	if err != nil {
		return fmt.Errorf("jwks: read failed: %w", err)
	}
	if int64(len(data)) > s.cfg.MaxSize {
		return fmt.Errorf("jwks: response is larger than %d bytes", s.cfg.MaxSize)
	}
	keys, err := Parse(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.keys = keys
	s.loadedAt = s.cfg.Now()
	s.mu.Unlock()
	return nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
	// Private parameters: their presence means the document holds a private key.
	D  string `json:"d"`
	P  string `json:"p"`
	Q  string `json:"q"`
	DP string `json:"dp"`
	DQ string `json:"dq"`
	QI string `json:"qi"`
	K  string `json:"k"`
}

// Parse reads a JWKS document and returns its public keys by key ID. Keys of an
// unsupported type or curve, and keys with use "enc", are skipped. A key without a
// kid, a repeated kid, a malformed key or any private key material is an error, as
// is a document with no usable key.
func Parse(data []byte) (map[string]crypto.PublicKey, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, errors.New("jwks: not a JSON key set")
	}
	if len(doc.Keys) > maxKeys {
		return nil, fmt.Errorf("jwks: more than %d keys", maxKeys)
	}
	out := map[string]crypto.PublicKey{}
	for i, raw := range doc.Keys {
		var k jwk
		if err := json.Unmarshal(raw, &k); err != nil {
			return nil, fmt.Errorf("jwks: key %d is not a JWK", i)
		}
		if k.D != "" || k.P != "" || k.Q != "" || k.DP != "" || k.DQ != "" || k.QI != "" || k.K != "" {
			return nil, fmt.Errorf("jwks: key %d contains private key material; publish public keys only", i)
		}
		if k.Use == "enc" {
			continue
		}
		pub, err := k.publicKey()
		if errors.Is(err, errSkip) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("jwks: key %d: %w", i, err)
		}
		if k.Kid == "" {
			return nil, fmt.Errorf("jwks: key %d has no kid", i)
		}
		if _, dup := out[k.Kid]; dup {
			return nil, fmt.Errorf("jwks: key %d repeats a kid", i)
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, errors.New("jwks: the key set has no usable key")
	}
	return out, nil
}

var errSkip = errors.New("unsupported key")

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err1 := b64Int(k.N)
		e, err2 := b64Int(k.E)
		if err1 != nil || err2 != nil || n.Sign() <= 0 || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
			return nil, errors.New("invalid RSA parameters")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		var ec ecdh.Curve
		switch k.Crv {
		case "P-256":
			curve, ec = elliptic.P256(), ecdh.P256()
		case "P-384":
			curve, ec = elliptic.P384(), ecdh.P384()
		case "P-521":
			curve, ec = elliptic.P521(), ecdh.P521()
		default:
			return nil, errSkip
		}
		x, y, err := coordinates(k, (curve.Params().BitSize+7)/8)
		if err != nil {
			return nil, err
		}
		point := append([]byte{4}, append(append([]byte{}, x...), y...)...)
		if _, err := ec.NewPublicKey(point); err != nil { // checks the point is on the curve
			return nil, errors.New("EC point is not on the curve")
		}
		return &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
	case "OKP":
		if k.Crv != "Ed25519" {
			return nil, errSkip
		}
		x, err := base64.RawURLEncoding.Strict().DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("invalid Ed25519 key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, errSkip
}

func coordinates(k jwk, size int) (x, y []byte, err error) {
	x, err1 := base64.RawURLEncoding.Strict().DecodeString(k.X)
	y, err2 := base64.RawURLEncoding.Strict().DecodeString(k.Y)
	if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
		return nil, nil, errors.New("invalid EC coordinates")
	}
	return x, y, nil
}

func b64Int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad base64")
	}
	return new(big.Int).SetBytes(b), nil
}
