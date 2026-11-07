package s3_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Wetech-Code/go-common-utilities/s3"
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
