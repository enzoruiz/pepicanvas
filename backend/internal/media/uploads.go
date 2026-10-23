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

// ErrUploadInProgress reports a retry of an upload whose earlier attempt
// stored its bytes and has not finished. The client may retry later.
var ErrUploadInProgress = errors.New("media: upload in progress")

// ErrObjectExists reports a Put to a key that already holds an object.
var ErrObjectExists = errors.New("media: stored object already exists")

var (
	errBodyShort = errors.New("body is shorter than the declared size")
	errBodyLong  = errors.New("body is longer than the declared size")
)

// Storage holds asset bytes under internal keys. Put records owner with the
// object and rejects an invalid owner with ErrInvalid. Put never replaces an
// existing object: it returns ErrObjectExists. Open of a missing key returns
// ErrNotFound; Delete of a missing key succeeds.
type Storage interface {
	Put(ctx context.Context, key string, owner Owner, body io.Reader, size int64) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Owner names the asset whose bytes an object holds. Storage records it with
// the object, so cleanup can prove who owned an object that has no row.
type Owner struct {
	Key     CanvasKey
	AssetID string
}

// Valid reports whether every identifier of the owner is well formed.
func (owner Owner) Valid() bool {
	return ValidateKey(owner.Key) == nil && validateIdentifier("asset", owner.AssetID) == nil
}

// Object describes one stored object. Owner is set, and Owned true, only when
// the object carries valid owner metadata.
type Object struct {
	Key        string
	ModifiedAt time.Time
	Owner      Owner
	Owned      bool
}

// ObjectStore is a Storage that can also list and describe its objects, for
// reconciliation against the committed assets.
type ObjectStore interface {
	Storage
	// Stat describes the object stored under key, with its owner, or returns
	// ErrNotFound.
	Stat(ctx context.Context, key string) (Object, error)
	// List returns up to limit objects whose keys sort after after, in key
	// order. Listed objects carry no owner; Stat reads it. A non-positive
	// limit is ErrInvalid.
	List(ctx context.Context, after string, limit int) ([]Object, error)
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
// failures wrap ErrUploadFailed, and domain errors pass through. A retry of an
// abandoned upload, while its operation is retained, repeats the recorded
// response without reserving or writing: a rejection returns a *Rejection with
// the same code, and a failure or cancellation an error wrapping
// ErrUploadFailed.
func (u *Uploads) Upload(ctx context.Context, req UploadRequest) (UploadResult, error) {
	replaces := ""
	if req.Replace {
		replaces = req.ReferenceID
	}
	reserve := Reserve{DeclaredBytes: req.DeclaredBytes, ReplacesReference: replaces}
	if err := validateUpload(req, reserve); err != nil {
		return UploadResult{}, err
	}
	switch abandon, err := u.store.Abandoned(ctx, req.Key, stage(req.Operation, abandonSuffix).ID); {
	case err == nil:
		return UploadResult{}, abandonedError(abandon)
	case !errors.Is(err, ErrNotFound):
		return UploadResult{}, requestError(ctx, err)
	}
	reserved, err := u.store.Apply(ctx, req.Key, stage(req.Operation, reserveSuffix), reserve)
	if err != nil {
		return UploadResult{}, requestError(ctx, err)
	}
	// A retry after success finds the asset ready: replay the completion
	// without touching the stored bytes.
	if asset, err := u.store.Ready(ctx, req.Key, reserved.AssetID); err == nil {
		return u.replay(ctx, req, asset)
	} else if !errors.Is(err, ErrNotFound) {
		return UploadResult{}, u.abort(ctx, req, reserved, err)
	}

	body := &exactReader{r: req.Body, remaining: req.DeclaredBytes}
	owner := Owner{Key: req.Key, AssetID: reserved.AssetID}
	if err := u.storage.Put(ctx, reserved.StorageKey, owner, body, req.DeclaredBytes); err != nil {
		// Another attempt of this upload stored its bytes first, and storage
		// never replaces them. This attempt never aborts that one: it receives
		// its result once completed, its recorded abandon, or ErrUploadInProgress.
		if errors.Is(err, ErrObjectExists) {
			return u.collided(ctx, req, reserved)
		}
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

// collided answers an attempt whose bytes another attempt of the same upload
// stored first, without touching the reservation or the stored bytes.
func (u *Uploads) collided(ctx context.Context, req UploadRequest, reserved Result) (UploadResult, error) {
	asset, err := u.store.Ready(ctx, req.Key, reserved.AssetID)
	if err == nil {
		return u.replay(ctx, req, asset)
	}
	if !errors.Is(err, ErrNotFound) {
		return UploadResult{}, requestError(ctx, err)
	}
	switch abandon, err := u.store.Abandoned(ctx, req.Key, stage(req.Operation, abandonSuffix).ID); {
	case err == nil:
		return UploadResult{}, abandonedError(abandon)
	case !errors.Is(err, ErrNotFound):
		return UploadResult{}, requestError(ctx, err)
	}
	return UploadResult{}, ErrUploadInProgress
}

// replay repeats the completion of an asset that is already ready.
func (u *Uploads) replay(ctx context.Context, req UploadRequest, asset Asset) (UploadResult, error) {
	completed, err := u.store.Apply(ctx, req.Key, stage(req.Operation, completeSuffix),
		Complete{AssetID: asset.ID, ReferenceID: req.ReferenceID, Kind: asset.Kind})
	if err != nil {
		return UploadResult{}, requestError(ctx, err)
	}
	return UploadResult{AssetID: asset.ID, Kind: asset.Kind, Usage: completed.Usage}, nil
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
// the error the caller sees: the recorded response when another attempt of
// this upload abandoned it first, else the cause.
func (u *Uploads) abort(ctx context.Context, req UploadRequest, reserved Result, cause error) error {
	abandon := Abandon{AssetID: reserved.AssetID, Reason: AbandonFailed}
	if rejection, rejected := cause.(*Rejection); rejected {
		abandon.Reason, abandon.Rejection = AbandonRejected, rejection.Code()
	} else if ctx.Err() != nil {
		abandon.Reason = AbandonCancelled
		cause = ctx.Err()
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	operation := stage(req.Operation, abandonSuffix)
	_, err := u.store.Apply(cleanup, req.Key, operation, abandon)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrOperationConflict):
		// The asset already left pending. Its bytes are deleted only when it
		// did not become ready, e.g. through a completion whose answer was lost.
		if _, err := u.store.Ready(cleanup, req.Key, reserved.AssetID); !errors.Is(err, ErrNotFound) {
			return cause
		}
		u.discard(cleanup, req.Key, reserved)
		if recorded, err := u.store.Abandoned(cleanup, req.Key, operation.ID); err == nil {
			return abandonedError(recorded)
		}
		return cause
	default:
		// The asset stays pending until its reservation expires and Sweep
		// deletes its bytes.
		return cause
	}
	u.discard(cleanup, req.Key, reserved)
	return cause
}

// discard deletes the stored bytes of an asset that left pending and purges
// it once they are gone.
func (u *Uploads) discard(ctx context.Context, key CanvasKey, reserved Result) {
	if err := u.storage.Delete(ctx, reserved.StorageKey); err != nil {
		return
	}
	_ = u.store.Purge(ctx, key, reserved.AssetID)
}

// abandonedError is the response of an abandoned upload: the rejection with
// its code, or an upload failure for a failed or cancelled one.
func abandonedError(abandon Abandon) error {
	if abandon.Reason == AbandonRejected {
		return newRejection(abandon.Rejection)
	}
	return fmt.Errorf("%w: the upload was abandoned", ErrUploadFailed)
}

func validateUpload(req UploadRequest, reserve Reserve) error {
	if len(req.Operation.ID) > MaxIdentifierBytes-len(completeSuffix) {
		return fmt.Errorf("%w: upload operation identifier must have at most %d bytes", ErrInvalid, MaxIdentifierBytes-len(completeSuffix))
	}
	// The stage suffix would make an empty identifier valid.
	if err := validateIdentifier("operation", req.Operation.ID); err != nil {
		return err
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
// ends early or continues past them. A failure is final: a later Read or
// finish reports it again, even though the byte that revealed it was consumed.
type exactReader struct {
	r         io.Reader
	remaining int64
	err       error
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
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
			e.err = errBodyShort
			return n, e.err
		}
		err = nil
	}
	return n, err
}

// finish reports whether the body ends exactly at the declared size.
func (e *exactReader) finish() error {
	if e.err != nil {
		return e.err
	}
	if e.remaining > 0 {
		e.err = errBodyShort
		return e.err
	}
	var probe [1]byte
	for {
		n, err := e.r.Read(probe[:])
		if n > 0 {
			e.err = errBodyLong
			return e.err
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			e.err = err
			return e.err
		}
	}
}
