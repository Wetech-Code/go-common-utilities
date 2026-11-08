package otels3_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/Wetech-Code/go-common-utilities/internal/otelkit"
	"github.com/Wetech-Code/go-common-utilities/internal/oteltest"
	"github.com/Wetech-Code/go-common-utilities/telemetry/otels3"
)

func client(t *testing.T, env *oteltest.Env, h http.HandlerFunc) *awss3.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return awss3.New(awss3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider("id", "secret", ""),
		Retryer:                    aws.NopRetryer{},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		APIOptions:                 otels3.APIOptions(env.TP, env.MP),
	})
}

func TestOperationSpansAndMetrics(t *testing.T) {
	env := oteltest.New(t)
	c := client(t, env, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "locked") {
			w.WriteHeader(403)
			w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>no for secret-bucket</Message></Error>`))
			return
		}
		w.WriteHeader(200)
	})
	ctx := context.Background()
	if _, err := c.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String("secret-bucket")}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("locked"), Key: aws.String("customers/alice.pdf")}); err == nil {
		t.Fatal("want an error")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	c.HeadBucket(cctx, &awss3.HeadBucketInput{Bucket: aws.String("b")})

	s := env.Spans()
	if len(s) != 3 {
		t.Fatalf("spans = %+v", s)
	}
	if s[0].Name != "S3 HeadBucket" || s[0].Kind != "client" || s[0].Failed || s[0].Attrs["rpc.system"].AsString() != "aws-api" ||
		s[0].Attrs["rpc.service"].AsString() != "S3" || s[0].Attrs["rpc.method"].AsString() != "HeadBucket" || s[0].Attrs["db.system.name"].AsString() != "aws.s3" {
		t.Errorf("head span = %+v", s[0])
	}
	if s[1].Name != "S3 GetObject" || !s[1].Failed || s[1].Attrs["error.type"].AsString() != "AccessDenied" {
		t.Errorf("get span = %+v", s[1])
	}
	if s[2].Attrs["error.type"].AsString() != "context.Canceled" {
		t.Errorf("cancelled span = %+v", s[2])
	}
	for _, sp := range s {
		for k, v := range sp.Attrs {
			for _, secret := range []string{"secret-bucket", "alice.pdf", "locked"} {
				if strings.Contains(v.Emit(), secret) {
					t.Errorf("attribute %s leaks %q", k, secret)
				}
			}
		}
	}
	pts := env.Histogram(t, otelkit.DurationMetric)
	if len(pts) != 3 {
		t.Errorf("points = %v", pts)
	}
}

func TestNilProvidersAreNoOps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := awss3.New(awss3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("id", "secret", ""),
		APIOptions:  otels3.APIOptions(nil, nil),
	})
	if _, err := c.HeadBucket(context.Background(), &awss3.HeadBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Fatal(err)
	}
}
