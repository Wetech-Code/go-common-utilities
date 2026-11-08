package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Wrap returns a Client that uses an SDK client you built yourself, bound to
// bucket. It does not contact the service and does not check that the bucket
// exists. The caller keeps ownership of native; Close on the returned Client
// has nothing to release.
func Wrap(native *awss3.Client, bucket string) *Client {
	return &Client{S3: native, Bucket: bucket, presign: awss3.NewPresignClient(native)}
}

// Part size limits of S3 multipart upload.
const (
	MinPartSize     = 5 << 20
	DefaultPartSize = 8 << 20
	deleteBatch     = 1000
)

// maxParts is the S3 limit on parts per upload; a variable only so a test can lower it.
var maxParts = 10000

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
	// Metadata holds the user metadata (x-amz-meta-*), keys in lower case.
	Metadata map[string]string
}

// Head returns the object's metadata without its content. It returns
// ErrNotFound if the key does not exist.
func (c *Client) Head(ctx context.Context, key string) (*ObjectInfo, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	out, err := c.HeadObject(ctx, &awss3.HeadObjectInput{Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	return &ObjectInfo{
		Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag),
		ContentType: aws.ToString(out.ContentType), LastModified: aws.ToTime(out.LastModified),
		Metadata: out.Metadata,
	}, nil
}

// Exists reports whether key exists. Only "not found" gives false with a nil
// error; other failures (permissions, network) are returned.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.Head(ctx, key)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	}
	return false, err
}

// Walk calls fn for every object whose key starts with prefix, in key order,
// fetching pages as needed. Stop early by returning an error from fn; Walk
// returns it unchanged.
func (c *Client) Walk(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	in := &awss3.ListObjectsV2Input{Bucket: aws.String(c.Bucket)}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}
	for {
		out, err := c.ListObjectsV2(ctx, in)
		if err != nil {
			return err
		}
		for _, o := range out.Contents {
			info := ObjectInfo{
				Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size),
				ETag: aws.ToString(o.ETag), LastModified: aws.ToTime(o.LastModified),
			}
			if err := fn(info); err != nil {
				return err
			}
		}
		if !aws.ToBool(out.IsTruncated) || out.NextContinuationToken == nil {
			return nil
		}
		in.ContinuationToken = out.NextContinuationToken
	}
}

// List returns every object whose key starts with prefix. Use Walk for buckets
// too large to hold the listing in memory. ContentType and Metadata are not
// filled in by listing; use Head for those.
func (c *Client) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	all := []ObjectInfo{}
	err := c.Walk(ctx, prefix, func(o ObjectInfo) error { all = append(all, o); return nil })
	if err != nil {
		return nil, err
	}
	return all, nil
}

// Copy copies srcKey to dstKey within the bucket, replacing dstKey. It returns
// ErrNotFound if srcKey does not exist. Objects over 5 GiB cannot be copied
// with a single request.
func (c *Client) Copy(ctx context.Context, srcKey, dstKey string) error {
	if err := checkKey(srcKey); err != nil {
		return err
	}
	if err := checkKey(dstKey); err != nil {
		return err
	}
	// CopySource is "bucket/key" and must be URL-escaped by the caller.
	segs := strings.Split(srcKey, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	_, err := c.CopyObject(ctx, &awss3.CopyObjectInput{
		Key:        aws.String(dstKey),
		CopySource: aws.String(url.PathEscape(c.Bucket) + "/" + strings.Join(segs, "/")),
	})
	return err
}

// DeleteMany removes keys in batches of 1000. Keys that do not exist are not an
// error. Every batch is attempted; the returned error joins all failures.
func (c *Client) DeleteMany(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if err := checkKey(k); err != nil {
			return err
		}
	}
	var errs []error
	for start := 0; start < len(keys); start += deleteBatch {
		end := min(start+deleteBatch, len(keys))
		ids := make([]types.ObjectIdentifier, 0, end-start)
		for _, k := range keys[start:end] {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(k)})
		}
		out, err := c.DeleteObjects(ctx, &awss3.DeleteObjectsInput{
			Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, e := range out.Errors {
			errs = append(errs, fmt.Errorf("s3: delete %q: %s: %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message)))
		}
	}
	return errors.Join(errs...)
}

// Download copies the object's content to w and returns the number of bytes
// written. It returns ErrNotFound if the key does not exist.
func (c *Client) Download(ctx context.Context, key string, w io.Writer) (int64, error) {
	rc, err := c.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(w, rc)
	if err != nil {
		return n, fmt.Errorf("s3: download %q: %w", key, err)
	}
	return n, nil
}

// Upload streams r to key using multipart upload, so r needs neither to be
// seekable nor to fit in memory, and objects may exceed 5 GiB. partSize is the
// size of each part; zero means DefaultPartSize, and values below MinPartSize
// are raised to it. The object is limited to 10000 parts. If anything fails the
// partial upload is aborted. Data smaller than one part is sent with a single
// PutObject.
func (c *Client) Upload(ctx context.Context, key string, r io.Reader, contentType string, partSize int64) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if partSize == 0 {
		partSize = DefaultPartSize
	}
	partSize = max(partSize, MinPartSize)

	buf := make([]byte, partSize)
	n, err := io.ReadFull(r, buf)
	switch {
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return c.Put(ctx, key, bytes.NewReader(buf[:n]), contentType)
	case err != nil:
		return fmt.Errorf("s3: upload %q: read: %w", key, err)
	}

	create := &awss3.CreateMultipartUploadInput{Key: aws.String(key)}
	if contentType != "" {
		create.ContentType = aws.String(contentType)
	}
	mp, err := c.CreateMultipartUpload(ctx, create)
	if err != nil {
		return err
	}
	var parts []types.CompletedPart
	abort := func(cause error) error {
		// The caller's context may be what failed, so abort with a fresh one.
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, aerr := c.AbortMultipartUpload(actx, &awss3.AbortMultipartUploadInput{Key: aws.String(key), UploadId: mp.UploadId})
		return errors.Join(cause, aerr)
	}
	send := func(data []byte) error {
		num := int32(len(parts) + 1)
		if int(num) > maxParts {
			return fmt.Errorf("s3: upload %q: more than %d parts; use a larger partSize", key, maxParts)
		}
		up, err := c.UploadPart(ctx, &awss3.UploadPartInput{
			Key: aws.String(key), UploadId: mp.UploadId, PartNumber: aws.Int32(num),
			Body: bytes.NewReader(data),
		})
		if err != nil {
			return err
		}
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(num)})
		return nil
	}
	for {
		if err := send(buf[:n]); err != nil {
			return abort(err)
		}
		n, err = io.ReadFull(r, buf)
		if err == io.EOF { // the data ended exactly on a part boundary
			break
		}
		if err == io.ErrUnexpectedEOF { // a final, shorter part
			if err := send(buf[:n]); err != nil {
				return abort(err)
			}
			break
		}
		if err != nil {
			return abort(fmt.Errorf("s3: upload %q: read: %w", key, err))
		}
	}
	_, err = c.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Key: aws.String(key), UploadId: mp.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return abort(err)
	}
	return nil
}

// GetTags returns the object's tags (an empty map if it has none). It returns
// ErrNotFound if the key does not exist.
func (c *Client) GetTags(ctx context.Context, key string) (map[string]string, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	out, err := c.GetObjectTagging(ctx, &awss3.GetObjectTaggingInput{Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	tags := make(map[string]string, len(out.TagSet))
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

// SetTags replaces all tags on the object. An empty map removes them.
func (c *Client) SetTags(ctx context.Context, key string, tags map[string]string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	set := make([]types.Tag, 0, len(tags))
	for k, v := range tags {
		set = append(set, types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	_, err := c.PutObjectTagging(ctx, &awss3.PutObjectTaggingInput{
		Key: aws.String(key), Tagging: &types.Tagging{TagSet: set},
	})
	return err
}

// PresignHead returns a URL that lets anyone holding it read key's metadata
// with an HTTP HEAD until expires has elapsed.
func (c *Client) PresignHead(ctx context.Context, key string, expires time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	if err := checkExpiry(expires); err != nil {
		return "", err
	}
	req, err := c.presign.PresignHeadObject(ctx,
		&awss3.HeadObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)},
		func(o *awss3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return "", fmt.Errorf("s3: presign head %q: %w", key, err)
	}
	return req.URL, nil
}

// PresignDelete returns a URL that lets anyone holding it delete key with an
// HTTP DELETE until expires has elapsed.
//
// Dangerous: whoever holds the URL can delete the object.
func (c *Client) PresignDelete(ctx context.Context, key string, expires time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	if err := checkExpiry(expires); err != nil {
		return "", err
	}
	req, err := c.presign.PresignDeleteObject(ctx,
		&awss3.DeleteObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)},
		func(o *awss3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return "", fmt.Errorf("s3: presign delete %q: %w", key, err)
	}
	return req.URL, nil
}

// PresignUploadPart returns a URL for uploading one part of a multipart upload
// started with CreateMultipartUpload, so a browser can upload large files in
// parts directly.
func (c *Client) PresignUploadPart(ctx context.Context, key, uploadID string, partNumber int32, expires time.Duration) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	if uploadID == "" || partNumber < 1 {
		return "", errors.New("s3: uploadID and a part number of at least 1 are required")
	}
	if err := checkExpiry(expires); err != nil {
		return "", err
	}
	req, err := c.presign.PresignUploadPart(ctx,
		&awss3.UploadPartInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), UploadId: aws.String(uploadID), PartNumber: aws.Int32(partNumber)},
		func(o *awss3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return "", fmt.Errorf("s3: presign upload part %q: %w", key, err)
	}
	return req.URL, nil
}

// PresignedPost is a browser-form upload: POST Fields plus the file to URL.
type PresignedPost struct {
	URL    string
	Fields map[string]string
}

// PresignPost returns what an HTML form needs to upload key with a POST until
// expires has elapsed. Send the Fields as form fields, followed by the file as a
// field named "file". Verified against SeaweedFS 4.52; a service without POST
// policy support will reject the form.
func (c *Client) PresignPost(ctx context.Context, key string, expires time.Duration) (*PresignedPost, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if err := checkExpiry(expires); err != nil {
		return nil, err
	}
	req, err := c.presign.PresignPostObject(ctx,
		&awss3.PutObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)},
		func(o *awss3.PresignPostOptions) { o.Expires = expires })
	if err != nil {
		return nil, fmt.Errorf("s3: presign post %q: %w", key, err)
	}
	return &PresignedPost{URL: req.URL, Fields: req.Values}, nil
}

// PresignHeadBucket returns a URL that checks the bucket with an HTTP HEAD.
func (c *Client) PresignHeadBucket(ctx context.Context, expires time.Duration) (string, error) {
	if err := checkExpiry(expires); err != nil {
		return "", err
	}
	req, err := c.presign.PresignHeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(c.Bucket)},
		func(o *awss3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return "", fmt.Errorf("s3: presign head bucket: %w", err)
	}
	return req.URL, nil
}

// PresignDeleteBucket returns a URL that deletes the (empty) bucket with an HTTP
// DELETE.
//
// Dangerous: whoever holds the URL can delete the bucket.
func (c *Client) PresignDeleteBucket(ctx context.Context, expires time.Duration) (string, error) {
	if err := checkExpiry(expires); err != nil {
		return "", err
	}
	req, err := c.presign.PresignDeleteBucket(ctx, &awss3.DeleteBucketInput{Bucket: aws.String(c.Bucket)},
		func(o *awss3.PresignOptions) { o.Expires = expires })
	if err != nil {
		return "", fmt.Errorf("s3: presign delete bucket: %w", err)
	}
	return req.URL, nil
}
