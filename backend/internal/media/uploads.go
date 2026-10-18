package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// CleanupTimeout bounds the cleanup of a failed upload. Cleanup runs detached
// from the request, so a cancelled request still releases its reservation.
const CleanupTimeout = 30 * time.Second

// Stage suffixes turn one client operation ID into the operation IDs of the
// upload stages, so a retry of the whole upload replays the committed stages.
const (
	reserveSuffix  = "/reserve"
	completeSuffix = "/complete"
	abandonSuffix  = "/abandon"
)

// ErrUploadFailed reports a storage or inspection failure. The reservation is
// released and the stored bytes are deleted or isolated for Sweep.
var ErrUploadFailed = errors.New("media: upload failed")

var (
	errBodyShort = errors.New("body is shorter than the declared size")
	errBodyLong  = errors.New("body is longer than the declared size")
)

// Storage holds asset bytes under internal keys. Open of a missing key returns
// ErrNotFound; Delete of a missing key succeeds.
type Storage interface {
	Put(ctx context.Context, key string, body io.Reader, size int64) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// UploadRequest is one client upload. Operation.ID identifies the whole
// upload; it may have at most MaxIdentifierBytes minus the longest stage
// suffix bytes. With Replace, ReferenceID is moved to the new asset on success.
type UploadRequest struct {
	Key           CanvasKey
	Operation     Operation
	DeclaredBytes int64
	ReferenceID   string
	Replace       bool
	Body          io.Reader
}

// UploadResult is the outcome of a successful upload.
type UploadResult struct {
	AssetID string
	Kind    Kind
	Usage   Usage
}

// Uploads runs the upload pipeline: reserve, store, inspect, complete. Any
// failure after the reservation releases it and deletes the stored bytes.
type Uploads struct {
	store     Store
	storage   Storage
	inspector Inspector
}

// NewUploads returns a pipeline over a quota store, an object storage, and an
// inspector.
func NewUploads(store Store, storage Storage, inspector Inspector) *Uploads {
	return &Uploads{store: store, storage: storage, inspector: inspector}
}

// Upload stores and validates one asset. A *Rejection is returned unchanged,
// a cancelled request returns the context error, storage and inspection
// failures wrap ErrUploadFailed, and domain errors pass through.
func (u *Uploads) Upload(ctx context.Context, req UploadRequest) (UploadResult, error) {
	replaces := ""
	if req.Replace {
		replaces = req.ReferenceID
	}
	reserve := Reserve{DeclaredBytes: req.DeclaredBytes, ReplacesReference: replaces}
	if err := validateUpload(req, reserve); err != nil {
		return UploadResult{}, err
	}
	reserved, err := u.store.Apply(ctx, req.Key, stage(req.Operation, reserveSuffix), reserve)
	if err != nil {
		return UploadResult{}, requestError(ctx, err)
	}
	// A retry after success finds the asset ready: replay the completion
	// without touching the stored bytes.
	if asset, err := u.store.Ready(ctx, req.Key, reserved.AssetID); err == nil {
		completed, err := u.store.Apply(ctx, req.Key, stage(req.Operation, completeSuffix),
			Complete{AssetID: asset.ID, ReferenceID: req.ReferenceID, Kind: asset.Kind})
		if err != nil {
			return UploadResult{}, requestError(ctx, err)
		}
		return UploadResult{AssetID: asset.ID, Kind: asset.Kind, Usage: completed.Usage}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return UploadResult{}, u.abort(ctx, req, reserved, err)
	}

	body := &exactReader{r: req.Body, remaining: req.DeclaredBytes}
	if err := u.storage.Put(ctx, reserved.StorageKey, body, req.DeclaredBytes); err != nil {
		return UploadResult{}, u.abort(ctx, req, reserved, fmt.Errorf("%w: store bytes: %w", ErrUploadFailed, err))
	}
	// Storage may stop reading at the declared size; the body must end there.
	if err := body.finish(); err != nil {
		return UploadResult{}, u.abort(ctx, req, reserved, fmt.Errorf("%w: store bytes: %w", ErrUploadFailed, err))
	}
	inspection, err := u.inspect(ctx, reserved.StorageKey)
	if err != nil {
		return UploadResult{}, u.abort(ctx, req, reserved, err)
	}
	if inspection.InputBytes != req.DeclaredBytes {
		return UploadResult{}, u.abort(ctx, req, reserved, newRejection(RejectionInvalidMetadata))
	}
	completed, err := u.store.Apply(ctx, req.Key, stage(req.Operation, completeSuffix),
		Complete{AssetID: reserved.AssetID, ReferenceID: req.ReferenceID, Kind: inspection.Kind})
	if err != nil {
		return UploadResult{}, u.abort(ctx, req, reserved, err)
	}
	// A replayed completion of an asset released since then must not leave
	// the bytes just written without an asset that owns them.
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if _, err := u.store.Ready(cleanup, req.Key, reserved.AssetID); errors.Is(err, ErrNotFound) {
		_ = u.storage.Delete(cleanup, reserved.StorageKey)
	}
	return UploadResult{AssetID: reserved.AssetID, Kind: inspection.Kind, Usage: completed.Usage}, nil
}

// Open returns a ready asset and its bytes. Any other state is ErrNotFound.
func (u *Uploads) Open(ctx context.Context, key CanvasKey, assetID string) (Asset, io.ReadCloser, error) {
	asset, err := u.store.Ready(ctx, key, assetID)
	if err != nil {
		return Asset{}, nil, err
	}
	body, err := u.storage.Open(ctx, asset.StorageKey)
	if err != nil {
		return Asset{}, nil, err
	}
	return asset, body, nil
}

// Sweep releases expired reservations and deletes the bytes of up to limit
// quarantined or deleting assets, purging each one whose bytes are gone. It
// continues past a failed item and returns the first error.
func (u *Uploads) Sweep(ctx context.Context, limit int) (int, error) {
	garbage, err := u.store.Collect(ctx, limit)
	if err != nil {
		return 0, err
	}
	purged := 0
	var first error
	for _, item := range garbage {
		if err := ctx.Err(); err != nil {
			return purged, errors.Join(first, err)
		}
		err := u.storage.Delete(ctx, item.StorageKey)
		if err == nil {
			err = u.store.Purge(ctx, item.Key, item.AssetID)
		}
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		purged++
	}
	return purged, first
}

// inspect reads the stored bytes back and inspects them.
func (u *Uploads) inspect(ctx context.Context, storageKey string) (Inspection, error) {
	stored, err := u.storage.Open(ctx, storageKey)
	if err != nil {
		return Inspection{}, fmt.Errorf("%w: open stored bytes: %w", ErrUploadFailed, err)
	}
	defer stored.Close()
	inspection, err := u.inspector.Inspect(ctx, stored)
	if err != nil {
		if rejection, ok := errors.AsType[*Rejection](err); ok {
			return Inspection{}, rejection
		}
		return Inspection{}, fmt.Errorf("%w: inspect: %w", ErrUploadFailed, err)
	}
	return inspection, nil
}

// abort releases the reservation of a failed upload, deletes its stored
// bytes, and purges the asset, on a context detached from the request. A
// failed delete leaves the asset quarantined or deleting for Sweep. It returns
// the error the caller sees.
func (u *Uploads) abort(ctx context.Context, req UploadRequest, reserved Result, cause error) error {
	_, rejected := cause.(*Rejection)
	reason := AbandonFailed
	switch {
	case rejected:
		reason = AbandonRejected
	case ctx.Err() != nil:
		reason = AbandonCancelled
		cause = ctx.Err()
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	_, err := u.store.Apply(cleanup, req.Key, stage(req.Operation, abandonSuffix), Abandon{AssetID: reserved.AssetID, Reason: reason})
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrOperationConflict):
		// The asset already left pending. Its bytes are deleted only when it
		// did not become ready, e.g. through a completion whose answer was lost.
		if _, err := u.store.Ready(cleanup, req.Key, reserved.AssetID); !errors.Is(err, ErrNotFound) {
			return cause
		}
	default:
		// The asset stays pending until its reservation expires and Sweep
		// deletes its bytes.
		return cause
	}
	if err := u.storage.Delete(cleanup, reserved.StorageKey); err != nil {
		return cause
	}
	_ = u.store.Purge(cleanup, req.Key, reserved.AssetID)
	return cause
}

func validateUpload(req UploadRequest, reserve Reserve) error {
	if len(req.Operation.ID) > MaxIdentifierBytes-len(completeSuffix) {
		return fmt.Errorf("%w: upload operation identifier must have at most %d bytes", ErrInvalid, MaxIdentifierBytes-len(completeSuffix))
	}
	if err := ValidateCommand(req.Key, stage(req.Operation, reserveSuffix), reserve); err != nil {
		return err
	}
	if err := validateIdentifier("reference", req.ReferenceID); err != nil {
		return err
	}
	if req.Body == nil {
		return fmt.Errorf("%w: upload body is required", ErrInvalid)
	}
	return nil
}

func stage(operation Operation, suffix string) Operation {
	return Operation{ID: operation.ID + suffix, CorrelationID: operation.CorrelationID}
}

// requestError returns the context error of a cancelled request.
func requestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
}

// exactReader passes through exactly remaining bytes and fails when the body
// ends early or continues past them.
type exactReader struct {
	r         io.Reader
	remaining int64
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.remaining == 0 {
		if err := e.finish(); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > e.remaining {
		p = p[:e.remaining]
	}
	n, err := e.r.Read(p)
	e.remaining -= int64(n)
	if errors.Is(err, io.EOF) {
		if e.remaining > 0 {
			return n, errBodyShort
		}
		err = nil
	}
	return n, err
}

// finish reports whether the body ends exactly at the declared size.
func (e *exactReader) finish() error {
	if e.remaining > 0 {
		return errBodyShort
	}
	var probe [1]byte
	for {
		n, err := e.r.Read(probe[:])
		if n > 0 {
			return errBodyLong
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
