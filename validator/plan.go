package validator

import (
	"net/textproto"
	"reflect"
	"strings"
)

type fieldPlan struct {
	index  []int
	goName string
	source Source
	name   string // the name used to look the value up (canonical for headers)
	shown  string // the name as declared in the tag, used in errors
	typ    reflect.Type
	isPtr  bool
	multi  bool         // slice of scalars from query or header
	elem   reflect.Type // scalar type to convert request strings to
	rules  *ruleSet
	each   *ruleSet
	norm   []string
	def    string
	hasDef bool
}

type plan struct {
	fields     []*fieldPlan
	bodyNames  map[string]bool
	queryNames map[string]bool
	hasBody    bool
}

// nestedField is a field inside a struct reached through dive.
type nestedField struct {
	index []int
	name  string
	typ   reflect.Type
	rules *ruleSet
	each  *ruleSet
	norm  []string
}

type nestedPlan struct {
	fields []*nestedField
}

func (v *Validator) buildPlan(t reflect.Type) (*plan, error) {
	p := &plan{bodyNames: map[string]bool{}, queryNames: map[string]bool{}}
	seen := map[string]bool{}
	if err := v.collect(t, nil, p, seen); err != nil {
		return nil, err
	}
	return p, nil
}

var sourceTags = []struct {
	tag    string
	source Source
}{{"body", Body}, {"query", Query}, {"path", Path}, {"header", Header}}

func (v *Validator) collect(t reflect.Type, prefix []int, p *plan, seen map[string]bool) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		index := append(append([]int{}, prefix...), i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			if _, tagged := firstSourceTag(f); tagged {
				return setup("embedded field %s cannot have a source tag", f.Name)
			}
			if err := v.collect(f.Type, index, p, seen); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		src, name, err := sourceOf(f)
		if err != nil {
			return err
		}
		if src == "" {
			if f.Tag.Get("validate") != "" || f.Tag.Get("normalize") != "" || f.Tag.Get("default") != "" {
				return setup("field %s has validation tags but no body, query, path or header tag, so it is never bound", f.Name)
			}
			continue
		}
		fp := &fieldPlan{index: index, goName: f.Name, source: src, name: name, shown: name, typ: f.Type}
		if src == Header {
			fp.name = textproto.CanonicalMIMEHeaderKey(name)
		}
		if key := string(src) + ":" + fp.name; seen[key] {
			return setup("%s %q is bound by more than one field", src, fp.name)
		} else {
			seen[key] = true
		}
		if err := v.describe(fp, f); err != nil {
			return err
		}
		p.fields = append(p.fields, fp)
		switch src {
		case Body:
			p.hasBody = true
			p.bodyNames[fp.name] = true
		case Query:
			p.queryNames[fp.name] = true
		}
	}
	return nil
}

func firstSourceTag(f reflect.StructField) (Source, bool) {
	for _, s := range sourceTags {
		if _, ok := f.Tag.Lookup(s.tag); ok {
			return s.source, true
		}
	}
	return "", false
}

func sourceOf(f reflect.StructField) (Source, string, error) {
	var src Source
	var name string
	for _, s := range sourceTags {
		if v, ok := f.Tag.Lookup(s.tag); ok {
			if src != "" {
				return "", "", setup("field %s has more than one of the body, query, path and header tags", f.Name)
			}
			if v == "" || strings.ContainsAny(v, ", \t") {
				return "", "", setup("field %s has an invalid %s name", f.Name, s.tag)
			}
			src, name = s.source, v
		}
	}
	return src, name, nil
}

func (v *Validator) describe(fp *fieldPlan, f reflect.StructField) error {
	t := f.Type
	fp.isPtr = t.Kind() == reflect.Pointer
	bt := base(t)

	switch fp.source {
	case Body:
		switch bt.Kind() {
		case reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
			return setup("field %s: %s cannot be decoded from JSON", f.Name, t)
		}
		fp.elem = bt
	default: // query, header, path
		if bt.Kind() == reflect.Slice && bt != durationType {
			if fp.source == Path {
				return setup("field %s: a path value cannot be a slice", f.Name)
			}
			if fp.isPtr || !supportedScalar(bt.Elem()) {
				return setup("field %s: %s is not a supported repeated %s value (use a slice of strings, numbers, bools, durations or times)", f.Name, t, fp.source)
			}
			fp.multi = true
			fp.elem = bt.Elem()
		} else {
			if !supportedScalar(bt) {
				return setup("field %s: %s is not a supported %s type", f.Name, t, fp.source)
			}
			fp.elem = bt
		}
	}

	rules, err := v.compile(f.Tag.Get("validate"), t)
	if err != nil {
		return setup("field %s: %v", f.Name, trimSetup(err))
	}
	fp.rules = rules
	if each := f.Tag.Get("each"); each != "" {
		if bt.Kind() != reflect.Slice {
			return setup("field %s: each needs a slice field", f.Name)
		}
		fp.each, err = v.compile(each, bt.Elem())
		if err != nil {
			return setup("field %s each: %v", f.Name, trimSetup(err))
		}
		if fp.each.required || fp.each.dive && base(bt.Elem()).Kind() != reflect.Struct {
			return setup("field %s: each cannot use required or dive on this element type", f.Name)
		}
	}
	if rules.dive {
		if _, err := v.nestedFor(base(elemOf(bt))); err != nil {
			return err
		}
	}
	if n := f.Tag.Get("normalize"); n != "" {
		ops, ok := validNormalize(n)
		if !ok || !isStringish(bt) {
			return setup("field %s: normalize supports trim, lower and upper on string fields", f.Name)
		}
		fp.norm = ops
	}
	if d, ok := f.Tag.Lookup("default"); ok {
		if rules.required {
			return setup("field %s: a field cannot be both required and have a default", f.Name)
		}
		if fp.source == Body || fp.multi {
			return setup("field %s: default is supported for scalar query, header and path fields only", f.Name)
		}
		if _, err := parseScalar(fp.elem, d); err != nil {
			return setup("field %s: the default value is not valid for %s", f.Name, t)
		}
		fp.def, fp.hasDef = d, true
	}
	return nil
}

func trimSetup(err error) string {
	return strings.TrimPrefix(err.Error(), ErrSetup.Error()+": ")
}

func elemOf(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Slice {
		return t.Elem()
	}
	return t
}

func isStringish(t reflect.Type) bool {
	return t.Kind() == reflect.String || (t.Kind() == reflect.Slice && base(t.Elem()).Kind() == reflect.String)
}

// nestedFor returns the validation plan for struct type t, building it once.
func (v *Validator) nestedFor(t reflect.Type) (*nestedPlan, error) {
	if t.Kind() != reflect.Struct {
		return nil, setup("dive needs a struct type, not %s", t)
	}
	if cached, ok := v.nested.Load(t); ok {
		return cached.(*nestedPlan), nil
	}
	np := &nestedPlan{}
	v.nested.Store(t, np) // stored first so a type that contains itself terminates
	if err := v.collectNested(t, nil, np); err != nil {
		v.nested.Delete(t)
		return nil, err
	}
	return np, nil
}

func (v *Validator) collectNested(t reflect.Type, prefix []int, np *nestedPlan) error {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		index := append(append([]int{}, prefix...), i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			if err := v.collectNested(f.Type, index, np); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if j := strings.Split(f.Tag.Get("json"), ",")[0]; j != "" && j != "-" {
			name = j
		}
		rules, err := v.compile(f.Tag.Get("validate"), f.Type)
		if err != nil {
			return setup("nested field %s.%s: %v", t.Name(), f.Name, trimSetup(err))
		}
		nf := &nestedField{index: index, name: name, typ: f.Type, rules: rules}
		if each := f.Tag.Get("each"); each != "" {
			if base(f.Type).Kind() != reflect.Slice {
				return setup("nested field %s.%s: each needs a slice field", t.Name(), f.Name)
			}
			if nf.each, err = v.compile(each, base(f.Type).Elem()); err != nil {
				return setup("nested field %s.%s each: %v", t.Name(), f.Name, trimSetup(err))
			}
		}
		if n := f.Tag.Get("normalize"); n != "" {
			ops, ok := validNormalize(n)
			if !ok || !isStringish(base(f.Type)) {
				return setup("nested field %s.%s: normalize supports trim, lower and upper on string fields", t.Name(), f.Name)
			}
			nf.norm = ops
		}
		if rules.required && (isNumber(base(f.Type)) || base(f.Type).Kind() == reflect.Bool) && f.Type.Kind() != reflect.Pointer {
			return setup("nested field %s.%s: required on a number or bool needs a pointer type, because zero cannot be told from missing", t.Name(), f.Name)
		}
		if rules.dive {
			if _, err := v.nestedFor(base(elemOf(base(f.Type)))); err != nil {
				return err
			}
		}
		np.fields = append(np.fields, nf)
	}
	return nil
}
