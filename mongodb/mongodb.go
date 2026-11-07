// Package mongodb creates an official MongoDB Go driver (v2) client from a
// validated Config.
//
// New returns the native *mongo.Client, so collections, queries, transactions
// and change streams all use the driver API directly. The package adds only
// validation, timeouts and a startup ping. It owns no schema or repositories.
//
// The caller owns the returned client and must disconnect it when finished:
//
//	client, err := mongodb.New(ctx, mongodb.Config{URI: os.Getenv("MONGODB_URI")})
//	if err != nil {
//		return err
//	}
//	defer client.Disconnect(context.Background())
//
// Wrap(client) returns a Client with plain methods for the driver's database,
// collection, index, change-stream, transaction and GridFS operations, decoding
// into structs and mapping a missing document to ErrNotFound. The driver client
// stays reachable as Client.Native.
//
// TLS is controlled by the URI (tls=true or a mongodb+srv:// URI). The package
// offers no option to skip certificate checks.
package mongodb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
)

// DefaultConnectTimeout is used when Config.ConnectTimeout is zero.
const DefaultConnectTimeout = 10 * time.Second

// Config describes a MongoDB client. Only URI is required.
type Config struct {
	// URI is a connection string such as "mongodb://user:pass@host:27017/?tls=true".
	URI string
	// AppName is reported to the server in the connection handshake.
	AppName string
	// MaxPoolSize caps connections per server. Zero keeps the driver default.
	MaxPoolSize uint64
	// ConnectTimeout bounds each new connection, server selection and the
	// startup ping. Default DefaultConnectTimeout.
	ConnectTimeout time.Duration
}

// Validate reports problems with the configuration without connecting.
// Errors never include the URI, which may contain a password.
func (c Config) Validate() error {
	switch {
	case c.URI == "":
		return errors.New("mongodb: URI is required")
	case !strings.HasPrefix(c.URI, "mongodb://") && !strings.HasPrefix(c.URI, "mongodb+srv://"):
		return errors.New("mongodb: URI must start with mongodb:// or mongodb+srv://")
	case c.ConnectTimeout < 0:
		return errors.New("mongodb: ConnectTimeout must not be negative")
	}
	return nil
}

// New validates cfg, creates the client and pings the primary, so a bad address
// or credential is reported at startup. ctx bounds the startup only. On error
// no client is left open.
func New(ctx context.Context, cfg Config) (*mongo.Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	timeout := cfg.ConnectTimeout
	if timeout == 0 {
		timeout = DefaultConnectTimeout
	}
	opts := options.Client().
		ApplyURI(cfg.URI).
		SetConnectTimeout(timeout).
		SetServerSelectionTimeout(timeout)
	if cfg.AppName != "" {
		opts.SetAppName(cfg.AppName)
	}
	if cfg.MaxPoolSize > 0 {
		opts.SetMaxPoolSize(cfg.MaxPoolSize)
	}

	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("mongodb: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := client.Ping(pingCtx, readpref.Primary()); err != nil {
		// Use a fresh context so cleanup still happens if ctx was cancelled.
		disconnectCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = client.Disconnect(disconnectCtx)
		return nil, fmt.Errorf("mongodb: ping: %w", err)
	}
	return client, nil
}
