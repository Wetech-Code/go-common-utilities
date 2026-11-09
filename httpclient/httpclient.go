// Package httpclient is a safe HTTP client for one service calling another (or an API
// gateway in front of several). It is for outbound calls to services you run, with
// JSON in mind, and adds what a plain http.Client leaves to every caller:
//
//	api, err := httpclient.New(httpclient.Config{
//		BaseURL:    "https://gateway.internal",
//		PathPrefix: "/api",                                  // optional
//		Headers:    map[string]string{"X-Caller": "orders"}, // sent on every call
//		HTTPClient: &http.Client{Transport: tel.HTTPTransport(mtlsTransport)}, // mutual TLS, traces
//	})
//	payments, err := api.WithPrefix("payments")                // URLs become /api/payments/...
//	var receipt Receipt
//	err = payments.JSON(ctx, http.MethodPost, httpclient.Path("charges", chargeID, "capture"),
//		nil, &receipt, httpclient.IdempotencyKey("order-77-capture"),
//		httpclient.ForwardFromContext("X-Tenant-ID"))
//
// For every call it: builds the URL safely (every path segment is escaped and dot
// segments, control characters and encoded tricks are refused, so a value can never
// change the route); sends the request ID from the context (generating one if there is
// none); bounds each attempt with a timeout and the whole call with your context;
// refuses redirects (so headers never follow a redirect to another host); caps the
// response size; and retries only what is safe to repeat. Failures never echo the URL,
// headers or response body in their text.
//
// What it deliberately does not do: decide how the other side knows who is calling
// (mutual TLS, a signed header, a token: give HTTPClient a transport, or Config.Sign
// a signer), or pretend a failed write succeeded.
//
// Retries: GET, HEAD and OPTIONS are retried on network errors and on 429, 502, 503
// and 504. Any other method is sent once unless you give an IdempotencyKey, which
// is the receiving service's promise that a repeat has no second effect (it must keep
// a receipt of the key). Backoff is exponential with jitter, honors Retry-After
// (capped), and stops when ctx ends.
package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wetech-Code/go-common-utilities/reqctx"
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultTimeout         = 10 * time.Second
	DefaultMaxRetries      = 2
	DefaultBackoff         = 100 * time.Millisecond
	DefaultMaxResponseSize = 8 << 20
	maxBackoff             = 2 * time.Second
	maxRetryAfter          = 5 * time.Second
)

var (
	// ErrInvalidRequest is wrapped by errors for arguments that cannot be sent: a bad
	// prefix, a path that could change the route, a forbidden header.
	ErrInvalidRequest = errors.New("httpclient: invalid request")
	// ErrTimeout is wrapped when the call or an attempt ran out of time.
	ErrTimeout = errors.New("httpclient: timed out")
	// ErrCanceled is wrapped when the caller's context was cancelled.
	ErrCanceled = errors.New("httpclient: canceled")
	// ErrUnavailable is wrapped when the other side could not be reached or answered
	// with something unusable.
	ErrUnavailable = errors.New("httpclient: service unavailable")
	// ErrResponseTooLarge is wrapped when a response is bigger than MaxResponseSize.
	ErrResponseTooLarge = errors.New("httpclient: response too large")
	// ErrStatus is wrapped by StatusError, returned by JSON for non-2xx responses.
	ErrStatus = errors.New("httpclient: unexpected status")
)

var (
	segmentRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	prefixRE  = regexp.MustCompile(`^(/[A-Za-z0-9_-][A-Za-z0-9._-]*)+$`)
)

// Config describes the other side and how to call it.
type Config struct {
	// BaseURL is the address, such as "https://gateway.internal". It must be http or
	// https (https outside a private network) and must not contain credentials or a query.
	BaseURL string
	// PathPrefix is an optional route prefix put in front of every endpoint, such as
	// "/api/v1". It must start with "/" and contain only letters, digits and - _ . /
	// segments, with no trailing slash.
	PathPrefix string
	// AllowedPrefixes, if set, is the list of segments WithPrefix may add. Any other
	// segment is refused before anything is sent.
	AllowedPrefixes []string
	// Headers are sent on every call, for example a name for this service. Names and
	// values are checked for characters that could split the request, and the headers
	// the client manages itself cannot be set.
	Headers map[string]string
	// HTTPClient sends the requests. Pass one whose Transport does mutual TLS and/or
	// is wrapped by telemetry's HTTPTransport. Redirects are refused and the client's
	// own Timeout is not used, whichever client is passed; the one you pass is copied,
	// never modified. Default: a fresh client.
	HTTPClient *http.Client
	// Timeout bounds each attempt. Default DefaultTimeout.
	Timeout time.Duration
	// MaxRetries is the number of retries after the first attempt, for calls that are
	// safe to repeat. Default DefaultMaxRetries; use NoRetries to turn retries off.
	MaxRetries int
	NoRetries  bool
	// Backoff is the first retry delay; it doubles each time up to 2 seconds. Default DefaultBackoff.
	Backoff time.Duration
	// MaxResponseSize caps the response body. Default DefaultMaxResponseSize.
	MaxResponseSize int64
	// Sign, if set, runs on every attempt just before the request is sent, to add
	// whatever proves this service's identity to the other side (for example a signed
	// header). body is the exact request body.
	Sign func(req *http.Request, body []byte) error
}

// Validate reports problems with the configuration without sending anything.
func (c Config) Validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("httpclient: BaseURL must be an http:// or https:// URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("httpclient: BaseURL must not contain credentials, a query or a fragment")
	}
	if c.PathPrefix != "" && !prefixRE.MatchString(c.PathPrefix) {
		return errors.New("httpclient: PathPrefix must start with / and contain only letters, digits, - _ . and / (no empty or dot segments, no trailing slash)")
	}
	for _, s := range c.AllowedPrefixes {
		if !segmentRE.MatchString(s) {
			return fmt.Errorf("httpclient: invalid prefix segment %q", s)
		}
	}
	for k, v := range c.Headers {
		if !validHeaderName(k) || reservedHeaders[strings.ToLower(k)] || !validHeaderValue(v) {
			return fmt.Errorf("httpclient: header %q cannot be set in Config.Headers", truncate(k, 40))
		}
	}
	if c.Timeout < 0 || c.MaxRetries < 0 || c.Backoff < 0 || c.MaxResponseSize < 0 {
		return errors.New("httpclient: Timeout, MaxRetries, Backoff and MaxResponseSize must not be negative")
	}
	return nil
}

// Client makes calls. It is safe for concurrent use.
type Client struct {
	cfg     Config
	base    *url.URL
	path    string // PathPrefix plus any WithPrefix segments
	http    *http.Client
	allowed map[string]bool
	sleep   func(context.Context, time.Duration) error
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}
	if cfg.NoRetries {
		cfg.MaxRetries = 0
	}
	if cfg.Backoff == 0 {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.MaxResponseSize == 0 {
		cfg.MaxResponseSize = DefaultMaxResponseSize
	}
	base, _ := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	c := &Client{cfg: cfg, base: base, path: cfg.PathPrefix, sleep: sleep}
	if cfg.HTTPClient != nil {
		hc := *cfg.HTTPClient
		c.http = &hc
	} else {
		c.http = &http.Client{}
	}
	c.http.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.http.Timeout = 0 // per-attempt timeouts use contexts, so the body read is covered too
	if len(cfg.AllowedPrefixes) > 0 {
		c.allowed = map[string]bool{}
		for _, s := range cfg.AllowedPrefixes {
			c.allowed[s] = true
		}
	}
	return c, nil
}

// WithPrefix returns a Client whose URLs have one more path segment, such as the name of
// a service behind a gateway: with PathPrefix "/api", WithPrefix("payments") calls
// "/api/payments/...". The segment must be lower-case letters, digits and "-", starting
// with a letter, and must be in AllowedPrefixes if that is set. The new Client shares
// the settings and connection pool of the original.
func (c *Client) WithPrefix(segment string) (*Client, error) {
	if !segmentRE.MatchString(segment) || (c.allowed != nil && !c.allowed[segment]) {
		return nil, fmt.Errorf("%w: the prefix segment is not allowed", ErrInvalidRequest)
	}
	n := *c
	n.path = c.path + "/" + segment
	return &n, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Endpoint is the part of the URL after the client's prefix, already escaped. Build it
// with Path; a string literal is accepted for fixed endpoints such as "orders/open".
type Endpoint string

// Path builds an Endpoint from segments, escaping each so that a value containing "/",
// "..", "?" or "%" stays one harmless segment.
//
//	httpclient.Path("orders", orderID, "items")
func Path(segments ...string) Endpoint {
	esc := make([]string, len(segments))
	for i, s := range segments {
		esc[i] = url.PathEscape(s)
	}
	return Endpoint(strings.Join(esc, "/"))
}

func validEndpoint(e Endpoint) error {
	s := strings.TrimPrefix(string(e), "/")
	if s == "" {
		return fmt.Errorf("%w: the endpoint is empty", ErrInvalidRequest)
	}
	if strings.ContainsAny(s, "?#\\") {
		return fmt.Errorf("%w: the endpoint must not contain ?, # or backslash (use the Query option)", ErrInvalidRequest)
	}
	segs := strings.Split(strings.TrimSuffix(s, "/"), "/")
	for _, seg := range segs {
		dec, err := url.PathUnescape(seg)
		if err != nil || seg == "" || dec == "." || dec == ".." || strings.ContainsAny(dec, "/\\") {
			return fmt.Errorf("%w: the endpoint has an empty, dot or invalid segment", ErrInvalidRequest)
		}
		for i := 0; i < len(dec); i++ {
			if dec[i] < 0x20 || dec[i] == 0x7f {
				return fmt.Errorf("%w: the endpoint has a control character", ErrInvalidRequest)
			}
		}
	}
	return nil
}

// ---- options ----

type call struct {
	query   url.Values
	body    []byte
	hasBody bool
	ctype   string
	headers http.Header
	forward []string
	idemKey string
	timeout time.Duration
	noRetry bool
	err     error
}

// Option changes one call.
type Option func(*call)

// Query adds query parameters.
func Query(v url.Values) Option { return func(c *call) { c.query = v } }

// Body sets a raw request body and its content type. The bytes are kept so a retry
// can send them again.
func Body(contentType string, b []byte) Option {
	return func(c *call) { c.body, c.hasBody, c.ctype = b, true, contentType }
}

// JSONBody sets a JSON request body from v.
func JSONBody(v any) Option {
	return func(c *call) {
		b, err := json.Marshal(v)
		if err != nil {
			c.err = fmt.Errorf("%w: the body cannot be encoded as JSON", ErrInvalidRequest)
			return
		}
		c.body, c.hasBody, c.ctype = b, true, "application/json"
	}
}

// ForwardFromContext copies the named header values captured in the call's context
// (see reqctx.WithHeaders and middleware.CaptureHeaders) onto the outgoing request.
// Names with no captured value are skipped. Nothing is forwarded unless you ask, and
// only values a trusted layer authenticated should ever be captured.
func ForwardFromContext(names ...string) Option {
	return func(c *call) {
		for _, n := range names {
			if !validHeaderName(n) || reservedHeaders[strings.ToLower(n)] {
				c.err = fmt.Errorf("%w: header %q cannot be forwarded", ErrInvalidRequest, truncate(n, 40))
				return
			}
		}
		c.forward = append(c.forward, names...)
	}
}

// IdempotencyKey sets X-Idempotency-Key and allows the call to be retried even if it
// is a write, because the receiving service promises a repeat has no second effect.
func IdempotencyKey(key string) Option { return func(c *call) { c.idemKey = key } }

// Timeout overrides the per-attempt timeout for this call.
func Timeout(d time.Duration) Option { return func(c *call) { c.timeout = d } }

// NoRetry sends the call exactly once.
func NoRetry() Option { return func(c *call) { c.noRetry = true } }

var reservedHeaders = map[string]bool{
	"host": true, "content-length": true, "content-type": true, "transfer-encoding": true, "connection": true,
	"upgrade": true, "te": true, "trailer": true, "traceparent": true, "tracestate": true,
	"x-request-id": true, "x-idempotency-key": true,
}

// Header adds a request header. The headers the client manages itself (request ID,
// idempotency key, content type, trace context and hop-by-hop headers) cannot be set
// this way. Names and values are checked for characters that could split the request.
func Header(name, value string) Option {
	return func(c *call) {
		if !validHeaderName(name) || reservedHeaders[strings.ToLower(name)] || !validHeaderValue(value) {
			c.err = fmt.Errorf("%w: header %q cannot be set", ErrInvalidRequest, truncate(name, 40))
			return
		}
		if c.headers == nil {
			c.headers = http.Header{}
		}
		c.headers.Add(name, value)
	}
}

func validHeaderName(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func validHeaderValue(s string) bool {
	if len(s) > 4096 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 && s[i] != '\t' || s[i] == 0x7f {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// ---- results and errors ----

// Response is what came back. Any HTTP status is a Response, including 4xx and 5xx;
// Do returns an error only when no response was obtained.
type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	Attempts int
	// RequestID is the request ID that was sent, for correlating with the other side's logs.
	RequestID string
}

// OK reports whether the status is 2xx.
func (r *Response) OK() bool { return r.Status >= 200 && r.Status < 300 }

// Error describes a call that produced no response. It wraps one of ErrInvalidRequest,
// ErrTimeout, ErrCanceled, ErrUnavailable or ErrResponseTooLarge.
type Error struct {
	Target    string // the client's path prefix, such as "/api/payments"; never the endpoint or query
	Attempts  int
	RequestID string
	Err       error
}

func (e *Error) Error() string {
	return fmt.Sprintf("httpclient: call to %q failed after %d attempt(s) (request %s): %v", e.Target, e.Attempts, e.RequestID, e.Err)
}
func (e *Error) Unwrap() error { return e.Err }

// StatusError is returned by JSON for a non-2xx response. Body holds the start of the
// response (the other side's own error body, which is meant for callers), and is not
// part of the error text.
type StatusError struct {
	Target    string
	Status    int
	Body      []byte
	RequestID string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("httpclient: %q answered %d (request %s)", e.Target, e.Status, e.RequestID)
}
func (e *StatusError) Unwrap() error { return ErrStatus }

// ---- calling ----

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

func retryableStatus(s int) bool {
	return s == http.StatusTooManyRequests || s == http.StatusBadGateway || s == http.StatusServiceUnavailable || s == http.StatusGatewayTimeout
}

// Do calls endpoint and returns whatever response arrives. It returns an *Error only if
// no response was obtained.
func (c *Client) Do(ctx context.Context, method string, endpoint Endpoint, opts ...Option) (*Response, error) {
	rid := reqctx.RequestID(ctx)
	if rid == "" {
		rid = reqctx.NewRequestID()
	}
	fail := func(attempts int, err error) (*Response, error) {
		return nil, &Error{Target: c.path, Attempts: attempts, RequestID: rid, Err: err}
	}

	var co call
	for _, o := range opts {
		o(&co)
	}
	if co.err != nil {
		return fail(0, co.err)
	}
	if method == "" || strings.ContainsAny(method, " \t\r\n") {
		return fail(0, fmt.Errorf("%w: invalid method", ErrInvalidRequest))
	}
	if err := validEndpoint(endpoint); err != nil {
		return fail(0, err)
	}
	if co.idemKey != "" && !reqctx.ValidID(co.idemKey) {
		return fail(0, fmt.Errorf("%w: invalid idempotency key", ErrInvalidRequest))
	}
	for _, name := range co.forward {
		if v := reqctx.HeaderValue(ctx, name); v != "" {
			if co.headers == nil {
				co.headers = http.Header{}
			}
			co.headers.Set(name, v)
		}
	}

	u := *c.base
	u.Path = c.base.Path + c.path + "/" + strings.TrimPrefix(string(endpoint), "/")
	if dec, err := url.PathUnescape(u.Path); err == nil {
		u.RawPath = u.Path
		u.Path = dec
	}
	if len(co.query) > 0 {
		u.RawQuery = co.query.Encode()
	}

	timeout := c.cfg.Timeout
	if co.timeout > 0 {
		timeout = co.timeout
	}
	canRetry := !co.noRetry && (safeMethod(method) || co.idemKey != "")
	maxAttempts := 1
	if canRetry {
		maxAttempts += c.cfg.MaxRetries
	}

	var lastErr error
	var lastResp *Response
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := c.once(ctx, method, u.String(), &co, rid, timeout)
		if err == nil && !retryableStatus(resp.Status) {
			resp.Attempts, resp.RequestID = attempt, rid
			return resp, nil
		}
		if err == nil {
			resp.Attempts, resp.RequestID = attempt, rid
			lastResp, lastErr = resp, nil
		} else {
			lastResp, lastErr = nil, err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(attempt, classifyContext(ctxErr))
		}
		if errors.Is(err, ErrResponseTooLarge) || errors.Is(err, ErrInvalidRequest) {
			return fail(attempt, err)
		}
		if attempt == maxAttempts {
			break
		}
		wait := backoff(c.cfg.Backoff, attempt)
		if lastResp != nil {
			if ra := retryAfter(lastResp.Header); ra > wait {
				wait = ra
			}
		}
		if err := c.sleep(ctx, wait); err != nil {
			return fail(attempt, classifyContext(err))
		}
	}
	if lastResp != nil {
		return lastResp, nil
	}
	return fail(maxAttempts, lastErr)
}

func classifyContext(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	return ErrCanceled
}

func backoff(base time.Duration, attempt int) time.Duration {
	d := base << (attempt - 1)
	if d > maxBackoff || d <= 0 {
		d = maxBackoff
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) // 50% to 100% of d
}

func retryAfter(h http.Header) time.Duration {
	s := h.Get("Retry-After")
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return min(time.Duration(n)*time.Second, maxRetryAfter)
	}
	if t, err := http.ParseTime(s); err == nil {
		return max(0, min(time.Until(t), maxRetryAfter))
	}
	return 0
}

// once makes one attempt.
func (c *Client) once(ctx context.Context, method, target string, co *call, rid string, timeout time.Duration) (*Response, error) {
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var body io.Reader
	if co.hasBody {
		body = bytes.NewReader(co.body)
	}
	req, err := http.NewRequestWithContext(actx, method, target, body)
	if err != nil {
		return nil, fmt.Errorf("%w: the request cannot be built", ErrInvalidRequest)
	}
	req.Header.Set("Accept", "application/json")
	if co.hasBody {
		req.Header.Set("Content-Type", co.ctype)
	}
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	for k, vs := range co.headers {
		for i, v := range vs {
			if i == 0 {
				req.Header.Set(k, v)
			} else {
				req.Header.Add(k, v)
			}
		}
	}
	req.Header.Set(reqctx.HeaderRequestID, rid)
	if co.idemKey != "" {
		req.Header.Set(reqctx.HeaderIdempotencyKey, co.idemKey)
	}
	if c.cfg.Sign != nil {
		if err := c.cfg.Sign(req, co.body); err != nil {
			return nil, fmt.Errorf("%w: signing the request failed", ErrInvalidRequest)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportError(actx, ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxResponseSize+1))
	if err != nil {
		return nil, transportError(actx, ctx, err)
	}
	if int64(len(data)) > c.cfg.MaxResponseSize {
		return nil, ErrResponseTooLarge
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, nil
}

// transportError classifies a failure without echoing the URL (url.Error includes it).
func transportError(attemptCtx, callCtx context.Context, err error) error {
	switch {
	case callCtx.Err() != nil:
		return classifyContext(callCtx.Err())
	case errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return ErrTimeout
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	// net.OpError text includes the peer's address; keep only what failed and why.
	var oe *net.OpError
	if errors.As(err, &oe) {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, oe.Op, oe.Err)
	}
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// JSON calls endpoint with an optional JSON body (in) and decodes a 2xx JSON response
// into out. in and out may be nil. A non-2xx response is returned as a *StatusError
// (errors.Is(err, ErrStatus)); no response at all is an *Error.
func (c *Client) JSON(ctx context.Context, method string, endpoint Endpoint, in, out any, opts ...Option) error {
	if in != nil {
		opts = append(opts[:len(opts):len(opts)], JSONBody(in))
	}
	resp, err := c.Do(ctx, method, endpoint, opts...)
	if err != nil {
		return err
	}
	if !resp.OK() {
		return &StatusError{Target: c.path, Status: resp.Status, Body: resp.Body[:min(len(resp.Body), 4096)], RequestID: resp.RequestID}
	}
	if out == nil || len(bytes.TrimSpace(resp.Body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return &Error{Target: c.path, Attempts: resp.Attempts, RequestID: resp.RequestID,
			Err: fmt.Errorf("%w: the response is not the expected JSON", ErrUnavailable)}
	}
	return nil
}
