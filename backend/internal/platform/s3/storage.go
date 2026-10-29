// Package s3 adapts media.Storage to an S3-compatible object store through
// minio-go.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// ErrUnavailable reports that the store could not be configured or reached.
// The cause is not wrapped because client errors can echo the endpoint or
// credentials.
var ErrUnavailable = errors.New("s3: storage unavailable")

// conditionalRequestConflict is the S3 error for a conditional write that
// raced another write to the same key.
const conditionalRequestConflict = "ConditionalRequestConflict"

// Owner metadata headers. Every identifier is printable ASCII without spaces,
// so it travels unchanged in an S3 user metadata header.
const (
	tenantHeader = "X-Amz-Meta-Pepicanvas-Tenant"
	canvasHeader = "X-Amz-Meta-Pepicanvas-Canvas"
	assetHeader  = "X-Amz-Meta-Pepicanvas-Asset"
)

// maxListPage is the most keys one S3 list request returns.
const maxListPage = 1000

var (
	errBodyTooShort = errors.New("s3: body is shorter than its declared size")
	errBodyTooLong  = errors.New("s3: body is longer than its declared size")
)

// Config locates one bucket. Endpoint is host[:port], without scheme,
// credentials, or path.
type Config struct {
	Endpoint  string
	Bucket    string
	AccessKey secret.Value
	SecretKey secret.Value
	Secure    bool
}

// Storage implements media.Storage over one bucket. Every call runs in an s3
// span of the caller's trace that records only its operation and outcome.
type Storage struct {
	client *minio.Client
	bucket string
}

var _ media.Storage = (*Storage)(nil)

// Open connects to the store and verifies the bucket exists.
func Open(ctx context.Context, config Config) (*Storage, error) {
	if config.Endpoint == "" || strings.ContainsAny(config.Endpoint, "@/?#") {
		return nil, fmt.Errorf("%w: invalid endpoint", ErrUnavailable)
	}
	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKey.Reveal(), config.SecretKey.Reveal(), ""),
		Secure: config.Secure,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: client rejected configuration", ErrUnavailable)
	}
	exists, err := client.BucketExists(ctx, config.Bucket)
	if err != nil {
		return nil, fmt.Errorf("%w: bucket check failed", ErrUnavailable)
	}
	if !exists {
		return nil, fmt.Errorf("%w: bucket not found", ErrUnavailable)
	}
	return &Storage{client: client, bucket: config.Bucket}, nil
}

// Put stores exactly size bytes in a single PUT, so a failed, cancelled, short,
// or long upload never leaves a readable object or orphan multipart parts. The
// owner is stored as user metadata; an invalid owner is media.ErrInvalid. The
// PUT is conditional: an existing object is never replaced and Put returns
// media.ErrObjectExists. Put returns when ctx is done even if body is blocked;
// the copy goroutine then ends when body returns from Read.
func (s *Storage) Put(ctx context.Context, key string, owner media.Owner, body io.Reader, size int64) (err error) {
	ctx, span := telemetry.Start(ctx, telemetry.SpanS3, telemetry.Attr("operation", "put"))
	defer func() { endSpan(span, err) }()
	if size < 0 {
		return fmt.Errorf("s3: negative size")
	}
	if !owner.Valid() {
		return fmt.Errorf("%w: object owner is malformed", media.ErrInvalid)
	}
	reader, writer := io.Pipe()
	verdict := make(chan error, 1)
	go func() {
		err := copyExact(writer, body, size)
		verdict <- err
		writer.CloseWithError(err)
	}()
	stop := context.AfterFunc(ctx, func() { writer.CloseWithError(ctx.Err()) })
	defer stop()
	if size == 0 {
		// The transport sends no body for Content-Length 0, so nothing
		// holds the request back: wait for the body to prove it is empty.
		select {
		case err := <-verdict:
			if err != nil {
				return fmt.Errorf("s3: put object: %w", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	options := minio.PutObjectOptions{
		UserMetadata: map[string]string{
			tenantHeader: owner.Key.TenantID,
			canvasHeader: owner.Key.CanvasID,
			assetHeader:  owner.AssetID,
		},
		DisableMultipart: true,
		// Stream the body instead of buffering it to sign its hash.
		DisableContentSha256: true,
	}
	// If-None-Match: * makes the store refuse to replace an existing object.
	options.SetMatchETagExcept("*")
	_, err = s.client.PutObject(ctx, s.bucket, key, reader, size, options)
	reader.Close()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if code := minio.ToErrorResponse(err).Code; code == minio.PreconditionFailed || code == conditionalRequestConflict {
		return media.ErrObjectExists
	}
	if err != nil {
		return fmt.Errorf("s3: put object: %w", err)
	}
	return nil
}

// copyExact writes exactly size bytes of body to w and fails a shorter or
// longer body. It holds back the last byte until body proves it has no more:
// the transport may stop reading at Content-Length, and the store keeps no
// object until every declared byte arrives.
func copyExact(w io.Writer, body io.Reader, size int64) error {
	var last int64
	if size > 0 {
		last = 1
		if _, err := io.CopyN(w, body, size-1); err != nil {
			if errors.Is(err, io.EOF) {
				return errBodyTooShort
			}
			return err
		}
	}
	var tail [2]byte
	n, err := io.ReadFull(body, tail[:last+1])
	switch {
	case int64(n) > last:
		return errBodyTooLong
	case err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
		return err
	case int64(n) < last:
		return errBodyTooShort
	}
	if last == 0 {
		// An empty pipe write still waits for a reader.
		return nil
	}
	_, err = w.Write(tail[:last])
	return err
}

// Open returns the stored bytes, or media.ErrNotFound for a missing key.
func (s *Storage) Open(ctx context.Context, key string) (_ io.ReadCloser, err error) {
	ctx, span := telemetry.Start(ctx, telemetry.SpanS3, telemetry.Attr("operation", "open"))
	defer func() { endSpan(span, err) }()
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return nil, media.ErrNotFound
		}
		return nil, fmt.Errorf("s3: stat object: %w", err)
	}
	object, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("s3: get object: %w", err)
	}
	return object, nil
}

// Stat describes an object and its owner, or returns media.ErrNotFound for a
// missing key. An object without valid owner metadata is unowned.
func (s *Storage) Stat(ctx context.Context, key string) (_ media.Object, err error) {
	ctx, span := telemetry.Start(ctx, telemetry.SpanS3, telemetry.Attr("operation", "stat"))
	defer func() { endSpan(span, err) }()
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
			return media.Object{}, media.ErrNotFound
		}
		return media.Object{}, fmt.Errorf("s3: stat object: %w", err)
	}
	object := media.Object{Key: key, ModifiedAt: info.LastModified}
	owner := media.Owner{
		Key:     media.CanvasKey{TenantID: info.Metadata.Get(tenantHeader), CanvasID: info.Metadata.Get(canvasHeader)},
		AssetID: info.Metadata.Get(assetHeader),
	}
	if owner.Valid() {
		object.Owner, object.Owned = owner, true
	}
	return object, nil
}

// List returns up to limit objects whose keys sort after after, in key order.
func (s *Storage) List(ctx context.Context, after string, limit int) (_ []media.Object, err error) {
	ctx, span := telemetry.Start(ctx, telemetry.SpanS3, telemetry.Attr("operation", "list"))
	defer func() { endSpan(span, err) }()
	if limit <= 0 {
		return nil, fmt.Errorf("%w: list limit must be positive", media.ErrInvalid)
	}
	listCtx, cancel := context.WithCancel(ctx)
	objects := make([]media.Object, 0, min(limit, maxListPage))
	options := minio.ListObjectsOptions{Recursive: true, StartAfter: after, MaxKeys: min(limit, maxListPage)}
	listing := s.client.ListObjects(listCtx, s.bucket, options)
	// minio's listing goroutine blocks on its last send once cancelled, so
	// drain the channel until it closes.
	defer func() {
		cancel()
		for range listing {
		}
	}()
	for info := range listing {
		if info.Err != nil {
			return nil, fmt.Errorf("s3: list objects: %w", info.Err)
		}
		objects = append(objects, media.Object{Key: info.Key, ModifiedAt: info.LastModified})
		if len(objects) == limit {
			break
		}
	}
	// A cancelled listing may end without an error item.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return objects, nil
}

// Delete removes an object. Deleting a missing key succeeds.
func (s *Storage) Delete(ctx context.Context, key string) (err error) {
	ctx, span := telemetry.Start(ctx, telemetry.SpanS3, telemetry.Attr("operation", "delete"))
	defer func() { endSpan(span, err) }()
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("s3: remove object: %w", err)
	}
	return nil
}

// endSpan ends the span of one storage call with its closed outcome: ok,
// not_found, exists, or error. Keys, the bucket, and error text are never
// exported.
func endSpan(span *telemetry.Span, err error) {
	switch {
	case err == nil:
		span.SetAttr("outcome", "ok")
	case errors.Is(err, media.ErrNotFound):
		span.SetAttr("outcome", "not_found")
	case errors.Is(err, media.ErrObjectExists):
		span.SetAttr("outcome", "exists")
	default:
		span.SetAttr("outcome", "error")
	}
	span.End(err)
}
