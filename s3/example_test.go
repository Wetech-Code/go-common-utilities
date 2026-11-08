package s3_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Wetech-Code/go-common-utilities/s3"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Connect to a SeaweedFS S3 gateway, upload, download, share and delete.
func Example() {
	ctx := context.Background()
	c, err := s3.New(ctx, s3.Config{
		Endpoint:        os.Getenv("S3_ENDPOINT"), // e.g. https://seaweed.internal:8333
		AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		Bucket:          "uploads",
		UsePathStyle:    true, // SeaweedFS
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer c.Close() // the caller owns the client

	if err := c.Put(ctx, "notes/hello.txt", strings.NewReader("hello"), "text/plain"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}

	rc, err := c.Get(ctx, "notes/hello.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	fmt.Println(string(body))

	// A link anyone can use for the next 15 minutes.
	link, _ := c.PresignGet(ctx, "notes/hello.txt", 15*time.Minute)
	fmt.Println(link)

	_ = c.Delete(ctx, "notes/hello.txt")
}

// Stream a large file, list a prefix, and call any S3 operation with the SDK's input.
func ExampleClient_Upload() {
	ctx := context.Background()
	c := s3.Wrap(awss3.New(awss3.Options{ /* region, endpoint, credentials */ }), "uploads")

	f, err := os.Open("backup.tar")
	if err != nil {
		return
	}
	defer f.Close()
	_ = c.Upload(ctx, "backups/backup.tar", f, "application/x-tar", 0) // multipart, aborted on failure

	objs, _ := c.List(ctx, "backups/")
	for _, o := range objs {
		fmt.Println(o.Key, o.Size)
	}

	// Any other operation: the bucket is filled in when left empty.
	_, _ = c.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	})
}
