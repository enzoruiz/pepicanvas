package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

func TestMigrateIsIdempotentAndSurvivesRestart(t *testing.T) {
	server := pgtest.Start(t)
	ctx := context.Background()

	pool, err := postgres.Open(ctx, server.DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for range 2 {
		if err := postgres.Migrate(ctx, pool); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
	}
	first := schemaVersion(t, pool)
	if first < 1 {
		t.Fatalf("schema version = %d, want at least 1", first)
	}
	pool.Close()

	server.Restart(t)

	pool, err = postgres.Open(ctx, server.DSN(t))
	if err != nil {
		t.Fatalf("Open after restart: %v", err)
	}
	defer pool.Close()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after restart: %v", err)
	}
	if got := schemaVersion(t, pool); got != first {
		t.Fatalf("schema version after restart = %d, want %d", got, first)
	}
}

func TestOpenRejectsUnreachableServer(t *testing.T) {
	ctx := context.Background()
	_, err := postgres.Open(ctx, "postgres://nobody:decoy-password-7f3a@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err == nil {
		t.Fatal("Open succeeded against an unreachable server")
	}
	if strings.Contains(err.Error(), "decoy-password-7f3a") {
		t.Fatalf("Open error reveals the password: %v", err)
	}
}

func schemaVersion(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var version int64
	if err := pool.QueryRow(context.Background(), "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	return version
}
