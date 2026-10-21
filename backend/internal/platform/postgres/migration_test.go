package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/pgtest"
	"github.com/enzoruiz/pepicanvas/backend/migrations"
)

const (
	legacyAccount   = "000000000000000000000000000000c1"
	streamerAccount = "000000000000000000000000000000c2"
	operatorAccount = "000000000000000000000000000000c3"
)

func migrationProvider(t *testing.T, pool *pgxpool.Pool) *goose.Provider {
	t.Helper()
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	return provider
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// activationSchema reports what 00004 adds and the rows it would lose.
type activationSchema struct {
	version          int64
	operatorsTable   bool
	activationsTable bool
	ownerColumn      bool
	activated        int
	operators        int
}

func readActivationSchema(t *testing.T, pool *pgxpool.Pool) activationSchema {
	t.Helper()
	ctx := context.Background()
	var s activationSchema
	s.version = schemaVersion(t, pool)
	err := pool.QueryRow(ctx, `SELECT to_regclass('operators') IS NOT NULL,
		to_regclass('subscription_activations') IS NOT NULL,
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'tenants' AND column_name = 'owner_account_id')`).
		Scan(&s.operatorsTable, &s.activationsTable, &s.ownerColumn)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if s.ownerColumn {
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM tenants WHERE owner_account_id IS NOT NULL").Scan(&s.activated); err != nil {
			t.Fatal(err)
		}
	}
	if s.operatorsTable {
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM operators").Scan(&s.operators); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// legacyRows reads the pre-US-002 rows a rollback must keep.
func legacyRows(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var rows string
	err := pool.QueryRow(context.Background(), `SELECT concat_ws('|',
		(SELECT string_agg(account_id || ':' || email, ',' ORDER BY account_id) FROM accounts),
		(SELECT string_agg(tenant_id || ':' || subscription_status, ',' ORDER BY tenant_id) FROM tenants WHERE tenant_id = 'legacy-tenant'),
		(SELECT string_agg(tenant_id || ':' || account_id || ':' || role, ',' ORDER BY tenant_id) FROM memberships WHERE tenant_id = 'legacy-tenant'))`).Scan(&rows)
	if err != nil {
		t.Fatalf("read legacy rows: %v", err)
	}
	return rows
}

func TestActivationRollbackRefusesToLoseActivationsAndGrants(t *testing.T) {
	ctx := context.Background()
	pool, err := postgres.Open(ctx, pgtest.Start(t).DSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	provider := migrationProvider(t, pool)
	// The test exercises 00004 as the latest applied migration, so later
	// migrations never change what it observes.
	if _, err := provider.DownTo(ctx, 4); err != nil {
		t.Fatalf("DownTo(4): %v", err)
	}

	for _, account := range []string{legacyAccount, streamerAccount, operatorAccount} {
		exec(t, pool, "INSERT INTO accounts (account_id, email, password_hash, created_at) VALUES ($1, $1 || '@example.com', 'stub', now())", account)
	}
	exec(t, pool, "INSERT INTO tenants (tenant_id, subscription_status) VALUES ('legacy-tenant', 'active')")
	exec(t, pool, "INSERT INTO memberships (tenant_id, account_id, role) VALUES ('legacy-tenant', $1, 'owner')", legacyAccount)
	legacy := legacyRows(t, pool)

	refuse := func(name, message string, want activationSchema) {
		t.Helper()
		_, err := provider.DownTo(ctx, 3)
		if err == nil || !strings.Contains(err.Error(), message) {
			t.Fatalf("%s: rollback error = %v, want %q", name, err, message)
		}
		if got := readActivationSchema(t, pool); got != want {
			t.Fatalf("%s: after the refused rollback %+v, want %+v", name, got, want)
		}
		if got := legacyRows(t, pool); got != legacy {
			t.Fatalf("%s: legacy rows = %q, want %q", name, got, legacy)
		}
	}
	intact := activationSchema{version: 4, operatorsTable: true, activationsTable: true, ownerColumn: true}

	// An activated tenant would lose its owner and canvas.
	exec(t, pool, "INSERT INTO tenants (tenant_id, subscription_status, owner_account_id, canvas_id) VALUES ('activated-tenant', 'active', $1, 'activated-canvas')", streamerAccount)
	withTenant := intact
	withTenant.activated = 1
	refuse("activated tenant", "00004_activation down refused: 1 activated tenant(s)", withTenant)

	// An operator grant would be lost too.
	exec(t, pool, "INSERT INTO operators (account_id, granted_at) VALUES ($1, now())", operatorAccount)
	withBoth := withTenant
	withBoth.operators = 1
	refuse("activated tenant and operator", "00004_activation down refused: 1 activated tenant(s)", withBoth)
	exec(t, pool, "DELETE FROM tenants WHERE tenant_id = 'activated-tenant'")
	withGrant := intact
	withGrant.operators = 1
	refuse("operator grant", "00004_activation down refused: 1 operator grant(s)", withGrant)

	// With only legacy rows the rollback runs and keeps them.
	exec(t, pool, "DELETE FROM operators")
	if _, err := provider.DownTo(ctx, 3); err != nil {
		t.Fatalf("rollback with only legacy rows: %v", err)
	}
	if got, want := readActivationSchema(t, pool), (activationSchema{version: 3}); got != want {
		t.Fatalf("after the rollback %+v, want %+v", got, want)
	}
	if got := legacyRows(t, pool); got != legacy {
		t.Fatalf("legacy rows after the rollback = %q, want %q", got, legacy)
	}

	// Migrating up again restores the schema around the same legacy rows.
	if _, err := provider.UpTo(ctx, 4); err != nil {
		t.Fatalf("UpTo(4) after the rollback: %v", err)
	}
	if got := readActivationSchema(t, pool); got != intact {
		t.Fatalf("after re-applying %+v, want %+v", got, intact)
	}
	if got := legacyRows(t, pool); got != legacy {
		t.Fatalf("legacy rows after re-applying = %q, want %q", got, legacy)
	}
}
