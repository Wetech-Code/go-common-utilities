package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var durationType = reflect.TypeOf(time.Duration(0))

// Load fills the exported fields of the struct v points to from the settings,
// using struct tags. Every problem is recorded like the other readers, so one
// Err() reports them all, and a field whose setting is invalid keeps its zero
// value (or its default).
//
//	type Config struct {
//		Port     int           `env:"PORT" default:"8080" min:"1" max:"65535"`
//		DSN      string        `env:"DATABASE_URL" required:"true"`
//		LogLevel string        `env:"LOG_LEVEL" default:"info" enum:"debug,info,warn,error"`
//		Timeout  time.Duration `env:"TIMEOUT" default:"30s" max:"5m"`
//		Origins  []string      `env:"ALLOWED_ORIGINS"`
//		Labels   map[string]string `env:"LABELS"`
//		Redis    struct {
//			URL string `env:"URL" required:"true"`   // read as REDIS_URL
//		} `envPrefix:"REDIS_"`
//	}
//	var cfg Config
//	r := config.New()
//	r.Load(&cfg)
//	if err := r.Err(); err != nil { ... }
//
// Tags:
//
//	env       the variable name (the Reader's prefix is added). Fields without it are
//	          skipped, except structs, which are searched.
//	default   the value when the variable is unset or empty
//	required  "true": record ErrMissing when unset and there is no default
//	min, max  inclusive bounds for integers, floats and durations
//	enum      comma-separated allowed values for strings
//	envPrefix on a struct field: added to the names of the fields inside it
//
// Supported field types: string, bool, int and uint of every size, float32,
// float64, time.Duration, []string (comma-separated), map[string]string
// (key=value pairs) and nested structs. Any other tagged type records ErrInvalid.
// v must be a non-nil pointer to a struct.
func (r *Reader) Load(v any) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		r.errs = append(r.errs, fmt.Errorf("%w: Load needs a non-nil pointer to a struct", ErrInvalid))
		return
	}
	r.loadStruct(rv.Elem())
}

func (r *Reader) loadStruct(sv reflect.Value) {
	st := sv.Type()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := sv.Field(i)
		key, tagged := f.Tag.Lookup("env")
		if !tagged || key == "" {
			if f.Type.Kind() == reflect.Struct {
				old := r.prefix
				r.prefix += f.Tag.Get("envPrefix")
				r.loadStruct(fv)
				r.prefix = old
			}
			continue
		}

		raw, ok := r.get(key)
		if !ok {
			def, hasDef := f.Tag.Lookup("default")
			if !hasDef {
				if f.Tag.Get("required") == "true" {
					r.errs = append(r.errs, fmt.Errorf("%w: %s", ErrMissing, r.prefix+key))
				}
				continue
			}
			raw = def
		}
		r.setField(key, fv, f, raw)
	}
}

func (r *Reader) setField(key string, fv reflect.Value, f reflect.StructField, raw string) {
	bad := func(want string) { r.badValue(key, want) }
	tag := f.Tag
	switch fv.Kind() {
	case reflect.String:
		if enum, ok := tag.Lookup("enum"); ok {
			allowed := splitList(enum)
			if !contains(allowed, raw) {
				bad("one of " + strings.Join(allowed, ", "))
				return
			}
		}
		fv.SetString(raw)

	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			bad("a boolean")
			return
		}
		fv.SetBool(b)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if fv.Type() == durationType {
			d, err := time.ParseDuration(raw)
			if err != nil {
				bad("a duration such as 30s")
				return
			}
			if !r.inRange(key, tag, float64(d), func(x float64) string { return time.Duration(x).String() }, "a duration") {
				return
			}
			fv.SetInt(int64(d))
			return
		}
		n, err := strconv.ParseInt(raw, 10, fv.Type().Bits())
		if err != nil {
			bad("an integer")
			return
		}
		if !r.inRange(key, tag, float64(n), func(x float64) string { return strconv.FormatInt(int64(x), 10) }, "an integer") {
			return
		}
		fv.SetInt(n)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(raw, 10, fv.Type().Bits())
		if err != nil {
			bad("a non-negative integer")
			return
		}
		if !r.inRange(key, tag, float64(n), func(x float64) string { return strconv.FormatUint(uint64(x), 10) }, "an integer") {
			return
		}
		fv.SetUint(n)

	case reflect.Float32, reflect.Float64:
		x, err := strconv.ParseFloat(raw, fv.Type().Bits())
		if err != nil {
			bad("a number")
			return
		}
		if !r.inRange(key, tag, x, func(x float64) string { return strconv.FormatFloat(x, 'g', -1, 64) }, "a number") {
			return
		}
		fv.SetFloat(x)

	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.String {
			r.unsupported(key, fv)
			return
		}
		fv.Set(reflect.ValueOf(splitList(raw)).Convert(fv.Type()))

	case reflect.Map:
		if fv.Type().Key().Kind() != reflect.String || fv.Type().Elem().Kind() != reflect.String {
			r.unsupported(key, fv)
			return
		}
		m, ok := parseMap(raw)
		if !ok {
			bad("comma-separated key=value pairs")
			return
		}
		out := reflect.MakeMapWithSize(fv.Type(), len(m))
		for k, v := range m {
			out.SetMapIndex(reflect.ValueOf(k).Convert(fv.Type().Key()), reflect.ValueOf(v).Convert(fv.Type().Elem()))
		}
		fv.Set(out)

	default:
		r.unsupported(key, fv)
	}
}

// inRange applies the min and max tags to x. It records ErrInvalid and returns
// false when x is outside them or a bound cannot be parsed. For durations the
// bounds are written as durations ("5m"), otherwise as numbers.
func (r *Reader) inRange(key string, tag reflect.StructTag, x float64, show func(float64) string, noun string) bool {
	parse := func(s string) (float64, bool) {
		if noun == "a duration" {
			d, err := time.ParseDuration(s)
			return float64(d), err == nil
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	minS, hasMin := tag.Lookup("min")
	maxS, hasMax := tag.Lookup("max")
	var lo, hi float64
	var ok bool
	if hasMin {
		if lo, ok = parse(minS); !ok {
			r.errs = append(r.errs, fmt.Errorf("%w: the min tag of %s is not %s", ErrInvalid, r.prefix+key, noun))
			return false
		}
	}
	if hasMax {
		if hi, ok = parse(maxS); !ok {
			r.errs = append(r.errs, fmt.Errorf("%w: the max tag of %s is not %s", ErrInvalid, r.prefix+key, noun))
			return false
		}
	}
	switch {
	case hasMin && hasMax && (x < lo || x > hi):
		r.badValue(key, noun+" from "+show(lo)+" to "+show(hi))
	case hasMin && !hasMax && x < lo:
		r.badValue(key, noun+" of at least "+show(lo))
	case hasMax && !hasMin && x > hi:
		r.badValue(key, noun+" of at most "+show(hi))
	default:
		return true
	}
	return false
}

func (r *Reader) unsupported(key string, fv reflect.Value) {
	r.errs = append(r.errs, fmt.Errorf("%w: %s has the unsupported type %s", ErrInvalid, r.prefix+key, fv.Type()))
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
