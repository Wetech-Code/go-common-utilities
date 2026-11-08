//go:build integration

package otels3_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/s3"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otels3"
)

func TestIntegrationOperationSpans(t *testing.T) {
	endpoint := os.Getenv("GCU_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("GCU_TEST_S3_ENDPOINT not set")
	}
	access, secret := os.Getenv("GCU_TEST_S3_ACCESS_KEY"), os.Getenv("GCU_TEST_S3_SECRET_KEY")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	env := oteltest.New(t)

	bucket := fmt.Sprintf("gcu-otel-%d", time.Now().UnixNano()%1e9)
	native := awss3.New(awss3.Options{
		Region: s3.DefaultRegion, BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider(access, secret, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	if _, err := native.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	defer native.DeleteBucket(context.Background(), &awss3.DeleteBucketInput{Bucket: aws.String(bucket)})

	c, err := s3.New(ctx, s3.Config{
		Endpoint: endpoint, AccessKeyID: access, SecretAccessKey: secret, Bucket: bucket, UsePathStyle: true,
		APIOptions: otels3.APIOptions(env.TP, env.MP),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Put(ctx, "customers/alice.txt", strings.NewReader("hello"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	defer c.Delete(context.Background(), "customers/alice.txt")
	rc, err := c.Get(ctx, "customers/alice.txt")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if _, err := c.Get(ctx, "customers/none.txt"); !errors.Is(err, s3.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	seen := map[string]int{}
	codes := map[string]bool{}
	for _, sp := range env.Spans() {
		seen[sp.Name]++
		if sp.Failed {
			codes[sp.Attrs["error.type"].AsString()] = true
		}
		for k, v := range sp.Attrs {
			if strings.Contains(v.Emit(), "alice") || strings.Contains(v.Emit(), bucket) {
				t.Errorf("attribute %s leaks a bucket or key: %s", k, v.Emit())
			}
		}
	}
	for _, name := range []string{"S3 HeadBucket", "S3 PutObject", "S3 GetObject"} {
		if seen[name] == 0 {
			t.Errorf("no %q span; got %v", name, seen)
		}
	}
	if !codes["NoSuchKey"] {
		t.Errorf("failure codes = %v; want NoSuchKey", codes)
	}
}
