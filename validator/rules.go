package validator

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	uuidRE       = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	identifierRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	alphanumRE   = regexp.MustCompile(`^[A-Za-z0-9]+$`)
)

type rule struct {
	name string
	arg  string
	i    int64
	u    uint64
	f    float64
	n    int
	set  []string
	re   *regexp.Regexp
	fn   func(any, string) error
}

// ruleSet is the compiled form of a validate tag.
type ruleSet struct {
	required     bool
	dive         bool
	allowControl bool
	rules        []rule
}

// base returns the type a value is checked as: pointers are followed.
func base(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func isNumber(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// compile parses a validate tag for a value of type t.
func (v *Validator) compile(tag string, t reflect.Type) (*ruleSet, error) {
	rs := &ruleSet{}
	bt := base(t)
	for _, tok := range strings.Split(tag, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		name, arg, hasArg := strings.Cut(tok, "=")
		switch name {
		case "required":
			rs.required = true
			continue
		case "dive":
			rs.dive = true
			continue
		case "allowcontrol":
			rs.allowControl = true
			continue
		}
		r := rule{name: name, arg: arg}
		switch name {
		case "min", "max", "len":
			if !hasArg {
				return nil, setup("rule %q needs a value, such as %s=5", name, name)
			}
			var err error
			switch {
			case bt == durationType:
				var d time.Duration
				d, err = time.ParseDuration(arg)
				r.i = int64(d)
			case bt.Kind() == reflect.String || bt.Kind() == reflect.Slice || bt.Kind() == reflect.Map:
				r.n, err = strconv.Atoi(arg)
				if err == nil && r.n < 0 {
					err = fmt.Errorf("negative")
				}
			case bt.Kind() >= reflect.Int && bt.Kind() <= reflect.Int64:
				r.i, err = strconv.ParseInt(arg, 10, 64)
			case bt.Kind() >= reflect.Uint && bt.Kind() <= reflect.Uint64:
				r.u, err = strconv.ParseUint(arg, 10, 64)
			case bt.Kind() == reflect.Float32 || bt.Kind() == reflect.Float64:
				r.f, err = strconv.ParseFloat(arg, 64)
			default:
				return nil, setup("rule %q cannot be used on %s", name, t)
			}
			if err != nil {
				return nil, setup("rule %q has an invalid value for %s", tok, t)
			}
			if name == "len" && isNumber(bt) {
				return nil, setup("rule len cannot be used on %s", t)
			}
		case "oneof":
			r.set = strings.Fields(arg)
			if len(r.set) == 0 || !(bt.Kind() == reflect.String || (isNumber(bt) && bt.Kind() != reflect.Float32 && bt.Kind() != reflect.Float64)) {
				return nil, setup("rule oneof needs values and a string or integer field, not %s", t)
			}
		case "uuid", "identifier", "alphanum", "url":
			if bt.Kind() != reflect.String {
				return nil, setup("rule %q needs a string field, not %s", name, t)
			}
		case "pattern":
			re, ok := v.patterns[arg]
			if !ok || bt.Kind() != reflect.String {
				return nil, setup("rule pattern=%s needs a registered pattern and a string field", arg)
			}
			r.re = re
		default:
			fn, ok := v.customRules[name]
			if !ok {
				return nil, setup("unknown validation rule %q", name)
			}
			r.fn = fn
		}
		rs.rules = append(rs.rules, r)
	}
	if rs.dive {
		bt2 := bt
		if bt2.Kind() == reflect.Slice {
			bt2 = base(bt2.Elem())
		}
		if bt2.Kind() != reflect.Struct {
			return nil, setup("dive needs a struct, or a slice of structs, not %s", t)
		}
	}
	return rs, nil
}

// check runs the rules on a present, non-nil value (pointers already followed).
// It returns a code and a client-safe message, or "" if the value passes.
func (rs *ruleSet) check(val reflect.Value) (code, msg string) {
	for _, r := range rs.rules {
		if code, msg = r.check(val); code != "" {
			return code, msg
		}
	}
	return "", ""
}

func (r rule) check(val reflect.Value) (string, string) {
	switch r.name {
	case "min", "max", "len":
		return r.checkSize(val)
	case "oneof":
		var s string
		switch {
		case val.Kind() == reflect.String:
			s = val.String()
		case val.CanInt():
			s = strconv.FormatInt(val.Int(), 10)
		default:
			s = strconv.FormatUint(val.Uint(), 10)
		}
		for _, allowed := range r.set {
			if s == allowed {
				return "", ""
			}
		}
		return CodeOneOf, "must be one of: " + strings.Join(r.set, ", ")
	case "uuid":
		if !uuidRE.MatchString(val.String()) {
			return CodeFormat, "must be a UUID"
		}
	case "identifier":
		if !identifierRE.MatchString(val.String()) {
			return CodeFormat, "must be an identifier of letters, digits and . _ : - (at most 128 characters)"
		}
	case "alphanum":
		if !alphanumRE.MatchString(val.String()) {
			return CodeFormat, "must contain only letters and digits"
		}
	case "url":
		s := val.String()
		u, err := url.Parse(s)
		if err != nil || len(s) > 2048 || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return CodeFormat, "must be an absolute http or https URL"
		}
	case "pattern":
		if !r.re.MatchString(val.String()) {
			return CodePattern, "has an invalid format"
		}
	default:
		if err := r.fn(val.Interface(), r.arg); err != nil {
			return CodeRule, err.Error()
		}
	}
	return "", ""
}

func (r rule) checkSize(val reflect.Value) (string, string) {
	more, less := CodeMin, CodeMax
	if r.name == "len" {
		more, less = CodeLength, CodeLength
	}
	cmpInt := func(got, want int64, unit string) (string, string) {
		switch {
		case r.name == "min" && got < want:
			return more, fmt.Sprintf("must be at least %d%s", want, unit)
		case r.name == "max" && got > want:
			return less, fmt.Sprintf("must be at most %d%s", want, unit)
		case r.name == "len" && got != want:
			return more, fmt.Sprintf("must be exactly %d%s", want, unit)
		}
		return "", ""
	}
	switch {
	case val.Type() == durationType:
		d := time.Duration(val.Int())
		want := time.Duration(r.i)
		switch {
		case r.name == "min" && d < want:
			return more, "must be at least " + want.String()
		case r.name == "max" && d > want:
			return less, "must be at most " + want.String()
		}
	case val.Kind() == reflect.String:
		return cmpInt(int64(utf8.RuneCountInString(val.String())), int64(r.n), " characters")
	case val.Kind() == reflect.Slice || val.Kind() == reflect.Map:
		return cmpInt(int64(val.Len()), int64(r.n), " items")
	case val.CanInt():
		return cmpInt(val.Int(), r.i, "")
	case val.CanUint():
		got, want := val.Uint(), r.u
		switch {
		case r.name == "min" && got < want:
			return more, fmt.Sprintf("must be at least %d", want)
		case r.name == "max" && got > want:
			return less, fmt.Sprintf("must be at most %d", want)
		}
	case val.CanFloat():
		got, want := val.Float(), r.f
		switch {
		case r.name == "min" && got < want:
			return more, fmt.Sprintf("must be at least %v", want)
		case r.name == "max" && got > want:
			return less, fmt.Sprintf("must be at most %v", want)
		}
	}
	return "", ""
}

// normalize applies explicit normalization operations to a string.
func normalize(s string, ops []string) string {
	for _, op := range ops {
		switch op {
		case "trim":
			s = strings.TrimSpace(s)
		case "lower":
			s = strings.ToLower(s)
		case "upper":
			s = strings.ToUpper(s)
		}
	}
	return s
}

func validNormalize(tag string) ([]string, bool) {
	var ops []string
	for _, op := range strings.Split(tag, ",") {
		op = strings.TrimSpace(op)
		if op == "" {
			continue
		}
		if op != "trim" && op != "lower" && op != "upper" {
			return nil, false
		}
		ops = append(ops, op)
	}
	return ops, true
}
