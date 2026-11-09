package validator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ErrSetup is wrapped by errors that mean the destination struct or the
// validator's options are wrong (a programming mistake, to be fixed in code),
// as opposed to a bad request. Map it to a 500 response, never a 4xx.
var ErrSetup = errors.New("validator: invalid destination or options")

// Source says where in the request a value came from.
type Source string

// The places a value can be bound from.
const (
	Body    Source = "body"
	Query   Source = "query"
	Path    Source = "path"
	Header  Source = "header"
	Request Source = "request" // the request as a whole, such as an unsupported content type
)

// Codes name why a value was rejected. They are stable and safe to show to clients.
const (
	CodeRequired        = "required"
	CodeType            = "type"
	CodeMalformedJSON   = "malformed_json"
	CodeUnknownField    = "unknown_field"
	CodeDuplicate       = "duplicate"
	CodeTooLarge        = "too_large"
	CodeMediaType       = "unsupported_media_type"
	CodeMin             = "min"
	CodeMax             = "max"
	CodeLength          = "length"
	CodeOneOf           = "oneof"
	CodeFormat          = "format"
	CodePattern         = "pattern"
	CodeControl         = "control_characters"
	CodeRule            = "rule"
	CodeTooMany         = "too_many"
	CodeTooDeep         = "too_deep"
	CodeUnknownQueryKey = "unknown_query_parameter"
)

// FieldError is one rejected value. It never contains the value itself, which
// may be a secret or personal data.
type FieldError struct {
	Source  Source `json:"source"`
	Name    string `json:"field"` // the name in the request, such as "status" or "X-User-ID"; nested: "items[2].sku"
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Errors is what Bind returns for a bad request: every problem found, not just
// the first. Use errors.As to get it from the error.
type Errors struct {
	Fields []FieldError `json:"errors"`
}

func (e *Errors) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = fmt.Sprintf("%s %s: %s", f.Source, f.Name, f.Message)
	}
	return "validator: " + strings.Join(parts, "; ")
}

// StatusCode is the HTTP status for the response: 413 if the body was too
// large, 415 for an unsupported content type, 400 otherwise.
func (e *Errors) StatusCode() int {
	status := http.StatusBadRequest
	for _, f := range e.Fields {
		switch f.Code {
		case CodeTooLarge:
			return http.StatusRequestEntityTooLarge
		case CodeMediaType:
			status = http.StatusUnsupportedMediaType
		}
	}
	return status
}

// JSON returns the body to send with the status: {"errors":[{"source","field","code","message"}]}.
func (e *Errors) JSON() []byte {
	b, _ := json.Marshal(e)
	return b
}

// Has reports whether any error has the given source and name.
func (e *Errors) Has(source Source, name string) bool {
	for _, f := range e.Fields {
		if f.Source == source && f.Name == name {
			return true
		}
	}
	return false
}

func setup(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrSetup, fmt.Sprintf(format, args...))
}
