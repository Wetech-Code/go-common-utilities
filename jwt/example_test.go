package jwt_test

import (
	"fmt"
	"time"

	"github.com/Wetech-Code/go-common-utilities/jwt"
)

// Sign and verify with a shared secret (HS256). The secret must be at least 32
// random bytes; load it from your configuration, never from source code.
func Example() {
	secret := []byte("0123456789abcdef0123456789abcdef") // example only
	c, err := jwt.New(jwt.Config{
		Algorithm: "HS256",
		Key:       jwt.Key{Secret: secret},
		Issuer:    "api.example.com",
		Audience:  []string{"web"},
		TTL:       15 * time.Minute,
		Leeway:    30 * time.Second,
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	token, err := c.Sign(jwt.Claims{Subject: "user-42", Extra: map[string]any{"role": "admin"}})
	if err != nil {
		fmt.Println(err)
		return
	}
	claims, err := c.Verify(token)
	if err != nil {
		fmt.Println(err) // errors.Is(err, jwt.ErrExpired), jwt.ErrNotYetValid, jwt.ErrInvalid
		return
	}
	fmt.Println(claims.Subject, claims.Issuer, claims.Extra["role"])
	// Output: user-42 api.example.com admin
}

// A service that only verifies tokens needs just the public key; the issuer keeps
// the private key. Old keys keep working during a rotation.
func ExampleKey() {
	pub, err := jwt.ParsePublicKeyPEM([]byte(`-----BEGIN PUBLIC KEY-----...`)) // from a file or secret store
	if err != nil {
		fmt.Println("no key in this example")
		return
	}
	_, _ = jwt.New(jwt.Config{
		Algorithm: "RS256",
		Key:       jwt.Key{ID: "2026-11", Public: pub},
		OldKeys:   []jwt.Key{{ID: "2026-05", Public: pub}},
		Issuer:    "auth.example.com",
	})
	// Output: no key in this example
}
