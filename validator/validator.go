// Package validator turns an HTTP request into a typed, checked Go value in one
// call. A handler declares what it expects as a struct; Bind reads the JSON
// body, query string, route path values and headers into it, applies explicit
// normalization and validation rules, and returns either the filled struct or
// every problem it found.
//
//	type UpdateOrder struct {
//		OrderID string  `path:"orderId"   validate:"required,uuid"`
//		Status string  `body:"status"  validate:"required,oneof=pending shipped delivered" normalize:"trim,lower"`
//		Note   *string `body:"note"    validate:"max=500"`
//		Notify bool    `query:"notify"`
//		UserID string  `header:"X-User-ID"         validate:"required,identifier"`
//		TenantID string `header:"X-Tenant-ID" validate:"required,identifier"`
//		Key    string  `header:"X-Idempotency-Key" validate:"required,max=64"`
//	}
//
//	func (h *Handler) Update(w http.ResponseWriter, r *http.Request) {
//		var req UpdateOrder
//		if err := validator.Bind(r, &req); err != nil {
//			var verr *validator.Errors
//			if errors.As(err, &verr) {
//				w.Header().Set("Content-Type", "application/json")
//				w.WriteHeader(verr.StatusCode())
//				w.Write(verr.JSON())
//				return
//			}
//			http.Error(w, "internal error", http.StatusInternalServerError) // a setup mistake, fix the struct
//			return
//		}
//		// req is fully populated and checked; call the service.
//	}
//
// What it does: checks syntax, shape and size. What it does not do: decide
// whether the caller may do something, or whether a header is genuine. A header
// field only proves that a value has the right format; the identity headers
// (X-User-ID and friends) can be trusted only if the ingress in front of your
// service authenticated the caller and replaced any value the client sent.
// Business rules and authorization belong in the service.
//
// Tags (a field has at most one source tag; fields with none are not bound):
//
//	body:"name"    a top-level key of the JSON object body
//	query:"name"   a query-string parameter
//	path:"name"    a route path value (r.PathValue, or your router via the PathValue option)
//	header:"Name"  a request header
//	validate:"..." comma-separated rules, see below
//	normalize:"trim,lower,upper"  explicit normalization, applied before validation
//	default:"x"    the value when the request does not provide one (not with required)
//	each:"..."     validate rules applied to every element of a slice field
//
// Rules: required, min=N, max=N, len=N (numbers, durations, string length in
// characters, slice and map size), oneof=a b c, uuid, identifier, alphanum, url,
// pattern=name (a registered regular expression), dive (validate the fields of a
// nested struct or of each element of a slice of structs), allowcontrol (permit
// control characters in a string), and any rule added with the Rule option.
//
// Field types: string, bool ("true" or "false" only), integers, floats,
// time.Duration, time.Time (RFC 3339) and types implementing
// encoding.TextUnmarshaler; pointers for optional values; slices of those for
// query and header values that may repeat. Body fields can be any type that
// encoding/json can decode.
//
// Strictness built in, because the alternative is silently choosing a value an
// attacker controls:
//
//   - A scalar query or header value that appears more than once is an error, as is
//     a repeated Content-Type header.
//   - JSON bodies must be an object, valid UTF-8, with no duplicate keys at any
//     depth, no trailing data, nesting within a limit and size within MaxBodySize.
//     Keys that differ only by letter case ("sku" and "SKU") count as duplicates,
//     because encoding/json would otherwise let one silently overwrite the other
//     when decoding nested structs. Top-level keys must match a field's name
//     exactly; unknown keys are rejected unless you allow them.
//   - A malformed query string is rejected instead of being half-read.
//   - Strings must be valid UTF-8 without control characters (tab, newline and
//     carriage return are allowed in body text) unless the field is marked
//     allowcontrol. Bad values are rejected, never quietly cleaned.
//   - On any error the destination struct is left unchanged.
//
// Errors list every problem, name the source and field and a stable code, and
// never contain the rejected value. Mistakes in the struct or options are
// reported as ErrSetup, not as a bad request.
package validator

import (
	"errors"
	"net/http"
	"reflect"
	"regexp"
	"sync"
)

// Defaults applied when no option says otherwise.
const (
	DefaultMaxBodySize  = 1 << 20
	DefaultMaxValueSize = 8 << 10
	DefaultMaxDepth     = 32
	DefaultMaxQuery     = 100
)

// Validator binds and validates requests. It caches what it learns about each
// destination type, so create one and reuse it. It is safe for concurrent use.
type Validator struct {
	maxBody            int64
	maxValue           int
	maxDepth           int
	maxQuery           int
	allowUnknownBody   bool
	rejectUnknownQuery bool
	pathValue          func(*http.Request, string) string
	patterns           map[string]*regexp.Regexp
	customRules        map[string]func(any, string) error

	plans  sync.Map // reflect.Type -> *planResult
	nested sync.Map // reflect.Type -> *nestedPlan
}

// Option changes how a Validator behaves.
type Option func(*Validator)

// MaxBodySize sets the largest JSON body accepted, in bytes. Default 1 MiB.
func MaxBodySize(n int64) Option { return func(v *Validator) { v.maxBody = n } }

// MaxValueSize sets the longest query, header or path value accepted, in bytes.
// Default 8 KiB.
func MaxValueSize(n int) Option { return func(v *Validator) { v.maxValue = n } }

// MaxJSONDepth sets how deeply nested a JSON body may be. Default 32.
func MaxJSONDepth(n int) Option { return func(v *Validator) { v.maxDepth = n } }

// AllowUnknownBodyFields accepts JSON keys that no field declares, ignoring them.
// By default they are an error.
func AllowUnknownBodyFields() Option { return func(v *Validator) { v.allowUnknownBody = true } }

// RejectUnknownQuery makes a query parameter that no field declares an error.
// By default they are ignored, which suits tracking parameters.
func RejectUnknownQuery() Option { return func(v *Validator) { v.rejectUnknownQuery = true } }

// PathValue sets how path values are read, for routers other than net/http's
// (for example chi.URLParam). Default: r.PathValue.
func PathValue(fn func(r *http.Request, name string) string) Option {
	return func(v *Validator) { v.pathValue = fn }
}

// Pattern registers a regular expression for the rule pattern=name. Go's regexp
// runs in linear time, so patterns cannot cause catastrophic backtracking.
func Pattern(name string, re *regexp.Regexp) Option {
	return func(v *Validator) { v.patterns[name] = re }
}

// Rule registers a validation rule of your own, used as `validate:"name"` or
// `validate:"name=arg"`. fn receives the field's value (after pointers are
// followed) and the argument, and returns nil if it is acceptable. The text of a
// returned error is shown to the client, so it must not contain the value.
func Rule(name string, fn func(value any, arg string) error) Option {
	return func(v *Validator) { v.customRules[name] = fn }
}

// New returns a Validator with the given options.
func New(opts ...Option) (*Validator, error) {
	v := &Validator{
		maxBody: DefaultMaxBodySize, maxValue: DefaultMaxValueSize, maxDepth: DefaultMaxDepth, maxQuery: DefaultMaxQuery,
		pathValue:   func(r *http.Request, name string) string { return r.PathValue(name) },
		patterns:    map[string]*regexp.Regexp{},
		customRules: map[string]func(any, string) error{},
	}
	for _, o := range opts {
		o(v)
	}
	switch {
	case v.maxBody <= 0 || v.maxValue <= 0 || v.maxDepth <= 0:
		return nil, setup("MaxBodySize, MaxValueSize and MaxJSONDepth must be positive")
	case v.pathValue == nil:
		return nil, setup("PathValue must not be nil")
	}
	for name, re := range v.patterns {
		if name == "" || re == nil {
			return nil, setup("Pattern needs a name and a regular expression")
		}
	}
	for name, fn := range v.customRules {
		switch name {
		case "", "required", "dive", "allowcontrol", "min", "max", "len", "oneof", "uuid", "identifier", "alphanum", "url", "pattern":
			return nil, setup("Rule %q is a built-in name or empty", name)
		}
		if fn == nil {
			return nil, setup("Rule %q has no function", name)
		}
	}
	return v, nil
}

var std = mustNew()

func mustNew() *Validator {
	v, err := New()
	if err != nil {
		panic(err) // the defaults are valid; cannot happen
	}
	return v
}

// Bind reads the request into dst, which must be a non-nil pointer to a struct,
// using the default Validator. See the package documentation.
func Bind(r *http.Request, dst any) error { return std.Bind(r, dst) }

// Parse is Bind into a new value of type T, using the default Validator.
func Parse[T any](r *http.Request) (T, error) { return ParseWith[T](std, r) }

// ParseWith is Parse using v.
func ParseWith[T any](v *Validator, r *http.Request) (T, error) {
	var out T
	if err := v.Bind(r, &out); err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

// Bind reads the request into dst, which must be a non-nil pointer to a struct.
// On success dst is replaced by the new value; on error dst is not modified, and
// the error is a *Errors for a bad request or wraps ErrSetup for a mistake in
// the struct or options.
func (v *Validator) Bind(r *http.Request, dst any) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return setup("Bind needs a non-nil pointer to a struct, got %T", dst)
	}
	if r == nil {
		return setup("Bind needs a request")
	}
	t := rv.Elem().Type()
	res, err := v.planFor(t)
	if err != nil {
		return err
	}
	out := reflect.New(t).Elem()
	var errs []FieldError
	if err := v.bind(r, res.plan, out, &errs); err != nil {
		return err
	}
	if len(errs) > 0 {
		return &Errors{Fields: errs}
	}
	rv.Elem().Set(out)
	return nil
}

type planResult struct {
	plan *plan
	err  error
}

func (v *Validator) planFor(t reflect.Type) (*planResult, error) {
	if cached, ok := v.plans.Load(t); ok {
		res := cached.(*planResult)
		return res, res.err
	}
	p, err := v.buildPlan(t)
	res := &planResult{plan: p, err: err}
	v.plans.Store(t, res)
	return res, err
}

// IsSetupError reports whether err is a mistake in the destination struct or
// the options rather than a bad request.
func IsSetupError(err error) bool { return errors.Is(err, ErrSetup) }
