// Package jwt signs and verifies JSON Web Tokens (RFC 7519) with safe defaults.
// It is a thin, opinionated layer over github.com/golang-jwt/jwt/v5; it does not
// implement the cryptography itself.
//
//	c, err := jwt.New(jwt.Config{
//		Algorithm: "HS256",
//		Key:       jwt.Key{Secret: secret}, // at least 32 random bytes
//		Issuer:    "api.example.com",
//		Audience:  []string{"web"},
//		TTL:       15 * time.Minute,
//	})
//	token, err := c.Sign(jwt.Claims{Subject: "user-42", Extra: map[string]any{"role": "admin"}})
//	claims, err := c.Verify(token) // errors.Is(err, jwt.ErrExpired) etc.
//
// Safety rules built in:
//
//   - The algorithm comes from your Config, never from the token. A token whose
//     header says another algorithm (including "none") is rejected, which closes
//     the classic algorithm-confusion attacks.
//   - Tokens must carry an expiry (exp), and Sign always sets one. Issuer and
//     audience are checked when you configure them. A token issued in the future
//     is rejected.
//   - Keys are checked up front: HMAC secrets must be at least as long as the hash
//     output, RSA keys at least 2048 bits, and ECDSA keys must match their curve.
//   - Keys can be rotated: tokens carry a key ID (kid), and OldKeys still verify
//     tokens signed before a rotation.
//   - Errors name a category and never include the token, keys or claim values.
//   - Verify refuses tokens over MaxTokenSize before parsing them.
//
// A signed token is not encrypted: anyone can read its claims, so do not put
// secrets in them. Verifying a token proves who issued it and that it is
// unchanged; deciding what its holder may do is the caller's job.
package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	gjwt "github.com/golang-jwt/jwt/v5"
)

var (
	// ErrInvalid is wrapped by every verification failure that is not an expiry:
	// a malformed token, a bad signature, an unexpected algorithm, an unknown key,
	// or a wrong issuer or audience.
	ErrInvalid = errors.New("jwt: invalid token")
	// ErrExpired is wrapped when the token's exp time has passed.
	ErrExpired = errors.New("jwt: token has expired")
	// ErrNotYetValid is wrapped when the token's nbf time or its iat time is still
	// in the future.
	ErrNotYetValid = errors.New("jwt: token is not valid yet")
	// ErrNoSigningKey is returned by Sign when the Codec was built with verification
	// keys only.
	ErrNoSigningKey = errors.New("jwt: no signing key configured")
	// ErrConfig is wrapped by errors for unusable configuration or claims.
	ErrConfig = errors.New("jwt: invalid configuration")
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultMaxTokenSize = 8 << 10
)

// Key is one key. Set what the algorithm needs:
//
//   - HS256, HS384, HS512: Secret (used to sign and to verify).
//   - RS*, PS*, ES*, EdDSA: Private (*rsa.PrivateKey, *ecdsa.PrivateKey or
//     ed25519.PrivateKey) to sign and verify, or only Public (*rsa.PublicKey,
//     *ecdsa.PublicKey or ed25519.PublicKey) to verify.
//
// ID is written as the token's kid header and used to pick the key when
// verifying. It is optional for a single key and needed for rotation.
type Key struct {
	ID      string
	Secret  []byte
	Private crypto.PrivateKey
	Public  crypto.PublicKey
}

// KeySource supplies verification keys by key ID, for keys that live outside
// your configuration, such as an identity provider's published key set (see
// package jwt/jwks). kid is the token's kid header, or "" if it has none.
// Implementations must be safe for concurrent use, and should return an error
// rather than guess when they cannot find a key.
type KeySource interface {
	PublicKey(ctx context.Context, kid string) (crypto.PublicKey, error)
}

// Config describes how tokens are signed and verified.
type Config struct {
	// Algorithm is required: HS256, HS384, HS512, RS256, RS384, RS512, PS256, PS384,
	// PS512, ES256, ES384, ES512 or EdDSA. Only this algorithm is accepted.
	Algorithm string
	// Key signs new tokens (when it has signing material) and verifies tokens.
	Key Key
	// OldKeys only verify, so tokens signed with a previous key stay valid during
	// a rotation. Each needs an ID, different from the others and from Key.ID, and
	// the token's kid header selects it.
	OldKeys []Key
	// KeySource, if set, replaces Key and OldKeys: the Codec only verifies, with
	// keys looked up by the token's kid. The algorithm is still fixed by Algorithm,
	// and every key the source returns is checked against it like a configured key
	// (type, RSA size, curve).
	KeySource KeySource
	// Issuer is written as iss when signing and required when verifying.
	// Audience is written as aud when signing (unless the Claims set their own)
	// and, when verifying, at least one entry must appear in the token's aud.
	// Both are optional but strongly recommended.
	Issuer   string
	Audience []string
	// TTL is how long tokens made by Sign live. Required to call Sign (a Codec
	// that only verifies, or signs with SignWithTTL, can leave it zero).
	TTL time.Duration
	// Leeway tolerates clock differences between services when checking exp, nbf
	// and iat. Default zero; 30 seconds is common.
	Leeway time.Duration
	// MaxTokenSize is the longest token Verify will parse. Default DefaultMaxTokenSize.
	MaxTokenSize int
	// Now returns the current time. Default time.Now; set it in tests.
	Now func() time.Time
}

// Claims are the claims of a token. Sign reads Subject, Audience, ID, NotBefore
// and Extra; Verify fills in every field.
type Claims struct {
	Subject string
	// Audience overrides Config.Audience when signing.
	Audience []string
	ID       string
	// NotBefore, if set when signing, makes the token invalid before that time.
	NotBefore time.Time
	// Extra holds custom claims. Numbers read back as float64 and objects as
	// map[string]any (standard encoding/json behavior). Reserved names (iss, sub,
	// aud, exp, nbf, iat, jti) are not allowed here.
	Extra map[string]any

	// Filled in by Verify.
	Issuer    string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

var reserved = []string{"iss", "sub", "aud", "exp", "nbf", "iat", "jti"}

var methods = map[string]gjwt.SigningMethod{
	"HS256": gjwt.SigningMethodHS256, "HS384": gjwt.SigningMethodHS384, "HS512": gjwt.SigningMethodHS512,
	"RS256": gjwt.SigningMethodRS256, "RS384": gjwt.SigningMethodRS384, "RS512": gjwt.SigningMethodRS512,
	"PS256": gjwt.SigningMethodPS256, "PS384": gjwt.SigningMethodPS384, "PS512": gjwt.SigningMethodPS512,
	"ES256": gjwt.SigningMethodES256, "ES384": gjwt.SigningMethodES384, "ES512": gjwt.SigningMethodES512,
	"EdDSA": gjwt.SigningMethodEdDSA,
}

// Codec signs and verifies tokens. It is safe for concurrent use.
type Codec struct {
	source   KeySource
	cfg      Config
	method   gjwt.SigningMethod
	signKey  any // nil when verify-only
	signKID  string
	verify   map[string]any // by kid
	fallback any            // used when a token has no kid
}

// Validate reports problems with the configuration. Errors never include key
// material.
func (c Config) Validate() error {
	_, err := c.build()
	return err
}

// New validates cfg and returns a Codec.
func New(cfg Config) (*Codec, error) { return cfg.build() }

func (cfg Config) build() (*Codec, error) {
	method, ok := methods[cfg.Algorithm]
	if !ok {
		return nil, fmt.Errorf("%w: Algorithm must be one of %s", ErrConfig, strings.Join(sortedAlgorithms(), ", "))
	}
	switch {
	case cfg.TTL < 0 || cfg.Leeway < 0 || cfg.MaxTokenSize < 0:
		return nil, fmt.Errorf("%w: TTL, Leeway and MaxTokenSize must not be negative", ErrConfig)
	}
	if cfg.MaxTokenSize == 0 {
		cfg.MaxTokenSize = DefaultMaxTokenSize
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Audience = slices.Clone(cfg.Audience)
	for _, a := range cfg.Audience {
		if a == "" {
			return nil, fmt.Errorf("%w: Audience entries must not be empty", ErrConfig)
		}
	}

	c := &Codec{cfg: cfg, method: method, verify: map[string]any{}}
	if cfg.KeySource != nil {
		if cfg.Key.ID != "" || len(cfg.Key.Secret) > 0 || cfg.Key.Private != nil || cfg.Key.Public != nil || len(cfg.OldKeys) > 0 {
			return nil, fmt.Errorf("%w: KeySource replaces Key and OldKeys; set only one of them", ErrConfig)
		}
		if strings.HasPrefix(cfg.Algorithm, "HS") {
			return nil, fmt.Errorf("%w: KeySource supplies public keys and cannot be used with HMAC algorithms", ErrConfig)
		}
		c.source = cfg.KeySource
		return c, nil
	}
	primary, signKey, err := keyFor(cfg.Algorithm, cfg.Key)
	if err != nil {
		return nil, fmt.Errorf("%w: Key: %s", ErrConfig, err)
	}
	c.signKey = signKey
	c.signKID = cfg.Key.ID
	c.verify[cfg.Key.ID] = primary
	c.fallback = primary
	for i, k := range cfg.OldKeys {
		if k.ID == "" {
			return nil, fmt.Errorf("%w: OldKeys[%d] needs an ID", ErrConfig, i)
		}
		if _, dup := c.verify[k.ID]; dup {
			return nil, fmt.Errorf("%w: OldKeys[%d] repeats the key ID %q", ErrConfig, i, k.ID)
		}
		pub, _, err := keyFor(cfg.Algorithm, k)
		if err != nil {
			return nil, fmt.Errorf("%w: OldKeys[%d]: %s", ErrConfig, i, err)
		}
		c.verify[k.ID] = pub
	}
	return c, nil
}

func sortedAlgorithms() []string {
	out := make([]string, 0, len(methods))
	for a := range methods {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// keyFor checks k against alg and returns the key to verify with and, if k can
// sign, the key to sign with.
func keyFor(alg string, k Key) (verify, sign any, err error) {
	switch {
	case strings.HasPrefix(alg, "HS"):
		if k.Private != nil || k.Public != nil {
			return nil, nil, errors.New("HMAC algorithms use Secret only")
		}
		min := map[string]int{"HS256": 32, "HS384": 48, "HS512": 64}[alg]
		if len(k.Secret) < min {
			return nil, nil, fmt.Errorf("Secret must be at least %d bytes for %s", min, alg)
		}
		secret := slices.Clone(k.Secret)
		return secret, secret, nil
	case alg == "EdDSA":
		return asymmetric(alg, k, ed25519Key)
	case strings.HasPrefix(alg, "ES"):
		return asymmetric(alg, k, ecdsaKey)
	default: // RS*, PS*
		return asymmetric(alg, k, rsaKey)
	}
}

type checker func(alg string, private crypto.PrivateKey, public crypto.PublicKey) (verify, sign any, err error)

func asymmetric(alg string, k Key, check checker) (any, any, error) {
	if len(k.Secret) > 0 {
		return nil, nil, errors.New("this algorithm uses Private or Public, not Secret")
	}
	if k.Private == nil && k.Public == nil {
		return nil, nil, errors.New("Private or Public is required")
	}
	return check(alg, k.Private, k.Public)
}

func rsaKey(alg string, private crypto.PrivateKey, public crypto.PublicKey) (any, any, error) {
	var pub *rsa.PublicKey
	var priv *rsa.PrivateKey
	if private != nil {
		p, ok := private.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("%s needs an *rsa.PrivateKey", alg)
		}
		priv, pub = p, &p.PublicKey
	}
	if public != nil {
		p, ok := public.(*rsa.PublicKey)
		if !ok {
			return nil, nil, fmt.Errorf("%s needs an *rsa.PublicKey", alg)
		}
		if pub != nil && !pub.Equal(p) {
			return nil, nil, errors.New("Public does not match Private")
		}
		pub = p
	}
	if pub.N.BitLen() < 2048 {
		return nil, nil, errors.New("RSA keys must be at least 2048 bits")
	}
	if priv != nil {
		return pub, priv, nil
	}
	return pub, nil, nil
}

func ecdsaKey(alg string, private crypto.PrivateKey, public crypto.PublicKey) (any, any, error) {
	curve := map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()}[alg]
	var pub *ecdsa.PublicKey
	var priv *ecdsa.PrivateKey
	if private != nil {
		p, ok := private.(*ecdsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("%s needs an *ecdsa.PrivateKey", alg)
		}
		priv, pub = p, &p.PublicKey
	}
	if public != nil {
		p, ok := public.(*ecdsa.PublicKey)
		if !ok {
			return nil, nil, fmt.Errorf("%s needs an *ecdsa.PublicKey", alg)
		}
		if pub != nil && !pub.Equal(p) {
			return nil, nil, errors.New("Public does not match Private")
		}
		pub = p
	}
	if pub.Curve != curve {
		return nil, nil, fmt.Errorf("%s needs a %s key", alg, curve.Params().Name)
	}
	if priv != nil {
		return pub, priv, nil
	}
	return pub, nil, nil
}

func ed25519Key(alg string, private crypto.PrivateKey, public crypto.PublicKey) (any, any, error) {
	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	if private != nil {
		p, ok := private.(ed25519.PrivateKey)
		if !ok || len(p) != ed25519.PrivateKeySize {
			return nil, nil, errors.New("EdDSA needs an ed25519.PrivateKey")
		}
		priv = p
		pub = p.Public().(ed25519.PublicKey)
	}
	if public != nil {
		p, ok := public.(ed25519.PublicKey)
		if !ok || len(p) != ed25519.PublicKeySize {
			return nil, nil, errors.New("EdDSA needs an ed25519.PublicKey")
		}
		if pub != nil && !pub.Equal(p) {
			return nil, nil, errors.New("Public does not match Private")
		}
		pub = p
	}
	if priv != nil {
		return pub, priv, nil
	}
	return pub, nil, nil
}

// Sign returns a signed token for c using Config.TTL, which must be set. It
// returns ErrNoSigningKey on a Codec that only verifies.
func (cd *Codec) Sign(c Claims) (string, error) { return cd.SignWithTTL(c, cd.cfg.TTL) }

// SignWithTTL is Sign with a lifetime other than Config.TTL, for example a
// short-lived token for a single action. ttl must be positive.
func (cd *Codec) SignWithTTL(c Claims, ttl time.Duration) (string, error) {
	if cd.signKey == nil {
		return "", ErrNoSigningKey
	}
	if ttl <= 0 {
		return "", fmt.Errorf("%w: the token lifetime must be positive (set Config.TTL or use SignWithTTL)", ErrConfig)
	}
	for _, name := range reserved {
		if _, clash := c.Extra[name]; clash {
			return "", fmt.Errorf("%w: Extra must not contain the reserved claim %q", ErrConfig, name)
		}
	}
	now := cd.cfg.Now()
	m := gjwt.MapClaims{}
	for k, v := range c.Extra {
		m[k] = v
	}
	if cd.cfg.Issuer != "" {
		m["iss"] = cd.cfg.Issuer
	}
	if c.Subject != "" {
		m["sub"] = c.Subject
	}
	aud := c.Audience
	if len(aud) == 0 {
		aud = cd.cfg.Audience
	}
	if len(aud) > 0 {
		m["aud"] = slices.Clone(aud)
	}
	if c.ID != "" {
		m["jti"] = c.ID
	}
	m["iat"] = gjwt.NewNumericDate(now)
	m["exp"] = gjwt.NewNumericDate(now.Add(ttl))
	if !c.NotBefore.IsZero() {
		m["nbf"] = gjwt.NewNumericDate(c.NotBefore)
	}

	t := gjwt.NewWithClaims(cd.method, m)
	if cd.signKID != "" {
		t.Header["kid"] = cd.signKID
	}
	s, err := t.SignedString(cd.signKey)
	if err != nil {
		// Do not wrap err: library messages can describe the key.
		return "", errors.New("jwt: signing failed (check that the claims are JSON-encodable)")
	}
	return s, nil
}

// Verify checks the token's signature, algorithm, expiry, not-before, issued-at,
// issuer and audience, and returns its claims. Failures wrap ErrExpired,
// ErrNotYetValid or ErrInvalid (check with errors.Is); the error never contains
// the token or any claim.
func (cd *Codec) Verify(token string) (*Claims, error) {
	return cd.VerifyContext(context.Background(), token)
}

// VerifyContext is Verify with a context, which bounds a lookup in a KeySource
// (for example a fetch of a remote key set).
func (cd *Codec) VerifyContext(ctx context.Context, token string) (*Claims, error) {
	if len(token) > cd.cfg.MaxTokenSize {
		return nil, fmt.Errorf("%w: token is larger than %d bytes", ErrInvalid, cd.cfg.MaxTokenSize)
	}
	opts := []gjwt.ParserOption{
		gjwt.WithValidMethods([]string{cd.cfg.Algorithm}),
		gjwt.WithExpirationRequired(),
		gjwt.WithIssuedAt(),
		gjwt.WithLeeway(cd.cfg.Leeway),
		gjwt.WithTimeFunc(cd.cfg.Now),
		// Refuse base64 with non-zero padding bits, so one token has one encoding.
		gjwt.WithStrictDecoding(),
	}
	if cd.cfg.Issuer != "" {
		opts = append(opts, gjwt.WithIssuer(cd.cfg.Issuer))
	}
	m := gjwt.MapClaims{}
	if _, err := gjwt.NewParser(opts...).ParseWithClaims(token, m, func(t *gjwt.Token) (any, error) { return cd.keyFunc(ctx, t) }); err != nil {
		return nil, classify(err)
	}
	if len(cd.cfg.Audience) > 0 {
		got, _ := m.GetAudience()
		if !slices.ContainsFunc(cd.cfg.Audience, func(a string) bool { return slices.Contains(got, a) }) {
			return nil, fmt.Errorf("%w: audience does not match", ErrInvalid)
		}
	}
	return claimsFrom(m), nil
}

func (cd *Codec) keyFunc(ctx context.Context, t *gjwt.Token) (any, error) {
	raw, has := t.Header["kid"]
	kid, isString := raw.(string)
	if has && !isString {
		return nil, errors.New("kid is not a string")
	}
	if cd.source != nil {
		pub, err := cd.source.PublicKey(ctx, kid)
		if err != nil {
			return nil, errors.New("no key for this token")
		}
		verify, _, err := keyFor(cd.cfg.Algorithm, Key{Public: pub})
		if err != nil {
			return nil, errors.New("key does not suit the algorithm")
		}
		return verify, nil
	}
	if !has {
		return cd.fallback, nil
	}
	key, found := cd.verify[kid]
	if !found {
		return nil, errors.New("unknown key id")
	}
	return key, nil
}

// classify turns a library error into one of ours, without echoing the library's
// message (which can include parts of the input).
func classify(err error) error {
	switch {
	case errors.Is(err, gjwt.ErrTokenExpired):
		return ErrExpired
	case errors.Is(err, gjwt.ErrTokenNotValidYet), errors.Is(err, gjwt.ErrTokenUsedBeforeIssued):
		return ErrNotYetValid
	}
	reason := "token rejected"
	switch {
	case errors.Is(err, gjwt.ErrTokenMalformed):
		reason = "malformed token"
	case errors.Is(err, gjwt.ErrSignatureInvalid):
		reason = "signature does not match"
	case errors.Is(err, gjwt.ErrTokenSignatureInvalid):
		reason = "signature does not match or algorithm not allowed"
	case errors.Is(err, gjwt.ErrTokenUnverifiable):
		reason = "no usable key for this token"
	case errors.Is(err, gjwt.ErrTokenInvalidIssuer):
		reason = "issuer does not match"
	case errors.Is(err, gjwt.ErrTokenRequiredClaimMissing):
		reason = "a required claim (exp) is missing"
	case errors.Is(err, gjwt.ErrTokenInvalidClaims):
		reason = "invalid claims"
	}
	return fmt.Errorf("%w: %s", ErrInvalid, reason)
}

func claimsFrom(m gjwt.MapClaims) *Claims {
	c := &Claims{Extra: map[string]any{}}
	c.Subject, _ = m.GetSubject()
	c.Issuer, _ = m.GetIssuer()
	c.Audience, _ = m.GetAudience()
	if id, ok := m["jti"].(string); ok {
		c.ID = id
	}
	if d, _ := m.GetIssuedAt(); d != nil {
		c.IssuedAt = d.Time
	}
	if d, _ := m.GetExpirationTime(); d != nil {
		c.ExpiresAt = d.Time
	}
	if d, _ := m.GetNotBefore(); d != nil {
		c.NotBefore = d.Time
	}
	for k, v := range m {
		if !slices.Contains(reserved, k) {
			c.Extra[k] = v
		}
	}
	return c
}

// Unverified is what Inspect returns.
type Unverified struct {
	Header map[string]any
	Claims map[string]any
}

// Inspect decodes a token WITHOUT checking its signature, expiry or anything else.
// Use it to read the kid or iss header before choosing a key, or to debug.
//
// Dangerous: the contents are whatever the sender wrote. Never make an
// authorization decision from them; use Codec.Verify.
func Inspect(token string) (*Unverified, error) {
	if len(token) > DefaultMaxTokenSize {
		return nil, fmt.Errorf("%w: token is larger than %d bytes", ErrInvalid, DefaultMaxTokenSize)
	}
	m := gjwt.MapClaims{}
	t, _, err := gjwt.NewParser().ParseUnverified(token, m)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed token", ErrInvalid)
	}
	return &Unverified{Header: t.Header, Claims: m}, nil
}
