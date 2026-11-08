// Package s3 wraps the AWS SDK for Go v2 S3 client for S3-compatible storage,
// in particular the SeaweedFS S3 gateway, and adds a few object helpers.
//
// A Client is bound to one bucket. It has plain helpers for the common work
// (Put, Get, Head, List, Copy, Upload, DeleteMany, tags, presigned URLs) and a
// method for every operation of the SDK's S3 client, which takes the SDK's input
// struct with the bucket filled in. The native SDK client is also exported as
// Client.S3. Wrap builds a Client around an SDK client you already have.
//
//	c, err := s3.New(ctx, s3.Config{
//		Endpoint:        "https://seaweed.internal:8333",
//		AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
//		SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
//		Bucket:          "uploads",
//		UsePathStyle:    true, // required by SeaweedFS unless it has bucket DNS set up
//	})
//	if err != nil {
//		return err
//	}
//	defer c.Close()
//
// Certificates are always verified; the package offers no option to skip that.
// Authorization (who may read or write which key) is the caller's concern.
package s3

//go:generate go run ../internal/gens3 -out client_gen.go

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// Defaults applied when the matching Config field is zero.
const (
	DefaultRegion         = "us-east-1"
	DefaultConnectTimeout = 10 * time.Second
	// MaxPresignExpiry is the longest lifetime S3 accepts for a presigned URL.
	MaxPresignExpiry = 7 * 24 * time.Hour
)

// ErrNotFound is returned by Get when the object does not exist.
var ErrNotFound = errors.New("s3: object not found")

// Config describes a connection to one bucket.
type Config struct {
	// Endpoint is the base URL of an S3-compatible service, such as
	// "https://seaweed.internal:8333". Leave empty for AWS S3.
	Endpoint string
	// Region is used for request signing. Default DefaultRegion; most
	// S3-compatible services accept any value.
	Region string
	// AccessKeyID and SecretAccessKey are required.
	AccessKeyID     string
	SecretAccessKey string
	// Bucket is the bucket every helper operates on. It must already exist.
	Bucket string
	// UsePathStyle addresses buckets as endpoint/bucket/key instead of
	// bucket.endpoint/key. Set it for SeaweedFS and most self-hosted services.
	UsePathStyle bool
	// ConnectTimeout bounds the startup check. Default DefaultConnectTimeout.
	ConnectTimeout time.Duration
	// APIOptions are added to every request's middleware stack. Use
	// otels3.APIOptions from the telemetry/otels3 package for OpenTelemetry
	// spans and metrics, or any smithy middleware of your own.
	APIOptions []func(*middleware.Stack) error
}

// Validate reports problems with the configuration without connecting.
// Errors never include the credentials.
func (c Config) Validate() error {
	switch {
	case c.AccessKeyID == "" || c.SecretAccessKey == "":
		return errors.New("s3: AccessKeyID and SecretAccessKey are required")
	case c.Bucket == "":
		return errors.New("s3: Bucket is required")
	case c.ConnectTimeout < 0:
		return errors.New("s3: ConnectTimeout must not be negative")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("s3: Endpoint must be an http:// or https:// URL")
		}
		if u.User != nil {
			return errors.New("s3: Endpoint must not contain credentials")
		}
	}
	return nil
}

// Client talks to one bucket. It is safe for concurrent use.
type Client struct {
	// S3 is the native AWS SDK client, for operations the helpers do not cover.
	S3 *awss3.Client
	// Bucket is the bucket the helpers use.
	Bucket string

	presign *awss3.PresignClient
	http    *http.Client
}

// New validates cfg, builds the client and checks that the bucket is reachable
// with the given credentials, so mistakes surface at startup. ctx bounds the
// startup only. The caller must call Close when finished.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	region := cfg.Region
	if region == "" {
		region = DefaultRegion
	}

	// A private transport (with default certificate verification) so that Close
	// can release its idle connections without touching http.DefaultTransport.
	hc := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}

	opts := awss3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: cfg.UsePathStyle,
		HTTPClient:   hc,
		// Only add checksums when an operation requires them. The SDK default
		// (always) sends trailing CRC32 checksums that many S3-compatible
		// gateways reject or mishandle.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	opts.APIOptions = append(opts.APIOptions, cfg.APIOptions...)
	native := awss3.New(opts)
	c := &Client{S3: native, Bucket: cfg.Bucket, presign: awss3.NewPresignClient(native), http: hc}

	timeout := cfg.ConnectTimeout
	if timeout == 0 {
		timeout = DefaultConnectTimeout
	}
	pingCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Ping checks that the bucket exists and the credentials may access it.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.S3.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(c.Bucket)}); err != nil {
		return fmt.Errorf("s3: check bucket %q: %w", c.Bucket, err)
	}
	return nil
}

// Close releases idle network connections. The Client must not be used for new
// requests afterwards.
func (c *Client) Close() {
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// Put uploads body as key with the given content type (empty means the service
// default). The body must be seekable so the request can be signed and retried;
// use bytes.NewReader for in-memory data or an *os.File. An existing object with
// the same key is replaced.
//
// ponytail: a single PutObject call, so objects are limited to 5 GiB and
// non-seekable streams must be buffered by the caller. Add multipart upload via
// c.S3 if larger objects are needed.
func (c *Client) Put(ctx context.Context, key string, body io.ReadSeeker, contentType string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	in := &awss3.PutObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), Body: body}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err := c.S3.PutObject(ctx, in); err != nil {
		return fmt.Errorf("s3: put %q: %w", key, err)
	}
	return nil
}

// Get returns the object's content. The caller must close the returned reader.
// It returns ErrNotFound (testable with errors.Is) if the key does not exist.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	out, err := c.S3.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %q", ErrNotFound, key)
		}
		return nil, fmt.Errorf("s3: get %q: %w", key, err)
	}
	return out.Body, nil
}

// Delete removes key. Deleting a key that does not exist is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if _, err := c.S3.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("s3: delete %q: %w", key, err)
	}
	return nil
}

// PresignGet returns a URL that lets anyone holding it download key until
// expires has elapsed. It does not check that the object exists.
func (c *Client) PresignGet(ctx context.Context, key string, expires time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	if err := checkExpiry(expires); err != nil {
		return "", err
	}
	req, err := c.presign.PresignGetObject(ctx,
		&awss3.GetObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)},
		func(o *awss3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return "", fmt.Errorf("s3: presign get %q: %w", key, err)
	}
	return req.URL, nil
}

// PresignPut returns a URL that lets anyone holding it upload key with an HTTP
// PUT until expires has elapsed.
func (c *Client) PresignPut(ctx context.Context, key string, expires time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	if err := checkExpiry(expires); err != nil {
		return "", err
	}
	req, err := c.presign.PresignPutObject(ctx,
		&awss3.PutObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)},
		func(o *awss3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return "", fmt.Errorf("s3: presign put %q: %w", key, err)
	}
	return req.URL, nil
}

// checkKey rejects keys that are empty or that a proxy or gateway could
// interpret as a path rather than a name: control characters, backslashes, a
// leading slash, and "." or ".." segments (which could escape the bucket in
// path-style URLs).
func checkKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("s3: key must not be empty")
	}
	if strings.HasPrefix(key, "/") || strings.ContainsRune(key, '\\') {
		return errors.New("s3: key must not start with / or contain backslashes")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return errors.New("s3: key must not contain control characters")
		}
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return errors.New(`s3: key must not contain "." or ".." path segments`)
		}
	}
	return nil
}

func checkExpiry(d time.Duration) error {
	if d <= 0 || d > MaxPresignExpiry {
		return fmt.Errorf("s3: presign expiry must be between 1ns and %v", MaxPresignExpiry)
	}
	return nil
}

// opError adds the operation name to err. For object-level operations, a
// missing key or version is reported so that errors.Is(err, ErrNotFound) holds
// while the SDK error stays reachable through errors.As.
func opError(op string, err error) error {
	if strings.Contains(op, "Object") && isNotFound(err) {
		return fmt.Errorf("%w: %s: %w", ErrNotFound, op, err)
	}
	return fmt.Errorf("s3: %s: %w", op, err)
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "NoSuchVersion":
			return true
		}
	}
	return false
}
