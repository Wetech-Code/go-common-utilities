package jwks_test

import (
	"context"
	"fmt"

	"github.com/Wetech-Code/go-common-utilities/jwt"
	"github.com/Wetech-Code/go-common-utilities/jwt/jwks"
)

// Verify tokens issued by an identity provider using its published key set.
func Example() {
	set, err := jwks.New(jwks.Config{URL: "https://idp.example.com/.well-known/jwks.json"})
	if err != nil {
		fmt.Println(err)
		return
	}
	c, err := jwt.New(jwt.Config{
		Algorithm: "RS256",
		KeySource: set,
		Issuer:    "https://idp.example.com/",
		Audience:  []string{"my-api"},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	var bearerToken string // from the Authorization header
	claims, err := c.VerifyContext(context.Background(), bearerToken)
	if err != nil {
		fmt.Println("rejected:", err) // errors.Is(err, jwt.ErrExpired) etc.
		return
	}
	fmt.Println("hello", claims.Subject)
}
