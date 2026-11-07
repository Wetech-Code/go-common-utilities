package config_test

import (
	"fmt"
	"time"

	"github.com/Wetech-Code/go-common-utilities/config"
)

func Example() {
	// config.New() reads the real environment; FromMap keeps this example self-contained.
	r := config.FromMap(map[string]string{
		"DATABASE_URL": "postgres://localhost/app",
		"PORT":         "9000",
		"TIMEOUT":      "5s",
	})

	dsn := r.Require("DATABASE_URL")
	port := r.Int("PORT", 8080)
	timeout := r.Duration("TIMEOUT", 10*time.Second)
	debug := r.Bool("DEBUG", false)

	if err := r.Err(); err != nil {
		fmt.Println("bad configuration:", err)
		return
	}
	fmt.Println(dsn, port, timeout, debug)
	// Output: postgres://localhost/app 9000 5s false
}

func ExampleReader_Err() {
	r := config.FromMap(map[string]string{"PORT": "abc"})
	r.Require("DATABASE_URL")
	r.Int("PORT", 8080)
	fmt.Println(r.Err())
	// Output:
	// missing required setting: DATABASE_URL
	// invalid setting: PORT must be an integer
}
