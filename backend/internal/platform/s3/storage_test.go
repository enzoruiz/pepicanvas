package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// largeBytes exceeds the client's default part size, so only a disabled
// multipart upload sends it in a single PUT.
const largeBytes = 17 << 20

// owner is recorded with every object these tests store.
var owner = media.Owner{Key: media.CanvasKey{TenantID: "tenant-a", CanvasID: "canvas-1"}, AssetID: "asset-1"}

func config(server *s3test.Server) s3.Config {
	return s3.Config{
		Endpoint:  server.Endpoint,
		Bucket:    s3test.Bucket,
		AccessKey: secret.New(secret.KindPassword, s3test.AccessKey),
		SecretKey: secret.New(secret.KindPassword, s3test.SecretKey),
	}
}

func open(t *testing.T, server *s3test.Server) *s3.Storage {
	t.Helper()
	storage, err := s3.Open(context.Background(), config(server))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return storage
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	return data
}

func read(t *testing.T, storage *s3.Storage, key string) []byte {
	t.Helper()
	body, err := storage.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("Open(%s): %v", key, err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return data
}

func wantNoObject(t *testing.T, server *s3test.Server, storage *s3.Storage, key string) {
	t.Helper()
	if server.Exists(t, key) {
		t.Fatalf("bucket holds %s, want no object", key)
	}
	if body, err := storage.Open(context.Background(), key); !errors.Is(err, media.ErrNotFound) || body != nil {
		t.Fatalf("Open(%s) = %v, %v; want ErrNotFound", key, body, err)
	}
	if uploads := server.IncompleteUploads(t); len(uploads) != 0 {
		t.Fatalf("incomplete multipart uploads = %v, want none", uploads)
	}
}

// stallingReader returns prefix, signals started, then blocks until release
// is closed and fails, like a client connection that stops sending.
type stallingReader struct {
	prefix  *bytes.Reader
	started chan struct{}
	release chan struct{}
}

func (r *stallingReader) Read(p []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
	}
	select {
	case r.started <- struct{}{}:
	default:
	}
	<-r.release
	return 0, errors.New("client went away")
}

// failingTail returns its data, then fails instead of ending.
type failingTail struct {
	data *bytes.Reader
	err  error
}

func (r *failingTail) Read(p []byte) (int, error) {
	if r.data.Len() > 0 {
		return r.data.Read(p)
	}
	return 0, r.err
}

// slowTail returns its data at once and then, after delay, one more byte, so
// the extra byte arrives after the transport has read the declared size.
type slowTail struct {
	data  *bytes.Reader
	delay time.Duration
	done  bool
}

func (r *slowTail) Read(p []byte) (int, error) {
	if r.data.Len() > 0 {
		return r.data.Read(p)
	}
	if r.done || len(p) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	r.done = true
	p[0] = 'x'
	return 1, nil
}

func TestStorage(t *testing.T) {
	server := s3test.Start(t)
	storage := open(t, server)
	var _ media.Storage = storage
	ctx := context.Background()

	t.Run("round trip returns the exact bytes", func(t *testing.T) {
		for _, n := range []int{0, 1, 300 << 10, largeBytes} {
			key := fmt.Sprintf("assets/round-trip-%d", n)
			data := random(t, n)
			if err := storage.Put(ctx, key, owner, bytes.NewReader(data), int64(n)); err != nil {
				t.Fatalf("Put(%d bytes): %v", n, err)
			}
			if got := read(t, storage, key); !bytes.Equal(got, data) {
				t.Fatalf("Open(%s) returned %d bytes, want the %d bytes stored", key, len(got), n)
			}
		}
	})

	t.Run("Open of a missing key is ErrNotFound", func(t *testing.T) {
		body, err := storage.Open(ctx, "assets/never-stored")
		if !errors.Is(err, media.ErrNotFound) || body != nil {
			t.Fatalf("Open = %v, %v; want ErrNotFound", body, err)
		}
	})

	t.Run("Delete removes an object and succeeds on a missing key", func(t *testing.T) {
		if err := storage.Delete(ctx, "assets/never-stored"); err != nil {
			t.Fatalf("Delete(missing) = %v, want nil", err)
		}
		key := "assets/delete-me"
		if err := storage.Put(ctx, key, owner, strings.NewReader("bytes"), 5); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := storage.Delete(ctx, key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		wantNoObject(t, server, storage, key)
		if err := storage.Delete(ctx, key); err != nil {
			t.Fatalf("second Delete = %v, want nil", err)
		}
	})

	t.Run("a Put cancelled while streaming leaves nothing", func(t *testing.T) {
		for _, n := range []int{300 << 10, largeBytes} {
			key := fmt.Sprintf("assets/cancelled-%d", n)
			body := &stallingReader{prefix: bytes.NewReader(random(t, 128<<10)), started: make(chan struct{}, 1), release: make(chan struct{})}
			putCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- storage.Put(putCtx, key, owner, body, int64(n)) }()
			select {
			case <-body.started:
			case err := <-done:
				t.Fatalf("Put returned %v before the body stalled", err)
			case <-time.After(30 * time.Second):
				t.Fatal("Put never read past the prefix")
			}
			cancel()
			var err error
			select {
			case err = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("Put did not return after cancellation")
			}
			close(body.release)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Put = %v, want context.Canceled", err)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("a Put whose body is shorter than its size leaves nothing", func(t *testing.T) {
		for _, n := range []int{1000, largeBytes} {
			key := fmt.Sprintf("assets/short-%d", n)
			if err := storage.Put(ctx, key, owner, bytes.NewReader(random(t, n/2)), int64(n)); err == nil {
				t.Fatalf("Put of %d of %d bytes = nil, want an error", n/2, n)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("a Put whose body is longer than its size leaves nothing", func(t *testing.T) {
		for _, n := range []int{1000, largeBytes} {
			key := fmt.Sprintf("assets/long-%d", n)
			if err := storage.Put(ctx, key, owner, bytes.NewReader(random(t, n+1)), int64(n)); err == nil {
				t.Fatalf("Put of %d of %d bytes = nil, want an error", n+1, n)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("a Put whose extra byte arrives late leaves nothing", func(t *testing.T) {
		for _, n := range []int{0, 1, 1000} {
			key := fmt.Sprintf("assets/late-tail-%d", n)
			body := &slowTail{data: bytes.NewReader(random(t, n)), delay: 300 * time.Millisecond}
			if err := storage.Put(ctx, key, owner, body, int64(n)); err == nil {
				t.Errorf("Put of %d bytes plus a late byte = nil, want an error", n)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("a Put whose body fails after its size leaves nothing", func(t *testing.T) {
		key := "assets/failing-tail"
		body := &failingTail{data: bytes.NewReader(random(t, 1000)), err: errors.New("body is too long")}
		if err := storage.Put(ctx, key, owner, body, 1000); err == nil {
			t.Fatal("Put = nil, want the body's error")
		}
		wantNoObject(t, server, storage, key)
	})

	t.Run("a Put never replaces an existing object", func(t *testing.T) {
		key := "assets/conditional"
		if err := storage.Put(ctx, key, owner, strings.NewReader("first"), 5); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := storage.Put(ctx, key, owner, strings.NewReader("other"), 5); !errors.Is(err, media.ErrObjectExists) {
			t.Fatalf("second Put = %v, want ErrObjectExists", err)
		}
		if got := read(t, storage, key); string(got) != "first" {
			t.Fatalf("Open(%s) = %q, want %q", key, got, "first")
		}
	})
}

// TE-013 S5 and S8: an object records the asset that owns it, so the
// reconciler can prove ownership of an object without a row.
func TestObjectStore(t *testing.T) {
	server := s3test.Start(t)
	storage := open(t, server)
	var _ media.ObjectStore = storage
	ctx := context.Background()

	t.Run("Stat returns the owner recorded by Put", func(t *testing.T) {
		before := time.Now().Add(-time.Minute)
		other := media.Owner{Key: media.CanvasKey{TenantID: "tenant-b", CanvasID: "canvas-1"}, AssetID: "asset-1"}
		for key, want := range map[string]media.Owner{"assets/owned-a": owner, "assets/owned-b": other} {
			if err := storage.Put(ctx, key, want, strings.NewReader("bytes"), 5); err != nil {
				t.Fatalf("Put(%s): %v", key, err)
			}
			object, err := storage.Stat(ctx, key)
			if err != nil {
				t.Fatalf("Stat(%s): %v", key, err)
			}
			if object.Key != key || !object.Owned || object.Owner != want {
				t.Fatalf("Stat(%s) = %+v, want key %s owned by %+v", key, object, key, want)
			}
			if object.ModifiedAt.Before(before) || object.ModifiedAt.After(time.Now().Add(time.Minute)) {
				t.Fatalf("Stat(%s).ModifiedAt = %v, want about now", key, object.ModifiedAt)
			}
		}
	})

	t.Run("Stat of a missing key is ErrNotFound", func(t *testing.T) {
		if object, err := storage.Stat(ctx, "assets/never-stored"); !errors.Is(err, media.ErrNotFound) {
			t.Fatalf("Stat = %+v, %v; want ErrNotFound", object, err)
		}
	})

	t.Run("an object without valid owner metadata is unowned", func(t *testing.T) {
		cases := map[string]map[string]string{
			"assets/no-metadata": nil,
			"assets/partial":     {"Pepicanvas-Tenant": "tenant-a", "Pepicanvas-Canvas": "canvas-1"},
			"assets/malformed":   {"Pepicanvas-Tenant": "tenant a", "Pepicanvas-Canvas": "canvas-1", "Pepicanvas-Asset": "asset-1"},
		}
		for key, metadata := range cases {
			if _, err := server.Client.PutObject(ctx, s3test.Bucket, key, strings.NewReader("x"), 1, minio.PutObjectOptions{UserMetadata: metadata}); err != nil {
				t.Fatalf("raw put %s: %v", key, err)
			}
			object, err := storage.Stat(ctx, key)
			if err != nil {
				t.Fatalf("Stat(%s): %v", key, err)
			}
			if object.Owned || object.Owner != (media.Owner{}) {
				t.Fatalf("Stat(%s) = %+v, want unowned with no owner", key, object)
			}
		}
	})

	t.Run("Put rejects an invalid owner and stores nothing", func(t *testing.T) {
		for _, bad := range []media.Owner{{}, {Key: owner.Key}, {Key: media.CanvasKey{TenantID: "tenant a", CanvasID: "c"}, AssetID: "a"}} {
			key := "assets/bad-owner"
			if err := storage.Put(ctx, key, bad, strings.NewReader("bytes"), 5); !errors.Is(err, media.ErrInvalid) {
				t.Fatalf("Put(owner %+v) = %v, want ErrInvalid", bad, err)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("List pages keys in order after a cursor", func(t *testing.T) {
		if _, err := storage.List(ctx, "", 0); !errors.Is(err, media.ErrInvalid) {
			t.Fatalf("List(limit 0) = %v, want ErrInvalid", err)
		}
		want := server.Keys(t)
		if len(want) < 5 {
			t.Fatalf("bucket holds %v, want at least 5 objects", want)
		}
		var got []string
		after := ""
		for page := 0; ; page++ {
			objects, err := storage.List(ctx, after, 2)
			if err != nil {
				t.Fatalf("List(%q, 2): %v", after, err)
			}
			if len(objects) > 2 {
				t.Fatalf("List(%q, 2) returned %d objects", after, len(objects))
			}
			if len(objects) == 0 {
				break
			}
			for _, object := range objects {
				if object.ModifiedAt.IsZero() || object.Owned {
					t.Fatalf("listed %+v, want a modification time and no owner", object)
				}
				got = append(got, object.Key)
			}
			after = objects[len(objects)-1].Key
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("paged keys = %v, want %v", got, want)
		}
	})

	t.Run("List stopped before the last key leaves no listing goroutine", func(t *testing.T) {
		// A limit past one S3 page stops the listing while minio still holds
		// keys of the next page to send.
		const limit = 1001
		var wg sync.WaitGroup
		slots := make(chan struct{}, 16)
		errs := make(chan error, limit+10)
		for i := range limit + 10 {
			slots <- struct{}{}
			wg.Go(func() {
				defer func() { <-slots }()
				if _, err := server.Client.PutObject(ctx, s3test.Bucket, fmt.Sprintf("bulk/%04d", i), strings.NewReader("x"), 1, minio.PutObjectOptions{}); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		if err := <-errs; err != nil {
			t.Fatalf("raw put: %v", err)
		}
		for range 3 {
			if objects, err := storage.List(ctx, "", limit); err != nil || len(objects) != limit {
				t.Fatalf("List(%d) = %d objects, %v", limit, len(objects), err)
			}
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			leaked := listingGoroutines()
			if leaked == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%d listing goroutines still running after List returned", leaked)
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
}

// listingGoroutines counts goroutines started by minio's ListObjects.
func listingGoroutines() int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "minio-go/v7.(*Client).ListObjects.func")
		}
		buf = make([]byte, 2*len(buf))
	}
}

func TestOpenFailsWithoutEchoingCredentials(t *testing.T) {
	server := s3test.Start(t)
	const decoyAccess, decoySecret = "decoy-access-7f3a9c1e", "decoy-secret-b41d0e77"
	cases := []struct {
		name   string
		config func(s3.Config) s3.Config
	}{
		{"nonexistent bucket", func(c s3.Config) s3.Config { c.Bucket = "no-such-bucket"; return c }},
		{"wrong secret key", func(c s3.Config) s3.Config {
			c.SecretKey = secret.New(secret.KindPassword, decoySecret)
			return c
		}},
		{"wrong access key", func(c s3.Config) s3.Config {
			c.AccessKey = secret.New(secret.KindPassword, decoyAccess)
			c.SecretKey = secret.New(secret.KindPassword, decoySecret)
			return c
		}},
		{"endpoint with userinfo", func(c s3.Config) s3.Config {
			c.Endpoint = decoyAccess + ":" + decoySecret + "@" + c.Endpoint
			return c
		}},
		{"unreachable endpoint", func(c s3.Config) s3.Config { c.Endpoint = "127.0.0.1:1"; return c }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cfg := tc.config(config(server))
			storage, err := s3.Open(ctx, cfg)
			if err == nil || storage != nil {
				t.Fatalf("Open = %v, %v; want an error", storage, err)
			}
			if !errors.Is(err, s3.ErrUnavailable) {
				t.Fatalf("Open error = %v, want ErrUnavailable", err)
			}
			for _, raw := range []string{cfg.AccessKey.Reveal(), cfg.SecretKey.Reveal(), decoyAccess, decoySecret, s3test.SecretKey} {
				if strings.Contains(err.Error(), raw) {
					t.Fatalf("Open error %q echoes credential %q", err, raw)
				}
			}
		})
	}
}

func TestConfigFormatRedactsCredentials(t *testing.T) {
	cfg := s3.Config{
		Endpoint:  "localhost:9000",
		Bucket:    "media",
		AccessKey: secret.New(secret.KindPassword, "raw-access-5e1c"),
		SecretKey: secret.New(secret.KindPassword, "raw-secret-9d2b"),
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		out := fmt.Sprintf(verb, cfg) + fmt.Sprintf(verb, &cfg)
		for _, raw := range []string{"raw-access-5e1c", "raw-secret-9d2b"} {
			if strings.Contains(out, raw) {
				t.Fatalf("Sprintf(%s, Config) = %q, echoes %q", verb, out, raw)
			}
		}
	}
}
