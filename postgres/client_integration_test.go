//go:build integration

package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type Mood string

type hiddenBase struct{ Tag string }

type withHidden struct {
	*hiddenBase
	ID int
}

type Address struct {
	Street string
	Zip    int32
}

type registered struct {
	H       pgtype.Hstore
	Hs      []pgtype.Hstore
	Addr    Address
	OptAddr *Address
	Moods   []Mood
	Pi      int
	Fr      pgtype.Range[float64]
}

type ptrAddr struct{ Addr *Address }
type strictAddr struct{ Addr Address }
type lenientAddr struct {
	Addr Address `db:"addr,nullzero"`
}

type Base struct{ CreatedAt string }

type allTypes struct {
	ID      int
	B       bool
	F4      float32
	F8      float64
	N       float64
	Np      pgtype.Numeric
	T       string
	Vc      string
	By      []byte
	Ts      time.Time
	Tstz    time.Time
	D       time.Time
	Tm      pgtype.Time
	U       string
	IP      netip.Addr
	J       map[string]any
	Js      struct{ A int }
	Raw     json.RawMessage
	ArrText []string
	ArrInt  []int32
	I2      int16
	I8      int64
	Mood    Mood
	Iv      pgtype.Interval
	NN      sql.NullString
	NI      *int
	Missing string // no column of this name
}

type stamps struct{ CreatedAt time.Time }

type person struct {
	stamps
	UserID      int64
	FullName    string
	HomeAddress *string
	Note        string `db:"extra"`
	Ignored     string `db:"-"`
}

type user struct {
	ID    int64
	Email string
	Age   *int
}

// Exercises every Client method and helper against a real server. Tables are
// uniquely named and dropped afterwards. Run with:
// GCU_TEST_POSTGRES_URL=postgres://... go test -tags integration -run IntegrationClient ./postgres
func TestIntegrationClient(t *testing.T) {
	url := os.Getenv("GCU_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("GCU_TEST_POSTGRES_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := postgres.New(ctx, postgres.Config{URL: url, MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	c := postgres.Wrap(pool)
	defer c.Close()

	tbl := fmt.Sprintf("gcu_users_%d", time.Now().UnixNano())
	parent := tbl + "_parent"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	eq := func(got, want any) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	_, err = c.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id bigint PRIMARY KEY)`, parent))
	must(err)
	_, err = c.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (
		id bigint PRIMARY KEY,
		email text NOT NULL UNIQUE,
		age int CHECK (age >= 0),
		parent_id bigint REFERENCES %s(id))`, tbl, parent))
	must(err)
	defer func() {
		c.Exec(context.Background(), fmt.Sprintf("DROP TABLE IF EXISTS %s, %s", tbl, parent))
	}()

	t.Run("Ping Stat Config", func(t *testing.T) {
		must(c.Ping(ctx))
		if c.Stat().MaxConns() != 8 {
			t.Fatalf("Stat().MaxConns() = %d", c.Stat().MaxConns())
		}
		if c.Config().MaxConns != 8 {
			t.Fatalf("Config().MaxConns = %d", c.Config().MaxConns)
		}
	})

	t.Run("Exec ExecTag Query QueryRow", func(t *testing.T) {
		n, err := c.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, email, age) VALUES ($1,$2,$3),($4,$5,$6)", tbl), 1, "a@x.io", 30, 2, "b@x.io", nil)
		must(err)
		eq(n, 2)
		tag, err := c.ExecTag(ctx, fmt.Sprintf("UPDATE %s SET age = 31 WHERE id = $1", tbl), 1)
		must(err)
		eq(tag.String(), "UPDATE 1")

		rows, err := c.Query(ctx, fmt.Sprintf("SELECT id FROM %s ORDER BY id", tbl))
		must(err)
		var ids []int64
		for rows.Next() {
			var id int64
			must(rows.Scan(&id))
			ids = append(ids, id)
		}
		must(rows.Err())
		rows.Close()
		eq(ids, []int64{1, 2})

		var email string
		must(c.QueryRow(ctx, fmt.Sprintf("SELECT email FROM %s WHERE id = $1", tbl), 1).Scan(&email))
		eq(email, "a@x.io")
		err = c.QueryRow(ctx, fmt.Sprintf("SELECT email FROM %s WHERE id = $1", tbl), 999).Scan(&email)
		if !errors.Is(err, postgres.ErrNotFound) {
			t.Fatalf("missing row: want ErrNotFound, got %v", err)
		}
	})

	t.Run("Exists Get Select SelectMaps", func(t *testing.T) {
		ok, err := c.Exists(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE email = $1", tbl), "a@x.io")
		must(err)
		eq(ok, true)
		ok, err = c.Exists(ctx, fmt.Sprintf("SELECT 1 FROM %s WHERE email = $1", tbl), "nobody@x.io")
		must(err)
		eq(ok, false)

		u, err := postgres.Get[user](ctx, c, fmt.Sprintf("SELECT id, email, age FROM %s WHERE id = $1", tbl), 1)
		must(err)
		eq(u.Email, "a@x.io")
		eq(*u.Age, 31)
		u2, err := postgres.Get[user](ctx, c, fmt.Sprintf("SELECT id, email, age FROM %s WHERE id = $1", tbl), 2)
		must(err)
		if u2.Age != nil {
			t.Fatalf("NULL age should scan to nil, got %v", *u2.Age)
		}
		count, err := postgres.Get[int64](ctx, c, fmt.Sprintf("SELECT count(*) FROM %s", tbl))
		must(err)
		eq(count, 2)
		now, err := postgres.Get[time.Time](ctx, c, "SELECT now()")
		must(err)
		if time.Since(now) > time.Minute {
			t.Fatalf("now() = %v", now)
		}
		if _, err := postgres.Get[user](ctx, c, fmt.Sprintf("SELECT id, email, age FROM %s WHERE id = $1", tbl), 999); !errors.Is(err, postgres.ErrNotFound) {
			t.Fatalf("Get missing: want ErrNotFound, got %v", err)
		}
		if _, err := postgres.Get[user](ctx, c, fmt.Sprintf("SELECT id, email, age FROM %s", tbl)); !errors.Is(err, postgres.ErrTooManyRows) {
			t.Fatalf("Get many: want ErrTooManyRows, got %v", err)
		}

		users, err := postgres.Select[user](ctx, c, fmt.Sprintf("SELECT id, email, age FROM %s ORDER BY id", tbl))
		must(err)
		eq(len(users), 2)
		emails, err := postgres.Select[string](ctx, c, fmt.Sprintf("SELECT email FROM %s ORDER BY id", tbl))
		must(err)
		eq(emails, []string{"a@x.io", "b@x.io"})
		none, err := postgres.Select[user](ctx, c, fmt.Sprintf("SELECT id, email, age FROM %s WHERE id < 0", tbl))
		must(err)
		if none == nil || len(none) != 0 {
			t.Fatalf("no rows should give an empty non-nil slice, got %#v", none)
		}
		maps, err := postgres.SelectMaps(ctx, c, fmt.Sprintf("SELECT id, email FROM %s ORDER BY id", tbl))
		must(err)
		eq(maps[0]["email"], "a@x.io")
		if _, err := postgres.Select[user](ctx, c, "SELECT * FROM table_that_does_not_exist"); err == nil {
			t.Fatal("bad SQL should fail")
		}
	})

	t.Run("struct scanning", func(t *testing.T) {
		people := tbl + "_people"
		_, err := c.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (
			user_id bigint, full_name text, home_address text, created_at timestamptz, extra text, ignored text)`, people))
		must(err)
		defer c.Exec(context.Background(), "DROP TABLE IF EXISTS "+people)
		_, err = c.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES
			(1, 'Ann Lee', '1 Main St', '2026-01-02 03:04:05+00', 'note-a', 'x'),
			(2, 'Bo Chen', NULL, '2026-02-03 04:05:06+00', 'note-b', 'y')`, people))
		must(err)
		all := "SELECT user_id, full_name, home_address, created_at, extra, ignored FROM " + people + " ORDER BY user_id"

		// snake_case names, embedded struct, nullable pointer, tag, skipped field
		rows, err := postgres.Select[person](ctx, c, all)
		must(err)
		eq(len(rows), 2)
		eq(rows[0].UserID, 1)
		eq(rows[0].FullName, "Ann Lee")
		eq(*rows[0].HomeAddress, "1 Main St")
		eq(rows[0].CreatedAt.UTC().Format(time.RFC3339), "2026-01-02T03:04:05Z")
		eq(rows[0].Note, "note-a")
		eq(rows[0].Ignored, "") // db:"-" is never filled, even though the column exists
		if rows[1].HomeAddress != nil {
			t.Fatalf("NULL should scan to a nil pointer, got %q", *rows[1].HomeAddress)
		}

		// pointer elements
		ptrs, err := postgres.Select[*person](ctx, c, all)
		must(err)
		eq(len(ptrs), 2)
		eq(ptrs[1].FullName, "Bo Chen")

		// method forms: *[]T, *[]*T, *T, **T
		var viaSlice []person
		must(c.Select(ctx, &viaSlice, all))
		eq(len(viaSlice), 2)
		var viaPtrSlice []*person
		must(c.Select(ctx, &viaPtrSlice, all))
		eq(viaPtrSlice[0].UserID, 1)
		var one person
		must(c.Get(ctx, &one, "SELECT user_id, full_name FROM "+people+" WHERE user_id = $1", 2))
		eq(one.FullName, "Bo Chen")
		var onePtr *person
		must(c.Get(ctx, &onePtr, "SELECT user_id, full_name FROM "+people+" WHERE user_id = $1", 1))
		eq(onePtr.FullName, "Ann Lee")
		gp, err := postgres.Get[*person](ctx, c, "SELECT user_id FROM "+people+" WHERE user_id = 1")
		must(err)
		eq(gp.UserID, 1)

		// lax matching: extra columns are ignored, missing ones leave the field alone
		lax, err := postgres.Get[person](ctx, c, "SELECT user_id, 'surplus' AS not_a_field FROM "+people+" WHERE user_id = 1")
		must(err)
		eq(lax.UserID, 1)
		eq(lax.FullName, "")

		// missing / too many / empty
		var missing person
		if err := c.Get(ctx, &missing, "SELECT user_id FROM "+people+" WHERE user_id = 99"); !errors.Is(err, postgres.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
		if err := c.Get(ctx, &missing, "SELECT user_id FROM "+people); !errors.Is(err, postgres.ErrTooManyRows) {
			t.Fatalf("want ErrTooManyRows, got %v", err)
		}
		var empty []person
		must(c.Select(ctx, &empty, "SELECT user_id FROM "+people+" WHERE user_id < 0"))
		if empty == nil || len(empty) != 0 {
			t.Fatalf("no rows should give an empty non-nil slice, got %#v", empty)
		}

		// single values, including pgtype structs that scan themselves
		var ids []int64
		must(c.Select(ctx, &ids, "SELECT user_id FROM "+people+" ORDER BY user_id"))
		eq(ids, []int64{1, 2})
		var n int64
		must(c.Get(ctx, &n, "SELECT count(*) FROM "+people))
		eq(n, 2)
		num, err := postgres.Get[pgtype.Numeric](ctx, c, "SELECT 1.5::numeric")
		must(err)
		f, _ := num.Float64Value()
		eq(f.Float64, 1.5)
		nullable, err := postgres.Get[*string](ctx, c, "SELECT home_address FROM "+people+" WHERE user_id = 2")
		must(err)
		if nullable != nil {
			t.Fatal("NULL into *string should be nil")
		}

		// the same functions work inside a transaction
		must(c.WithTx(ctx, func(tx postgres.Tx) error {
			p, err := postgres.Get[person](ctx, tx, "SELECT user_id, full_name FROM "+people+" WHERE user_id = 1")
			if err != nil {
				return err
			}
			eq(p.FullName, "Ann Lee")
			return nil
		}))

		// NULL into a non-pointer field is an error, not a silent zero value
		type strict struct {
			Address string `db:"home_address"`
		}
		_, err = postgres.Get[strict](ctx, c, "SELECT home_address FROM "+people+" WHERE user_id = 2")
		if err == nil || !strings.Contains(err.Error(), "NULL") {
			t.Fatalf("want a NULL scan error, got %v", err)
		}

		// bad destinations fail clearly
		if err := c.Get(ctx, person{}, all); err == nil {
			t.Fatal("a non-pointer dest should fail")
		}
		var notSlice person
		if err := c.Select(ctx, &notSlice, all); err == nil {
			t.Fatal("Select into a non-slice should fail")
		}
	})

	t.Run("struct scanning: every column type", func(t *testing.T) {
		mood := fmt.Sprintf("gcu_mood_%d", time.Now().UnixNano())
		types := tbl + "_types"
		_, err := c.Exec(ctx, fmt.Sprintf("CREATE TYPE %s AS ENUM ('sad','ok','happy')", mood))
		must(err)
		defer c.Exec(context.Background(), "DROP TYPE IF EXISTS "+mood)
		_, err = c.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (
			id int, b bool, f4 real, f8 double precision, n numeric(10,2), np numeric(10,2), t text, vc varchar(10),
			by bytea, ts timestamp, tstz timestamptz, d date, tm time, u uuid, ip inet, j jsonb, js json, raw jsonb,
			arr_text text[], arr_int int[], i2 smallint, i8 bigint, mood %s, iv interval, nn text, ni int)`, types, mood))
		must(err)
		defer c.Exec(context.Background(), "DROP TABLE IF EXISTS "+types)
		_, err = c.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (
			7, true, 1.5, 2.25, 12.34, 56.78, 'hello', 'vc', '\xdeadbeef', '2026-03-04 05:06:07', '2026-03-04 05:06:07+00',
			'2026-03-04', '01:02:03', 'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '10.1.2.3', '{"k":"v","n":1}', '{"a":5}',
			'{"r":true}', '{x,y,z}', '{1,2,3}', 12, 9000000000, 'happy', '1 day 02:00:00', NULL, NULL)`, types))
		must(err)

		got := allTypes{Missing: "untouched"}
		must(c.Get(ctx, &got, "SELECT * FROM "+types))
		eq(got.ID, 7)
		eq(got.B, true)
		eq(got.F4, 1.5)
		eq(got.F8, 2.25)
		eq(got.N, 12.34)
		np, _ := got.Np.Float64Value()
		eq(np.Float64, 56.78)
		eq(got.T, "hello")
		eq(got.Vc, "vc")
		eq(fmt.Sprintf("%x", got.By), "deadbeef")
		eq(got.Ts.Format("2006-01-02 15:04:05"), "2026-03-04 05:06:07")
		eq(got.Tstz.UTC().Format(time.RFC3339), "2026-03-04T05:06:07Z")
		eq(got.D.Format("2006-01-02"), "2026-03-04")
		eq(got.Tm.Microseconds, int64(3723000000))
		eq(got.U, "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")
		eq(got.IP.String(), "10.1.2.3")
		eq(got.J["k"], "v")
		eq(got.Js.A, 5)
		eq(string(got.Raw), `{"r": true}`)
		eq(got.ArrText, []string{"x", "y", "z"})
		eq(got.ArrInt, []int32{1, 2, 3})
		eq(got.I2, int16(12))
		eq(got.I8, int64(9000000000))
		eq(got.Mood, Mood("happy"))
		eq(got.Iv.Days, int32(1))
		eq(got.NN.Valid, false)
		if got.NI != nil {
			t.Fatal("NULL int should scan to a nil *int")
		}
		eq(got.Missing, "untouched") // no such column: the field keeps its value
	})

	t.Run("struct scanning: json, embedded pointers, duplicates, maps", func(t *testing.T) {
		// a whole struct decoded from one json/jsonb column that matches no field
		type doc struct {
			UserID   int64  `json:"user_id"`
			FullName string `json:"full_name"`
		}
		d, err := postgres.Get[doc](ctx, c, `SELECT to_jsonb(p) FROM (SELECT 3 AS user_id, 'Cy Doe' AS full_name) p`)
		must(err)
		eq(d.UserID, 3)
		eq(d.FullName, "Cy Doe")
		docs, err := postgres.Select[doc](ctx, c, `SELECT row_to_json(p) FROM (VALUES (1,'a'),(2,'b')) p(user_id, full_name)`)
		must(err)
		eq(len(docs), 2)
		eq(docs[1].FullName, "b")
		// ...but a json column that names a field is decoded into that field
		type holder struct{ Meta struct{ X int } }
		h, err := postgres.Get[holder](ctx, c, `SELECT '{"X":4}'::jsonb AS meta`)
		must(err)
		eq(h.Meta.X, 4)

		// embedded pointer structs are allocated only when a column needs them
		type withBase struct {
			*Base
			UserID int64
		}
		wb, err := postgres.Get[withBase](ctx, c, `SELECT 1 AS user_id, 'yesterday' AS created_at`)
		must(err)
		eq(wb.CreatedAt, "yesterday")
		wn, err := postgres.Get[withBase](ctx, c, `SELECT 2 AS user_id`)
		must(err)
		if wn.Base != nil {
			t.Fatal("the embedded pointer should stay nil when no column needs it")
		}

		// the same column name twice would silently overwrite a field, so it is an error
		_, err = postgres.Get[person](ctx, c, `SELECT 1 AS user_id, 2 AS user_id`)
		if err == nil || !strings.Contains(err.Error(), "same field") {
			t.Fatalf("want a duplicate column error, got %v", err)
		}
		// quoted mixed-case column names still match
		q, err := postgres.Get[person](ctx, c, `SELECT 5 AS "UserID"`)
		must(err)
		eq(q.UserID, 5)

		// rows as maps, through Select and Get
		ms, err := postgres.Select[map[string]any](ctx, c, `SELECT 1 AS a, 'x' AS b UNION ALL SELECT 2, 'y'`)
		must(err)
		eq(len(ms), 2)
		eq(ms[1]["b"], "y")
		m, err := postgres.Get[map[string]any](ctx, c, `SELECT 9 AS n`)
		must(err)
		eq(m["n"], int32(9))
	})

	t.Run("struct scanning: nullzero and unexported embedded pointer", func(t *testing.T) {
		type nz struct {
			Nick   string    `db:"nick,nullzero"`
			N      int       `db:"n,nullzero"`
			When   time.Time `db:"w,nullzero"`
			Strict string    `db:"s"`
		}
		got := nz{Nick: "old", N: 9, Strict: "keep"}
		must(c.Get(ctx, &got, `SELECT NULL::text AS nick, NULL::int AS n, NULL::timestamptz AS w, 'x' AS s`))
		eq(got.Nick, "")
		eq(got.N, 0)
		if !got.When.IsZero() {
			t.Fatalf("NULL with nullzero should give the zero time, got %v", got.When)
		}
		eq(got.Strict, "x")
		// a value is still copied through when it is not NULL
		var full nz
		must(c.Get(ctx, &full, `SELECT 'bob' AS nick, 5 AS n, '2026-05-06 07:08:09+00'::timestamptz AS w, 'y' AS s`))
		eq(full.Nick, "bob")
		eq(full.N, 5)
		eq(full.When.UTC().Format(time.RFC3339), "2026-05-06T07:08:09Z")
		// without the option NULL stays an error
		type plain struct {
			Nick string `db:"nick"`
		}
		var pl plain
		if err := c.Get(ctx, &pl, `SELECT NULL::text AS nick`); err == nil {
			t.Fatal("NULL into a plain string should still be an error")
		}

		// an embedded pointer to an unexported struct type is allocated and filled
		var h withHidden
		must(c.Get(ctx, &h, `SELECT 1 AS id, 'tagged' AS tag`))
		eq(h.ID, 1)
		if h.hiddenBase == nil {
			t.Fatal("the unexported embedded pointer should have been allocated")
		}
		eq(h.hiddenBase.Tag, "tagged")
		var none withHidden
		must(c.Get(ctx, &none, `SELECT 2 AS id`))
		if none.hiddenBase != nil {
			t.Fatal("it must stay nil when no column needs it")
		}
	})

	t.Run("registered types: hstore, composite, enum, domain, range", func(t *testing.T) {
		suffix := fmt.Sprint(time.Now().UnixNano())
		addr, mood, posint, frange := "gcu_addr_"+suffix, "gcu_mood_"+suffix, "gcu_posint_"+suffix, "gcu_frange_"+suffix
		reg := tbl + "_reg"
		hadHstore, err := postgres.Get[bool](ctx, c, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'hstore')")
		must(err)
		for _, ddl := range []string{
			"CREATE EXTENSION IF NOT EXISTS hstore",
			fmt.Sprintf("CREATE TYPE %s AS (street text, zip int)", addr),
			fmt.Sprintf("CREATE TYPE %s AS ENUM ('sad','ok','happy')", mood),
			fmt.Sprintf("CREATE DOMAIN %s AS int CHECK (VALUE > 0)", posint),
			fmt.Sprintf("CREATE TYPE %s AS RANGE (subtype = float8)", frange),
			fmt.Sprintf(`CREATE TABLE %s (h hstore, hs hstore[], addr %s, opt_addr %s, moods %s[], pi %s, fr %s)`, reg, addr, addr, mood, posint, frange),
		} {
			_, err := c.Exec(ctx, ddl)
			must(err)
		}
		defer func() {
			bg := context.Background()
			c.Exec(bg, "DROP TABLE IF EXISTS "+reg)
			c.Exec(bg, fmt.Sprintf("DROP TYPE IF EXISTS %s, %s, %s CASCADE", addr, mood, frange))
			c.Exec(bg, "DROP DOMAIN IF EXISTS "+posint)
			if !hadHstore {
				c.Exec(bg, "DROP EXTENSION IF EXISTS hstore CASCADE")
			}
		}()
		_, err = c.Exec(ctx, fmt.Sprintf(`INSERT INTO %s VALUES (
			'a=>1, b=>NULL', ARRAY['x=>y'::hstore], ROW('Main St', 123), NULL, '{happy,sad}', 5, '[1.5,2.5)')`, reg))
		must(err)

		// Names that exist only after the DDL above, so a separate pool registers them.
		tpool, err := postgres.New(ctx, postgres.Config{URL: url, Types: []string{"hstore", addr, mood, posint, frange}})
		must(err)
		tc := postgres.Wrap(tpool)
		defer tc.Close()

		got, err := postgres.Get[registered](ctx, tc, "SELECT * FROM "+reg)
		must(err)
		eq(*got.H["a"], "1")
		if got.H["b"] != nil {
			t.Fatal("an hstore NULL value should be a nil pointer")
		}
		eq(len(got.Hs), 1)
		eq(*got.Hs[0]["x"], "y")
		eq(got.Addr.Street, "Main St")
		eq(got.Addr.Zip, 123)
		if got.OptAddr != nil {
			t.Fatal("a NULL composite into a pointer field should be nil")
		}
		eq(got.Moods, []Mood{"happy", "sad"})
		eq(got.Pi, 5)
		eq(got.Fr.Lower, 1.5)
		eq(got.Fr.Upper, 2.5)

		// a composite value fills a *struct field, allocating it
		ptrRow, err := postgres.Get[ptrAddr](ctx, tc, fmt.Sprintf("SELECT ROW('Elm', 9)::%s AS addr", addr))
		must(err)
		eq(ptrRow.Addr.Street, "Elm")
		// a NULL composite into a plain struct field is an error unless nullzero is set
		_, err = postgres.Get[strictAddr](ctx, tc, fmt.Sprintf("SELECT NULL::%s AS addr", addr))
		if err == nil || !strings.Contains(err.Error(), "NULL composite") {
			t.Fatalf("want a NULL composite error, got %v", err)
		}
		lenient, err := postgres.Get[lenientAddr](ctx, tc, fmt.Sprintf("SELECT NULL::%s AS addr", addr))
		must(err)
		eq(lenient.Addr.Street, "")

		// a type that is not registered cannot be scanned into a struct, so registration matters
		if _, err := postgres.Get[ptrAddr](ctx, c, fmt.Sprintf("SELECT ROW('Elm', 9)::%s AS addr", addr)); err == nil {
			t.Fatal("scanning an unregistered composite into a struct should fail")
		}

		// registering something that does not exist fails at startup
		if p, err := postgres.New(ctx, postgres.Config{URL: url, Types: []string{"gcu_no_such_type"}}); err == nil {
			p.Close()
			t.Fatal("an unknown type should make New fail")
		} else if !strings.Contains(err.Error(), "register type") {
			t.Fatalf("error should say what failed, got %v", err)
		}
	})

	t.Run("WithTx WithTxOptions Begin BeginTx", func(t *testing.T) {
		insert := func(tx postgres.Tx, id int, email string) error {
			_, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (id, email) VALUES ($1,$2)", tbl), id, email)
			return err
		}
		count := func() int64 {
			n, _ := postgres.Get[int64](ctx, c, fmt.Sprintf("SELECT count(*) FROM %s", tbl))
			return n
		}
		before := count()

		must(c.WithTx(ctx, func(tx postgres.Tx) error { return insert(tx, 100, "tx1@x.io") }))
		eq(count(), before+1)

		boom := errors.New("boom")
		err := c.WithTx(ctx, func(tx postgres.Tx) error {
			if err := insert(tx, 101, "tx2@x.io"); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("WithTx should return fn's error, got %v", err)
		}
		eq(count(), before+1) // rolled back

		func() {
			defer func() {
				if recover() == nil {
					t.Error("a panic in fn must propagate")
				}
			}()
			c.WithTx(ctx, func(tx postgres.Tx) error {
				insert(tx, 102, "tx3@x.io")
				panic("kaboom")
			})
		}()
		eq(count(), before+1) // rolled back on panic

		err = c.WithTxOptions(ctx, postgres.TxOptions{AccessMode: pgx.ReadOnly}, func(tx postgres.Tx) error {
			return insert(tx, 103, "ro@x.io")
		})
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("read-only tx should refuse writes, got %v", err)
		}

		tx, err := c.Begin(ctx)
		must(err)
		must(insert(tx, 104, "begin@x.io"))
		must(tx.Commit(ctx))
		eq(count(), before+2)
		tx, err = c.BeginTx(ctx, postgres.TxOptions{IsoLevel: pgx.Serializable})
		must(err)
		must(insert(tx, 105, "begintx@x.io"))
		must(tx.Rollback(ctx))
		eq(count(), before+2)

		// A cancelled context still ends with a rollback, not a leaked transaction.
		cctx, stop := context.WithCancel(ctx)
		err = c.WithTx(cctx, func(tx postgres.Tx) error {
			stop()
			return errors.New("cancelled")
		})
		if err == nil {
			t.Fatal("want an error")
		}
		eq(c.Stat().AcquiredConns(), 0)
	})

	t.Run("constraint errors", func(t *testing.T) {
		q := func(sql string, args ...any) error { _, err := c.Exec(ctx, sql, args...); return err }
		err := q(fmt.Sprintf("INSERT INTO %s (id, email) VALUES (900, 'a@x.io')", tbl))
		if !postgres.IsUniqueViolation(err) || postgres.ConstraintName(err) != tbl+"_email_key" {
			t.Fatalf("unique: %v (%s)", err, postgres.ConstraintName(err))
		}
		err = q(fmt.Sprintf("INSERT INTO %s (id, email, parent_id) VALUES (901, 'fk@x.io', 424242)", tbl))
		if !postgres.IsForeignKeyViolation(err) {
			t.Fatalf("foreign key: %v", err)
		}
		err = q(fmt.Sprintf("INSERT INTO %s (id, email) VALUES (902, NULL)", tbl))
		if !postgres.IsNotNullViolation(err) {
			t.Fatalf("not null: %v", err)
		}
		err = q(fmt.Sprintf("INSERT INTO %s (id, email, age) VALUES (903, 'chk@x.io', -1)", tbl))
		if !postgres.IsCheckViolation(err) {
			t.Fatalf("check: %v", err)
		}
		eq(postgres.ErrorCode(err), postgres.CodeCheckViolation)
	})

	t.Run("CopyFrom CopyFromRows", func(t *testing.T) {
		rows := [][]any{{200, "c1@x.io"}, {201, "c2@x.io"}, {202, "c3@x.io"}}
		n, err := c.CopyFromRows(ctx, postgres.Identifier{tbl}, []string{"id", "email"}, rows)
		must(err)
		eq(n, 3)
		n, err = c.CopyFrom(ctx, postgres.Identifier{tbl}, []string{"id", "email"}, pgx.CopyFromRows([][]any{{210, "d1@x.io"}}))
		must(err)
		eq(n, 1)
		if _, err := c.CopyFromRows(ctx, postgres.Identifier{tbl}, []string{"id", "email"}, [][]any{{200, "dup@x.io"}}); !postgres.IsUniqueViolation(err) {
			t.Fatalf("COPY duplicate: want unique violation, got %v", err)
		}
	})

	t.Run("SendBatch RunBatch", func(t *testing.T) {
		b := &postgres.Batch{}
		b.Queue(fmt.Sprintf("INSERT INTO %s (id, email) VALUES ($1,$2)", tbl), 300, "bt1@x.io")
		b.Queue(fmt.Sprintf("SELECT email FROM %s WHERE id = $1", tbl), 300)
		results := c.SendBatch(ctx, b)
		tag, err := results.Exec()
		must(err)
		eq(tag.RowsAffected(), 1)
		var email string
		must(results.QueryRow().Scan(&email))
		eq(email, "bt1@x.io")
		must(results.Close())

		ok := &postgres.Batch{}
		ok.Queue(fmt.Sprintf("INSERT INTO %s (id, email) VALUES ($1,$2)", tbl), 301, "bt2@x.io")
		ok.Queue(fmt.Sprintf("SELECT count(*) FROM %s", tbl))
		must(c.RunBatch(ctx, ok))

		bad := &postgres.Batch{}
		bad.Queue(fmt.Sprintf("INSERT INTO %s (id, email) VALUES ($1,$2)", tbl), 302, "bt3@x.io")
		bad.Queue(fmt.Sprintf("INSERT INTO %s (id, email) VALUES ($1,$2)", tbl), 302, "dup@x.io")
		err = c.RunBatch(ctx, bad)
		if err == nil || !strings.Contains(err.Error(), "statement 2") || !postgres.IsUniqueViolation(err) {
			t.Fatalf("RunBatch should name statement 2 and keep the cause, got %v", err)
		}
	})

	t.Run("Acquire AcquireFunc AcquireAllIdle Reset", func(t *testing.T) {
		conn, err := c.Acquire(ctx)
		must(err)
		var one int
		must(conn.QueryRow(ctx, "SELECT 1").Scan(&one))
		conn.Release()
		must(c.AcquireFunc(ctx, func(cn *postgres.Conn) error {
			// Session state lives on this one connection.
			if _, err := cn.Exec(ctx, "SET application_name = 'gcu-acquire'"); err != nil {
				return err
			}
			var name string
			if err := cn.QueryRow(ctx, "SHOW application_name").Scan(&name); err != nil {
				return err
			}
			eq(name, "gcu-acquire")
			_, err := cn.Exec(ctx, "RESET application_name")
			return err
		}))
		idle := c.AcquireAllIdle(ctx)
		for _, cn := range idle {
			cn.Release()
		}
		c.Reset()
		must(c.Ping(ctx)) // the pool reopens connections on demand
	})

	t.Run("Notify Listen Wait Close", func(t *testing.T) {
		l, err := c.Listen(ctx, "gcu_events", "GCU_Case")
		must(err)
		must(c.Notify(ctx, "gcu_events", "hello"))
		wctx, stop := context.WithTimeout(ctx, 5*time.Second)
		n, err := l.Wait(wctx)
		stop()
		must(err)
		eq(n.Channel+":"+n.Payload, "gcu_events:hello")
		must(c.Notify(ctx, "GCU_Case", "exact-case"))
		wctx, stop = context.WithTimeout(ctx, 5*time.Second)
		n, err = l.Wait(wctx)
		stop()
		must(err)
		eq(n.Channel+":"+n.Payload, "GCU_Case:exact-case")

		// Nothing pending: Wait honours its context.
		wctx, stop = context.WithTimeout(ctx, 200*time.Millisecond)
		if _, err := l.Wait(wctx); err == nil {
			t.Fatal("Wait with nothing to receive should time out")
		}
		stop()
		must(l.Close())
		eq(c.Stat().AcquiredConns(), 0)
	})

	t.Run("cancelled context and concurrency", func(t *testing.T) {
		cctx, stop := context.WithCancel(ctx)
		stop()
		if _, err := c.Exec(cctx, "SELECT 1"); err == nil {
			t.Fatal("Exec with a cancelled context should fail")
		}
		if _, err := postgres.Get[int](cctx, c, "SELECT 1"); err == nil {
			t.Fatal("Get with a cancelled context should fail")
		}

		counter := tbl + "_counter"
		_, err := c.Exec(ctx, fmt.Sprintf("CREATE TABLE %s (n int)", counter))
		must(err)
		defer c.Exec(context.Background(), "DROP TABLE IF EXISTS "+counter)
		_, err = c.Exec(ctx, fmt.Sprintf("INSERT INTO %s VALUES (0)", counter))
		must(err)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := c.WithTx(ctx, func(tx postgres.Tx) error {
					_, err := tx.Exec(ctx, fmt.Sprintf("UPDATE %s SET n = n + 1", counter))
					return err
				})
				if err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		n, err := postgres.Get[int](ctx, c, "SELECT n FROM "+counter)
		must(err)
		eq(n, 20)
	})

	t.Run("Close", func(t *testing.T) {
		pool2, err := postgres.New(ctx, postgres.Config{URL: url})
		must(err)
		c2 := postgres.Wrap(pool2)
		must(c2.Ping(ctx))
		c2.Close()
		c2.Close() // safe to call twice
		if err := c2.Ping(ctx); err == nil {
			t.Fatal("Ping after Close should fail")
		}
	})
}
