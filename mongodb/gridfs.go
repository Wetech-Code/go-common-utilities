package mongodb

import (
	"context"
	"io"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Bucket stores files too large for a document in GridFS, as chunks in the
// "<name>.files" and "<name>.chunks" collections.
type Bucket struct {
	// Native is the underlying driver bucket, for streaming reads and writes
	// (OpenUploadStream, OpenDownloadStream) and for the files and chunks collections.
	Native *mongo.GridFSBucket
}

// Bucket returns the database's GridFS bucket; the default name is "fs". It does not
// talk to the server.
func (d *Database) Bucket(opts ...options.Lister[options.BucketOptions]) *Bucket {
	return &Bucket{Native: d.Native.GridFSBucket(opts...)}
}

// Upload stores everything read from r as a file and returns its id.
func (b *Bucket) Upload(ctx context.Context, filename string, r io.Reader, opts ...options.Lister[options.GridFSUploadOptions]) (any, error) {
	id, err := b.Native.UploadFromStream(ctx, filename, r, opts...)
	if err != nil {
		return nil, err
	}
	return id, nil
}

// UploadWithID is Upload with an id you choose.
func (b *Bucket) UploadWithID(ctx context.Context, id any, filename string, r io.Reader, opts ...options.Lister[options.GridFSUploadOptions]) error {
	return b.Native.UploadFromStreamWithID(ctx, id, filename, r, opts...)
}

// Download writes the file with the given id to w and returns the bytes written. It
// returns ErrNotFound if there is no such file.
func (b *Bucket) Download(ctx context.Context, id any, w io.Writer) (int64, error) {
	n, err := b.Native.DownloadToStream(ctx, id, w)
	return n, notFound(err)
}

// DownloadByName writes the newest revision of the named file to w (choose another
// revision with options.GridFSName().SetRevision) and returns the bytes written. It
// returns ErrNotFound if there is no such file.
func (b *Bucket) DownloadByName(ctx context.Context, filename string, w io.Writer, opts ...options.Lister[options.GridFSNameOptions]) (int64, error) {
	n, err := b.Native.DownloadToStreamByName(ctx, filename, w, opts...)
	return n, notFound(err)
}

// Delete removes the file and its chunks. It returns ErrNotFound if there is no such file.
func (b *Bucket) Delete(ctx context.Context, id any) error {
	return notFound(b.Native.Delete(ctx, id))
}

// Rename changes a file's name. It returns ErrNotFound if there is no such file.
func (b *Bucket) Rename(ctx context.Context, id any, newFilename string) error {
	return notFound(b.Native.Rename(ctx, id, newFilename))
}

// Find decodes the file documents matching filter into dest, a pointer to a slice
// such as *[]bson.M. No match gives an empty, non-nil slice.
func (b *Bucket) Find(ctx context.Context, filter any, dest any, opts ...options.Lister[options.GridFSFindOptions]) error {
	cur, err := b.Native.Find(ctx, filterOrEmpty(filter), opts...)
	if err != nil {
		return err
	}
	return decodeAll(ctx, cur, dest)
}

// Drop removes the whole bucket: every file and chunk.
//
// Dangerous: this removes all files in the bucket.
func (b *Bucket) Drop(ctx context.Context) error { return b.Native.Drop(ctx) }
