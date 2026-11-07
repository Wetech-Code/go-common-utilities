//go:build integration

package s3_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Run against a SeaweedFS S3 gateway (any S3-compatible service should work):
//
//	GCU_TEST_S3_ENDPOINT=http://localhost:8333 GCU_TEST_S3_ACCESS_KEY=... \
//	GCU_TEST_S3_SECRET_KEY=... go test -tags integration ./s3
//
// The test creates a bucket named GCU_TEST_S3_BUCKET (default "gcu-test") if it
// does not exist, and removes the objects it writes.
func TestIntegrationLifecycle(t *testing.T) {
	endpoint := os.Getenv("GCU_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("GCU_TEST_S3_ENDPOINT not set")
	}
	cfg := s3.Config{
		Endpoint:        endpoint,
		AccessKeyID:     os.Getenv("GCU_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("GCU_TEST_S3_SECRET_KEY"),
		Bucket:          os.Getenv("GCU_TEST_S3_BUCKET"),
		UsePathStyle:    true,
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "gcu-test"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ensureBucket(t, ctx, cfg)

	c, err := s3.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	key := fmt.Sprintf("it/%d/hello world.txt", time.Now().UnixNano())
	defer c.Delete(ctx, key)

	// Put / Get round trip with a content type and a key that needs URL encoding.
	if err := c.Put(ctx, key, strings.NewReader("hello seaweed"), "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "hello seaweed" {
		t.Errorf("Get = %q", body)
	}

	// Presigned GET and PUT work with a plain HTTP client, no credentials.
	getURL, err := c.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := httpDo(t, ctx, http.MethodGet, getURL, nil); got != "hello seaweed" {
		t.Errorf("presigned GET = %q", got)
	}
	putURL, err := c.PresignPut(ctx, key+".up", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Delete(ctx, key+".up")
	httpDo(t, ctx, http.MethodPut, putURL, strings.NewReader("via presign"))
	rc, err = c.Get(ctx, key+".up")
	if err != nil {
		t.Fatalf("Get after presigned PUT: %v", err)
	}
	body, _ = io.ReadAll(rc)
	rc.Close()
	if string(body) != "via presign" {
		t.Errorf("after presigned PUT = %q", body)
	}

	// Delete, then the object is gone and Delete stays idempotent.
	if err := c.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, key); !errors.Is(err, s3.ErrNotFound) {
		t.Errorf("Get after Delete: want ErrNotFound, got %v", err)
	}
	if err := c.Delete(ctx, key); err != nil {
		t.Errorf("second Delete: %v", err)
	}

	// Wrong credentials are rejected at startup.
	bad := cfg
	bad.SecretAccessKey = "wrong-" + cfg.SecretAccessKey
	if c2, err := s3.New(ctx, bad); err == nil {
		c2.Close()
		t.Error("New with wrong secret should fail")
	}
}

func httpDo(t *testing.T, ctx context.Context, method, url string, body io.Reader) string {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s presigned URL: status %d: %s", method, resp.StatusCode, b)
	}
	return string(b)
}

// ensureBucket creates the bucket with the native SDK; the package under test
// deliberately never creates buckets.
func ensureBucket(t *testing.T, ctx context.Context, cfg s3.Config) {
	t.Helper()
	native := awss3.New(awss3.Options{
		Region:                     s3.DefaultRegion,
		BaseEndpoint:               aws.String(cfg.Endpoint),
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	_, err := native.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(cfg.Bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		t.Fatalf("create bucket: %v", err)
	}
}
