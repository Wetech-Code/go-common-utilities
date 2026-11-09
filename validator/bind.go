package validator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

func (v *Validator) bind(r *http.Request, p *plan, out reflect.Value, errs *[]FieldError) error {
	add := func(src Source, name, code, msg string) {
		*errs = append(*errs, FieldError{Source: src, Name: name, Code: code, Message: msg})
	}

	var body map[string]json.RawMessage
	bodyOK := true
	if p.hasBody {
		body, bodyOK = v.readBody(r, p, add)
	}

	var query url.Values
	needQuery := len(p.queryNames) > 0 || v.rejectUnknownQuery
	if needQuery {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			add(Query, "", CodeFormat, "the query string is malformed")
		} else {
			query = q
			if len(q) > v.maxQuery {
				add(Query, "", CodeTooMany, fmt.Sprintf("has more than %d parameters", v.maxQuery))
			}
			if v.rejectUnknownQuery {
				for k := range q {
					if !p.queryNames[k] {
						add(Query, safeName(k), CodeUnknownQueryKey, "is not an accepted parameter")
					}
				}
			}
		}
	}

	for _, f := range p.fields {
		fv := out.FieldByIndex(f.index)
		var present bool
		switch f.source {
		case Body:
			if !bodyOK {
				continue // the body itself was rejected; do not pile on "required" errors
			}
			present = v.bindBody(f, fv, body, add)
		case Query:
			if query != nil {
				present = v.bindStrings(f, fv, query[f.name], add)
			}
		case Header:
			present = v.bindStrings(f, fv, r.Header.Values(f.name), add)
		case Path:
			if s := v.pathValue(r, f.name); s != "" {
				present = v.bindStrings(f, fv, []string{s}, add)
			}
		}
		if !present && f.hasDef {
			d, _ := parseScalar(f.elem, f.def)
			setScalar(f, fv, d)
			present = true
		}
		if !present {
			if f.rules.required {
				add(f.source, f.shown, CodeRequired, "is required")
			}
			continue
		}
		if f.norm != nil {
			normalizeValue(fv, f.norm)
		}
		v.checkField(f.source, f.shown, fv, f.rules, f.each, f.source == Body, add)
	}
	return nil
}

// ---- body ----

func (v *Validator) readBody(r *http.Request, p *plan, add func(Source, string, string, string)) (map[string]json.RawMessage, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, v.maxBody+1))
	if err != nil {
		add(Request, "body", CodeMalformedJSON, "the body could not be read")
		return nil, false
	}
	if int64(len(data)) > v.maxBody {
		add(Request, "body", CodeTooLarge, fmt.Sprintf("is larger than %d bytes", v.maxBody))
		return nil, false
	}
	if len(data) == 0 {
		return nil, true
	}

	cts := r.Header.Values("Content-Type")
	switch {
	case len(cts) > 1:
		add(Header, "Content-Type", CodeDuplicate, "must appear only once")
		return nil, false
	case len(cts) == 0:
		add(Header, "Content-Type", CodeMediaType, "must be application/json")
		return nil, false
	}
	mt, params, err := mime.ParseMediaType(cts[0])
	if err != nil || !(mt == "application/json" || strings.HasSuffix(mt, "+json")) {
		add(Header, "Content-Type", CodeMediaType, "must be application/json")
		return nil, false
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		add(Header, "Content-Type", CodeMediaType, "must use the UTF-8 charset")
		return nil, false
	}

	if !utf8.Valid(data) {
		add(Request, "body", CodeMalformedJSON, "is not valid UTF-8")
		return nil, false
	}
	if code, msg := scanJSON(data, v.maxDepth); code != "" {
		add(Request, "body", code, msg)
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		add(Request, "body", CodeMalformedJSON, "is not a valid JSON object")
		return nil, false
	}
	if !v.allowUnknownBody {
		for k := range fields {
			if !p.bodyNames[k] {
				add(Body, safeName(k), CodeUnknownField, "is not an accepted field")
			}
		}
	}
	return fields, true
}

// scanJSON checks that data is exactly one JSON object with no duplicate keys at
// any depth and no more than maxDepth levels of nesting.
func scanJSON(data []byte, maxDepth int) (code, msg string) {
	type frame struct {
		object    bool
		expectKey bool
		seen      map[string]struct{}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var stack []frame
	first := true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return CodeMalformedJSON, "is not a valid JSON object"
		}
		if first {
			first = false
			if d, ok := tok.(json.Delim); !ok || d != '{' {
				return CodeMalformedJSON, "must be a JSON object"
			}
		} else if len(stack) == 0 {
			return CodeMalformedJSON, "has data after the JSON object"
		}

		// A key or a value, depending on where we are.
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:n-1]
				if n-1 > 0 && stack[n-2].object {
					stack[n-2].expectKey = true
				}
				continue
			}
			key, ok := tok.(string)
			if !ok {
				return CodeMalformedJSON, "is not a valid JSON object"
			}
			// encoding/json matches keys case-insensitively when it decodes into a
			// struct, so "sku" and "SKU" would silently overwrite each other. Treat
			// keys that differ only by case as the same key.
			folded := foldKey(key)
			if _, dup := stack[n-1].seen[folded]; dup {
				return CodeDuplicate, "has a repeated key (keys that differ only by letter case count as the same key)"
			}
			stack[n-1].seen[folded] = struct{}{}
			stack[n-1].expectKey = false
			continue
		}

		switch d := tok.(type) {
		case json.Delim:
			switch d {
			case '{', '[':
				if len(stack) >= maxDepth {
					return CodeTooDeep, fmt.Sprintf("is nested more than %d levels", maxDepth)
				}
				f := frame{object: d == '{', expectKey: d == '{'}
				if f.object {
					f.seen = map[string]struct{}{}
				}
				stack = append(stack, f)
			case '}', ']':
				n := len(stack)
				stack = stack[:n-1]
				if n-1 > 0 && stack[n-2].object {
					stack[n-2].expectKey = true
				}
			}
		default: // a scalar value
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].expectKey = true
			}
		}
	}
	if len(stack) != 0 || first {
		return CodeMalformedJSON, "is not a valid JSON object"
	}
	return "", ""
}

func (v *Validator) bindBody(f *fieldPlan, fv reflect.Value, body map[string]json.RawMessage, add func(Source, string, string, string)) bool {
	raw, ok := body[f.name]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false
	}
	if f.elem == durationType {
		return v.bindBodyDuration(f, fv, raw, add)
	}
	target := reflect.New(f.typ)
	dec := json.NewDecoder(bytes.NewReader(raw))
	if !v.allowUnknownBody {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(target.Interface()); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			add(Body, f.shown, CodeUnknownField, "contains a field that is not accepted")
		} else {
			add(Body, f.shown, CodeType, "has the wrong type or form")
		}
		return false
	}
	fv.Set(target.Elem())
	return true
}

// bindBodyDuration reads a duration given as a JSON string such as "30s"; a bare
// number is refused because its unit would be ambiguous.
func (v *Validator) bindBodyDuration(f *fieldPlan, fv reflect.Value, raw json.RawMessage, add func(Source, string, string, string)) bool {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		add(Body, f.shown, CodeType, "must be a duration string such as 30s")
		return false
	}
	d, err := parseScalar(durationType, s)
	if err != nil {
		add(Body, f.shown, CodeType, err.Error())
		return false
	}
	setScalar(f, fv, d)
	return true
}

// ---- query, header, path ----

func (v *Validator) bindStrings(f *fieldPlan, fv reflect.Value, vals []string, add func(Source, string, string, string)) bool {
	// An empty value counts as absent.
	nonEmpty := vals[:0:0]
	for _, s := range vals {
		if s != "" {
			nonEmpty = append(nonEmpty, s)
		}
	}
	if len(nonEmpty) == 0 {
		return false
	}
	vals = nonEmpty
	if !f.multi && len(vals) > 1 {
		add(f.source, f.shown, CodeDuplicate, "must appear only once")
		return false
	}
	for _, s := range vals {
		if len(s) > v.maxValue {
			add(f.source, f.shown, CodeMax, fmt.Sprintf("is longer than %d bytes", v.maxValue))
			return false
		}
	}

	if f.multi {
		slice := reflect.MakeSlice(base(f.typ), 0, len(vals))
		for _, s := range vals {
			item, err := parseScalar(f.elem, s)
			if err != nil {
				add(f.source, f.shown, CodeType, err.Error())
				return false
			}
			slice = reflect.Append(slice, item)
		}
		fv.Set(slice)
		return true
	}
	item, err := parseScalar(f.elem, vals[0])
	if err != nil {
		add(f.source, f.shown, CodeType, err.Error())
		return false
	}
	setScalar(f, fv, item)
	return true
}

func setScalar(f *fieldPlan, fv reflect.Value, item reflect.Value) {
	if f.isPtr {
		p := reflect.New(f.elem)
		p.Elem().Set(item)
		fv.Set(p)
		return
	}
	fv.Set(item)
}

// ---- checks ----

func normalizeValue(fv reflect.Value, ops []string) {
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return
		}
		fv = fv.Elem()
	}
	switch {
	case fv.Kind() == reflect.String && fv.CanSet():
		fv.SetString(normalize(fv.String(), ops))
	case fv.Kind() == reflect.Slice:
		for i := 0; i < fv.Len(); i++ {
			normalizeValue(fv.Index(i), ops)
		}
	}
}

// checkField validates one present value: text hygiene, the rules, the rules for
// each element, and nested structs.
func (v *Validator) checkField(src Source, name string, fv reflect.Value, rs, each *ruleSet, multiline bool, add func(Source, string, string, string)) {
	for fv.Kind() == reflect.Pointer {
		if fv.IsNil() {
			return
		}
		fv = fv.Elem()
	}
	allow := rs.allowControl
	switch fv.Kind() {
	case reflect.String:
		if !allow && !cleanText(fv.String(), multiline) {
			add(src, name, CodeControl, "must be valid text without control characters")
			return
		}
		if rs.required && fv.String() == "" {
			add(src, name, CodeRequired, "is required")
			return
		}
	case reflect.Slice:
		for i := 0; i < fv.Len(); i++ {
			el := fv.Index(i)
			for el.Kind() == reflect.Pointer && !el.IsNil() {
				el = el.Elem()
			}
			if el.Kind() == reflect.String && !allow && !(each != nil && each.allowControl) && !cleanText(el.String(), multiline) {
				add(src, indexName(name, i), CodeControl, "must be valid text without control characters")
				return
			}
		}
	}
	if code, msg := rs.check(fv); code != "" {
		add(src, name, code, msg) // keep going: report the elements and nested fields too
	}
	if fv.Kind() == reflect.Slice {
		if each != nil {
			for i := 0; i < fv.Len(); i++ {
				el := fv.Index(i)
				for el.Kind() == reflect.Pointer {
					if el.IsNil() {
						break
					}
					el = el.Elem()
				}
				if el.Kind() == reflect.Pointer {
					continue
				}
				if code, msg := each.check(el); code != "" {
					add(src, indexName(name, i), code, msg)
				}
			}
		}
	}
	if rs.dive {
		v.dive(src, name, fv, multiline, add)
	}
}

func (v *Validator) dive(src Source, name string, fv reflect.Value, multiline bool, add func(Source, string, string, string)) {
	switch fv.Kind() {
	case reflect.Slice:
		for i := 0; i < fv.Len(); i++ {
			el := fv.Index(i)
			for el.Kind() == reflect.Pointer && !el.IsNil() {
				el = el.Elem()
			}
			if el.Kind() == reflect.Struct {
				v.diveStruct(src, indexName(name, i), el, multiline, add)
			}
		}
	case reflect.Struct:
		v.diveStruct(src, name, fv, multiline, add)
	}
}

func (v *Validator) diveStruct(src Source, path string, sv reflect.Value, multiline bool, add func(Source, string, string, string)) {
	np, err := v.nestedFor(sv.Type())
	if err != nil {
		add(src, path, CodeRule, "cannot be validated") // a setup error would have been reported at plan time
		return
	}
	for _, nf := range np.fields {
		fv := sv.FieldByIndex(nf.index)
		name := path + "." + nf.name
		if nf.norm != nil {
			normalizeValue(fv, nf.norm)
		}
		if isMissing(fv) {
			if nf.rules.required {
				add(src, name, CodeRequired, "is required")
			}
			continue
		}
		v.checkField(src, name, fv, nf.rules, nf.each, multiline, add)
	}
}

// isMissing reports whether a nested value was not provided: nil pointers, empty
// strings, and nil or empty slices and maps.
func isMissing(fv reflect.Value) bool {
	switch fv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return fv.IsNil()
	case reflect.String:
		return fv.Len() == 0
	case reflect.Slice, reflect.Map:
		return fv.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return fv.IsZero()
	}
	return false
}

// foldKey maps a key to a form where keys that encoding/json would consider equal
// (Unicode simple case folding) are identical.
func foldKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		low := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < low {
				low = f
			}
		}
		b.WriteRune(low)
	}
	return b.String()
}

func indexName(name string, i int) string { return name + "[" + strconv.Itoa(i) + "]" }

// safeName makes an attacker-supplied key fit to show back: bounded and printable.
func safeName(s string) string {
	if !cleanText(s, false) {
		return "?"
	}
	if r := []rune(s); len(r) > 64 {
		return string(r[:64]) + "..."
	}
	return s
}
