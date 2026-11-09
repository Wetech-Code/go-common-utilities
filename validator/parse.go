package validator

import (
	"encoding"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	durationType = reflect.TypeOf(time.Duration(0))
	timeType     = reflect.TypeOf(time.Time{})
	textType     = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// supportedScalar reports whether a request string can be converted to t.
func supportedScalar(t reflect.Type) bool {
	if t == timeType || t == durationType {
		return true
	}
	if reflect.PointerTo(t).Implements(textType) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// parseScalar converts s to a new value of type t. The error text names only the
// expected form, never s.
func parseScalar(t reflect.Type, s string) (reflect.Value, error) {
	v := reflect.New(t).Elem()
	switch {
	case t == timeType:
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return v, errors.New("must be an RFC 3339 timestamp such as 2026-11-09T10:00:00Z")
		}
		v.Set(reflect.ValueOf(tm))
		return v, nil
	case t == durationType:
		d, err := time.ParseDuration(s)
		if err != nil {
			return v, errors.New("must be a duration such as 30s")
		}
		v.SetInt(int64(d))
		return v, nil
	case reflect.PointerTo(t).Implements(textType):
		if err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
			return v, errors.New("has an invalid format")
		}
		return v, nil
	}
	switch t.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		switch strings.ToLower(s) {
		case "true":
			v.SetBool(true)
		case "false":
			v.SetBool(false)
		default:
			return v, errors.New(`must be "true" or "false"`)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, t.Bits())
		if err != nil || strings.HasPrefix(s, "+") {
			return v, errors.New("must be an integer in range")
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, t.Bits())
		if err != nil || strings.HasPrefix(s, "+") {
			return v, errors.New("must be a non-negative integer in range")
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, t.Bits())
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || strings.ContainsAny(s, "xX_pP") || strings.Trim(s, "0123456789.eE+-") != "" {
			return v, errors.New("must be a number")
		}
		v.SetFloat(f)
	default:
		return v, setup("unsupported type %s", t)
	}
	return v, nil
}

// cleanText reports whether s is valid UTF-8 without control characters. When
// multiline is true, tab, newline and carriage return are allowed.
func cleanText(s string, multiline bool) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			if multiline && (c == '\t' || c == '\n' || c == '\r') {
				continue
			}
			return false
		}
	}
	return true
}
