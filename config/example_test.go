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

// Fill a whole struct from tags; every problem is reported at once.
func ExampleReader_Load() {
	type Settings struct {
		Port     int           `env:"PORT" default:"8080" min:"1" max:"65535"`
		DSN      string        `env:"DATABASE_URL" required:"true"`
		LogLevel string        `env:"LOG_LEVEL" default:"info" enum:"debug,info,warn,error"`
		Timeout  time.Duration `env:"TIMEOUT" default:"30s"`
	}
	r := config.FromMap(map[string]string{"DATABASE_URL": "postgres://localhost/app", "PORT": "9000"})
	var s Settings
	r.Load(&s)
	if err := r.Err(); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(s.Port, s.DSN, s.LogLevel, s.Timeout)
	// Output: 9000 postgres://localhost/app info 30s
}

// Prefix every variable and read Docker/Kubernetes secrets from files.
func ExampleReader_WithPrefix() {
	r := config.FromMap(map[string]string{"APP_PORT": "9000"}).WithPrefix("APP_")
	fmt.Println(r.IntRange("PORT", 8080, 1, 65535))
	// Output: 9000
}

func ExampleReader_Bytes() {
	r := config.FromMap(map[string]string{"MAX_UPLOAD": "10MiB", "CACHE": "1.5GB"})
	fmt.Println(r.Bytes("MAX_UPLOAD", 0), r.Bytes("CACHE", 0))
	// Output: 10485760 1500000000
}

func ExampleReader_Enum() {
	r := config.FromMap(map[string]string{"LOG_FORMAT": "yaml"})
	r.Enum("LOG_FORMAT", "json", "json", "text")
	fmt.Println(r.Err())
	// Output: invalid setting: LOG_FORMAT must be one of json, text
}

// FromFile reads a .env file for local development; real environment variables win.
func ExampleFromFile() {
	r, err := config.FromFile(".env") // KEY=VALUE lines, # comments, quotes
	if err != nil {
		fmt.Println("no .env file:", err)
		return
	}
	fmt.Println(r.Int("PORT", 8080))
}
