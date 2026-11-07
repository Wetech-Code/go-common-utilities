package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a query that must produce a row produces none. It
// replaces pgx.ErrNoRows so callers can use errors.Is without importing pgx.
var ErrNotFound = errors.New("postgres: not found")

// ErrTooManyRows is returned by Get when the query produces more than one row.
var ErrTooManyRows = pgx.ErrTooManyRows

// Types from pgx that appear in Client method signatures, re-exported so ordinary
// code does not need to import pgx directly.
type (
	// Tx is a transaction. Within WithTx it is the full pgx.Tx: Exec, Query,
	// QueryRow, CopyFrom, SendBatch, Prepare, LargeObjects and nested Begin (a
	// savepoint) are all available on it.
	Tx = pgx.Tx
	// TxOptions sets isolation level, access mode and deferrability.
	TxOptions = pgx.TxOptions
	// Rows is a query result. The caller must Close it.
	Rows = pgx.Rows
	// Batch queues statements to send in one round trip.
	Batch = pgx.Batch
	// BatchResults reads the results of a Batch. The caller must Close it.
	BatchResults = pgx.BatchResults
	// Identifier names a table or column safely quoted, such as Identifier{"public", "users"}.
	Identifier = pgx.Identifier
	// CopyFromSource supplies rows to CopyFrom.
	CopyFromSource = pgx.CopyFromSource
	// Notification is a message received through LISTEN/NOTIFY.
	Notification = pgconn.Notification
	// CommandTag is the server's summary of a finished statement, such as "INSERT 0 1".
	CommandTag = pgconn.CommandTag
	// Stat describes the pool's connections.
	Stat = pgxpool.Stat
	// Conn is a connection checked out of the pool. The caller must Release it.
	Conn = pgxpool.Conn
)

// Querier is anything that can run a query: a Client, a Tx, a *pgxpool.Pool or a
// *pgxpool.Conn. Get, Select and SelectMaps accept it so the same code works
// inside and outside a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Client wraps a pgx pool with plain methods for every pool operation: running
// SQL, transactions, bulk COPY, batches, dedicated connections and LISTEN/NOTIFY.
// SQL stays SQL: Client builds no queries and knows no schema, so it is not an ORM.
//
// A missing row is ErrNotFound; constraint and serialization errors can be
// recognised with IsUniqueViolation and the other Is* functions. Native is the
// underlying pool for anything not wrapped. Closing a Client closes Native.
type Client struct {
	// Native is the underlying pgx pool.
	Native *pgxpool.Pool
}

// Wrap returns a Client around pool, typically the result of New:
//
//	pool, err := postgres.New(ctx, postgres.Config{URL: url})
//	if err != nil { return err }
//	c := postgres.Wrap(pool)
//	defer c.Close()
func Wrap(pool *pgxpool.Pool) *Client { return &Client{Native: pool} }

// ---- pool lifecycle ----

// Ping checks that the server answers.
func (c *Client) Ping(ctx context.Context) error { return c.Native.Ping(ctx) }

// Close closes all connections after the in-use ones are released. It is safe to
// call more than once.
func (c *Client) Close() { c.Native.Close() }

// Reset closes all connections and lets the pool open new ones on demand.
func (c *Client) Reset() { c.Native.Reset() }

// Stat returns a snapshot of the pool's connection counts.
func (c *Client) Stat() *Stat { return c.Native.Stat() }

// Config returns a copy of the pool's configuration.
func (c *Client) Config() *pgxpool.Config { return c.Native.Config() }

// ---- running SQL ----

// Exec runs a statement that returns no rows and reports how many rows it
// affected. Arguments use $1, $2, ... placeholders.
func (c *Client) Exec(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := c.Native.Exec(ctx, sql, args...)
	return tag.RowsAffected(), err
}

// ExecTag is Exec that returns the full command tag, such as "INSERT 0 1".
func (c *Client) ExecTag(ctx context.Context, sql string, args ...any) (CommandTag, error) {
	return c.Native.Exec(ctx, sql, args...)
}

// Query runs a query and returns its rows. The caller must Close them. For the
// common cases prefer Select, SelectMaps and Get, which close the rows.
func (c *Client) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	return c.Native.Query(ctx, sql, args...)
}

// Row is the result of QueryRow.
type Row struct{ row pgx.Row }

// Scan copies the columns of the row into dest. It returns ErrNotFound if the
// query produced no row.
func (r Row) Scan(dest ...any) error {
	if err := r.row.Scan(dest...); err != nil {
		return mapNoRows(err)
	}
	return nil
}

// QueryRow runs a query expected to return one row. Errors, including a missing
// row, appear when you call Scan on the result.
func (c *Client) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return Row{c.Native.QueryRow(ctx, sql, args...)}
}

// Exists reports whether query returns at least one row. query is a complete
// SELECT, for example "SELECT 1 FROM users WHERE email = $1".
func (c *Client) Exists(ctx context.Context, query string, args ...any) (bool, error) {
	var ok bool
	if err := c.QueryRow(ctx, "SELECT EXISTS ("+query+")", args...).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

func mapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// Get runs a query that must return exactly one row and returns it as a T. It
// returns ErrNotFound for no rows and ErrTooManyRows for more than one.
//
// T is a struct, a pointer to a struct, a map[string]any, or a single value
// (int64, string, time.Time, []byte, a pointer for a nullable column, ...). A
// struct is filled by column name: a field matches a column by its `db:"name"`
// tag, or by its Go name ignoring case (ID matches id) or in snake_case (UserID
// matches user_id). Columns with no field are ignored, fields with no column keep
// their value, `db:"-"` skips a field, and embedded structs (including embedded
// pointers) are flattened. A NULL into a non-pointer field is an error unless the
// field's tag has the nullzero option, `db:"name,nullzero"`. A composite type
// (registered with Config.Types) fills a struct field by attribute name. If the
// query returns one json or jsonb column that
// matches no field, the whole value is decoded into the struct. Two columns that
// map to the same field are an error. A single-value T needs a one-column query.
//
//	type User struct {
//		ID     int64
//		Email  string
//		UserID int64 `db:"owner_id"` // optional tag
//	}
//	u, err := postgres.Get[User](ctx, c, "SELECT id, email, owner_id FROM users WHERE id = $1", 7)
func Get[T any](ctx context.Context, q Querier, sql string, args ...any) (T, error) {
	var v T
	err := getInto(ctx, q, &v, sql, args)
	return v, err
}

// Select runs a query and returns every row as a []T, using the same rules as
// Get. No rows gives an empty, non-nil slice. T may be a pointer to a struct.
func Select[T any](ctx context.Context, q Querier, sql string, args ...any) ([]T, error) {
	var out []T
	err := selectInto(ctx, q, &out, sql, args)
	return out, err
}

// Get runs a query that must return exactly one row and scans it into dest, a
// pointer to a struct or a single value, using the rules described on the Get
// function. It returns ErrNotFound for no rows and ErrTooManyRows for more than
// one. Inside a transaction use the Get function with the Tx.
//
//	var u User
//	err := c.Get(ctx, &u, "SELECT id, email FROM users WHERE id = $1", 7)
func (c *Client) Get(ctx context.Context, dest any, sql string, args ...any) error {
	return getInto(ctx, c, dest, sql, args)
}

// Select runs a query and replaces the slice dest points to with all its rows:
// *[]User, *[]*User, *[]string, ... No rows leaves an empty, non-nil slice.
//
//	var users []User
//	err := c.Select(ctx, &users, "SELECT id, email FROM users ORDER BY id")
func (c *Client) Select(ctx context.Context, dest any, sql string, args ...any) error {
	return selectInto(ctx, c, dest, sql, args)
}

// SelectMaps runs a query and returns each row as a column-name to value map, for
// results whose shape is not known in advance.
func SelectMaps(ctx context.Context, q Querier, sql string, args ...any) ([]map[string]any, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToMap)
}

// ---- transactions ----

// Begin starts a transaction. The caller must Commit or Rollback it; prefer
// WithTx, which does that for you.
func (c *Client) Begin(ctx context.Context) (Tx, error) { return c.Native.Begin(ctx) }

// BeginTx starts a transaction with the given options.
func (c *Client) BeginTx(ctx context.Context, opts TxOptions) (Tx, error) {
	return c.Native.BeginTx(ctx, opts)
}

// WithTx runs fn inside a transaction. It commits if fn returns nil and rolls
// back if fn returns an error or panics (the panic then continues). The rollback
// still happens when ctx is cancelled. fn may be retried by the caller on
// IsSerializationFailure or IsDeadlock errors, but WithTx never retries on its
// own, because that could repeat side effects.
func (c *Client) WithTx(ctx context.Context, fn func(Tx) error) error {
	return c.WithTxOptions(ctx, TxOptions{}, fn)
}

// WithTxOptions is WithTx with explicit transaction options.
func (c *Client) WithTxOptions(ctx context.Context, opts TxOptions, fn func(Tx) error) (err error) {
	tx, err := c.Native.BeginTx(ctx, opts)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if done {
			return
		}
		// fn panicked: roll back, then let the panic continue.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	if err = fn(tx); err != nil {
		done = true
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if rbErr := tx.Rollback(rollbackCtx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("postgres: rollback: %w", rbErr))
		}
		return err
	}
	done = true
	return tx.Commit(ctx)
}

// ---- bulk and batches ----

// CopyFrom bulk-loads rows into a table with the COPY protocol and returns how
// many rows were copied. It is much faster than many INSERTs.
func (c *Client) CopyFrom(ctx context.Context, table Identifier, columns []string, src CopyFromSource) (int64, error) {
	return c.Native.CopyFrom(ctx, table, columns, src)
}

// CopyFromRows is CopyFrom for rows already in memory.
func (c *Client) CopyFromRows(ctx context.Context, table Identifier, columns []string, rows [][]any) (int64, error) {
	return c.Native.CopyFrom(ctx, table, columns, pgx.CopyFromRows(rows))
}

// SendBatch sends the queued statements in one round trip and returns their
// results, which the caller reads in order and must Close.
func (c *Client) SendBatch(ctx context.Context, b *Batch) BatchResults {
	return c.Native.SendBatch(ctx, b)
}

// RunBatch sends the batch and checks every statement succeeded, discarding any
// rows. It stops at the first failure and reports which statement it was. Use
// SendBatch to read results.
func (c *Client) RunBatch(ctx context.Context, b *Batch) error {
	results := c.Native.SendBatch(ctx, b)
	for i := 0; i < b.Len(); i++ {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("postgres: batch statement %d: %w", i+1, err)
		}
	}
	return results.Close()
}

// ---- dedicated connections ----

// Acquire checks a connection out of the pool, for session state such as
// advisory locks, temp tables or SET commands. The caller must Release it.
func (c *Client) Acquire(ctx context.Context) (*Conn, error) { return c.Native.Acquire(ctx) }

// AcquireFunc checks out a connection, runs fn and releases it, even if fn panics.
func (c *Client) AcquireFunc(ctx context.Context, fn func(*Conn) error) error {
	return c.Native.AcquireFunc(ctx, fn)
}

// AcquireAllIdle checks out every idle connection. The caller must Release each.
func (c *Client) AcquireAllIdle(ctx context.Context) []*Conn { return c.Native.AcquireAllIdle(ctx) }

// ---- LISTEN / NOTIFY ----

// Notify sends payload to every session listening on channel. Inside a
// transaction the notification is delivered only when it commits; to do that run
// "SELECT pg_notify($1, $2)" on the Tx.
func (c *Client) Notify(ctx context.Context, channel, payload string) error {
	if channel == "" {
		return errors.New("postgres: channel must not be empty")
	}
	_, err := c.Native.Exec(ctx, "SELECT pg_notify($1, $2)", channel, payload)
	return err
}

// Listener receives notifications on a dedicated connection.
type Listener struct {
	conn *pgxpool.Conn
}

// Listen starts listening on channels. Channel names are used exactly as given
// (they are case-sensitive). The Listener holds one pool connection until Close,
// so the caller must always Close it.
func (c *Client) Listen(ctx context.Context, channels ...string) (*Listener, error) {
	if len(channels) == 0 {
		return nil, errors.New("postgres: at least one channel is required")
	}
	for _, ch := range channels {
		if ch == "" {
			return nil, errors.New("postgres: channel must not be empty")
		}
	}
	conn, err := c.Native.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	for _, ch := range channels {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			conn.Release()
			return nil, fmt.Errorf("postgres: listen %q: %w", ch, err)
		}
	}
	return &Listener{conn: conn}, nil
}

// Wait blocks until a notification arrives or ctx ends.
func (l *Listener) Wait(ctx context.Context) (*Notification, error) {
	return l.conn.Conn().WaitForNotification(ctx)
}

// Close stops listening and returns the connection to the pool.
func (l *Listener) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := l.conn.Exec(ctx, "UNLISTEN *")
	l.conn.Release()
	return err
}

// ---- errors ----

// SQLSTATE codes for the errors callers most often handle.
const (
	CodeUniqueViolation      = "23505"
	CodeForeignKeyViolation  = "23503"
	CodeNotNullViolation     = "23502"
	CodeCheckViolation       = "23514"
	CodeSerializationFailure = "40001"
	CodeDeadlockDetected     = "40P01"
)

// ErrorCode returns the five-character SQLSTATE of a server error, or "" if err
// is not one.
func ErrorCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ConstraintName returns the name of the constraint a server error violated, or "".
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// IsUniqueViolation reports a duplicate key (SQLSTATE 23505).
func IsUniqueViolation(err error) bool { return ErrorCode(err) == CodeUniqueViolation }

// IsForeignKeyViolation reports a missing or still-referenced row (SQLSTATE 23503).
func IsForeignKeyViolation(err error) bool { return ErrorCode(err) == CodeForeignKeyViolation }

// IsNotNullViolation reports a NULL in a NOT NULL column (SQLSTATE 23502).
func IsNotNullViolation(err error) bool { return ErrorCode(err) == CodeNotNullViolation }

// IsCheckViolation reports a failed CHECK constraint (SQLSTATE 23514).
func IsCheckViolation(err error) bool { return ErrorCode(err) == CodeCheckViolation }

// IsSerializationFailure reports a transaction that lost a serialization race
// (SQLSTATE 40001); retrying the whole transaction usually succeeds.
func IsSerializationFailure(err error) bool { return ErrorCode(err) == CodeSerializationFailure }

// IsDeadlock reports a deadlock the server broke (SQLSTATE 40P01); retrying the
// whole transaction usually succeeds.
func IsDeadlock(err error) bool { return ErrorCode(err) == CodeDeadlockDetected }
