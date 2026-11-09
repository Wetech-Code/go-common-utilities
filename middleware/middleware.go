// Package middleware holds the small net/http middleware every service needs:
// request IDs, identity capture, panic recovery, access logging, body and time
// limits. Each is a func(http.Handler) http.Handler, so they work with net/http,
// chi and anything else that uses that shape, and they combine with Chain:
//
//	h := middleware.Chain(mux,
//		middleware.RequestID(),         // outermost: every later step and log line has the ID
//		middleware.AccessLog(log),
//		middleware.Recover(log),
//		middleware.CaptureHeaders("X-Tenant-ID"), // only behind a layer that authenticated the caller
//		middleware.MaxBody(1<<20),
//	)
//	handler := tel.HTTPHandler(h, "orders") // optional: spans and request metrics around it all
//
// Nothing here logs headers, query strings or bodies, which can hold secrets.
package middleware

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Wetech-Code/go-common-utilities/reqctx"
)

// Middleware wraps a handler.
type Middleware func(http.Handler) http.Handler

// Chain wraps h so that the first middleware is the outermost: it sees the
// request first and the response last.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] != nil {
			h = mws[i](h)
		}
	}
	return h
}

// writeJSONError sends a small, fixed JSON error. The message is a constant,
// never request data.
func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// ---- request ID ----

// RequestIDOption changes RequestID.
type RequestIDOption func(*requestIDConfig)

type requestIDConfig struct{ alwaysGenerate bool }

// AlwaysGenerate makes RequestID ignore any incoming X-Request-ID and create a
// new one. Use it at the edge (the first service a client reaches) where the header comes from
// untrusted clients.
func AlwaysGenerate() RequestIDOption { return func(c *requestIDConfig) { c.alwaysGenerate = true } }

// RequestID puts a request ID in the request context (reqctx.RequestID) and in the
// response's X-Request-ID header. An incoming X-Request-ID is reused only if it is
// a valid ID (see reqctx.ValidID) and appears once; anything else is replaced by a
// new random ID. The ID is for correlation only; it never grants access.
func RequestID(opts ...RequestIDOption) Middleware {
	var cfg requestIDConfig
	for _, o := range opts {
		o(&cfg)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := ""
			if !cfg.alwaysGenerate {
				if vals := r.Header.Values(reqctx.HeaderRequestID); len(vals) == 1 && reqctx.ValidID(vals[0]) {
					id = vals[0]
				}
			}
			if id == "" {
				id = reqctx.NewRequestID()
			}
			w.Header().Set(reqctx.HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(reqctx.WithRequestID(r.Context(), id)))
		})
	}
}

// ---- captured headers ----

// CaptureHeaders reads the named request headers into the request context
// (reqctx.HeaderValue) after checking their format, so code far from the handler can
// forward them. A named header that appears more than once, or whose value is not a
// valid ID (see reqctx.ValidID), is rejected with 400 before the handler runs, rather
// than choosing one. Headers that are absent are simply not captured.
//
//	middleware.CaptureHeaders("X-Tenant-ID", "X-User-ID")
//
// These values prove nothing unless the layer in front of the service authenticated the
// caller and replaced whatever the client sent. Use it only behind such a layer, and at
// that layer remove client-supplied values first with StripHeaders.
func CaptureHeaders(names ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured := map[string]string{}
			for _, name := range names {
				vals := r.Header.Values(name)
				switch {
				case len(vals) == 0:
				case len(vals) > 1 || !reqctx.ValidID(vals[0]):
					writeJSONError(w, http.StatusBadRequest, "invalid_header")
					return
				default:
					captured[name] = vals[0]
				}
			}
			if len(captured) > 0 {
				r = r.WithContext(reqctx.WithHeaders(r.Context(), captured))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// StripHeaders removes the named request headers before the handler runs. Use it
// at the edge to drop headers a client may have sent that only a trusted layer should
// set, before that layer sets its own authenticated values.
func StripHeaders(names ...string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, n := range names {
				r.Header.Del(n)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ---- recover ----

// Recover turns a panic in a handler into a 500 response with a fixed JSON body,
// and logs it (with the request ID, never the request) at error level. The panic
// value is logged as text; the stack is not, so nothing from the request ends up
// in the logs. http.ErrAbortHandler is passed through, as net/http expects.
func Recover(log *slog.Logger) Middleware {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.ErrorContext(r.Context(), "handler panicked", "panic", sanitize(v), "method", r.Method, "path", r.URL.Path)
				if !sw.wrote { // a response already started cannot be replaced
					writeJSONError(w, http.StatusInternalServerError, "internal_error")
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

func sanitize(v any) string {
	var s string
	switch x := v.(type) {
	case error:
		s = x.Error()
	case string:
		s = x
	default:
		s = "non-text panic value"
	}
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// ---- access log ----

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Unwrap lets http.NewResponseController reach the real writer (flushing,
// deadlines, hijacking).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("middleware: the response writer cannot be hijacked")
}

// AccessLog logs one line per request after it finishes: method, path (without
// the query string), status, response bytes, duration and the request ID. 5xx
// responses log at error level, 4xx at warn, the rest at info. Headers, query
// strings and bodies are never logged. Put it inside RequestID so the ID is set,
// and use logger.Config.ContextAttrs to add trace IDs.
func AccessLog(log *slog.Logger) Middleware {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			level := slog.LevelInfo
			switch {
			case sw.status >= 500:
				level = slog.LevelError
			case sw.status >= 400:
				level = slog.LevelWarn
			}
			log.Log(r.Context(), level, "request",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"bytes", sw.bytes, "duration_ms", time.Since(start).Milliseconds())
		})
	}
}

// ---- limits ----

// MaxBody limits the request body to n bytes. Reading past the limit fails, and
// the server closes the connection after the response.
func MaxBody(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout gives each request d to finish. The handler's context is cancelled at the
// deadline (so database and client calls that honor it stop), and if the handler
// has not responded by then the client gets 503 with a fixed JSON body. The
// handler's response is buffered, so do not use it for streaming endpoints.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, `{"error":"timeout"}`)
	}
}

// SecureHeaders adds headers that suit an API: no content sniffing, no framing,
// no caching of responses, and no referrer. It does not set Strict-Transport-
// Security; set that where TLS is terminated.
func SecureHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Cache-Control", "no-store")
			h.Set("Referrer-Policy", "no-referrer")
			next.ServeHTTP(w, r)
		})
	}
}
