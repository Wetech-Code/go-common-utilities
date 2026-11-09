// Package reqctx carries per-request facts that code far from the HTTP handler
// needs: a request ID for correlating logs across services, and a small set of
// header values that a trusted layer in front of the service chose to pass along
// (for example a tenant or user ID that an API gateway authenticated).
//
// A request ID is only for tracing and logging; it never grants anything. Captured
// header values are only as trustworthy as whatever put them there: see the warning
// on WithHeaders and package middleware's CaptureHeaders.
package reqctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/textproto"
	"regexp"
)

// Header names for the two conventions every service shares.
const (
	HeaderRequestID      = "X-Request-ID"
	HeaderIdempotencyKey = "X-Idempotency-Key"
)

var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// NewRequestID returns a new random request ID: 32 lower-case hex characters.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("reqctx: the system random source failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// ValidID reports whether s is safe to use as a request ID or a captured header
// value: 1 to 128 characters of letters, digits and . _ : -, starting with a letter
// or digit. Anything else (spaces, control characters, newlines, quotes) could forge
// log lines or smuggle headers.
func ValidID(s string) bool { return idRE.MatchString(s) }

type ctxKey int

const (
	requestIDKey ctxKey = iota
	headersKey
)

// WithRequestID returns ctx carrying id. An invalid id is replaced by a new one,
// so a context never holds a value that is unsafe to log or forward.
func WithRequestID(ctx context.Context, id string) context.Context {
	if !ValidID(id) {
		id = NewRequestID()
	}
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID in ctx, or "" if there is none.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// WithHeaders returns ctx carrying the given header values, keyed by canonical header
// name, added to any already there. Values that are not ValidID are dropped.
//
// Warning: a value read back with HeaderValue proves nothing by itself. It is
// trustworthy only if the component that stored it authenticated the caller and
// replaced anything the client sent. Never store values taken from untrusted input
// without that guarantee.
func WithHeaders(ctx context.Context, values map[string]string) context.Context {
	merged := map[string]string{}
	for k, v := range headersFrom(ctx) {
		merged[k] = v
	}
	for k, v := range values {
		if ValidID(v) {
			merged[textproto.CanonicalMIMEHeaderKey(k)] = v
		}
	}
	return context.WithValue(ctx, headersKey, merged)
}

func headersFrom(ctx context.Context) map[string]string {
	m, _ := ctx.Value(headersKey).(map[string]string)
	return m
}

// HeaderValue returns the captured value for the header name, or "" if there is none.
func HeaderValue(ctx context.Context, name string) string {
	return headersFrom(ctx)[textproto.CanonicalMIMEHeaderKey(name)]
}

// Attrs returns the request_id attribute for ctx, for logger.Config.ContextAttrs
// (combine it with other functions using logger.JoinContextAttrs).
func Attrs(ctx context.Context) []slog.Attr {
	if id := RequestID(ctx); id != "" {
		return []slog.Attr{slog.String("request_id", id)}
	}
	return nil
}
