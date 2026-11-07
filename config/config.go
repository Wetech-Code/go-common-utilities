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
package config

import (
	"errors"
	"fmt"
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

func (r *Reader) get(key string) (string, bool) {
	v, ok := r.lookup(key)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (r *Reader) invalid(key, want string) {
	r.errs = append(r.errs, fmt.Errorf("%w: %s must be %s", ErrInvalid, key, want))
}

// Require returns the value of key and records ErrMissing if it is unset or empty.
func (r *Reader) Require(key string) string {
	v, ok := r.get(key)
	if !ok {
		r.errs = append(r.errs, fmt.Errorf("%w: %s", ErrMissing, key))
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
		r.invalid(key, "an integer")
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
		r.invalid(key, "a boolean")
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
		r.invalid(key, "a duration such as 30s")
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
