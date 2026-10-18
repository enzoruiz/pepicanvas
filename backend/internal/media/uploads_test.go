package media_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

const correlation = "0123456789abcdef0123456789abcdef"

var (
	errStorage = errors.New("storage unavailable")
	errDeleteA = errors.New("delete failed: first")
	errDeleteB = errors.New("delete failed: second")
)

// fakeStorage is an in-memory object store. A fault or block can be set per
// method; a block waits until the call's context is done.
type fakeStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
	faults  map[string]func(key string) error
	blocks  map[string]bool
	started chan struct{}
}

func newFakeStorage() *fakeStorage {
	return &fakeStorage{
		objects: map[string][]byte{},
		faults:  map[string]func(string) error{},
		blocks:  map[string]bool{},
		started: make(chan struct{}, 16),
	}
}

func (s *fakeStorage) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	s.mu.Lock()
	s.puts = append(s.puts, key)
	s.mu.Unlock()
	// Like an S3 client given the object size, read exactly size bytes.
	data := make([]byte, size)
	if _, err := io.ReadFull(body, data); err != nil {
		return err
	}
	s.mu.Lock()
	s.objects[key] = data
	s.mu.Unlock()
	// A fault or cancellation after the write leaves an ambiguous object behind.
	return s.interrupt(ctx, "put", key)
}

func (s *fakeStorage) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := s.interrupt(ctx, "open", key); err != nil {
		return nil, err
	}
	s.mu.Lock()
	data, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		return nil, media.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), nil
}

func (s *fakeStorage) Delete(ctx context.Context, key string) error {
	if err := s.interrupt(ctx, "delete", key); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

func (s *fakeStorage) interrupt(ctx context.Context, method, key string) error {
	s.mu.Lock()
	fault, block := s.faults[method], s.blocks[method]
	s.mu.Unlock()
	if block {
		s.started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	if fault != nil {
		if err := fault(key); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *fakeStorage) fail(method string, fault func(key string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[method] = fault
}

func (s *fakeStorage) block(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocks[method] = true
}

func (s *fakeStorage) object(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	return data, ok
}

func (s *fakeStorage) objectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}

func (s *fakeStorage) putKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...)
}

func failWith(err error) func(string) error {
	return func(string) error { return err }
}

// fakeInspector reads the whole stream and accepts it as an image unless a
// behavior is set.
type fakeInspector struct {
	mu      sync.Mutex
	inspect func(ctx context.Context, data []byte) (media.Inspection, error)
	started chan struct{}
}

func (f *fakeInspector) Inspect(ctx context.Context, r io.Reader) (media.Inspection, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return media.Inspection{}, err
	}
	f.mu.Lock()
	inspect := f.inspect
	f.mu.Unlock()
	if inspect == nil {
		return imageOf(data), nil
	}
	return inspect(ctx, data)
}

func (f *fakeInspector) set(inspect func(ctx context.Context, data []byte) (media.Inspection, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inspect = inspect
}

func (f *fakeInspector) reject(rejection *media.Rejection) {
	f.set(func(context.Context, []byte) (media.Inspection, error) { return media.Inspection{}, rejection })
}

// expire makes the inspection run out of its own deadline while the request
// is still alive.
func (f *fakeInspector) expire() {
	f.set(func(ctx context.Context, _ []byte) (media.Inspection, error) {
		inner, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		<-inner.Done()
		return media.Inspection{}, inner.Err()
	})
}

func (f *fakeInspector) block() {
	f.set(func(ctx context.Context, _ []byte) (media.Inspection, error) {
		f.started <- struct{}{}
		<-ctx.Done()
		return media.Inspection{}, ctx.Err()
	})
}

func imageOf(data []byte) media.Inspection {
	return media.Inspection{Kind: media.KindImage, InputBytes: int64(len(data)), StreamCount: 1, Dimensions: media.Dimensions{Width: 16, Height: 16}}
}

// dimensionsRejection obtains a real rejection from the acceptance contract.
func dimensionsRejection(t *testing.T) *media.Rejection {
	t.Helper()
	err := media.Validate(media.Inspection{
		Kind: media.KindImage, InputBytes: 5, StreamCount: 1, Dimensions: media.Dimensions{Width: 5000, Height: 5000},
	}, media.DefaultLimits())
	var rejection *media.Rejection
	if !errors.As(err, &rejection) || rejection.Code() != media.RejectionDimensionsExceeded {
		t.Fatalf("Validate = %v, want a dimensions rejection", err)
	}
	return rejection
}

type server struct {
	store    *postgres.MediaStore
	canvases *postgres.CanvasStore
}

func startServer(t *testing.T) *server {
	t.Helper()
	ctx := context.Background()
	pg := pgtest.Start(t)
	pool, err := postgres.Open(ctx, pg.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return &server{store: postgres.NewMediaStore(pool), canvases: postgres.NewCanvasStore(pool)}
}

var canvasSeq, operationSeq atomic.Int64

type fixture struct {
	key       media.CanvasKey
	store     *postgres.MediaStore
	storage   *fakeStorage
	inspector *fakeInspector
	uploads   *media.Uploads
}

// fixture creates a fresh canvas with its own fake storage and inspector.
func (s *server) fixture(t *testing.T) *fixture {
	t.Helper()
	key := media.CanvasKey{TenantID: "tenant-a", CanvasID: fmt.Sprintf("canvas-%d", canvasSeq.Add(1))}
	if _, err := s.canvases.Create(context.Background(), canvas.Key{TenantID: key.TenantID, CanvasID: key.CanvasID}); err != nil {
		t.Fatalf("Create canvas: %v", err)
	}
	storage, inspector := newFakeStorage(), &fakeInspector{started: make(chan struct{}, 16)}
	return &fixture{key: key, store: s.store, storage: storage, inspector: inspector, uploads: media.NewUploads(s.store, storage, inspector)}
}

func (f *fixture) request(content, reference string) media.UploadRequest {
	return media.UploadRequest{
		Key:           f.key,
		Operation:     media.Operation{ID: fmt.Sprintf("upload-%d", operationSeq.Add(1)), CorrelationID: correlation},
		DeclaredBytes: int64(len(content)),
		ReferenceID:   reference,
		Body:          strings.NewReader(content),
	}
}

func (f *fixture) mustUpload(t *testing.T, req media.UploadRequest) media.UploadResult {
	t.Helper()
	result, err := f.uploads.Upload(context.Background(), req)
	if err != nil {
		t.Fatalf("Upload(%s): %v", req.Operation.ID, err)
	}
	return result
}

// reservation replays the committed Reserve stage of req, which returns the
// asset ID and storage key the upload received.
func (f *fixture) reservation(t *testing.T, req media.UploadRequest) media.Result {
	t.Helper()
	replaces := ""
	if req.Replace {
		replaces = req.ReferenceID
	}
	operation := media.Operation{ID: req.Operation.ID + "/reserve", CorrelationID: req.Operation.CorrelationID}
	result, err := f.store.Apply(context.Background(), f.key, operation, media.Reserve{DeclaredBytes: req.DeclaredBytes, ReplacesReference: replaces})
	if err != nil {
		t.Fatalf("replay Reserve of %s: %v", req.Operation.ID, err)
	}
	return result
}

func (f *fixture) wantUsage(t *testing.T, used, reserved int64) {
	t.Helper()
	got, err := f.store.Usage(context.Background(), f.key)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if want := (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: used, ReservedBytes: reserved}); got != want {
		t.Fatalf("Usage = %+v, want %+v", got, want)
	}
}

// garbage reports whether the asset is listed for deletion (quarantined or
// deleting).
func (f *fixture) garbage(t *testing.T, assetID string) bool {
	t.Helper()
	listed, err := f.store.Collect(context.Background(), 1000)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, item := range listed {
		if item.Key == f.key && item.AssetID == assetID {
			return true
		}
	}
	return false
}

func (f *fixture) wantNotServed(t *testing.T, assetID string) {
	t.Helper()
	if _, err := f.store.Ready(context.Background(), f.key, assetID); !errors.Is(err, media.ErrNotFound) {
		t.Fatalf("Ready(%s) = %v, want ErrNotFound", assetID, err)
	}
	asset, body, err := f.uploads.Open(context.Background(), f.key, assetID)
	if !errors.Is(err, media.ErrNotFound) || body != nil || asset != (media.Asset{}) {
		t.Fatalf("Open(%s) = %+v, %v, %v; want ErrNotFound", assetID, asset, body, err)
	}
}

// wantGone checks S8: the asset is not served, not listed, and its bytes are
// gone from storage.
func (f *fixture) wantGone(t *testing.T, reserved media.Result) {
	t.Helper()
	f.wantNotServed(t, reserved.AssetID)
	if f.garbage(t, reserved.AssetID) {
		t.Fatalf("asset %s is still listed for deletion, want purged", reserved.AssetID)
	}
	if _, ok := f.storage.object(reserved.StorageKey); ok {
		t.Fatalf("storage still holds %s", reserved.StorageKey)
	}
}

// wantIsolated checks that the asset is not served but waits for deletion.
func (f *fixture) wantIsolated(t *testing.T, reserved media.Result) {
	t.Helper()
	f.wantNotServed(t, reserved.AssetID)
	if !f.garbage(t, reserved.AssetID) {
		t.Fatalf("asset %s is not listed for deletion", reserved.AssetID)
	}
}

func (f *fixture) wantServed(t *testing.T, assetID, content string) media.Asset {
	t.Helper()
	asset, body, err := f.uploads.Open(context.Background(), f.key, assetID)
	if err != nil {
		t.Fatalf("Open(%s): %v", assetID, err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %s: %v", assetID, err)
	}
	if string(data) != content {
		t.Fatalf("Open(%s) bytes = %q, want %q", assetID, data, content)
	}
	return asset
}

// cancelWhenStarted cancels the returned context once started receives.
func cancelWhenStarted(t *testing.T, started <-chan struct{}) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		select {
		case <-started:
			cancel()
		case <-time.After(30 * time.Second):
		}
	}()
	return ctx
}

func wantRejection(t *testing.T, err error, code media.RejectionCode) *media.Rejection {
	t.Helper()
	var rejection *media.Rejection
	if !errors.As(err, &rejection) || rejection.Code() != code {
		t.Fatalf("Upload error = %v, want rejection %s", err, code)
	}
	if errors.Is(err, media.ErrUploadFailed) {
		t.Fatalf("Upload error = %v, a rejection must not be an upload failure", err)
	}
	return rejection
}

func wantCancelled(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, context.Canceled) || errors.Is(err, media.ErrUploadFailed) {
		t.Fatalf("Upload error = %v, want context.Canceled only", err)
	}
}

func wantFailed(t *testing.T, err error, cause error) {
	t.Helper()
	if !errors.Is(err, media.ErrUploadFailed) || !errors.Is(err, cause) {
		t.Fatalf("Upload error = %v, want ErrUploadFailed caused by %v", err, cause)
	}
}

func TestUploadPipeline(t *testing.T) {
	srv := startServer(t)
	ctx := context.Background()

	// S3: the declared size is reserved while the bytes are inspected and
	// becomes used quota only after a successful completion.
	t.Run("success reserves while inspecting and consumes on completion", func(t *testing.T) {
		f := srv.fixture(t)
		req := f.request("hello", "ref-1")
		var during media.Usage
		var duringErr error
		f.inspector.set(func(ctx context.Context, data []byte) (media.Inspection, error) {
			during, duringErr = f.store.Usage(ctx, f.key)
			return imageOf(data), nil
		})
		result := f.mustUpload(t, req)
		if duringErr != nil || during != (media.Usage{QuotaBytes: media.QuotaBytes, ReservedBytes: 5}) {
			t.Fatalf("usage during inspection = %+v, %v; want 5 reserved", during, duringErr)
		}
		reserved := f.reservation(t, req)
		want := media.UploadResult{AssetID: reserved.AssetID, Kind: media.KindImage, Usage: media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 5}}
		if result != want {
			t.Fatalf("Upload = %+v, want %+v", result, want)
		}
		f.wantUsage(t, 5, 0)
		asset := f.wantServed(t, result.AssetID, "hello")
		wantAsset := media.Asset{ID: reserved.AssetID, State: media.StateReady, Kind: media.KindImage, DeclaredBytes: 5, StorageKey: reserved.StorageKey, References: 1}
		if asset != wantAsset {
			t.Fatalf("Open asset = %+v, want %+v", asset, wantAsset)
		}
		if keys := f.storage.putKeys(); len(keys) != 1 || keys[0] != reserved.StorageKey || f.storage.objectCount() != 1 {
			t.Fatalf("storage puts = %v with %d objects, want one put under %s", keys, f.storage.objectCount(), reserved.StorageKey)
		}
	})

	// S7 reserve stage: a reservation over the quota stores nothing.
	t.Run("reserve over quota stores nothing", func(t *testing.T) {
		f := srv.fixture(t)
		if _, err := f.store.Apply(ctx, f.key, media.Operation{ID: "filler"}, media.Reserve{DeclaredBytes: media.QuotaBytes - 3}); err != nil {
			t.Fatalf("filler Reserve: %v", err)
		}
		_, err := f.uploads.Upload(ctx, f.request("hello", "ref-1"))
		if !errors.Is(err, media.ErrQuotaExceeded) {
			t.Fatalf("Upload = %v, want ErrQuotaExceeded", err)
		}
		f.wantUsage(t, 0, media.QuotaBytes-3)
		if keys := f.storage.putKeys(); len(keys) != 0 || f.storage.objectCount() != 0 {
			t.Fatalf("storage puts = %v, want none", keys)
		}
	})

	t.Run("request cancelled before reserve stores nothing", func(t *testing.T) {
		f := srv.fixture(t)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := f.uploads.Upload(cancelled, f.request("hello", "ref-1"))
		wantCancelled(t, err)
		f.wantUsage(t, 0, 0)
		if keys := f.storage.putKeys(); len(keys) != 0 {
			t.Fatalf("storage puts = %v, want none", keys)
		}
	})

	t.Run("invalid requests reserve nothing", func(t *testing.T) {
		f := srv.fixture(t)
		longest := f.request("hello", "ref-ok")
		longest.Operation.ID = strings.Repeat("o", media.MaxIdentifierBytes-len("/complete"))
		tooLong := f.request("hello", "ref-1")
		tooLong.Operation.ID = strings.Repeat("o", media.MaxIdentifierBytes-len("/complete")+1)
		noBody := f.request("hello", "ref-2")
		noBody.Body = nil
		badReference := f.request("hello", "bad reference")
		zero := f.request("", "ref-3")
		for name, req := range map[string]media.UploadRequest{"operation too long": tooLong, "nil body": noBody, "bad reference": badReference, "zero bytes": zero} {
			if _, err := f.uploads.Upload(ctx, req); !errors.Is(err, media.ErrInvalid) {
				t.Fatalf("%s: Upload = %v, want ErrInvalid", name, err)
			}
		}
		f.wantUsage(t, 0, 0)
		if keys := f.storage.putKeys(); len(keys) != 0 {
			t.Fatalf("storage puts = %v, want none", keys)
		}
		result := f.mustUpload(t, longest)
		f.wantServed(t, result.AssetID, "hello")
	})

	// S7, S8, S10: every failure or cancellation after the reservation
	// releases it, deletes the stored bytes, and purges the asset.
	stages := []struct {
		name   string
		body   string
		setup  func(t *testing.T, f *fixture) context.Context
		verify func(t *testing.T, err error)
	}{
		{
			name:   "body shorter than declared",
			body:   "hell",
			verify: func(t *testing.T, err error) { wantFailed(t, err, media.ErrUploadFailed) },
		},
		{
			name:   "body longer than declared",
			body:   "hello!",
			verify: func(t *testing.T, err error) { wantFailed(t, err, media.ErrUploadFailed) },
		},
		{
			name: "put failure",
			setup: func(t *testing.T, f *fixture) context.Context {
				f.storage.fail("put", failWith(errStorage))
				return ctx
			},
			verify: func(t *testing.T, err error) { wantFailed(t, err, errStorage) },
		},
		{
			name: "put cancelled",
			setup: func(t *testing.T, f *fixture) context.Context {
				f.storage.block("put")
				return cancelWhenStarted(t, f.storage.started)
			},
			verify: wantCancelled,
		},
		{
			name: "open failure",
			setup: func(t *testing.T, f *fixture) context.Context {
				f.storage.fail("open", failWith(errStorage))
				return ctx
			},
			verify: func(t *testing.T, err error) { wantFailed(t, err, errStorage) },
		},
		{
			name: "inspection rejection",
			setup: func(t *testing.T, f *fixture) context.Context {
				f.inspector.reject(dimensionsRejection(t))
				return ctx
			},
			verify: func(t *testing.T, err error) {
				wantRejection(t, err, media.RejectionDimensionsExceeded)
				if _, unchanged := err.(*media.Rejection); !unchanged {
					t.Fatalf("Upload error = %#v, want the rejection unchanged", err)
				}
			},
		},
		{
			name:   "inspection timeout",
			setup:  func(t *testing.T, f *fixture) context.Context { f.inspector.expire(); return ctx },
			verify: func(t *testing.T, err error) { wantFailed(t, err, context.DeadlineExceeded) },
		},
		{
			name: "inspection cancelled by the request",
			setup: func(t *testing.T, f *fixture) context.Context {
				f.inspector.block()
				return cancelWhenStarted(t, f.inspector.started)
			},
			verify: wantCancelled,
		},
		{
			name: "inspected size differs from declared",
			setup: func(t *testing.T, f *fixture) context.Context {
				f.inspector.set(func(_ context.Context, data []byte) (media.Inspection, error) {
					inspection := imageOf(data)
					inspection.InputBytes++
					return inspection, nil
				})
				return ctx
			},
			verify: func(t *testing.T, err error) { wantRejection(t, err, media.RejectionInvalidMetadata) },
		},
	}
	for _, stage := range stages {
		t.Run(stage.name, func(t *testing.T) {
			f := srv.fixture(t)
			req := f.request("hello", "ref-1")
			if stage.body != "" {
				req.Body = strings.NewReader(stage.body)
			}
			uploadCtx := ctx
			if stage.setup != nil {
				uploadCtx = stage.setup(t, f)
			}
			_, err := f.uploads.Upload(uploadCtx, req)
			stage.verify(t, err)
			reserved := f.reservation(t, req)
			f.wantUsage(t, 0, 0)
			f.wantGone(t, reserved)
			if keys := f.storage.putKeys(); len(keys) != 1 || keys[0] != reserved.StorageKey {
				t.Fatalf("storage puts = %v, want one under %s", keys, reserved.StorageKey)
			}
			if f.storage.objectCount() != 0 {
				t.Fatalf("storage holds %d objects, want none", f.storage.objectCount())
			}
		})
	}

	// S7 complete stage: a failed completion deletes the stored bytes and
	// releases the reservation; the existing asset is untouched.
	t.Run("complete failure deletes the stored bytes", func(t *testing.T) {
		f := srv.fixture(t)
		first := f.mustUpload(t, f.request("first", "ref-1"))
		req := f.request("second", "ref-1")
		if _, err := f.uploads.Upload(ctx, req); !errors.Is(err, media.ErrReferenceExists) {
			t.Fatalf("Upload = %v, want ErrReferenceExists", err)
		}
		f.wantUsage(t, 5, 0)
		f.wantGone(t, f.reservation(t, req))
		f.wantServed(t, first.AssetID, "first")
		if f.storage.objectCount() != 1 {
			t.Fatalf("storage holds %d objects, want only the first", f.storage.objectCount())
		}
	})

	// S5 through the pipeline: no capacity for the replacement keeps the old
	// asset ready with its bytes and stores nothing new.
	t.Run("replacement without capacity keeps the old asset", func(t *testing.T) {
		f := srv.fixture(t)
		old := f.mustUpload(t, f.request("hello", "ref-1"))
		if _, err := f.store.Apply(ctx, f.key, media.Operation{ID: "filler"}, media.Reserve{DeclaredBytes: media.QuotaBytes - 5 - 3}); err != nil {
			t.Fatalf("filler Reserve: %v", err)
		}
		req := f.request("bigger", "ref-1")
		req.Replace = true
		if _, err := f.uploads.Upload(ctx, req); !errors.Is(err, media.ErrQuotaExceeded) {
			t.Fatalf("Upload = %v, want ErrQuotaExceeded", err)
		}
		f.wantUsage(t, 5, media.QuotaBytes-5-3)
		f.wantServed(t, old.AssetID, "hello")
		if keys := f.storage.putKeys(); len(keys) != 1 || f.storage.objectCount() != 1 {
			t.Fatalf("storage puts = %v with %d objects, want only the old upload", keys, f.storage.objectCount())
		}
	})

	// S9 at pipeline level: a retry after success replays the same result
	// without a second reservation or a second write.
	t.Run("retry after success replays the result", func(t *testing.T) {
		f := srv.fixture(t)
		req := f.request("hello", "ref-1")
		first := f.mustUpload(t, req)
		retry := req
		retry.Body = strings.NewReader("hello")
		again := f.mustUpload(t, retry)
		if again != first {
			t.Fatalf("retry = %+v, want %+v", again, first)
		}
		f.wantUsage(t, 5, 0)
		f.wantServed(t, first.AssetID, "hello")
		if keys := f.storage.putKeys(); len(keys) != 1 || f.storage.objectCount() != 1 {
			t.Fatalf("storage puts = %v with %d objects, want one", keys, f.storage.objectCount())
		}
		conflicting := req
		conflicting.ReferenceID = "ref-2"
		conflicting.Body = strings.NewReader("hello")
		if _, err := f.uploads.Upload(ctx, conflicting); !errors.Is(err, media.ErrOperationConflict) {
			t.Fatalf("retry with another reference = %v, want ErrOperationConflict", err)
		}
		f.wantUsage(t, 5, 0)
		f.wantServed(t, first.AssetID, "hello")
	})

	t.Run("retry after a failure leaves nothing behind", func(t *testing.T) {
		f := srv.fixture(t)
		req := f.request("hello", "ref-1")
		f.storage.fail("put", failWith(errStorage))
		_, err := f.uploads.Upload(ctx, req)
		wantFailed(t, err, errStorage)
		f.storage.fail("put", nil)
		retry := req
		retry.Body = strings.NewReader("hello")
		if _, err := f.uploads.Upload(ctx, retry); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("retry = %v, want ErrNotFound (the reservation was abandoned)", err)
		}
		f.wantUsage(t, 0, 0)
		f.wantGone(t, f.reservation(t, req))
		if f.storage.objectCount() != 0 {
			t.Fatalf("storage holds %d objects, want none", f.storage.objectCount())
		}
	})

	t.Run("retry after a rejection with another outcome leaves nothing behind", func(t *testing.T) {
		f := srv.fixture(t)
		req := f.request("hello", "ref-1")
		f.inspector.reject(dimensionsRejection(t))
		_, err := f.uploads.Upload(ctx, req)
		wantRejection(t, err, media.RejectionDimensionsExceeded)
		f.inspector.set(nil)
		retry := req
		retry.Body = strings.NewReader("hello")
		if _, err := f.uploads.Upload(ctx, retry); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("retry = %v, want ErrNotFound (the reservation was abandoned)", err)
		}
		f.wantUsage(t, 0, 0)
		f.wantGone(t, f.reservation(t, req))
		if f.storage.objectCount() != 0 {
			t.Fatalf("storage holds %d objects, want none", f.storage.objectCount())
		}
	})

	// A retry after the asset was released and purged replays the response
	// but must not leave the rewritten bytes orphaned in storage.
	t.Run("retry after release does not orphan bytes", func(t *testing.T) {
		f := srv.fixture(t)
		req := f.request("hello", "ref-1")
		first := f.mustUpload(t, req)
		reserved := f.reservation(t, req)
		if _, err := f.store.Apply(ctx, f.key, media.Operation{ID: "detach"}, media.Detach{ReferenceID: "ref-1"}); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		if err := f.storage.Delete(ctx, reserved.StorageKey); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if err := f.store.Purge(ctx, f.key, reserved.AssetID); err != nil {
			t.Fatalf("Purge: %v", err)
		}
		retry := req
		retry.Body = strings.NewReader("hello")
		again, err := f.uploads.Upload(ctx, retry)
		if err != nil || again != first {
			t.Fatalf("retry = %+v, %v; want the replayed %+v", again, err, first)
		}
		f.wantUsage(t, 0, 0)
		f.wantGone(t, reserved)
	})
}

// S5, S8: a successful replacement releases the old asset, whose bytes are
// collected by Sweep.
func TestUploadReplacementReleasesOldBytes(t *testing.T) {
	f := startServer(t).fixture(t)
	ctx := context.Background()
	oldReq := f.request("hello", "ref-1")
	old := f.mustUpload(t, oldReq)
	oldReserved := f.reservation(t, oldReq)
	req := f.request("goodbye", "ref-1")
	req.Replace = true
	replaced := f.mustUpload(t, req)
	if replaced.AssetID == old.AssetID || replaced.Usage != (media.Usage{QuotaBytes: media.QuotaBytes, UsedBytes: 7}) {
		t.Fatalf("replacement = %+v, want a new asset using 7 bytes", replaced)
	}
	f.wantUsage(t, 7, 0)
	asset := f.wantServed(t, replaced.AssetID, "goodbye")
	if asset.References != 1 {
		t.Fatalf("replacement references = %d, want 1", asset.References)
	}
	f.wantIsolated(t, oldReserved)
	purged, err := f.uploads.Sweep(ctx, 10)
	if err != nil || purged != 1 {
		t.Fatalf("Sweep = %d, %v; want 1, nil", purged, err)
	}
	f.wantGone(t, oldReserved)
	f.wantServed(t, replaced.AssetID, "goodbye")
	if f.storage.objectCount() != 1 {
		t.Fatalf("storage holds %d objects, want only the replacement", f.storage.objectCount())
	}
}

// S8, S11: when deleting the bytes fails during cleanup, the asset stays
// isolated (not ready, not served, not counted) until Sweep deletes it.
func TestUploadCleanupFailureIsolatesUntilSweep(t *testing.T) {
	srv := startServer(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		setup  func(t *testing.T, f *fixture)
		verify func(t *testing.T, err error)
	}{
		{
			name:   "failed upload stays deleting",
			setup:  func(t *testing.T, f *fixture) { f.storage.fail("open", failWith(errStorage)) },
			verify: func(t *testing.T, err error) { wantFailed(t, err, errStorage) },
		},
		{
			name:   "rejected upload stays quarantined",
			setup:  func(t *testing.T, f *fixture) { f.inspector.reject(dimensionsRejection(t)) },
			verify: func(t *testing.T, err error) { wantRejection(t, err, media.RejectionDimensionsExceeded) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := srv.fixture(t)
			tc.setup(t, f)
			f.storage.fail("delete", failWith(errDeleteA))
			req := f.request("hello", "ref-1")
			_, err := f.uploads.Upload(ctx, req)
			tc.verify(t, err)
			reserved := f.reservation(t, req)
			f.wantUsage(t, 0, 0)
			f.wantIsolated(t, reserved)
			if _, ok := f.storage.object(reserved.StorageKey); !ok {
				t.Fatalf("storage lost %s although Delete failed", reserved.StorageKey)
			}

			f.storage.fail("delete", nil)
			purged, err := f.uploads.Sweep(ctx, 10)
			if err != nil || purged != 1 {
				t.Fatalf("Sweep = %d, %v; want 1, nil", purged, err)
			}
			f.wantUsage(t, 0, 0)
			f.wantGone(t, reserved)
		})
	}
}

// Sweep deletes and purges every listed asset, continues past a failing
// item, and returns the first error.
func TestSweepContinuesPastFailingItem(t *testing.T) {
	f := startServer(t).fixture(t)
	ctx := context.Background()
	if _, err := f.uploads.Sweep(ctx, 0); !errors.Is(err, media.ErrInvalid) {
		t.Fatalf("Sweep(0) = %v, want ErrInvalid", err)
	}
	f.storage.fail("open", failWith(errStorage))
	f.storage.fail("delete", failWith(errDeleteA))
	for i := range 3 {
		_, err := f.uploads.Upload(ctx, f.request("hello", fmt.Sprintf("ref-%d", i)))
		wantFailed(t, err, errStorage)
	}
	listed, err := f.store.Collect(ctx, 10)
	if err != nil || len(listed) != 3 {
		t.Fatalf("Collect = %d items, %v; want 3", len(listed), err)
	}
	failing := map[string]error{listed[0].StorageKey: errDeleteA, listed[2].StorageKey: errDeleteB}
	f.storage.fail("delete", func(key string) error { return failing[key] })

	purged, err := f.uploads.Sweep(ctx, 10)
	if purged != 1 || !errors.Is(err, errDeleteA) || errors.Is(err, errDeleteB) {
		t.Fatalf("Sweep = %d, %v; want 1 and the first error only", purged, err)
	}
	f.wantGone(t, media.Result{AssetID: listed[1].AssetID, StorageKey: listed[1].StorageKey})
	for _, item := range []media.Garbage{listed[0], listed[2]} {
		f.wantIsolated(t, media.Result{AssetID: item.AssetID, StorageKey: item.StorageKey})
		if _, ok := f.storage.object(item.StorageKey); !ok {
			t.Fatalf("storage lost %s although Delete failed", item.StorageKey)
		}
	}

	f.storage.fail("delete", nil)
	if purged, err := f.uploads.Sweep(ctx, 10); err != nil || purged != 2 {
		t.Fatalf("second Sweep = %d, %v; want 2, nil", purged, err)
	}
	for _, item := range listed {
		f.wantGone(t, media.Result{AssetID: item.AssetID, StorageKey: item.StorageKey})
	}
	f.wantUsage(t, 0, 0)
	if f.storage.objectCount() != 0 {
		t.Fatalf("storage holds %d objects, want none", f.storage.objectCount())
	}
}
