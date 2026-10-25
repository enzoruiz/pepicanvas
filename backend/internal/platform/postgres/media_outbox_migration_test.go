package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
)

// mediaOutboxSchema reports what 00011 adds and the data it would lose.
type mediaOutboxSchema struct {
	version         int64
	usageVersion    bool
	nullRevision    bool
	mediaRecords    int
	versionedUsages int
}

func readMediaOutboxSchema(t *testing.T, pool *pgxpool.Pool) mediaOutboxSchema {
	t.Helper()
	s := mediaOutboxSchema{version: schemaVersion(t, pool)}
	err := pool.QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'media_usage' AND column_name = 'version'),
			(SELECT is_nullable = 'YES' FROM information_schema.columns WHERE table_name = 'canvas_outbox' AND column_name = 'revision')`).
		Scan(&s.usageVersion, &s.nullRevision)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	s.mediaRecords = count(t, pool, "SELECT count(*) FROM canvas_outbox WHERE kind = 'media_usage'")
	if s.usageVersion {
		s.versionedUsages = count(t, pool, "SELECT count(*) FROM media_usage WHERE version > 0")
	}
	return s
}

const insertMediaRecord = `INSERT INTO canvas_outbox (tenant_id, canvas_id, revision, operation_id, kind, mutation, object_id, z, created_at, next_attempt_at)
	VALUES ('tenant-a', 'canvas-1', $1, 'op', $2, $3, $4, 0, now(), now())`

func TestMediaOutboxSchemaKeepsUsageRecordsApartFromRevisions(t *testing.T) {
	ctx := context.Background()
	canvases, pool := openStore(t, pgtest.Start(t))
	mustCreate(t, canvases, canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"})
	exec(t, pool, "INSERT INTO media_usage (tenant_id, canvas_id) VALUES ('tenant-a', 'canvas-1')")
	if got, want := readMediaOutboxSchema(t, pool), (mediaOutboxSchema{version: 11, usageVersion: true, nullRevision: true}); got != want {
		t.Fatalf("schema %+v, want %+v", got, want)
	}

	usage := `{"version":1,"quota_bytes":1000000000,"used_bytes":0,"reserved_bytes":5}`
	exec(t, pool, insertMediaRecord, nil, "media_usage", usage, "")
	for name, args := range map[string][]any{
		"usage with a revision":    {1, "media_usage", `{"version":2}`, ""},
		"usage naming an object":   {nil, "media_usage", `{"version":2}`, "0123456789abcdef0123456789abcdef"},
		"duplicated usage version": {nil, "media_usage", usage, ""},
		"mutation without revision": {nil, "set_content",
			`{"object_id":"0123456789abcdef0123456789abcdef","content":"x"}`, "0123456789abcdef0123456789abcdef"},
		"unknown kind": {nil, "media_asset", `{"version":3}`, ""},
	} {
		if _, err := pool.Exec(ctx, insertMediaRecord, args...); err == nil {
			t.Fatalf("%s: accepted, want a violation", name)
		}
	}
	exec(t, pool, "UPDATE media_usage SET version = 1")
	if _, err := pool.Exec(ctx, "UPDATE media_usage SET version = -1"); err == nil {
		t.Fatal("negative usage version accepted")
	}
}

func TestMediaOutboxRollbackRefusesToLoseUsageRecords(t *testing.T) {
	ctx := context.Background()
	canvases, pool := openStore(t, pgtest.Start(t))
	provider := migrationProvider(t, pool)
	mustCreate(t, canvases, canvas.Key{TenantID: "tenant-a", CanvasID: "canvas-1"})
	exec(t, pool, "INSERT INTO media_usage (tenant_id, canvas_id, version) VALUES ('tenant-a', 'canvas-1', 1)")
	exec(t, pool, insertMediaRecord, nil, "media_usage", `{"version":1,"quota_bytes":1000000000,"used_bytes":0,"reserved_bytes":5}`, "")
	intact := mediaOutboxSchema{version: 11, usageVersion: true, nullRevision: true, mediaRecords: 1, versionedUsages: 1}

	_, err := provider.DownTo(ctx, 10)
	if err == nil || !strings.Contains(err.Error(), "00011_media_outbox down refused: 1 media usage record(s)") {
		t.Fatalf("rollback error = %v, want the refusal for records", err)
	}
	if got := readMediaOutboxSchema(t, pool); got != intact {
		t.Fatalf("after the refused rollback %+v, want %+v", got, intact)
	}

	// Without records the rollback proceeds and drops the versions, which
	// consumers hold only in memory.
	exec(t, pool, "DELETE FROM canvas_outbox")
	if _, err := provider.DownTo(ctx, 10); err != nil {
		t.Fatalf("rollback without usage records: %v", err)
	}
	if got, want := readMediaOutboxSchema(t, pool), (mediaOutboxSchema{version: 10}); got != want {
		t.Fatalf("after the rollback %+v, want %+v", got, want)
	}
	if _, err := provider.UpTo(ctx, 11); err != nil {
		t.Fatalf("UpTo(11) after the rollback: %v", err)
	}
	if got, want := readMediaOutboxSchema(t, pool), (mediaOutboxSchema{version: 11, usageVersion: true, nullRevision: true}); got != want {
		t.Fatalf("after re-applying %+v, want %+v", got, want)
	}
}
