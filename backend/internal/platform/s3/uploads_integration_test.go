package s3_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/mediaexec"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
)

var uploadKey = media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-1"}

// recordingStorage is the real storage; it remembers every key written so a
// test can look for the bytes of an upload that failed.
type recordingStorage struct {
	*s3.Storage
	mu   sync.Mutex
	keys []string
}

func (r *recordingStorage) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	r.mu.Lock()
	r.keys = append(r.keys, key)
	r.mu.Unlock()
	return r.Storage.Put(ctx, key, body, size)
}

func (r *recordingStorage) last(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.keys) == 0 {
		t.Fatal("no object was written")
	}
	return r.keys[len(r.keys)-1]
}

// pipeline is media.Uploads over PostgreSQL and MinIO.
type pipeline struct {
	uploads *media.Uploads
	store   *postgres.MediaStore
	storage *recordingStorage
	server  *s3test.Server
	ready   map[string]bool // storage keys of ready assets
	seq     int
}

func newPipeline(t *testing.T, inspector media.Inspector) *pipeline {
	t.Helper()
	ctx := context.Background()
	server := s3test.Start(t)
	pool, err := postgres.Open(ctx, pgtest.Start(t).DSN(t))
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := postgres.NewCanvasStore(pool).Create(ctx, canvas.Key{TenantID: uploadKey.TenantID, CanvasID: uploadKey.CanvasID}); err != nil {
		t.Fatalf("create canvas: %v", err)
	}
	store := postgres.NewMediaStore(pool)
	storage := &recordingStorage{Storage: open(t, server)}
	return &pipeline{
		uploads: media.NewUploads(store, storage, inspector),
		store:   store, storage: storage, server: server,
		ready: map[string]bool{},
	}
}

func (p *pipeline) request(data []byte, body io.Reader) media.UploadRequest {
	p.seq++
	if body == nil {
		body = bytes.NewReader(data)
	}
	return media.UploadRequest{
		Key:           uploadKey,
		Operation:     media.Operation{ID: fmt.Sprintf("upload-%d", p.seq), CorrelationID: "0123456789abcdef0123456789abcdef"},
		DeclaredBytes: int64(len(data)),
		ReferenceID:   fmt.Sprintf("object-%d", p.seq),
		Body:          body,
	}
}

// upload stores data and checks the asset is served byte-exact.
func (p *pipeline) upload(t *testing.T, data []byte) media.UploadResult {
	t.Helper()
	before := p.usage(t)
	result, err := p.uploads.Upload(context.Background(), p.request(data, nil))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	asset, body, err := p.uploads.Open(context.Background(), uploadKey, result.AssetID)
	if err != nil {
		t.Fatalf("Open(ready asset): %v", err)
	}
	defer body.Close()
	served, err := io.ReadAll(body)
	if err != nil || !bytes.Equal(served, data) {
		t.Fatalf("served %d bytes (%v), want the %d bytes uploaded", len(served), err, len(data))
	}
	if !p.server.Exists(t, asset.StorageKey) {
		t.Fatalf("bucket lacks the ready asset %s", asset.StorageKey)
	}
	want := before
	want.UsedBytes += int64(len(data))
	if got := p.usage(t); got != want {
		t.Fatalf("usage after upload = %+v, want %+v", got, want)
	}
	p.ready[asset.StorageKey] = true
	return result
}

func (p *pipeline) usage(t *testing.T) media.Usage {
	t.Helper()
	usage, err := p.store.Usage(context.Background(), uploadKey)
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	return usage
}

// wantNothingRecoverable checks that a failed or released asset can no
// longer be read by any path and that the bucket holds only ready assets.
func (p *pipeline) wantNothingRecoverable(t *testing.T, assetID, storageKey string, usage media.Usage) {
	t.Helper()
	if assetID != "" {
		if _, body, err := p.uploads.Open(context.Background(), uploadKey, assetID); !errors.Is(err, media.ErrNotFound) || body != nil {
			t.Fatalf("Uploads.Open(%s) = %v, %v; want ErrNotFound", assetID, body, err)
		}
	}
	if p.server.Exists(t, storageKey) {
		t.Fatalf("bucket still holds %s", storageKey)
	}
	if body, err := p.storage.Open(context.Background(), storageKey); !errors.Is(err, media.ErrNotFound) || body != nil {
		t.Fatalf("Storage.Open(%s) = %v, %v; want ErrNotFound", storageKey, body, err)
	}
	if incomplete := p.server.IncompleteUploads(t); len(incomplete) != 0 {
		t.Fatalf("incomplete multipart uploads = %v", incomplete)
	}
	for _, key := range p.server.Keys(t) {
		if !p.ready[key] {
			t.Fatalf("bucket holds %s, which is not a ready asset", key)
		}
	}
	if got := p.usage(t); got != usage {
		t.Fatalf("usage = %+v, want %+v", got, usage)
	}
}

// fakeInspector reads the whole input, then answers with a fixed outcome.
type fakeInspector struct {
	outcome func(ctx context.Context, n int64) (media.Inspection, error)
}

func (f fakeInspector) Inspect(ctx context.Context, input io.Reader) (media.Inspection, error) {
	n, err := io.Copy(io.Discard, input)
	if err != nil {
		return media.Inspection{}, err
	}
	return f.outcome(ctx, n)
}

func validImage(_ context.Context, n int64) (media.Inspection, error) {
	return media.Inspection{Kind: media.KindImage, InputBytes: n, StreamCount: 1, Dimensions: media.Dimensions{Width: 16, Height: 16}}, nil
}

func TestUploadsOverS3LeaveNothingRecoverable(t *testing.T) {
	var mode string
	inspector := fakeInspector{outcome: func(ctx context.Context, n int64) (media.Inspection, error) {
		switch mode {
		case "reject":
			return media.Inspection{}, media.Validate(media.Inspection{Kind: "svg"}, media.DefaultLimits())
		case "timeout":
			deadline, cancel := context.WithTimeout(ctx, time.Millisecond)
			defer cancel()
			<-deadline.Done()
			return media.Inspection{}, deadline.Err()
		default:
			return validImage(ctx, n)
		}
	}}
	p := newPipeline(t, inspector)
	data := random(t, 300<<10)

	mode = "accept"
	kept := p.upload(t, data)
	baseline := p.usage(t)

	t.Run("rejected upload", func(t *testing.T) {
		mode = "reject"
		_, err := p.uploads.Upload(context.Background(), p.request(data, nil))
		var rejection *media.Rejection
		if !errors.As(err, &rejection) || rejection.Code() != media.RejectionUnsupportedKind {
			t.Fatalf("Upload = %v, want the unsupported kind rejection", err)
		}
		p.wantNothingRecoverable(t, "", p.storage.last(t), baseline)
	})

	t.Run("cancelled while the body streams", func(t *testing.T) {
		mode = "accept"
		ctx, cancel := context.WithCancel(context.Background())
		body := &stallingReader{prefix: bytes.NewReader(data[:128<<10]), started: make(chan struct{}, 1), release: make(chan struct{})}
		defer close(body.release)
		go func() { <-body.started; cancel() }()
		if _, err := p.uploads.Upload(ctx, p.request(data, body)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Upload = %v, want context.Canceled", err)
		}
		p.wantNothingRecoverable(t, "", p.storage.last(t), baseline)
	})

	t.Run("body one byte longer than declared", func(t *testing.T) {
		mode = "accept"
		longer := append(bytes.Clone(data), 'x')
		if _, err := p.uploads.Upload(context.Background(), p.request(data, bytes.NewReader(longer))); !errors.Is(err, media.ErrUploadFailed) {
			t.Fatalf("Upload = %v, want ErrUploadFailed", err)
		}
		p.wantNothingRecoverable(t, "", p.storage.last(t), baseline)
	})

	t.Run("inspection times out", func(t *testing.T) {
		mode = "timeout"
		if _, err := p.uploads.Upload(context.Background(), p.request(data, nil)); err == nil {
			t.Fatal("Upload = nil, want an error")
		}
		p.wantNothingRecoverable(t, "", p.storage.last(t), baseline)
	})

	t.Run("released asset after Sweep", func(t *testing.T) {
		mode = "accept"
		released := p.upload(t, data)
		asset, err := p.store.Ready(context.Background(), uploadKey, released.AssetID)
		if err != nil {
			t.Fatalf("Ready: %v", err)
		}
		if _, err := p.store.Apply(context.Background(), uploadKey, media.Operation{ID: "detach-released"},
			media.Detach{ReferenceID: fmt.Sprintf("object-%d", p.seq)}); err != nil {
			t.Fatalf("Detach: %v", err)
		}
		delete(p.ready, asset.StorageKey)
		if _, err := p.uploads.Sweep(context.Background(), 10); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		p.wantNothingRecoverable(t, released.AssetID, asset.StorageKey, baseline)
	})

	if _, _, err := p.uploads.Open(context.Background(), uploadKey, kept.AssetID); err != nil {
		t.Fatalf("the kept asset is no longer served: %v", err)
	}
}

// TestUploadsOverS3WithRealInspector runs the real ffprobe and ffmpeg inside
// a delegated cgroup (TE-005), so rejected, expired, and interrupted
// inspections are proven to leave no temporary file and no stored object.
func TestUploadsOverS3WithRealInspector(t *testing.T) {
	root := os.Getenv("PEPICANVAS_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("PEPICANVAS_TEST_CGROUP_ROOT is not set to a disposable delegated cgroup")
	}
	const ffmpeg, ffprobe = "/usr/bin/ffmpeg", "/usr/bin/ffprobe"
	png := filepath.Join(t.TempDir(), "valid.png")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=red:s=64x48", "-frames:v", "1", png).CombinedOutput(); err != nil {
		t.Fatalf("generate fixture: %v: %s", err, out)
	}
	valid, err := os.ReadFile(png)
	if err != nil {
		t.Fatal(err)
	}

	runner, result := mediaexec.NewCgroupRunner(root)
	if result != mediaexec.SuccessfulResult() {
		t.Fatalf("NewCgroupRunner = %#v", result)
	}
	tempRoot := t.TempDir()
	adapter := func(limits media.Limits) media.Inspector {
		inspector, err := mediaexec.New(mediaexec.Config{FFprobePath: ffprobe, FFmpegPath: ffmpeg, TempRoot: tempRoot, Limits: limits}, runner)
		if err != nil {
			t.Fatalf("mediaexec.New: %v", err)
		}
		return inspector
	}
	short := media.DefaultLimits()
	short.InspectionTimeout = time.Millisecond

	var active media.Inspector
	var cancelRequest context.CancelFunc
	p := newPipeline(t, inspectorFunc(func(ctx context.Context, input io.Reader) (media.Inspection, error) {
		return active.Inspect(ctx, input)
	}))
	wantEmptyTempRoot := func(t *testing.T) {
		t.Helper()
		if entries, err := os.ReadDir(tempRoot); err != nil || len(entries) != 0 {
			t.Fatalf("TempRoot holds %d entries (%v), want none", len(entries), err)
		}
	}

	active = adapter(media.DefaultLimits())
	p.upload(t, valid)
	wantEmptyTempRoot(t)
	baseline := p.usage(t)

	t.Run("undecodable bytes are rejected", func(t *testing.T) {
		active = adapter(media.DefaultLimits())
		garbage := append(slices.Clone(valid[:16]), random(t, 4096)...)
		if _, err := p.uploads.Upload(context.Background(), p.request(garbage, nil)); err == nil {
			t.Fatal("Upload = nil, want an error")
		}
		p.wantNothingRecoverable(t, "", p.storage.last(t), baseline)
		wantEmptyTempRoot(t)
	})

	t.Run("inspection expires", func(t *testing.T) {
		active = adapter(short)
		if _, err := p.uploads.Upload(context.Background(), p.request(valid, nil)); err == nil {
			t.Fatal("Upload = nil, want an error")
		}
		p.wantNothingRecoverable(t, "", p.storage.last(t), baseline)
		wantEmptyTempRoot(t)
	})

	t.Run("request cancelled during inspection", func(t *testing.T) {
		real := adapter(media.DefaultLimits())
		// Cancel once the adapter has staged the whole input, while the
		// tools are about to run.
		active = inspectorFunc(func(ctx context.Context, input io.Reader) (media.Inspection, error) {
			return real.Inspect(ctx, &cancelAtEOF{r: input, cancel: cancelRequest})
		})
		var ctx context.Context
		ctx, cancelRequest = context.WithCancel(context.Background())
		defer cancelRequest()
		if _, err := p.uploads.Upload(ctx, p.request(valid, nil)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Upload = %v, want context.Canceled", err)
		}
		p.wantNothingRecoverable(t, "", p.storage.last(t), baseline)
		wantEmptyTempRoot(t)
	})
}

type inspectorFunc func(context.Context, io.Reader) (media.Inspection, error)

func (f inspectorFunc) Inspect(ctx context.Context, input io.Reader) (media.Inspection, error) {
	return f(ctx, input)
}

type cancelAtEOF struct {
	r      io.Reader
	cancel context.CancelFunc
}

func (c *cancelAtEOF) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if errors.Is(err, io.EOF) {
		c.cancel()
	}
	return n, err
}
