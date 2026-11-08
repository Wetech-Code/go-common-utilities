package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// TestEverySDKOperationIsWrapped fails when the SDK gains an operation that
// "go generate ./s3" has not wrapped yet.
func TestEverySDKOperationIsWrapped(t *testing.T) {
	sdk := reflect.TypeOf(&awss3.Client{})
	ours := reflect.TypeOf(&Client{})
	skipped := map[string]string{"Options": "returns the client's options, not an S3 operation"}
	var missing []string
	for i := 0; i < sdk.NumMethod(); i++ {
		name := sdk.Method(i).Name
		if skipped[name] != "" {
			continue
		}
		if _, ok := ours.MethodByName(name); !ok || !slices.Contains(generatedOperations, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("not wrapped; run go generate ./s3: %v", missing)
	}
	if len(generatedOperations) != sdk.NumMethod()-len(skipped) {
		t.Errorf("generated %d operations, SDK has %d", len(generatedOperations), sdk.NumMethod()-len(skipped))
	}
}

// recorder is a tiny S3 stand-in: it records each request and answers from a
// per-test handler.
type recorder struct {
	mu   sync.Mutex
	reqs []string // "METHOD /path?query"
	h    func(w http.ResponseWriter, r *http.Request)
}

func newWrapped(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{h: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.reqs = append(rec.reqs, r.Method+" "+r.URL.RequestURI())
		rec.mu.Unlock()
		rec.h(w, r)
	}))
	t.Cleanup(srv.Close)
	native := awss3.New(awss3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider("id", "secret", ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Retryer:                    aws.NopRetryer{},
	})
	return Wrap(native, "bkt"), rec
}

func (r *recorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return ""
	}
	return r.reqs[len(r.reqs)-1]
}

func (r *recorder) count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, q := range r.reqs {
		if strings.HasPrefix(q, prefix) {
			n++
		}
	}
	return n
}

func TestGeneratedMethodsDefaultTheBucketWithoutChangingTheInput(t *testing.T) {
	c, rec := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<ListBucketResult></ListBucketResult>`))
	})
	ctx := context.Background()
	in := &awss3.ListObjectsV2Input{}
	if _, err := c.ListObjectsV2(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); !strings.HasPrefix(got, "GET /bkt?") {
		t.Errorf("default bucket: request = %q", got)
	}
	if in.Bucket != nil {
		t.Error("the caller's input was modified")
	}
	// nil input is allowed, and an explicit bucket wins.
	if _, err := c.ListObjectsV2(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String("other")}); err != nil {
		t.Fatal(err)
	}
	if got := rec.last(); !strings.HasPrefix(got, "GET /other?") {
		t.Errorf("explicit bucket: request = %q", got)
	}
}

func TestErrorsNameTheOperationAndMapNotFound(t *testing.T) {
	c, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound)
		case strings.Contains(r.URL.Path, "missing"):
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>nope</Message></Error>`))
		default:
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`<Error><Code>AccessDenied</Code><Message>no</Message></Error>`))
		}
	})
	ctx := context.Background()

	_, err := c.GetObject(ctx, &awss3.GetObjectInput{Key: aws.String("missing")})
	var api smithy.APIError
	if !errors.Is(err, ErrNotFound) || !errors.As(err, &api) || api.ErrorCode() != "NoSuchKey" {
		t.Errorf("GetObject on a missing key = %v; want ErrNotFound wrapping the SDK error", err)
	}
	if _, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Key: aws.String("k")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("HeadObject 404 = %v; want ErrNotFound", err)
	}
	_, err = c.PutObject(ctx, &awss3.PutObjectInput{Key: aws.String("k"), Body: strings.NewReader("x")})
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "s3: PutObject:") {
		t.Errorf("PutObject denied = %v; want an error naming PutObject, not ErrNotFound", err)
	}
	// Bucket-level operations never map to ErrNotFound.
	if _, err := c.HeadBucket(ctx, nil); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("HeadBucket 404 = %v; want a plain error", err)
	}
}

func TestExistsSeparatesMissingFromFailures(t *testing.T) {
	status := http.StatusOK
	c, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
	ctx := context.Background()
	for _, tc := range []struct {
		status  int
		want    bool
		wantErr bool
	}{{200, true, false}, {404, false, false}, {403, false, true}} {
		status = tc.status
		got, err := c.Exists(ctx, "k")
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("status %d: Exists = %v, %v", tc.status, got, err)
		}
	}
	if _, err := c.Exists(ctx, "../k"); err == nil {
		t.Error("Exists must validate the key")
	}
}

func TestHeadReturnsMetadata(t *testing.T) {
	c, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "12")
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("X-Amz-Meta-Owner", "me")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
	})
	info, err := c.Head(context.Background(), "dir/k")
	if err != nil {
		t.Fatal(err)
	}
	if info.Key != "dir/k" || info.Size != 12 || info.ContentType != "text/plain" || info.ETag != `"abc"` ||
		info.Metadata["owner"] != "me" || info.LastModified.Year() != 2006 {
		t.Errorf("Head = %+v", info)
	}
}

func TestListWalksEveryPageAndStopsEarly(t *testing.T) {
	c, rec := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("continuation-token")
		switch tok {
		case "":
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>t1</NextContinuationToken>
<Contents><Key>a</Key><Size>1</Size></Contents><Contents><Key>b</Key><Size>2</Size></Contents></ListBucketResult>`)
		default:
			fmt.Fprint(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>c</Key><Size>3</Size></Contents></ListBucketResult>`)
		}
	})
	ctx := context.Background()
	all, err := c.List(ctx, "p/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range all {
		keys = append(keys, o.Key)
	}
	if !slices.Equal(keys, []string{"a", "b", "c"}) || all[2].Size != 3 {
		t.Errorf("List = %v", all)
	}
	if !strings.Contains(rec.last(), "prefix=p%2F") {
		t.Errorf("prefix not sent: %q", rec.last())
	}

	stop := errors.New("stop")
	seen := 0
	err = c.Walk(ctx, "", func(ObjectInfo) error {
		seen++
		if seen == 2 {
			return stop
		}
		return nil
	})
	if err != stop || seen != 2 {
		t.Errorf("Walk early stop: err=%v seen=%d", err, seen)
	}
	// An empty bucket lists as an empty, non-nil slice.
	empty, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `<ListBucketResult/>`) })
	if got, err := empty.List(ctx, ""); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty List = %#v, %v", got, err)
	}
}

func TestCopySendsAnEscapedSourceInTheSameBucket(t *testing.T) {
	var src string
	c, rec := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		src = r.Header.Get("X-Amz-Copy-Source")
		fmt.Fprint(w, `<CopyObjectResult><ETag>"x"</ETag></CopyObjectResult>`)
	})
	if err := c.Copy(context.Background(), "a b/ü.txt", "dst"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.last(), "PUT /bkt/dst") || !strings.HasPrefix(src, "bkt/a%20b/%C3%BC.txt") {
		t.Errorf("request %q, copy source %q", rec.last(), src)
	}
	if c.Copy(context.Background(), "", "dst") == nil || c.Copy(context.Background(), "a", "../b") == nil {
		t.Error("Copy must validate both keys")
	}
}

func TestDeleteManyBatchesAndReportsFailures(t *testing.T) {
	c, rec := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("<Key>bad</Key>")) {
			fmt.Fprint(w, `<DeleteResult><Error><Key>bad</Key><Code>AccessDenied</Code><Message>no</Message></Error></DeleteResult>`)
			return
		}
		fmt.Fprint(w, `<DeleteResult/>`)
	})
	ctx := context.Background()
	keys := make([]string, 2500)
	for i := range keys {
		keys[i] = "k" + strconv.Itoa(i)
	}
	if err := c.DeleteMany(ctx, keys); err != nil {
		t.Fatal(err)
	}
	if n := rec.count("POST /bkt?delete"); n != 3 {
		t.Errorf("2500 keys sent in %d requests, want 3", n)
	}
	err := c.DeleteMany(ctx, []string{"ok", "bad"})
	if err == nil || !strings.Contains(err.Error(), `"bad"`) || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("DeleteMany = %v", err)
	}
	if err := c.DeleteMany(ctx, nil); err != nil {
		t.Errorf("DeleteMany(nil) = %v", err)
	}
	if err := c.DeleteMany(ctx, []string{"ok", "/abs"}); err == nil {
		t.Error("DeleteMany must validate keys before sending anything")
	}
}

func TestDownloadStreamsAndMapsNotFound(t *testing.T) {
	c, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gone") {
			w.WriteHeader(404)
			fmt.Fprint(w, `<Error><Code>NoSuchKey</Code></Error>`)
			return
		}
		w.Write([]byte("payload"))
	})
	var buf bytes.Buffer
	n, err := c.Download(context.Background(), "k", &buf)
	if err != nil || n != 7 || buf.String() != "payload" {
		t.Errorf("Download = %d, %v, %q", n, err, buf.String())
	}
	if _, err := c.Download(context.Background(), "gone", &buf); !errors.Is(err, ErrNotFound) {
		t.Errorf("Download missing = %v", err)
	}
	if _, err := c.Download(context.Background(), "k", failWriter{}); err == nil {
		t.Error("a failing writer must fail Download")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestTagsRoundTrip(t *testing.T) {
	var put string
	c, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			b, _ := io.ReadAll(r.Body)
			put = string(b)
			return
		}
		fmt.Fprint(w, `<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag></TagSet></Tagging>`)
	})
	ctx := context.Background()
	tags, err := c.GetTags(ctx, "k")
	if err != nil || !reflect.DeepEqual(tags, map[string]string{"a": "1"}) {
		t.Errorf("GetTags = %v, %v", tags, err)
	}
	if err := c.SetTags(ctx, "k", map[string]string{"x": "y"}); err != nil || !strings.Contains(put, "<Key>x</Key><Value>y</Value>") {
		t.Errorf("SetTags = %v, body %q", err, put)
	}
	if _, err := c.GetTags(ctx, ""); err == nil {
		t.Error("GetTags must validate the key")
	}
	if c.SetTags(ctx, "../k", nil) == nil {
		t.Error("SetTags must validate the key")
	}
}

// multipartServer implements just enough of multipart upload to count parts.
type multipartServer struct {
	mu        sync.Mutex
	parts     map[int][]byte
	completed int
	aborted   int
	puts      int
	failPart  int // 1-based part number to reject; 0 for none
}

func (m *multipartServer) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		if n == m.failPart {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `<Error><Code>InternalError</Code></Error>`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		m.parts[n] = b
		w.Header().Set("ETag", fmt.Sprintf(`"p%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		m.completed++
		fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"done"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		m.aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		m.puts++
	}
}

func TestUploadChoosesSinglePutOrMultipart(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		name      string
		size      int
		partSize  int64
		wantParts []int // sizes
		wantPuts  int
	}{
		{"empty", 0, 0, nil, 1},
		{"smaller than a part", 3 * mib, 0, nil, 1},
		{"exactly one part", 8 * mib, 0, nil, 1}, // ReadFull fills the buffer, then EOF: one part, no tail
		{"part boundary", 10 * mib, 5 * mib, []int{5 * mib, 5 * mib}, 0},
		{"with a short tail", 12 * mib, 5 * mib, []int{5 * mib, 5 * mib, 2 * mib}, 0},
		{"tiny part size is raised", 11 * mib, 1, []int{5 * mib, 5 * mib, 1 * mib}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &multipartServer{parts: map[int][]byte{}}
			c, _ := newWrapped(t, m.handle)
			data := bytes.Repeat([]byte("x"), tc.size)
			if err := c.Upload(context.Background(), "big", bytes.NewReader(data), "application/octet-stream", tc.partSize); err != nil {
				t.Fatal(err)
			}
			if tc.wantParts == nil && tc.name == "exactly one part" {
				// 8 MiB fills one default part exactly: a multipart upload with a single part.
				if len(m.parts) != 1 || m.completed != 1 {
					t.Errorf("parts=%d completed=%d", len(m.parts), m.completed)
				}
				return
			}
			if m.puts != tc.wantPuts {
				t.Errorf("single puts = %d, want %d", m.puts, tc.wantPuts)
			}
			if len(m.parts) != len(tc.wantParts) {
				t.Fatalf("parts = %d, want %d", len(m.parts), len(tc.wantParts))
			}
			total := 0
			for i, want := range tc.wantParts {
				if got := len(m.parts[i+1]); got != want {
					t.Errorf("part %d = %d bytes, want %d", i+1, got, want)
				}
				total += len(m.parts[i+1])
			}
			if len(tc.wantParts) > 0 && (m.completed != 1 || total != tc.size) {
				t.Errorf("completed=%d total=%d", m.completed, total)
			}
		})
	}
}

func TestUploadAbortsOnFailure(t *testing.T) {
	m := &multipartServer{parts: map[int][]byte{}, failPart: 2}
	c, _ := newWrapped(t, m.handle)
	data := bytes.Repeat([]byte("x"), 12<<20)
	err := c.Upload(context.Background(), "big", bytes.NewReader(data), "", 5<<20)
	if err == nil || !strings.Contains(err.Error(), "UploadPart") {
		t.Fatalf("Upload = %v; want the part failure", err)
	}
	if m.aborted != 1 || m.completed != 0 {
		t.Errorf("aborted=%d completed=%d", m.aborted, m.completed)
	}

	// A read error mid-stream also aborts.
	m2 := &multipartServer{parts: map[int][]byte{}}
	c2, _ := newWrapped(t, m2.handle)
	r := io.MultiReader(bytes.NewReader(bytes.Repeat([]byte("x"), 6<<20)), errReader{})
	if err := c2.Upload(context.Background(), "big", r, "", 5<<20); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Upload with a failing reader = %v", err)
	}
	if m2.aborted != 1 {
		t.Errorf("reader failure: aborted=%d", m2.aborted)
	}
	// A failing reader before the first part is full never starts an upload.
	if err := c2.Upload(context.Background(), "k", errReader{}, "", 0); err == nil {
		t.Error("expected the read error")
	}
	if err := c2.Upload(context.Background(), "/bad", strings.NewReader("x"), "", 0); err == nil {
		t.Error("Upload must validate the key")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func TestUploadRefusesMoreThanTheMaximumNumberOfParts(t *testing.T) {
	old := maxParts
	maxParts = 2
	t.Cleanup(func() { maxParts = old })
	m := &multipartServer{parts: map[int][]byte{}}
	c, _ := newWrapped(t, m.handle)
	err := c.Upload(context.Background(), "big", bytes.NewReader(make([]byte, 16<<20)), "", 5<<20)
	if err == nil || !strings.Contains(err.Error(), "more than 2 parts") {
		t.Fatalf("Upload = %v", err)
	}
	if m.aborted != 1 {
		t.Errorf("aborted=%d", m.aborted)
	}
}

func TestPresignVariantsValidateAndSign(t *testing.T) {
	c, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	type call struct {
		name string
		fn   func() (string, error)
		want string
	}
	for _, tc := range []call{
		{"head", func() (string, error) { return c.PresignHead(ctx, "k", time.Minute) }, "/bkt/k?"},
		{"delete", func() (string, error) { return c.PresignDelete(ctx, "k", time.Minute) }, "/bkt/k?"},
		{"upload part", func() (string, error) { return c.PresignUploadPart(ctx, "k", "u1", 3, time.Minute) }, "partNumber=3"},
		{"head bucket", func() (string, error) { return c.PresignHeadBucket(ctx, time.Minute) }, "/bkt?"},
		{"delete bucket", func() (string, error) { return c.PresignDeleteBucket(ctx, time.Minute) }, "/bkt?"},
	} {
		u, err := tc.fn()
		if err != nil || !strings.Contains(u, tc.want) || !strings.Contains(u, "X-Amz-Signature=") || !strings.Contains(u, "X-Amz-Expires=60") {
			t.Errorf("%s: %q, %v", tc.name, u, err)
		}
	}
	post, err := c.PresignPost(ctx, "up/k", time.Minute)
	if err != nil || post.URL == "" || post.Fields["key"] != "up/k" || post.Fields["X-Amz-Signature"] == "" {
		t.Errorf("PresignPost = %+v, %v", post, err)
	}

	bad := []struct {
		name string
		err  error
	}{
		{"head key", func() error { _, e := c.PresignHead(ctx, "", time.Minute); return e }()},
		{"head expiry", func() error { _, e := c.PresignHead(ctx, "k", 0); return e }()},
		{"delete key", func() error { _, e := c.PresignDelete(ctx, "../k", time.Minute); return e }()},
		{"delete expiry", func() error { _, e := c.PresignDelete(ctx, "k", MaxPresignExpiry+1); return e }()},
		{"part key", func() error { _, e := c.PresignUploadPart(ctx, "", "u", 1, time.Minute); return e }()},
		{"part upload id", func() error { _, e := c.PresignUploadPart(ctx, "k", "", 1, time.Minute); return e }()},
		{"part number", func() error { _, e := c.PresignUploadPart(ctx, "k", "u", 0, time.Minute); return e }()},
		{"part expiry", func() error { _, e := c.PresignUploadPart(ctx, "k", "u", 1, -1); return e }()},
		{"post key", func() error { _, e := c.PresignPost(ctx, "", time.Minute); return e }()},
		{"post expiry", func() error { _, e := c.PresignPost(ctx, "k", 0); return e }()},
		{"head bucket expiry", func() error { _, e := c.PresignHeadBucket(ctx, 0); return e }()},
		{"delete bucket expiry", func() error { _, e := c.PresignDeleteBucket(ctx, 0); return e }()},
	}
	for _, b := range bad {
		if b.err == nil {
			t.Errorf("%s: want an error", b.name)
		}
	}
}

func TestWrapDoesNotContactTheServiceAndCloseIsSafe(t *testing.T) {
	c, rec := newWrapped(t, func(w http.ResponseWriter, r *http.Request) {})
	if rec.last() != "" {
		t.Error("Wrap made a request")
	}
	if c.S3 == nil || c.Bucket != "bkt" {
		t.Errorf("Wrap = %+v", c)
	}
	c.Close()
	c.Close()
}

func TestPingNamesTheBucketOnFailure(t *testing.T) {
	status := http.StatusOK
	c, _ := newWrapped(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = http.StatusForbidden
	if err := c.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), `"bkt"`) {
		t.Errorf("Ping = %v", err)
	}
}
