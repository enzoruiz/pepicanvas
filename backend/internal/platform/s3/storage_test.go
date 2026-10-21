package s3_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/s3/s3test"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// largeBytes exceeds the client's default part size, so only a disabled
// multipart upload sends it in a single PUT.
const largeBytes = 17 << 20

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
			if err := storage.Put(ctx, key, bytes.NewReader(data), int64(n)); err != nil {
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
		if err := storage.Put(ctx, key, strings.NewReader("bytes"), 5); err != nil {
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
			go func() { done <- storage.Put(putCtx, key, body, int64(n)) }()
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
			if err := storage.Put(ctx, key, bytes.NewReader(random(t, n/2)), int64(n)); err == nil {
				t.Fatalf("Put of %d of %d bytes = nil, want an error", n/2, n)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("a Put whose body is longer than its size leaves nothing", func(t *testing.T) {
		for _, n := range []int{1000, largeBytes} {
			key := fmt.Sprintf("assets/long-%d", n)
			if err := storage.Put(ctx, key, bytes.NewReader(random(t, n+1)), int64(n)); err == nil {
				t.Fatalf("Put of %d of %d bytes = nil, want an error", n+1, n)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("a Put whose extra byte arrives late leaves nothing", func(t *testing.T) {
		for _, n := range []int{0, 1, 1000} {
			key := fmt.Sprintf("assets/late-tail-%d", n)
			body := &slowTail{data: bytes.NewReader(random(t, n)), delay: 300 * time.Millisecond}
			if err := storage.Put(ctx, key, body, int64(n)); err == nil {
				t.Errorf("Put of %d bytes plus a late byte = nil, want an error", n)
			}
			wantNoObject(t, server, storage, key)
		}
	})

	t.Run("a Put whose body fails after its size leaves nothing", func(t *testing.T) {
		key := "assets/failing-tail"
		body := &failingTail{data: bytes.NewReader(random(t, 1000)), err: errors.New("body is too long")}
		if err := storage.Put(ctx, key, body, 1000); err == nil {
			t.Fatal("Put = nil, want the body's error")
		}
		wantNoObject(t, server, storage, key)
	})

	t.Run("a Put never replaces an existing object", func(t *testing.T) {
		key := "assets/conditional"
		if err := storage.Put(ctx, key, strings.NewReader("first"), 5); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := storage.Put(ctx, key, strings.NewReader("other"), 5); !errors.Is(err, media.ErrObjectExists) {
			t.Fatalf("second Put = %v, want ErrObjectExists", err)
		}
		if got := read(t, storage, key); string(got) != "first" {
			t.Fatalf("Open(%s) = %q, want %q", key, got, "first")
		}
	})
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
