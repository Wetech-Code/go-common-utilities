// Package redis creates a go-redis client from a validated Config.
//
// New returns the native *redis.Client from github.com/redis/go-redis/v9, so
// every Redis command is used directly. The package adds only validation,
// timeouts and a startup ping. It defines no queues, locks or key conventions.
//
// The caller owns the returned client and must close it when finished:
//
//	rdb, err := redis.New(ctx, redis.Config{URL: os.Getenv("REDIS_URL")})
//	if err != nil {
//		return err
//	}
//	defer rdb.Close()
//
// Wrap(rdb) returns a Client with plain (value, error) methods for the common
// operations (strings, keys, hashes, lists, sets, sorted sets, pub/sub, pipelines);
// the full go-redis client stays available as Client.Native.
//
// Use a rediss:// URL for TLS. Certificates are verified against the host in the
// URL; the package offers no option to skip verification.
package redis

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// DefaultConnectTimeout is used when Config.DialTimeout is zero and bounds the
// startup ping.
const DefaultConnectTimeout = 10 * time.Second

// Config describes a Redis client. Only URL is required.
type Config struct {
	// URL is "redis://[user:pass@]host:port/db" or "rediss://..." for TLS.
	URL string
	// PoolSize caps connections. Zero keeps the go-redis default.
	PoolSize int
	// DialTimeout, ReadTimeout and WriteTimeout bound network operations.
	// Zero keeps the go-redis defaults, except DialTimeout which defaults to
	// DefaultConnectTimeout.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// Validate reports problems with the configuration without connecting.
// Errors never include the URL, which may contain a password.
func (c Config) Validate() error {
	switch {
	case c.URL == "":
		return errors.New("redis: URL is required")
	case !strings.HasPrefix(c.URL, "redis://") && !strings.HasPrefix(c.URL, "rediss://") && !strings.HasPrefix(c.URL, "unix://"):
		return errors.New("redis: URL must start with redis://, rediss:// or unix://")
	case c.PoolSize < 0:
		return errors.New("redis: PoolSize must not be negative")
	case c.DialTimeout < 0 || c.ReadTimeout < 0 || c.WriteTimeout < 0:
		return errors.New("redis: timeouts must not be negative")
	}
	return nil
}

// New validates cfg, creates the client and pings the server, so a bad address
// or password is reported at startup. ctx bounds the startup only. On error no
// client is left open.
func New(ctx context.Context, cfg Config) (*goredis.Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts, err := goredis.ParseURL(cfg.URL)
	if err != nil {
		// Report the problem without echoing the URL, which may hold a password.
		return nil, errors.New("redis: URL is not valid")
	}
	if cfg.PoolSize > 0 {
		opts.PoolSize = cfg.PoolSize
	}
	opts.DialTimeout = DefaultConnectTimeout
	if cfg.DialTimeout > 0 {
		opts.DialTimeout = cfg.DialTimeout
	}
	if cfg.ReadTimeout > 0 {
		opts.ReadTimeout = cfg.ReadTimeout
	}
	if cfg.WriteTimeout > 0 {
		opts.WriteTimeout = cfg.WriteTimeout
	}

	client := goredis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis: ping: %w", err)
	}
	return client, nil
}
