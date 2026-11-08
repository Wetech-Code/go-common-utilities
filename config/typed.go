package config

import (
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Int64 returns key parsed as a 64-bit integer, or def if it is unset or empty.
// An unparsable value records ErrInvalid and returns def.
func (r *Reader) Int64(key string, def int64) int64 {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		r.badValue(key, "an integer")
		return def
	}
	return n
}

// Uint returns key parsed as a non-negative integer, or def if it is unset or
// empty. An unparsable or negative value records ErrInvalid and returns def.
func (r *Reader) Uint(key string, def uint) uint {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseUint(v, 10, strconv.IntSize)
	if err != nil {
		r.badValue(key, "a non-negative integer")
		return def
	}
	return uint(n)
}

// Float64 returns key parsed as a floating-point number, or def if it is unset
// or empty. An unparsable value, NaN or infinity records ErrInvalid and returns def.
func (r *Reader) Float64(key string, def float64) float64 {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		r.badValue(key, "a number")
		return def
	}
	return f
}

// IntRange is Int that also requires the value to be from min to max inclusive.
// A value outside the range records ErrInvalid and returns def.
//
//	port := r.IntRange("PORT", 8080, 1, 65535)
func (r *Reader) IntRange(key string, def, min, max int) int {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.badValue(key, "an integer")
		return def
	}
	if n < min || n > max {
		r.badValue(key, "an integer from "+strconv.Itoa(min)+" to "+strconv.Itoa(max))
		return def
	}
	return n
}

// DurationRange is Duration that also requires the value to be from min to max
// inclusive. A value outside the range records ErrInvalid and returns def.
func (r *Reader) DurationRange(key string, def, min, max time.Duration) time.Duration {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.badValue(key, "a duration such as 30s")
		return def
	}
	if d < min || d > max {
		r.badValue(key, "a duration from "+min.String()+" to "+max.String())
		return def
	}
	return d
}

// Bytes returns key parsed as a size in bytes, or def if it is unset or empty.
// Accepted forms are a plain number of bytes ("1048576"), a decimal unit
// ("10MB", "1.5GB": KB, MB, GB and TB are powers of 1000) or a binary unit
// ("512KiB", "2GiB": KiB, MiB, GiB and TiB are powers of 1024), in any letter
// case. Fractions are allowed with units. An unparsable, negative or oversized
// value records ErrInvalid and returns def.
func (r *Reader) Bytes(key string, def int64) int64 {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	n, ok := parseBytes(v)
	if !ok {
		r.badValue(key, "a size such as 512KiB, 10MB or 1048576")
		return def
	}
	return n
}

var byteUnits = map[string]float64{
	"": 1, "B": 1,
	"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
	"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
}

func parseBytes(s string) (int64, bool) {
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.ToUpper(strings.TrimSpace(s[i:]))
	mult, ok := byteUnits[unit]
	if !ok || num == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if (unit == "" || unit == "B") && f != math.Trunc(f) {
		return 0, false // a fraction of a byte
	}
	total := f * mult
	if total >= math.MaxInt64 { // 2^63 as a float, so this also guards the conversion
		return 0, false
	}
	return int64(total), true
}

// URL returns key as a URL string after checking that it is absolute, has a host,
// and uses one of the given schemes (http and https if none are given), or def
// if it is unset or empty. An invalid value records ErrInvalid and returns def.
// def is returned as is and not checked. The error never includes the URL, which
// may hold credentials.
//
//	dsn := r.URL("DATABASE_URL", "", "postgres", "postgresql")
func (r *Reader) URL(key, def string, schemes ...string) string {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	if len(schemes) == 0 {
		schemes = []string{"http", "https"}
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || !contains(schemes, strings.ToLower(u.Scheme)) {
		r.badValue(key, "an absolute URL with scheme "+strings.Join(schemes, " or "))
		return def
	}
	return v
}

// Map returns key parsed as comma-separated key=value pairs ("env=prod, team=ops"),
// with surrounding spaces trimmed, or def if it is unset or empty. A pair without
// "=" or with an empty key records ErrInvalid and returns def. A repeated key
// takes its last value.
func (r *Reader) Map(key string, def map[string]string) map[string]string {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	m, ok := parseMap(v)
	if !ok {
		r.badValue(key, "comma-separated key=value pairs")
		return def
	}
	return m
}

func parseMap(s string) (map[string]string, bool) {
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		k, v, found := strings.Cut(pair, "=")
		if k = strings.TrimSpace(k); !found || k == "" {
			return nil, false
		}
		m[k] = strings.TrimSpace(v)
	}
	return m, true
}

// Enum returns key if it is one of allowed (compared exactly after trimming), or
// def if it is unset or empty. Any other value records ErrInvalid, listing the
// allowed values but not the rejected one, and returns def. def is returned as is
// and not checked.
//
//	format := r.Enum("LOG_FORMAT", "json", "json", "text")
func (r *Reader) Enum(key, def string, allowed ...string) string {
	v, ok := r.get(key)
	if !ok {
		return def
	}
	if !contains(allowed, v) {
		r.badValue(key, "one of "+strings.Join(allowed, ", "))
		return def
	}
	return v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
