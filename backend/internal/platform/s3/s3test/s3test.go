// Package s3test starts a disposable S3-compatible server for integration
// tests.
package s3test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	tclog "github.com/testcontainers/testcontainers-go/log"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// Image is the MinIO server pinned by digest (MinIO
// RELEASE.2026-09-22T19-25-18Z, runs as UID 65532).
const Image = "cgr.dev/chainguard/minio@sha256:709099319add48dfb44aa596a7070d3ddd8dd04d7ae881765719aec29df9e510"

// Credentials of the root user. They exist only inside the disposable
// container.
const (
	AccessKey = "pepicanvas-root"
	SecretKey = "pepicanvas-root-secret"
)

// Bucket is created when the server starts.
const Bucket = "pepicanvas-media"

const port = "9000/tcp"

// Server is one MinIO container owned by a test.
type Server struct {
	// Endpoint is host:port of the S3 API.
	Endpoint string
	// Client is a raw client with root credentials for direct inspection of
	// the bucket.
	Client *minio.Client
}

// Start runs a fresh server with Bucket created and terminates it when the
// test ends. Without a container runtime the test is skipped, unless
// pgtest.RequireEnv is set to 1.
func Start(t testing.TB) *Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": AccessKey, "MINIO_ROOT_PASSWORD": SecretKey},
			ExposedPorts: []string{port},
			Tmpfs:        map[string]string{"/data": "rw"},
			WaitingFor:   wait.ForHTTP("/minio/health/live").WithPort(port).WithStartupTimeout(time.Minute),
		},
		Logger:  tclog.TestLogger(t),
		Started: true,
	})
	if container != nil {
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	}
	if err != nil {
		if os.Getenv(pgtest.RequireEnv) != "1" {
			t.Skipf("MinIO container unavailable (set %s=1 to fail): %v", pgtest.RequireEnv, err)
		}
		t.Fatalf("start MinIO container: %v", err)
	}
	endpoint, err := container.PortEndpoint(ctx, port, "")
	if err != nil {
		t.Fatalf("MinIO endpoint: %v", err)
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(AccessKey, SecretKey, "")})
	if err != nil {
		t.Fatalf("MinIO client: %v", err)
	}
	if err := client.MakeBucket(ctx, Bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return &Server{Endpoint: endpoint, Client: client}
}

// Exists reports whether key is stored in Bucket.
func (s *Server) Exists(t testing.TB, key string) bool {
	t.Helper()
	_, err := s.Client.StatObject(context.Background(), Bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true
	}
	if minio.ToErrorResponse(err).Code == minio.NoSuchKey {
		return false
	}
	t.Fatalf("stat %s: %v", key, err)
	return false
}

// Keys lists every object key in Bucket.
func (s *Server) Keys(t testing.TB) []string {
	t.Helper()
	var keys []string
	for object := range s.Client.ListObjects(context.Background(), Bucket, minio.ListObjectsOptions{Recursive: true}) {
		if object.Err != nil {
			t.Fatalf("list objects: %v", object.Err)
		}
		keys = append(keys, object.Key)
	}
	return keys
}

// IncompleteUploads lists the keys of multipart uploads that were started
// and never completed or aborted.
func (s *Server) IncompleteUploads(t testing.TB) []string {
	t.Helper()
	var keys []string
	for upload := range s.Client.ListIncompleteUploads(context.Background(), Bucket, "", true) {
		if upload.Err != nil {
			t.Fatalf("list incomplete uploads: %v", upload.Err)
		}
		keys = append(keys, upload.Key)
	}
	return keys
}
