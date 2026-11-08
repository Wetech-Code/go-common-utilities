// Package postgres creates a pgx connection pool from a validated Config.
//
// New returns the native *pgxpool.Pool, so callers use pgx directly (Query,
// QueryRow, Exec, Begin, CopyFrom, ...). The package adds only validation,
// timeouts and a startup ping. It is not an ORM and owns no schema.
//
// The caller owns the returned pool and must call Close when finished:
//
//	pool, err := postgres.New(ctx, postgres.Config{URL: os.Getenv("DATABASE_URL")})
//	if err != nil {
//		return err
//	}
//	defer pool.Close()
//
// Wrap(pool) returns a Client with plain methods for every pool operation and the
// practical extras: Exec, Get/Select into structs, WithTx (commit or roll back for
// you), CopyFrom, batches, dedicated connections and LISTEN/NOTIFY, plus helpers
// that recognise constraint errors. SQL stays raw; the pool stays reachable as
// Client.Native.
//
// TLS is controlled by the connection URL (sslmode=verify-full is recommended
// for production). The package offers no option to skip certificate checks.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultConnectTimeout is used when Config.ConnectTimeout is zero.
const DefaultConnectTimeout = 10 * time.Second

// Config describes a PostgreSQL pool. Only URL is required; zero values keep
// the pgx defaults.
type Config struct {
	// URL is a connection string such as
	// "postgres://user:pass@host:5432/dbname?sslmode=verify-full".
	URL string
	// AppName is reported to the server as application_name.
	AppName string

	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration

	// ConnectTimeout bounds each new connection and the startup ping.
	// Default DefaultConnectTimeout.
	ConnectTimeout time.Duration

	// Types lists Postgres types to register on every new connection so pgx can
	// scan them: "hstore", enums, composite types, domains and ranges, each with
	// its array type. Names may be schema-qualified ("app.address"). List a
	// type's dependencies first (a composite after the types of its attributes).
	// A missing type, or a missing hstore extension, makes New fail at startup.
	// Built-in types need no registration.
	Types []string

	// Tracer, if set, observes every query, batch, COPY and connection on the
	// pool. Use otelpgx.New from the telemetry/otelpgx package for OpenTelemetry
	// spans and metrics, or any pgx.QueryTracer of your own.
	Tracer pgx.QueryTracer
}

// Validate reports problems with the configuration without connecting.
// Errors never include the URL, which may contain a password.
func (c Config) Validate() error {
	switch {
	case c.URL == "":
		return errors.New("postgres: URL is required")
	case c.MaxConns < 0 || c.MinConns < 0:
		return errors.New("postgres: MaxConns and MinConns must not be negative")
	case c.MaxConns > 0 && c.MinConns > c.MaxConns:
		return errors.New("postgres: MinConns must not exceed MaxConns")
	case c.MaxConnLifetime < 0 || c.MaxConnIdleTime < 0 || c.HealthCheckPeriod < 0 || c.ConnectTimeout < 0:
		return errors.New("postgres: durations must not be negative")
	}
	for _, name := range c.Types {
		if strings.TrimSpace(name) == "" {
			return errors.New("postgres: Types must not contain an empty name")
		}
	}
	return nil
}

// New validates cfg, creates the pool and pings the server, so a bad address or
// password is reported at startup. ctx bounds the startup only; it does not
// control the pool's lifetime. On error no pool is left open.
func New(ctx context.Context, cfg Config) (*pgxpool.Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgx redacts the password in this message.
		return nil, fmt.Errorf("postgres: parse URL: %w", err)
	}

	timeout := cfg.ConnectTimeout
	if timeout == 0 {
		timeout = DefaultConnectTimeout
	}
	pc.ConnConfig.ConnectTimeout = timeout
	if cfg.AppName != "" {
		pc.ConnConfig.RuntimeParams["application_name"] = cfg.AppName
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		pc.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		pc.MaxConnIdleTime = cfg.MaxConnIdleTime
	}
	if cfg.HealthCheckPeriod > 0 {
		pc.HealthCheckPeriod = cfg.HealthCheckPeriod
	}

	if len(cfg.Types) > 0 {
		pc.AfterConnect = registerTypes(cfg.Types)
	}
	if cfg.Tracer != nil {
		pc.ConnConfig.Tracer = cfg.Tracer
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// registerTypes returns an AfterConnect hook that registers the named types, and
// their array types, on each new connection.
func registerTypes(names []string) func(context.Context, *pgx.Conn) error {
	return func(ctx context.Context, conn *pgx.Conn) error {
		for _, name := range names {
			if err := registerType(ctx, conn, name); err != nil {
				return fmt.Errorf("postgres: register type %q: %w", name, err)
			}
		}
		return nil
	}
}

func registerType(ctx context.Context, conn *pgx.Conn, name string) error {
	typ, err := loadType(ctx, conn, name)
	if err != nil {
		return err
	}
	conn.TypeMap().RegisterType(typ)

	// Register the array type too, when there is one, so []T columns scan.
	var hasArray bool
	if err := conn.QueryRow(ctx, "SELECT typarray <> 0 FROM pg_type WHERE oid = $1::text::regtype::oid", name).Scan(&hasArray); err != nil {
		return err
	}
	if !hasArray {
		return nil
	}
	arr, err := conn.LoadType(ctx, name+"[]")
	if err != nil {
		return err
	}
	conn.TypeMap().RegisterType(arr)
	return nil
}

// loadType is pgx's LoadType plus hstore, which LoadType does not handle because
// it is a base type that needs a specific codec.
func loadType(ctx context.Context, conn *pgx.Conn, name string) (*pgtype.Type, error) {
	lower := strings.ToLower(name)
	if lower == "hstore" || strings.HasSuffix(lower, ".hstore") {
		var oid uint32
		if err := conn.QueryRow(ctx, "SELECT $1::text::regtype::oid", name).Scan(&oid); err != nil {
			return nil, fmt.Errorf("%w (is the hstore extension installed? CREATE EXTENSION hstore)", err)
		}
		return &pgtype.Type{Name: "hstore", OID: oid, Codec: pgtype.HstoreCodec{}}, nil
	}
	return conn.LoadType(ctx, name)
}
