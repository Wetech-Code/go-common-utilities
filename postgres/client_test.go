package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestErrorHelpers(t *testing.T) {
	pgErr := func(code, constraint string) error {
		return fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code, ConstraintName: constraint})
	}
	tests := []struct {
		name string
		err  error
		is   func(error) bool
		code string
	}{
		{"unique", pgErr(CodeUniqueViolation, "users_email_key"), IsUniqueViolation, "23505"},
		{"foreign key", pgErr(CodeForeignKeyViolation, "fk"), IsForeignKeyViolation, "23503"},
		{"not null", pgErr(CodeNotNullViolation, ""), IsNotNullViolation, "23502"},
		{"check", pgErr(CodeCheckViolation, "chk"), IsCheckViolation, "23514"},
		{"serialization", pgErr(CodeSerializationFailure, ""), IsSerializationFailure, "40001"},
		{"deadlock", pgErr(CodeDeadlockDetected, ""), IsDeadlock, "40P01"},
	}
	for _, tc := range tests {
		if !tc.is(tc.err) || ErrorCode(tc.err) != tc.code {
			t.Errorf("%s: is=%v code=%q", tc.name, tc.is(tc.err), ErrorCode(tc.err))
		}
		// Each helper must reject every other code.
		for _, other := range tests {
			if other.name != tc.name && tc.is(other.err) {
				t.Errorf("%s helper accepted a %s error", tc.name, other.name)
			}
		}
	}
	if got := ConstraintName(pgErr(CodeUniqueViolation, "users_email_key")); got != "users_email_key" {
		t.Errorf("ConstraintName = %q", got)
	}
	plain := errors.New("not a server error")
	if ErrorCode(plain) != "" || IsUniqueViolation(plain) || ConstraintName(plain) != "" || IsUniqueViolation(nil) {
		t.Error("non-server errors must give empty code and false")
	}
}

type fakeRow struct{ err error }

func (f fakeRow) Scan(...any) error { return f.err }

func TestRowScanMapsNoRows(t *testing.T) {
	if err := (Row{fakeRow{pgx.ErrNoRows}}).Scan(); !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
	boom := errors.New("boom")
	if err := (Row{fakeRow{boom}}).Scan(); err != boom {
		t.Errorf("other errors must pass through, got %v", err)
	}
	if err := (Row{fakeRow{nil}}).Scan(); err != nil {
		t.Errorf("nil must stay nil, got %v", err)
	}
}

func TestValidationHappensBeforeAnyConnection(t *testing.T) {
	c := &Client{} // no pool: these must fail on their own arguments, not panic
	ctx := context.Background()
	if err := c.Notify(ctx, "", "x"); err == nil {
		t.Error("Notify with an empty channel should fail")
	}
	if _, err := c.Listen(ctx); err == nil {
		t.Error("Listen with no channels should fail")
	}
	if _, err := c.Listen(ctx, "ok", ""); err == nil {
		t.Error("Listen with an empty channel should fail")
	}
}

func TestWrapKeepsNativePool(t *testing.T) {
	pool := &pgxpool.Pool{}
	if Wrap(pool).Native != pool {
		t.Fatal("Native should be the wrapped pool")
	}
}

func TestEveryPgxPoolMethodIsWrapped(t *testing.T) {
	native := reflect.TypeOf(&pgxpool.Pool{})
	ours := reflect.TypeOf(&Client{})
	var missing []string
	for i := 0; i < native.NumMethod(); i++ {
		if _, ok := ours.MethodByName(native.Method(i).Name); !ok {
			missing = append(missing, native.Method(i).Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("pgxpool.Pool methods with no Client wrapper: %v", missing)
	}
}

func TestSnake(t *testing.T) {
	for in, want := range map[string]string{
		"ID": "id", "UserID": "user_id", "CreatedAt": "created_at", "HTTPServer": "http_server",
		"Address2": "address2", "FullName": "full_name", "URL": "url", "Name": "name", "OAuth2Token": "o_auth2_token",
	} {
		if got := snake(in); got != want {
			t.Errorf("snake(%q) = %q, want %q", in, got, want)
		}
	}
}

type stamps struct{ CreatedAt string }

type planned struct {
	stamps
	UserID    int64
	FullName  string `db:"name"`
	Skipped   string `db:"-"`
	hidden    string //nolint:unused
	Inner     *stamps
	CreatedAt string // outer field wins over the embedded one
}

func TestFieldPlan(t *testing.T) {
	plan := fieldPlan(reflect.TypeFor[planned]())
	cases := map[string]bool{
		"user_id": true, "userid": true, // snake_case and case-insensitive Go name
		"name":       true,  // tag
		"fullname":   false, // a tagged field is matched only by its tag
		"full_name":  false,
		"skipped":    false, // db:"-"
		"hidden":     false, // unexported
		"created_at": true,
	}
	for col, want := range cases {
		if _, ok := plan[col]; ok != want {
			t.Errorf("column %q matched=%v, want %v", col, ok, want)
		}
	}
	// The outer CreatedAt (index 6) must win over the embedded stamps.CreatedAt (path 0,0).
	if got := plan["created_at"].path; len(got) != 1 || got[0] != 6 {
		t.Errorf("created_at -> %v, want the outer field [6]", got)
	}
	if got := plan["user_id"].path; len(got) != 1 || got[0] != 1 {
		t.Errorf("user_id -> %v, want [1]", got)
	}
}

func TestIsStruct(t *testing.T) {
	if !isStruct(reflect.TypeFor[planned]()) {
		t.Error("a plain struct is filled by column name")
	}
	if isStruct(reflect.TypeFor[time.Time]()) {
		t.Error("time.Time is a single value")
	}
	if isStruct(reflect.TypeFor[sql.NullString]()) {
		t.Error("a sql.Scanner struct is a single value")
	}
	if isStruct(reflect.TypeFor[int]()) || isStruct(reflect.TypeFor[*planned]()) {
		t.Error("only struct kinds count")
	}
}

func TestDestMustBeAPointer(t *testing.T) {
	ctx := context.Background()
	c := &Client{} // validation runs before any connection is used
	var u planned
	if err := c.Get(ctx, u, "SELECT 1"); err == nil {
		t.Error("Get with a non-pointer should fail")
	}
	if err := c.Get(ctx, (*planned)(nil), "SELECT 1"); err == nil {
		t.Error("Get with a nil pointer should fail")
	}
	if err := c.Select(ctx, &u, "SELECT 1"); err == nil {
		t.Error("Select with a pointer to a non-slice should fail")
	}
	if err := c.Select(ctx, []planned{}, "SELECT 1"); err == nil {
		t.Error("Select with a non-pointer should fail")
	}
}

type Base struct{ CreatedAt string }

type unexportedBase struct{ Hidden string }

type withEmbeddedPointers struct {
	*Base
	*unexportedBase // unexported type: allocated through reflect.NewAt
	UserID          int64
}

func TestFieldPlanEmbeddedPointers(t *testing.T) {
	plan := fieldPlan(reflect.TypeFor[withEmbeddedPointers]())
	if got := plan["created_at"].path; len(got) != 2 || got[0] != 0 || got[1] != 0 {
		t.Errorf("created_at -> %v, want the path through the embedded pointer [0 0]", got)
	}
	if _, ok := plan["hidden"]; !ok {
		t.Error("an embedded pointer to an unexported type is flattened too")
	}
	if _, ok := plan["user_id"]; !ok {
		t.Error("user_id should match")
	}

	// fieldByIndexAlloc allocates the nil embedded pointer on the way.
	var v withEmbeddedPointers
	f := fieldByIndexAlloc(reflect.ValueOf(&v).Elem(), plan["created_at"].path)
	f.SetString("2026")
	if v.Base == nil || v.Base.CreatedAt != "2026" {
		t.Errorf("embedded pointer was not allocated: %#v", v.Base)
	}
	h := fieldByIndexAlloc(reflect.ValueOf(&v).Elem(), plan["hidden"].path)
	h.SetString("deep")
	if v.unexportedBase == nil || v.unexportedBase.Hidden != "deep" {
		t.Errorf("unexported embedded pointer was not allocated: %#v", v.unexportedBase)
	}
}

func TestFieldPlanTagOptionsAndCaseCollisions(t *testing.T) {
	type tagged struct {
		A int `db:"alpha,omitempty"`
		B int `db:",omitempty"` // empty name falls back to the Go name
		C int `db:"Gamma"`      // tags are matched case-insensitively
	}
	plan := fieldPlan(reflect.TypeFor[tagged]())
	for _, col := range []string{"alpha", "b", "gamma"} {
		if _, ok := plan[col]; !ok {
			t.Errorf("column %q should match", col)
		}
	}
	if _, ok := plan["a"]; ok {
		t.Error("a tagged field must not also match its Go name")
	}
}

func TestNullZeroOption(t *testing.T) {
	type opts struct {
		A string `db:"a,nullzero"`
		B string `db:",nullzero"`
		C string
		D string `db:"d,omitempty,nullzero"`
	}
	plan := fieldPlan(reflect.TypeFor[opts]())
	for col, want := range map[string]bool{"a": true, "b": true, "c": false, "d": true} {
		if got := plan[col].nullZero; got != want {
			t.Errorf("column %q nullZero=%v, want %v", col, got, want)
		}
	}
}

func TestCanBeNull(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want bool
	}{
		{reflect.TypeFor[string](), false}, {reflect.TypeFor[int64](), false}, {reflect.TypeFor[time.Time](), false},
		{reflect.TypeFor[*string](), true}, {reflect.TypeFor[[]byte](), true}, {reflect.TypeFor[map[string]any](), true},
		{reflect.TypeFor[any](), true}, {reflect.TypeFor[sql.NullString](), true},
	} {
		if got := canBeNull(tc.typ); got != tc.want {
			t.Errorf("canBeNull(%s) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}
