// Package pgtest starts a disposable, restartable PostgreSQL server for
// integration tests.
package pgtest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tclog "github.com/testcontainers/testcontainers-go/log"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Image is the PostgreSQL server pinned by digest. Its version matches
// toolchain.postgresql in releases/client-support-release.json.
const Image = "postgres:17.4-alpine@sha256:7062a2109c4b51f3c792c7ea01e83ed12ef9a980886e3b3d380a7d2e5f6ce3f5"

// RequireEnv makes an unavailable container runtime fail instead of skip.
const RequireEnv = "PEPICANVAS_REQUIRE_POSTGRES"

// Server is one PostgreSQL container owned by a test.
type Server struct {
	container *tcpostgres.PostgresContainer
}

// Start runs a fresh server and terminates it when the test ends. Without a
// container runtime the test is skipped, unless RequireEnv is set to 1.
func Start(t testing.TB) *Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, Image,
		testcontainers.WithLogger(tclog.TestLogger(t)),
		tcpostgres.WithDatabase("pepicanvas"),
		tcpostgres.WithUsername("pepicanvas"),
		tcpostgres.WithPassword("pepicanvas"),
		tcpostgres.BasicWaitStrategies(),
	)
	if container != nil {
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(container) })
	}
	if err != nil {
		if os.Getenv(RequireEnv) != "1" {
			t.Skipf("PostgreSQL container unavailable (set %s=1 to fail): %v", RequireEnv, err)
		}
		t.Fatalf("start PostgreSQL container: %v", err)
	}
	return &Server{container: container}
}

// DSN returns the current connection string. The mapped port can change
// after Restart, so callers must ask again after restarting.
func (s *Server) DSN(t testing.TB) string {
	t.Helper()
	dsn, err := s.container.ConnectionString(context.Background(), "sslmode=disable")
	if err != nil {
		t.Fatalf("PostgreSQL connection string: %v", err)
	}
	return dsn
}

// Restart stops the server process and starts it again on the same data
// directory, which drops every open connection. It returns once the server
// accepts connections again; the log-based wait strategy cannot tell, because
// the ready messages of the previous run are still in the container log.
func (s *Server) Restart(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	timeout := 10 * time.Second
	if err := s.container.Stop(ctx, &timeout); err != nil {
		t.Fatalf("stop PostgreSQL container: %v", err)
	}
	if err := s.container.Start(ctx); err != nil {
		t.Fatalf("start PostgreSQL container: %v", err)
	}
	dsn := s.DSN(t)
	for {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			_ = conn.Close(ctx)
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("PostgreSQL did not accept connections after restart: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
