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
// TLS is controlled by the connection URL (sslmode=verify-full is recommended
// for production). The package offers no option to skip certificate checks.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

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
