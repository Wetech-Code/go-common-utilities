// Package config reads application settings from environment variables.
//
// A Reader collects problems instead of failing on the first one, so a service
// can read all of its settings and report every mistake at once:
//
//	r := config.New()
//	dsn := r.Require("DATABASE_URL")
//	port := r.Int("PORT", 8080)
//	if err := r.Err(); err != nil {
//		log.Fatal(err)
//	}
//
// An empty variable counts as unset. Error messages name the variable but never
// include its value, because the value may be a secret.
//
// Besides String, Int, Bool, Duration and Strings there are readers for other
// types (Int64, Uint, Float64, Bytes, URL, Map, Enum, IntRange, DurationRange),
// Has, and Invalid for your own checks. Optional behavior is switched on per
// Reader: WithPrefix reads APP_PORT as "PORT", and WithFileSecrets reads
// PASSWORD_FILE when PASSWORD is unset (Docker and Kubernetes secrets).
// FromFile reads a .env file beneath the real environment, and Load fills a
// whole struct from `env` tags.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrMissing is wrapped by errors for required variables that are unset or empty.
	ErrMissing = errors.New("missing required setting")
	// ErrInvalid is wrapped by errors for variables that cannot be parsed.
	ErrInvalid = errors.New("invalid setting")
)

// Reader reads settings and records any errors. The zero value is not usable;
// create one with New or FromMap. A Reader is not safe for concurrent use.
type Reader struct {
	lookup func(string) (string, bool)
	errs   []error
	prefix string
	files  bool
}

// New returns a Reader backed by the process environment.
func New() *Reader { return &Reader{lookup: os.LookupEnv} }

// FromMap returns a Reader backed by m. It is intended for tests and for
// settings that come from somewhere other than the environment.
func FromMap(m map[string]string) *Reader {
	return &Reader{lookup: func(k string) (string, bool) { v, ok := m[k]; return v, ok }}
}

// Err returns all errors recorded so far, joined, or nil if there were none.
func (r *Reader) Err() error { return errors.Join(r.errs...) }

// WithPrefix makes every read look up prefix+key, so with "APP_" the call
// String("PORT", "") reads APP_PORT. Errors name the full variable. It returns r
// so it can be chained: config.New().WithPrefix("APP_").
func (r *Reader) WithPrefix(prefix string) *Reader {
	r.prefix = prefix
	return r
}

// WithFileSecrets makes a setting that is unset fall back to the file named by
// the variable KEY_FILE, the convention of Docker and Kubernetes secrets: with
// DB_PASSWORD_FILE=/run/secrets/db, reading "DB_PASSWORD" returns the file's
// content with surrounding whitespace removed. A set variable always wins over its
// _FILE variant. Files larger than 1 MiB are refused. Errors name the variable but
// never include the file's content. It returns r so it can be chained.
func (r *Reader) WithFileSecrets() *Reader {
	r.files = true
	return r
}

// Has reports whether key is set to a non-empty value, without recording an error.
func (r *Reader) Has(key string) bool {
	_, ok := r.get(key)
	return ok
}

// Invalid records ErrInvalid for key, as "KEY must be <want>", for checks the
// Reader cannot know about, such as one setting that only makes sense with another:
//
//	if r.Bool("TLS", false) && !r.Has("TLS_CERT") {
//		r.Invalid("TLS_CERT", "set when TLS is enabled")
//	}
func (r *Reader) Invalid(key, want string) { r.invalid(r.prefix+key, want) }

const maxSecretFile = 1 << 20

func (r *Reader) get(key string) (string, bool) {
	name := r.prefix + key
	v, ok := r.lookup(name)
	if v = strings.TrimSpace(v); ok && v != "" {
		return v, true
	}
	if !r.files {
		return "", false
	}
	path, ok := r.lookup(name + "_FILE")
	if path = strings.TrimSpace(path); !ok || path == "" {
		return "", false
	}
	content, err := readSecretFile(path)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%w: %s_FILE could not be read as a file of at most 1 MiB", ErrInvalid, name))
		return "", false
	}
	v = strings.TrimSpace(content)
	return v, v != ""
}

func readSecretFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxSecretFile {
		return "", errors.New("too large")
	}
	return string(b), nil
}

func (r *Reader) invalid(name, want string) {
	r.errs = append(r.errs, fmt.Errorf("%w: %s must be %s", ErrInvalid, name, want))
}

// badValue records ErrInvalid for a value that failed to parse.
func (r *Reader) badValue(key, want string) { r.invalid(r.prefix+key, want) }

// Require returns the value of key and records ErrMissing if it is unset or empty.
func (r *Reader) Require(key string) string {
	v, ok := r.get(key)
	if !ok {
		r.errs = append(r.errs, fmt.Errorf("%w: %s", ErrMissing, r.prefix+key))
	}
	return v
}

// String returns the value of key, or def if it is unset or empty.
func (r *Reader) String(key, def string) string {
	if v, ok := r.get(key); ok {
		return v
	}
	return def
}

// Int returns key parsed as an integer, or def if it is unset or empty.
// An unparsable value records ErrInvalid and returns def.
func (r *Reader) Int(key string, def int) int {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.badValue(key, "an integer")
		return def
	}
	return n
}

// Bool returns key parsed with strconv.ParseBool ("1", "t", "true", "0", "f",
// "false", ...), or def if it is unset or empty. An unparsable value records
// ErrInvalid and returns def.
func (r *Reader) Bool(key string, def bool) bool {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.badValue(key, "a boolean")
		return def
	}
	return b
}

// Duration returns key parsed with time.ParseDuration ("500ms", "30s", "2h"),
// or def if it is unset or empty. An unparsable value records ErrInvalid and
// returns def.
func (r *Reader) Duration(key string, def time.Duration) time.Duration {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.badValue(key, "a duration such as 30s")
		return def
	}
	return d
}

// Strings returns key split on commas with surrounding spaces trimmed and empty
// items dropped, or def if it is unset or empty.
func (r *Reader) Strings(key string, def []string) []string {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
