package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
	"unsafe"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	scannerType = reflect.TypeFor[sql.Scanner]()
	timeType    = reflect.TypeFor[time.Time]()
	mapType     = reflect.TypeFor[map[string]any]()
)

// OIDs of the json and jsonb types.
const (
	oidJSON  = 114
	oidJSONB = 3802
)

// isStruct reports whether t is filled column by column. time.Time and types
// that scan themselves (sql.Scanner, such as pgtype.Numeric) are single values.
func isStruct(t reflect.Type) bool {
	return t.Kind() == reflect.Struct && t != timeType && !reflect.PointerTo(t).Implements(scannerType)
}

// field locates the struct field a column fills.
type field struct {
	path     []int // index path from the struct, through embedded structs
	nullZero bool  // db:",nullzero": a NULL column leaves the zero value instead of failing
}

var plans sync.Map // reflect.Type -> map[string]field

// embeddable reports whether an anonymous field of type f is flattened into the
// parent: a struct, or a pointer to a struct (allocated when a column needs it).
func embeddable(f reflect.StructField) bool {
	t := f.Type
	if t.Kind() == reflect.Pointer {
		return isStruct(t.Elem())
	}
	return isStruct(t)
}

func structOf(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		return t.Elem()
	}
	return t
}

// fieldPlan maps lower-case column names to the field index path in struct type t.
// A field matches a column by its `db:"name"` tag, or else by its Go name compared
// case-insensitively (ID matches id) or in snake_case (UserID matches user_id).
// `db:"-"` skips a field; unexported fields are ignored; embedded structs are
// flattened. A field declared on the outer struct wins over an embedded one.
func fieldPlan(t reflect.Type) map[string]field {
	if p, ok := plans.Load(t); ok {
		return p.(map[string]field)
	}
	plan := map[string]field{}
	buildPlan(t, nil, plan)
	plans.Store(t, plan)
	return plan
}

func buildPlan(t reflect.Type, prefix []int, plan map[string]field) {
	var embedded []reflect.StructField
	// Tagged names first, then untagged, then embedded, so the clearest match wins.
	for pass := 0; pass < 2; pass++ {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag, hasTag := f.Tag.Lookup("db")
			name, options, _ := strings.Cut(tag, ",")
			nullZero := hasOption(options, "nullzero")
			if name == "-" || (f.Anonymous && embeddable(f) && !hasTag) {
				if pass == 0 && f.Anonymous && embeddable(f) && name != "-" {
					embedded = append(embedded, f)
				}
				continue
			}
			if !f.IsExported() {
				continue
			}
			fld := field{path: append(append([]int{}, prefix...), i), nullZero: nullZero}
			switch {
			case pass == 0 && name != "":
				setIfAbsent(plan, strings.ToLower(name), fld)
			case pass == 1 && name == "":
				setIfAbsent(plan, strings.ToLower(f.Name), fld)
				setIfAbsent(plan, snake(f.Name), fld)
			}
		}
	}
	for _, f := range embedded {
		sub := map[string]field{}
		buildPlan(structOf(f.Type), append(append([]int{}, prefix...), f.Index...), sub)
		for k, v := range sub {
			setIfAbsent(plan, k, v)
		}
	}
}

func hasOption(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if strings.TrimSpace(o) == want {
			return true
		}
	}
	return false
}

func setIfAbsent(m map[string]field, k string, v field) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

// snake converts a Go name to snake_case: UserID -> user_id, HTTPServer -> http_server.
func snake(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) {
			prevLowerOrDigit := i > 0 && (unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1]))
			acronymEnd := i > 0 && unicode.IsUpper(r[i-1]) && i+1 < len(r) && unicode.IsLower(r[i+1])
			if prevLowerOrDigit || acronymEnd {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(c))
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// settable returns a settable view of v. An embedded pointer to an unexported
// struct type is itself unexported, so reflect refuses to allocate it; reflect.NewAt
// on the field's own address is the documented way around that, and it only ever
// touches memory inside the struct being scanned.
func settable(v reflect.Value) reflect.Value {
	if v.CanSet() || !v.CanAddr() {
		return v
	}
	return reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem()
}

// fieldByIndexAlloc returns the field at path, allocating any nil embedded
// pointer on the way.
func fieldByIndexAlloc(v reflect.Value, path []int) reflect.Value {
	for _, i := range path {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				settable(v).Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = settable(v.Field(i))
	}
	return v
}

// scanStruct scans the current row into v, an addressable struct. Columns with no
// matching field are discarded; fields with no matching column keep their value.
// If the query returns a single json or jsonb column that matches no field, the
// whole value is decoded into v with encoding/json instead.
func scanStruct(rows pgx.Rows, v reflect.Value) error {
	plan := fieldPlan(v.Type())
	fds := rows.FieldDescriptions()
	if len(fds) == 1 && (fds[0].DataTypeOID == oidJSON || fds[0].DataTypeOID == oidJSONB) {
		if _, matched := plan[strings.ToLower(fds[0].Name)]; !matched {
			return rows.Scan(v.Addr().Interface())
		}
	}
	targets := make([]any, len(fds))
	var after []func()
	filled := map[string]string{} // field path -> first column that filled it
	for i, fd := range fds {
		f, ok := plan[strings.ToLower(fd.Name)]
		if !ok {
			targets[i] = new(any)
			continue
		}
		key := fmt.Sprint(f.path)
		if first, dup := filled[key]; dup {
			return fmt.Errorf("postgres: columns %q and %q both map to the same field; alias one of them in the query", first, fd.Name)
		}
		filled[key] = fd.Name
		target, fn := scanTarget(fieldByIndexAlloc(v, f.path), f.nullZero, fd.DataTypeOID, rows.Conn())
		targets[i] = target
		if fn != nil {
			after = append(after, fn)
		}
	}
	if err := rows.Scan(targets...); err != nil {
		return err
	}
	for _, fn := range after {
		fn()
	}
	return nil
}

// canBeNull reports whether pgx can already scan SQL NULL into a field of type t.
func canBeNull(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice:
		return true
	}
	return reflect.PointerTo(t).Implements(scannerType)
}

// scanTarget returns what to hand to rows.Scan for field fv, plus an optional step
// to run after a successful scan.
func scanTarget(fv reflect.Value, nullZero bool, oid uint32, conn *pgx.Conn) (any, func()) {
	t := fv.Type()
	// A composite column fills a struct (or *struct) field by attribute name.
	if conn != nil && (isStruct(t) || (t.Kind() == reflect.Pointer && isStruct(t.Elem()))) {
		if typ, ok := conn.TypeMap().TypeForOID(oid); ok {
			if cc, ok := typ.Codec.(*pgtype.CompositeCodec); ok {
				return &compositeTarget{dest: fv, fields: cc.Fields, nullZero: nullZero}, nil
			}
		}
	}
	if nullZero && !canBeNull(t) {
		tmp := reflect.New(reflect.PointerTo(t)) // **T: pgx stores nil for NULL
		return tmp.Interface(), func() {
			if p := tmp.Elem(); !p.IsNil() {
				fv.Set(p.Elem())
			} else {
				fv.Set(reflect.Zero(t))
			}
		}
	}
	return fv.Addr().Interface(), nil
}

// compositeTarget scans a Postgres composite value into a struct field, matching
// the composite's attribute names to struct fields with the same rules as columns.
type compositeTarget struct {
	dest     reflect.Value // the struct field, or a *struct field
	fields   []pgtype.CompositeCodecField
	nullZero bool
}

func (c *compositeTarget) structValue() reflect.Value {
	if c.dest.Kind() == reflect.Pointer {
		if c.dest.IsNil() {
			c.dest.Set(reflect.New(c.dest.Type().Elem()))
		}
		return c.dest.Elem()
	}
	return c.dest
}

func (c *compositeTarget) ScanNull() error {
	if c.dest.Kind() != reflect.Pointer && !c.nullZero {
		return fmt.Errorf("postgres: cannot scan a NULL composite into the non-pointer field of type %s; use a pointer or db:\",nullzero\"", c.dest.Type())
	}
	c.dest.Set(reflect.Zero(c.dest.Type()))
	return nil
}

func (c *compositeTarget) ScanIndex(i int) any {
	sv := c.structValue()
	f, ok := fieldPlan(sv.Type())[strings.ToLower(c.fields[i].Name)]
	if !ok {
		return new(any)
	}
	return fieldByIndexAlloc(sv, f.path).Addr().Interface()
}

// scanOne scans the current row into target (addressable): a struct, a pointer
// to a struct (allocated), or a single-column value.
func scanOne(rows pgx.Rows, target reflect.Value) error {
	t := target.Type()
	switch {
	case t == mapType:
		values, err := rows.Values()
		if err != nil {
			return err
		}
		m := make(map[string]any, len(values))
		for i, fd := range rows.FieldDescriptions() {
			m[fd.Name] = values[i]
		}
		target.Set(reflect.ValueOf(m))
		return nil
	case isStruct(t):
		return scanStruct(rows, target)
	case t.Kind() == reflect.Pointer && isStruct(t.Elem()):
		p := reflect.New(t.Elem())
		if err := scanStruct(rows, p.Elem()); err != nil {
			return err
		}
		target.Set(p)
		return nil
	default:
		return rows.Scan(target.Addr().Interface())
	}
}

func requirePointer(dest any, what string) (reflect.Value, error) {
	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return reflect.Value{}, fmt.Errorf("postgres: dest must be a non-nil pointer to %s, got %T", what, dest)
	}
	return v.Elem(), nil
}

// getInto runs the query and scans its single row into dest.
func getInto(ctx context.Context, q Querier, dest any, sql string, args []any) error {
	target, err := requirePointer(dest, "a struct or value")
	if err != nil {
		return err
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrNotFound
	}
	if err := scanOne(rows, target); err != nil {
		return err
	}
	if rows.Next() {
		return ErrTooManyRows
	}
	return rows.Err()
}

// selectInto runs the query and appends every row to the slice dest points to.
func selectInto(ctx context.Context, q Querier, dest any, sql string, args []any) error {
	slice, err := requirePointer(dest, "a slice")
	if err != nil {
		return err
	}
	if slice.Kind() != reflect.Slice {
		return fmt.Errorf("postgres: dest must be a pointer to a slice, got %T", dest)
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := reflect.MakeSlice(slice.Type(), 0, 0) // empty, never nil
	for rows.Next() {
		elem := reflect.New(slice.Type().Elem()).Elem()
		if err := scanOne(rows, elem); err != nil {
			return err
		}
		out = reflect.Append(out, elem)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	slice.Set(out)
	return nil
}
