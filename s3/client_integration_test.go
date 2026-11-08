//go:build integration

package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wetech-Code/go-common-utilities/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func nativeFor(cfg s3.Config) *awss3.Client {
	return awss3.New(awss3.Options{
		Region:                     s3.DefaultRegion,
		BaseEndpoint:               aws.String(cfg.Endpoint),
		Credentials:                credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle:               true,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
}

func integrationConfig(t *testing.T) s3.Config {
	t.Helper()
	endpoint := os.Getenv("GCU_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("GCU_TEST_S3_ENDPOINT not set")
	}
	return s3.Config{
		Endpoint:        endpoint,
		AccessKeyID:     os.Getenv("GCU_TEST_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("GCU_TEST_S3_SECRET_KEY"),
		UsePathStyle:    true,
	}
}

// newBucket creates a bucket through the Client's own CreateBucket and returns a
// Client bound to it. The bucket is emptied and deleted when the test ends.
func newBucket(t *testing.T, ctx context.Context, cfg s3.Config, lock bool) *s3.Client {
	t.Helper()
	var b [6]byte
	rand.Read(b[:])
	name := fmt.Sprintf("gcu-c-%x", b)
	c := s3.Wrap(nativeFor(cfg), name)
	in := &awss3.CreateBucketInput{}
	if lock {
		in.ObjectLockEnabledForBucket = aws.Bool(true)
	}
	if _, err := c.CreateBucket(ctx, in); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		// Remove every version and delete marker, then the bucket.
		out, err := c.ListObjectVersions(cctx, nil)
		if err == nil {
			for _, v := range out.Versions {
				c.DeleteObject(cctx, &awss3.DeleteObjectInput{Key: v.Key, VersionId: v.VersionId, BypassGovernanceRetention: aws.Bool(true)})
			}
			for _, m := range out.DeleteMarkers {
				c.DeleteObject(cctx, &awss3.DeleteObjectInput{Key: m.Key, VersionId: m.VersionId})
			}
		}
		keys := []string{}
		c.Walk(cctx, "", func(o s3.ObjectInfo) error { keys = append(keys, o.Key); return nil })
		c.DeleteMany(cctx, keys)
		c.DeleteBucket(cctx, nil)
	})
	return c
}

func sum(b []byte) [32]byte { return sha256.Sum256(b) }

func TestIntegrationClientObjects(t *testing.T) {
	cfg := integrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := newBucket(t, ctx, cfg, false)

	t.Run("head exists get download", func(t *testing.T) {
		if err := c.Put(ctx, "a/one.txt", strings.NewReader("hello"), "text/plain"); err != nil {
			t.Fatal(err)
		}
		info, err := c.Head(ctx, "a/one.txt")
		if err != nil || info.Size != 5 || info.ContentType != "text/plain" || info.ETag == "" || time.Since(info.LastModified) > time.Hour {
			t.Errorf("Head = %+v, %v", info, err)
		}
		if ok, err := c.Exists(ctx, "a/one.txt"); !ok || err != nil {
			t.Errorf("Exists = %v, %v", ok, err)
		}
		if ok, err := c.Exists(ctx, "a/none.txt"); ok || err != nil {
			t.Errorf("Exists missing = %v, %v", ok, err)
		}
		if _, err := c.Head(ctx, "a/none.txt"); !errors.Is(err, s3.ErrNotFound) {
			t.Errorf("Head missing = %v", err)
		}
		var buf bytes.Buffer
		if n, err := c.Download(ctx, "a/one.txt", &buf); err != nil || n != 5 || buf.String() != "hello" {
			t.Errorf("Download = %d, %v, %q", n, err, buf.String())
		}
		if _, err := c.Download(ctx, "a/none.txt", &buf); !errors.Is(err, s3.ErrNotFound) {
			t.Errorf("Download missing = %v", err)
		}
		// GetObject and its not-found mapping keep the SDK error reachable.
		if _, err := c.GetObject(ctx, &awss3.GetObjectInput{Key: aws.String("a/none.txt")}); !errors.Is(err, s3.ErrNotFound) {
			t.Errorf("GetObject missing = %v", err)
		}
	})

	t.Run("copy escapes special characters", func(t *testing.T) {
		src := "sp ace/ü & plus+.txt"
		if err := c.Put(ctx, src, strings.NewReader("copy me"), ""); err != nil {
			t.Fatal(err)
		}
		if err := c.Copy(ctx, src, "copied/dst.txt"); err != nil {
			t.Fatalf("Copy: %v", err)
		}
		var buf bytes.Buffer
		if _, err := c.Download(ctx, "copied/dst.txt", &buf); err != nil || buf.String() != "copy me" {
			t.Errorf("copied = %q, %v", buf.String(), err)
		}
		if err := c.Copy(ctx, "no-such-source", "x"); !errors.Is(err, s3.ErrNotFound) {
			t.Errorf("Copy missing source = %v", err)
		}
	})

	t.Run("list walk delete many across pages", func(t *testing.T) {
		const n = 1105 // more than one ListObjectsV2 page
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("many/%05d", i)
			if err := c.Put(ctx, keys[i], strings.NewReader("x"), ""); err != nil {
				t.Fatal(err)
			}
		}
		all, err := c.List(ctx, "many/")
		if err != nil || len(all) != n || all[0].Key != keys[0] || all[n-1].Key != keys[n-1] || all[0].Size != 1 {
			t.Fatalf("List = %d objects, %v", len(all), err)
		}
		stop := errors.New("enough")
		seen := 0
		if err := c.Walk(ctx, "many/", func(s3.ObjectInfo) error { seen++; return stop }); err != stop || seen != 1 {
			t.Errorf("Walk stop = %v after %d", err, seen)
		}
		if err := c.DeleteMany(ctx, keys); err != nil {
			t.Fatalf("DeleteMany: %v", err)
		}
		if left, _ := c.List(ctx, "many/"); len(left) != 0 {
			t.Errorf("%d objects left", len(left))
		}
		if err := c.DeleteMany(ctx, keys[:3]); err != nil {
			t.Errorf("DeleteMany of missing keys: %v", err)
		}
	})

	t.Run("multipart upload", func(t *testing.T) {
		for _, size := range []int{0, 1 << 20, 10 << 20, 12<<20 + 345} {
			data := make([]byte, size)
			rand.Read(data)
			key := fmt.Sprintf("big/%d", size)
			// iotest-style one-byte-at-a-time reads are slow, so use a plain non-seekable reader.
			if err := c.Upload(ctx, key, io.MultiReader(bytes.NewReader(data)), "application/octet-stream", 5<<20); err != nil {
				t.Fatalf("Upload %d: %v", size, err)
			}
			var buf bytes.Buffer
			if _, err := c.Download(ctx, key, &buf); err != nil || sum(buf.Bytes()) != sum(data) {
				t.Errorf("size %d: downloaded %d bytes, checksum equal=%v, err=%v", size, buf.Len(), sum(buf.Bytes()) == sum(data), err)
			}
		}
		// Nothing is left behind by finished or failed uploads.
		if out, err := c.ListMultipartUploads(ctx, nil); err != nil || len(out.Uploads) != 0 {
			t.Errorf("leftover uploads: %v %v", out, err)
		}
		cancelled, stop := context.WithCancel(ctx)
		stop()
		if err := c.Upload(cancelled, "big/cancelled", bytes.NewReader(make([]byte, 6<<20)), "", 5<<20); err == nil {
			t.Error("a cancelled context must fail Upload")
		}
	})

	t.Run("tags", func(t *testing.T) {
		if err := c.SetTags(ctx, "a/one.txt", map[string]string{"team": "ops", "env": "test"}); err != nil {
			t.Fatal(err)
		}
		got, err := c.GetTags(ctx, "a/one.txt")
		if err != nil || len(got) != 2 || got["team"] != "ops" {
			t.Errorf("GetTags = %v, %v", got, err)
		}
		if err := c.SetTags(ctx, "a/one.txt", nil); err != nil {
			t.Fatal(err)
		}
		if got, _ := c.GetTags(ctx, "a/one.txt"); len(got) != 0 {
			t.Errorf("tags after clearing = %v", got)
		}
		if _, err := c.GetTags(ctx, "a/none.txt"); !errors.Is(err, s3.ErrNotFound) {
			t.Errorf("GetTags missing = %v", err)
		}
	})

	t.Run("presigned head delete upload part post", func(t *testing.T) {
		if err := c.Put(ctx, "p/obj", strings.NewReader("presigned"), ""); err != nil {
			t.Fatal(err)
		}
		headURL, err := c.PresignHead(ctx, "p/obj", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if resp := do(t, ctx, http.MethodHead, headURL, nil); resp.StatusCode != 200 || resp.ContentLength != 9 {
			t.Errorf("presigned HEAD: %d len %d", resp.StatusCode, resp.ContentLength)
		}
		hb, err := c.PresignHeadBucket(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if resp := do(t, ctx, http.MethodHead, hb, nil); resp.StatusCode != 200 {
			t.Errorf("presigned HEAD bucket: %d", resp.StatusCode)
		}

		// Multipart upload assembled from presigned part URLs only.
		mp, err := c.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Key: aws.String("p/parts")})
		if err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 5<<20+10)
		rand.Read(data)
		var parts []types.CompletedPart
		for i, chunk := range [][]byte{data[:5<<20], data[5<<20:]} {
			u, err := c.PresignUploadPart(ctx, "p/parts", aws.ToString(mp.UploadId), int32(i+1), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			resp := do(t, ctx, http.MethodPut, u, bytes.NewReader(chunk))
			parts = append(parts, types.CompletedPart{ETag: aws.String(resp.Header.Get("ETag")), PartNumber: aws.Int32(int32(i + 1))})
		}
		if _, err := c.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
			Key: aws.String("p/parts"), UploadId: mp.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		}); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := c.Download(ctx, "p/parts", &buf); err != nil || sum(buf.Bytes()) != sum(data) {
			t.Errorf("assembled object differs: %v", err)
		}

		// A browser-style POST form built from the fields we produce.
		post, err := c.PresignPost(ctx, "p/posted", time.Minute)
		if err != nil || post.URL == "" || len(post.Fields) == 0 {
			t.Fatalf("PresignPost = %+v, %v", post, err)
		}
		var form bytes.Buffer
		mw := multipart.NewWriter(&form)
		for k, v := range post.Fields {
			mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("file", "posted.txt")
		fw.Write([]byte("via post form"))
		mw.Close()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, post.URL, &form)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Errorf("POST form: %d %s", resp.StatusCode, body)
		}
		var posted bytes.Buffer
		if _, err := c.Download(ctx, "p/posted", &posted); err != nil || posted.String() != "via post form" {
			t.Errorf("after POST form: %q %v", posted.String(), err)
		}

		delURL, err := c.PresignDelete(ctx, "p/obj", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if resp := do(t, ctx, http.MethodDelete, delURL, nil); resp.StatusCode/100 != 2 {
			t.Errorf("presigned DELETE: %d", resp.StatusCode)
		}
		if ok, _ := c.Exists(ctx, "p/obj"); ok {
			t.Error("object still exists after presigned DELETE")
		}
		// The presigned bucket delete is only signed, not used: the bucket has content.
		if u, err := c.PresignDeleteBucket(ctx, time.Minute); err != nil || !strings.Contains(u, "X-Amz-Signature=") {
			t.Errorf("PresignDeleteBucket = %q, %v", u, err)
		}
	})
}

func do(t *testing.T, ctx context.Context, method, url string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// op is one S3 operation called with minimal valid input.
type op struct {
	name string
	call func() error
}

// TestIntegrationEveryOperation calls every generated operation against a real
// SeaweedFS gateway. SeaweedFS implements a subset of S3, so each operation is
// listed in refused if it is expected to be turned down; an operation outside
// that list must succeed. A refusal still proves the call reached the service
// and came back as an ordinary error rather than a panic.
func TestIntegrationEveryOperation(t *testing.T) {
	cfg := integrationConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c := newBucket(t, ctx, cfg, false)
	lock := newBucket(t, ctx, cfg, true)
	s := aws.String
	key := s("k")
	scratch := s("scratch")

	if err := c.Put(ctx, "k", strings.NewReader("0123456789"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "scratch", strings.NewReader("scratch"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	e := func(_ any, err error) error { return err }

	// want turns a mismatch into an error, so a call that returns success but did
	// not do what was asked still fails the test.
	want := func(ok bool, format string, args ...any) error {
		if ok {
			return nil
		}
		return fmt.Errorf(format, args...)
	}
	var mpID string
	var etag1, etag2 string
	if _, err := lock.PutObject(ctx, &awss3.PutObjectInput{Key: key, Body: strings.NewReader("0123456789"), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32}); err != nil {
		t.Fatalf("PutObject into the lock bucket: %v", err)
	}

	ops := []op{
		// Multipart: a real upload id, so the service answers on the merits.
		{"CreateMultipartUpload", func() error {
			out, err := c.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Key: s("mp")})
			if err != nil {
				return err
			}
			mpID = aws.ToString(out.UploadId)
			return nil
		}},
		{"ListMultipartUploads", func() error {
			out, err := c.ListMultipartUploads(ctx, nil)
			if err == nil && len(out.Uploads) != 1 {
				return fmt.Errorf("%d uploads in progress, want 1", len(out.Uploads))
			}
			return err
		}},
		{"UploadPart", func() error {
			out, err := c.UploadPart(ctx, &awss3.UploadPartInput{Key: s("mp"), UploadId: &mpID, PartNumber: aws.Int32(1), Body: bytes.NewReader(bytes.Repeat([]byte("p"), 5<<20))})
			if err == nil {
				etag1 = aws.ToString(out.ETag)
			}
			return err
		}},
		{"UploadPartCopy", func() error {
			out, err := c.UploadPartCopy(ctx, &awss3.UploadPartCopyInput{Key: s("mp"), UploadId: &mpID, PartNumber: aws.Int32(2), CopySource: s(c.Bucket + "/k")})
			if err == nil {
				etag2 = aws.ToString(out.CopyPartResult.ETag)
			}
			return err
		}},
		{"ListParts", func() error {
			out, err := c.ListParts(ctx, &awss3.ListPartsInput{Key: s("mp"), UploadId: &mpID})
			if err == nil && len(out.Parts) != 2 {
				return fmt.Errorf("%d parts, want 2", len(out.Parts))
			}
			return err
		}},
		{"CompleteMultipartUpload", func() error {
			return e(c.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{Key: s("mp"), UploadId: &mpID, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{ETag: &etag1, PartNumber: aws.Int32(1)}, {ETag: &etag2, PartNumber: aws.Int32(2)}}}}))
		}},
		{"AbortMultipartUpload", func() error {
			out, err := c.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Key: s("mp2")})
			if err != nil {
				return err
			}
			if _, err := c.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{Key: s("mp2"), UploadId: out.UploadId}); err != nil {
				return err
			}
			list, err := c.ListMultipartUploads(ctx, nil)
			if err == nil && len(list.Uploads) != 0 {
				return fmt.Errorf("%d uploads left after abort", len(list.Uploads))
			}
			return err
		}},

		// Buckets
		{"HeadBucket", func() error { return e(c.HeadBucket(ctx, nil)) }},
		{"ListBuckets", func() error { return e(c.ListBuckets(ctx, nil)) }},
		{"ListDirectoryBuckets", func() error { return e(c.ListDirectoryBuckets(ctx, nil)) }},
		{"GetBucketLocation", func() error { return e(c.GetBucketLocation(ctx, nil)) }},
		{"GetBucketVersioning", func() error {
			out, err := c.GetBucketVersioning(ctx, nil)
			if err != nil {
				return err
			}
			return want(out.Status == "", "versioning %q before it is enabled", out.Status)
		}},
		{"PutBucketVersioning", func() error {
			if _, err := c.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}}); err != nil {
				return err
			}
			out, err := c.GetBucketVersioning(ctx, nil)
			if err != nil {
				return err
			}
			return want(out.Status == types.BucketVersioningStatusEnabled, "versioning = %q after enabling", out.Status)
		}},
		{"GetBucketAccelerateConfiguration", func() error { return e(c.GetBucketAccelerateConfiguration(ctx, nil)) }},
		{"PutBucketAccelerateConfiguration", func() error {
			return e(c.PutBucketAccelerateConfiguration(ctx, &awss3.PutBucketAccelerateConfigurationInput{AccelerateConfiguration: &types.AccelerateConfiguration{Status: types.BucketAccelerateStatusEnabled}}))
		}},
		{"GetBucketRequestPayment", func() error {
			out, err := c.GetBucketRequestPayment(ctx, nil)
			if err != nil {
				return err
			}
			return want(out.Payer == types.PayerBucketOwner, "payer = %q", out.Payer)
		}},
		{"PutBucketRequestPayment", func() error {
			return e(c.PutBucketRequestPayment(ctx, &awss3.PutBucketRequestPaymentInput{RequestPaymentConfiguration: &types.RequestPaymentConfiguration{Payer: types.PayerBucketOwner}}))
		}},
		{"GetBucketLogging", func() error { return e(c.GetBucketLogging(ctx, nil)) }},
		{"PutBucketLogging", func() error {
			return e(c.PutBucketLogging(ctx, &awss3.PutBucketLoggingInput{BucketLoggingStatus: &types.BucketLoggingStatus{}}))
		}},
		{"GetBucketNotificationConfiguration", func() error { return e(c.GetBucketNotificationConfiguration(ctx, nil)) }},
		{"PutBucketNotificationConfiguration", func() error {
			return e(c.PutBucketNotificationConfiguration(ctx, &awss3.PutBucketNotificationConfigurationInput{NotificationConfiguration: &types.NotificationConfiguration{}}))
		}},
		{"GetBucketWebsite", func() error { return e(c.GetBucketWebsite(ctx, nil)) }},
		{"PutBucketWebsite", func() error {
			return e(c.PutBucketWebsite(ctx, &awss3.PutBucketWebsiteInput{WebsiteConfiguration: &types.WebsiteConfiguration{IndexDocument: &types.IndexDocument{Suffix: s("index.html")}}}))
		}},
		{"DeleteBucketWebsite", func() error { return e(c.DeleteBucketWebsite(ctx, nil)) }},
		{"GetBucketReplication", func() error { return e(c.GetBucketReplication(ctx, nil)) }},
		{"PutBucketReplication", func() error {
			return e(c.PutBucketReplication(ctx, &awss3.PutBucketReplicationInput{ReplicationConfiguration: &types.ReplicationConfiguration{Role: s("arn:aws:iam::1:role/r"), Rules: []types.ReplicationRule{{Status: types.ReplicationRuleStatusEnabled, Destination: &types.Destination{Bucket: s("arn:aws:s3:::dst")}, Filter: &types.ReplicationRuleFilter{Prefix: s("")}}}}}))
		}},
		{"DeleteBucketReplication", func() error { return e(c.DeleteBucketReplication(ctx, nil)) }},
		{"GetBucketAbac", func() error { return e(c.GetBucketAbac(ctx, nil)) }},
		{"PutBucketAbac", func() error {
			return e(c.PutBucketAbac(ctx, &awss3.PutBucketAbacInput{AbacStatus: &types.AbacStatus{Status: types.BucketAbacStatusEnabled}}))
		}},

		// Bucket configuration
		{"PutBucketTagging", func() error {
			return e(c.PutBucketTagging(ctx, &awss3.PutBucketTaggingInput{Tagging: &types.Tagging{TagSet: []types.Tag{{Key: s("a"), Value: s("b")}}}}))
		}},
		{"GetBucketTagging", func() error {
			out, err := c.GetBucketTagging(ctx, nil)
			if err != nil {
				return err
			}
			return want(len(out.TagSet) == 1 && aws.ToString(out.TagSet[0].Key) == "a" && aws.ToString(out.TagSet[0].Value) == "b", "tags = %v", out.TagSet)
		}},
		{"DeleteBucketTagging", func() error { return e(c.DeleteBucketTagging(ctx, nil)) }},
		{"PutBucketCors", func() error {
			return e(c.PutBucketCors(ctx, &awss3.PutBucketCorsInput{CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{AllowedMethods: []string{"GET"}, AllowedOrigins: []string{"*"}}}}}))
		}},
		{"GetBucketCors", func() error {
			out, err := c.GetBucketCors(ctx, nil)
			if err != nil {
				return err
			}
			return want(len(out.CORSRules) == 1 && out.CORSRules[0].AllowedOrigins[0] == "*", "cors = %v", out.CORSRules)
		}},
		{"DeleteBucketCors", func() error { return e(c.DeleteBucketCors(ctx, nil)) }},
		{"PutBucketLifecycleConfiguration", func() error {
			return e(c.PutBucketLifecycleConfiguration(ctx, &awss3.PutBucketLifecycleConfigurationInput{LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{ID: s("r"), Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: s("tmp/")}, Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)}}}}}))
		}},
		{"GetBucketLifecycleConfiguration", func() error {
			out, err := c.GetBucketLifecycleConfiguration(ctx, nil)
			if err != nil {
				return err
			}
			return want(len(out.Rules) == 1 && aws.ToString(out.Rules[0].ID) == "r", "lifecycle = %v", out.Rules)
		}},
		{"DeleteBucketLifecycle", func() error { return e(c.DeleteBucketLifecycle(ctx, nil)) }},
		{"PutBucketPolicy", func() error {
			return e(c.PutBucketPolicy(ctx, &awss3.PutBucketPolicyInput{Policy: s(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::` + c.Bucket + `/pub/*"}]}`)}))
		}},
		{"GetBucketPolicy", func() error {
			out, err := c.GetBucketPolicy(ctx, nil)
			if err != nil {
				return err
			}
			return want(strings.Contains(aws.ToString(out.Policy), "s3:GetObject"), "policy = %s", aws.ToString(out.Policy))
		}},
		{"GetBucketPolicyStatus", func() error { return e(c.GetBucketPolicyStatus(ctx, nil)) }},
		{"DeleteBucketPolicy", func() error { return e(c.DeleteBucketPolicy(ctx, nil)) }},
		{"PutBucketAcl", func() error {
			return e(c.PutBucketAcl(ctx, &awss3.PutBucketAclInput{ACL: types.BucketCannedACLPrivate}))
		}},
		{"GetBucketAcl", func() error { return e(c.GetBucketAcl(ctx, nil)) }},
		{"PutBucketEncryption", func() error {
			return e(c.PutBucketEncryption(ctx, &awss3.PutBucketEncryptionInput{ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256}}}}}))
		}},
		{"GetBucketEncryption", func() error {
			out, err := c.GetBucketEncryption(ctx, nil)
			if err != nil {
				return err
			}
			r := out.ServerSideEncryptionConfiguration.Rules
			return want(len(r) == 1 && r[0].ApplyServerSideEncryptionByDefault.SSEAlgorithm == types.ServerSideEncryptionAes256, "encryption = %v", r)
		}},
		{"DeleteBucketEncryption", func() error { return e(c.DeleteBucketEncryption(ctx, nil)) }},
		{"PutBucketOwnershipControls", func() error {
			return e(c.PutBucketOwnershipControls(ctx, &awss3.PutBucketOwnershipControlsInput{OwnershipControls: &types.OwnershipControls{Rules: []types.OwnershipControlsRule{{ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced}}}}))
		}},
		{"GetBucketOwnershipControls", func() error {
			out, err := c.GetBucketOwnershipControls(ctx, nil)
			if err != nil {
				return err
			}
			r := out.OwnershipControls.Rules
			return want(len(r) == 1 && r[0].ObjectOwnership == types.ObjectOwnershipBucketOwnerEnforced, "ownership = %v", r)
		}},
		{"DeleteBucketOwnershipControls", func() error { return e(c.DeleteBucketOwnershipControls(ctx, nil)) }},
		{"PutPublicAccessBlock", func() error {
			return e(c.PutPublicAccessBlock(ctx, &awss3.PutPublicAccessBlockInput{PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true)}}))
		}},
		{"GetPublicAccessBlock", func() error { return e(c.GetPublicAccessBlock(ctx, nil)) }},
		{"DeletePublicAccessBlock", func() error { return e(c.DeletePublicAccessBlock(ctx, nil)) }},

		// Analytics, inventory, metrics, tiering configurations
		{"PutBucketAnalyticsConfiguration", func() error {
			return e(c.PutBucketAnalyticsConfiguration(ctx, &awss3.PutBucketAnalyticsConfigurationInput{Id: s("a1"), AnalyticsConfiguration: &types.AnalyticsConfiguration{Id: s("a1"), StorageClassAnalysis: &types.StorageClassAnalysis{}}}))
		}},
		{"GetBucketAnalyticsConfiguration", func() error {
			return e(c.GetBucketAnalyticsConfiguration(ctx, &awss3.GetBucketAnalyticsConfigurationInput{Id: s("a1")}))
		}},
		{"ListBucketAnalyticsConfigurations", func() error { return e(c.ListBucketAnalyticsConfigurations(ctx, nil)) }},
		{"DeleteBucketAnalyticsConfiguration", func() error {
			return e(c.DeleteBucketAnalyticsConfiguration(ctx, &awss3.DeleteBucketAnalyticsConfigurationInput{Id: s("a1")}))
		}},
		{"PutBucketInventoryConfiguration", func() error {
			return e(c.PutBucketInventoryConfiguration(ctx, &awss3.PutBucketInventoryConfigurationInput{Id: s("i1"), InventoryConfiguration: &types.InventoryConfiguration{Id: s("i1"), IsEnabled: aws.Bool(true), IncludedObjectVersions: types.InventoryIncludedObjectVersionsAll, Schedule: &types.InventorySchedule{Frequency: types.InventoryFrequencyDaily}, Destination: &types.InventoryDestination{S3BucketDestination: &types.InventoryS3BucketDestination{Bucket: s("arn:aws:s3:::dst"), Format: types.InventoryFormatCsv}}}}))
		}},
		{"GetBucketInventoryConfiguration", func() error {
			return e(c.GetBucketInventoryConfiguration(ctx, &awss3.GetBucketInventoryConfigurationInput{Id: s("i1")}))
		}},
		{"ListBucketInventoryConfigurations", func() error { return e(c.ListBucketInventoryConfigurations(ctx, nil)) }},
		{"DeleteBucketInventoryConfiguration", func() error {
			return e(c.DeleteBucketInventoryConfiguration(ctx, &awss3.DeleteBucketInventoryConfigurationInput{Id: s("i1")}))
		}},
		{"PutBucketMetricsConfiguration", func() error {
			return e(c.PutBucketMetricsConfiguration(ctx, &awss3.PutBucketMetricsConfigurationInput{Id: s("m1"), MetricsConfiguration: &types.MetricsConfiguration{Id: s("m1")}}))
		}},
		{"GetBucketMetricsConfiguration", func() error {
			return e(c.GetBucketMetricsConfiguration(ctx, &awss3.GetBucketMetricsConfigurationInput{Id: s("m1")}))
		}},
		{"ListBucketMetricsConfigurations", func() error { return e(c.ListBucketMetricsConfigurations(ctx, nil)) }},
		{"DeleteBucketMetricsConfiguration", func() error {
			return e(c.DeleteBucketMetricsConfiguration(ctx, &awss3.DeleteBucketMetricsConfigurationInput{Id: s("m1")}))
		}},
		{"PutBucketIntelligentTieringConfiguration", func() error {
			return e(c.PutBucketIntelligentTieringConfiguration(ctx, &awss3.PutBucketIntelligentTieringConfigurationInput{Id: s("t1"), IntelligentTieringConfiguration: &types.IntelligentTieringConfiguration{Id: s("t1"), Status: types.IntelligentTieringStatusEnabled, Tierings: []types.Tiering{{AccessTier: types.IntelligentTieringAccessTierArchiveAccess, Days: aws.Int32(90)}}}}))
		}},
		{"GetBucketIntelligentTieringConfiguration", func() error {
			return e(c.GetBucketIntelligentTieringConfiguration(ctx, &awss3.GetBucketIntelligentTieringConfigurationInput{Id: s("t1")}))
		}},
		{"ListBucketIntelligentTieringConfigurations", func() error { return e(c.ListBucketIntelligentTieringConfigurations(ctx, nil)) }},
		{"DeleteBucketIntelligentTieringConfiguration", func() error {
			return e(c.DeleteBucketIntelligentTieringConfiguration(ctx, &awss3.DeleteBucketIntelligentTieringConfigurationInput{Id: s("t1")}))
		}},

		// Bucket metadata (AWS only)
		{"CreateBucketMetadataConfiguration", func() error {
			return e(c.CreateBucketMetadataConfiguration(ctx, &awss3.CreateBucketMetadataConfigurationInput{MetadataConfiguration: &types.MetadataConfiguration{JournalTableConfiguration: &types.JournalTableConfiguration{RecordExpiration: &types.RecordExpiration{Expiration: types.ExpirationStateDisabled}}}}))
		}},
		{"GetBucketMetadataConfiguration", func() error { return e(c.GetBucketMetadataConfiguration(ctx, nil)) }},
		{"DeleteBucketMetadataConfiguration", func() error { return e(c.DeleteBucketMetadataConfiguration(ctx, nil)) }},
		{"CreateBucketMetadataTableConfiguration", func() error {
			return e(c.CreateBucketMetadataTableConfiguration(ctx, &awss3.CreateBucketMetadataTableConfigurationInput{MetadataTableConfiguration: &types.MetadataTableConfiguration{S3TablesDestination: &types.S3TablesDestination{TableBucketArn: s("arn:aws:s3tables:us-east-1:1:bucket/b"), TableName: s("t")}}}))
		}},
		{"GetBucketMetadataTableConfiguration", func() error { return e(c.GetBucketMetadataTableConfiguration(ctx, nil)) }},
		{"DeleteBucketMetadataTableConfiguration", func() error { return e(c.DeleteBucketMetadataTableConfiguration(ctx, nil)) }},
		{"UpdateBucketMetadataInventoryTableConfiguration", func() error {
			return e(c.UpdateBucketMetadataInventoryTableConfiguration(ctx, &awss3.UpdateBucketMetadataInventoryTableConfigurationInput{InventoryTableConfiguration: &types.InventoryTableConfigurationUpdates{ConfigurationState: types.InventoryConfigurationStateEnabled}}))
		}},
		{"UpdateBucketMetadataJournalTableConfiguration", func() error {
			return e(c.UpdateBucketMetadataJournalTableConfiguration(ctx, &awss3.UpdateBucketMetadataJournalTableConfigurationInput{JournalTableConfiguration: &types.JournalTableConfigurationUpdates{RecordExpiration: &types.RecordExpiration{Expiration: types.ExpirationStateDisabled}}}))
		}},
		{"UpdateBucketMetadataAnnotationTableConfiguration", func() error {
			return e(c.UpdateBucketMetadataAnnotationTableConfiguration(ctx, &awss3.UpdateBucketMetadataAnnotationTableConfigurationInput{AnnotationTableConfiguration: &types.AnnotationTableConfigurationUpdates{}}))
		}},
		{"CreateSession", func() error { return e(c.CreateSession(ctx, nil)) }},

		// Objects
		{"PutObject", func() error {
			return e(c.PutObject(ctx, &awss3.PutObjectInput{Key: s("o1"), Body: strings.NewReader("v1"), ContentType: s("text/plain"), Metadata: map[string]string{"m": "1"}}))
		}},
		{"GetObject", func() error {
			out, err := c.GetObject(ctx, &awss3.GetObjectInput{Key: key, Range: s("bytes=2-4")})
			if err != nil {
				return err
			}
			defer out.Body.Close()
			b, _ := io.ReadAll(out.Body)
			if string(b) != "234" {
				return fmt.Errorf("range read = %q", b)
			}
			return nil
		}},
		{"HeadObject", func() error { return e(c.HeadObject(ctx, &awss3.HeadObjectInput{Key: key})) }},
		{"GetObjectAttributes", func() error {
			out, err := c.GetObjectAttributes(ctx, &awss3.GetObjectAttributesInput{Key: key, ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectSize, types.ObjectAttributesEtag}})
			if err != nil {
				return err
			}
			return want(aws.ToInt64(out.ObjectSize) == 10, "size = %d", aws.ToInt64(out.ObjectSize))
		}},
		{"CopyObject", func() error {
			return e(c.CopyObject(ctx, &awss3.CopyObjectInput{Key: s("k2"), CopySource: s(c.Bucket + "/k")}))
		}},
		{"RenameObject", func() error {
			return e(c.RenameObject(ctx, &awss3.RenameObjectInput{Key: s("k3"), RenameSource: s(c.Bucket + "/k2")}))
		}},
		{"ListObjects", func() error { return e(c.ListObjects(ctx, nil)) }},
		{"ListObjectsV2", func() error {
			out, err := c.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Prefix: s("o"), Delimiter: s("/")})
			if err != nil {
				return err
			}
			return want(len(out.Contents) == 1 && aws.ToString(out.Contents[0].Key) == "o1", "listed %d objects", len(out.Contents))
		}},
		{"ListObjectVersions", func() error {
			out, err := c.ListObjectVersions(ctx, nil)
			if err != nil {
				return err
			}
			return want(len(out.Versions) >= 2, "%d versions listed", len(out.Versions))
		}},
		{"PutObjectTagging", func() error {
			return e(c.PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{Key: key, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: s("a"), Value: s("b")}}}}))
		}},
		{"GetObjectTagging", func() error {
			out, err := c.GetObjectTagging(ctx, &awss3.GetObjectTaggingInput{Key: key})
			if err != nil {
				return err
			}
			return want(len(out.TagSet) == 1 && aws.ToString(out.TagSet[0].Value) == "b", "object tags = %v", out.TagSet)
		}},
		{"DeleteObjectTagging", func() error { return e(c.DeleteObjectTagging(ctx, &awss3.DeleteObjectTaggingInput{Key: key})) }},
		{"PutObjectAcl", func() error {
			return e(c.PutObjectAcl(ctx, &awss3.PutObjectAclInput{Key: key, ACL: types.ObjectCannedACLPrivate}))
		}},
		{"GetObjectAcl", func() error { return e(c.GetObjectAcl(ctx, &awss3.GetObjectAclInput{Key: key})) }},
		{"GetObjectTorrent", func() error {
			out, err := c.GetObjectTorrent(ctx, &awss3.GetObjectTorrentInput{Key: scratch})
			if err == nil {
				out.Body.Close()
			}
			return err
		}},
		{"RestoreObject", func() error {
			return e(c.RestoreObject(ctx, &awss3.RestoreObjectInput{Key: key, RestoreRequest: &types.RestoreRequest{Days: aws.Int32(1)}}))
		}},
		{"SelectObjectContent", func() error {
			out, err := c.SelectObjectContent(ctx, &awss3.SelectObjectContentInput{Key: key, Expression: s("select * from s3object"), ExpressionType: types.ExpressionTypeSql,
				InputSerialization:  &types.InputSerialization{CSV: &types.CSVInput{}},
				OutputSerialization: &types.OutputSerialization{CSV: &types.CSVOutput{}}})
			if err == nil {
				out.GetStream().Close()
			}
			return err
		}},
		{"WriteGetObjectResponse", func() error {
			return e(c.WriteGetObjectResponse(ctx, &awss3.WriteGetObjectResponseInput{RequestRoute: s("r"), RequestToken: s("t"), Body: strings.NewReader("x")}))
		}},
		{"UpdateObjectEncryption", func() error {
			return e(c.UpdateObjectEncryption(ctx, &awss3.UpdateObjectEncryptionInput{Key: scratch, ObjectEncryption: &types.ObjectEncryptionMemberSSEKMS{Value: types.SSEKMSEncryption{KMSKeyArn: s("arn:aws:kms:us-east-1:1:key/k")}}}))
		}},
		{"PutObjectAnnotation", func() error {
			return e(c.PutObjectAnnotation(ctx, &awss3.PutObjectAnnotationInput{Key: scratch, AnnotationName: s("n"), AnnotationPayload: strings.NewReader("{}")}))
		}},
		{"GetObjectAnnotation", func() error {
			return e(c.GetObjectAnnotation(ctx, &awss3.GetObjectAnnotationInput{Key: scratch, AnnotationName: s("n")}))
		}},
		{"ListObjectAnnotations", func() error { return e(c.ListObjectAnnotations(ctx, &awss3.ListObjectAnnotationsInput{Key: scratch})) }},
		{"DeleteObjectAnnotation", func() error {
			return e(c.DeleteObjectAnnotation(ctx, &awss3.DeleteObjectAnnotationInput{Key: scratch, AnnotationName: s("n")}))
		}},

		// Object lock (on the lock-enabled bucket)
		{"PutObjectLockConfiguration", func() error {
			return e(lock.PutObjectLockConfiguration(ctx, &awss3.PutObjectLockConfigurationInput{ObjectLockConfiguration: &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}}))
		}},
		{"GetObjectLockConfiguration", func() error {
			out, err := lock.GetObjectLockConfiguration(ctx, nil)
			if err != nil {
				return err
			}
			return want(out.ObjectLockConfiguration.ObjectLockEnabled == types.ObjectLockEnabledEnabled, "lock config = %v", out.ObjectLockConfiguration)
		}},
		{"PutObjectLegalHold", func() error {
			return e(lock.PutObjectLegalHold(ctx, &awss3.PutObjectLegalHoldInput{Key: key, LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOff}}))
		}},
		{"GetObjectLegalHold", func() error { return e(lock.GetObjectLegalHold(ctx, &awss3.GetObjectLegalHoldInput{Key: key})) }},
		{"PutObjectRetention", func() error {
			return e(lock.PutObjectRetention(ctx, &awss3.PutObjectRetentionInput{Key: key, Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(time.Now().Add(time.Hour))}}))
		}},
		{"GetObjectRetention", func() error { return e(lock.GetObjectRetention(ctx, &awss3.GetObjectRetentionInput{Key: key})) }},

		// Deleting (last)
		{"DeleteObject", func() error { return e(c.DeleteObject(ctx, &awss3.DeleteObjectInput{Key: s("o1")})) }},
		{"DeleteObjects", func() error {
			return e(c.DeleteObjects(ctx, &awss3.DeleteObjectsInput{Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: s("k2")}}}}))
		}},
		{"DeleteBucket", func() error { return e(c.DeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: s("gcu-does-not-exist")})) }},
	}
	// CreateBucket is exercised by newBucket above (it fails the test if it breaks).
	_ = lock

	// Operations SeaweedFS 4.52 does not implement answer 501 or 405 (or a 404 for
	// a configuration that does not exist), or the SDK rejects them client-side.
	refused := map[string]bool{}
	for _, n := range strings.Fields(`CreateBucketMetadataConfiguration CreateBucketMetadataTableConfiguration CreateSession
		DeleteBucket DeleteBucketAnalyticsConfiguration DeleteBucketIntelligentTieringConfiguration
		DeleteBucketInventoryConfiguration DeleteBucketMetadataConfiguration DeleteBucketMetadataTableConfiguration
		DeleteBucketMetricsConfiguration DeleteBucketReplication DeleteBucketWebsite DeletePublicAccessBlock GetBucketAbac
		GetBucketAnalyticsConfiguration GetBucketIntelligentTieringConfiguration GetBucketInventoryConfiguration
		GetBucketMetadataConfiguration GetBucketMetadataTableConfiguration GetBucketMetricsConfiguration GetBucketReplication
		GetBucketWebsite GetPublicAccessBlock ListDirectoryBuckets PutBucketAbac PutBucketAccelerateConfiguration
		PutBucketAnalyticsConfiguration PutBucketIntelligentTieringConfiguration PutBucketInventoryConfiguration
		PutBucketLogging PutBucketMetricsConfiguration PutBucketNotificationConfiguration PutBucketReplication
		PutBucketWebsite PutPublicAccessBlock RenameObject RestoreObject SelectObjectContent
		UpdateBucketMetadataAnnotationTableConfiguration UpdateBucketMetadataInventoryTableConfiguration
		UpdateBucketMetadataJournalTableConfiguration WriteGetObjectResponse`) {
		refused[n] = true
	}
	// SeaweedFS does not know these object sub-resources and treats the request as
	// a plain object request, so a "success" proves nothing (DeleteObjectAnnotation
	// deleted the object it was pointed at). They run against a scratch key and
	// only have to return without panicking.
	unreliable := map[string]bool{}
	for _, n := range strings.Fields(`GetObjectTorrent UpdateObjectEncryption PutObjectAnnotation GetObjectAnnotation
		ListObjectAnnotations DeleteObjectAnnotation`) {
		unreliable[n] = true
	}

	// Every SDK operation must be in the table (CreateBucket runs in newBucket).
	listed := map[string]bool{"CreateBucket": true}
	for _, o := range ops {
		listed[o.name] = true
	}
	sdk := reflect.TypeOf(&awss3.Client{})
	for i := 0; i < sdk.NumMethod(); i++ {
		if name := sdk.Method(i).Name; name != "Options" && !listed[name] {
			t.Errorf("operation %s is not exercised by this test", name)
		}
	}

	for _, o := range ops {
		err := o.call()
		switch {
		case unreliable[o.name]:
			t.Logf("%s (result not asserted): %v", o.name, errString(err))
		case refused[o.name]:
			if err == nil {
				t.Errorf("%s: expected SeaweedFS to refuse it, but it succeeded; update the refused list", o.name)
			} else if strings.Contains(err.Error(), "panic") {
				t.Errorf("%s: %v", o.name, err)
			}
		default:
			if err != nil {
				t.Errorf("%s: %v", o.name, err)
			}
		}
	}

	// The scratch operations must not have touched the real object.
	var kb bytes.Buffer
	if _, err := c.Download(ctx, "k", &kb); err != nil || kb.String() != "0123456789" {
		t.Errorf("object k after all operations = %q, %v", kb.String(), err)
	}
}

func errString(err error) string {
	if err == nil {
		return "ok"
	}
	s := err.Error()
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}
