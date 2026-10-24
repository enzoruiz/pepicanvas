package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

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

// deactivationSchema reports what 00008 adds and the rows it would lose.
type deactivationSchema struct {
	version       int64
	versionColumn bool
	records       int
	cuts          int
}

func readDeactivationSchema(t *testing.T, pool *pgxpool.Pool) deactivationSchema {
	t.Helper()
	ctx := context.Background()
	var s deactivationSchema
	var recordsTable, cutsTable bool
	s.version = schemaVersion(t, pool)
	err := pool.QueryRow(ctx, `SELECT to_regclass('subscription_deactivations') IS NOT NULL,
		to_regclass('access_cuts') IS NOT NULL,
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'tenants' AND column_name = 'access_version')`).
		Scan(&recordsTable, &cutsTable, &s.versionColumn)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if recordsTable != s.versionColumn || cutsTable != s.versionColumn {
		t.Fatalf("00008 is partly applied: records %v, cuts %v, version column %v", recordsTable, cutsTable, s.versionColumn)
	}
	if s.versionColumn {
		if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM subscription_deactivations), (SELECT count(*) FROM access_cuts)").Scan(&s.records, &s.cuts); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// invitationSchema reports what 00009 adds and the invitations it would lose.
type invitationSchema struct {
	version          int64
	invitationsTable bool
	operationsTable  bool
	invitations      int
}

func readInvitationSchema(t *testing.T, pool *pgxpool.Pool) invitationSchema {
	t.Helper()
	s := invitationSchema{version: schemaVersion(t, pool)}
	err := pool.QueryRow(context.Background(), `SELECT to_regclass('invitations') IS NOT NULL, to_regclass('invitation_operations') IS NOT NULL`).
		Scan(&s.invitationsTable, &s.operationsTable)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if s.invitationsTable {
		s.invitations = count(t, pool, "SELECT count(*) FROM invitations")
	}
	return s
}

func activatedRows(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var rows string
	if err := pool.QueryRow(context.Background(), `SELECT concat_ws('|',
		(SELECT string_agg(tenant_id || ':' || subscription_status || ':' || owner_account_id || ':' || canvas_id, ',' ORDER BY tenant_id) FROM tenants),
		(SELECT string_agg(account_id, ',' ORDER BY account_id) FROM operators))`).Scan(&rows); err != nil {
		t.Fatalf("read activated rows: %v", err)
	}
	return rows
}

func TestDeactivationRollbackRefusesToLoseRecordsAndCuts(t *testing.T) {
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
	if _, err := provider.DownTo(ctx, 8); err != nil {
		t.Fatalf("DownTo(8): %v", err)
	}

	for _, account := range []string{streamerAccount, operatorAccount} {
		exec(t, pool, "INSERT INTO accounts (account_id, email, password_hash, created_at) VALUES ($1, $1 || '@example.com', 'stub', now())", account)
	}
	exec(t, pool, "INSERT INTO operators (account_id, granted_at) VALUES ($1, now())", operatorAccount)
	exec(t, pool, "INSERT INTO tenants (tenant_id, subscription_status, owner_account_id, canvas_id) VALUES ('streamer-tenant', 'inactive', $1, 'streamer-canvas')", streamerAccount)
	if n := count(t, pool, "SELECT count(*) FROM tenants WHERE access_version = 1"); n != 1 {
		t.Fatalf("tenants at the default access version = %d, want 1", n)
	}
	exec(t, pool, "UPDATE tenants SET access_version = 2")
	kept := activatedRows(t, pool)

	refuse := func(name, message string, want deactivationSchema) {
		t.Helper()
		_, err := provider.DownTo(ctx, 7)
		if err == nil || !strings.Contains(err.Error(), message) {
			t.Fatalf("%s: rollback error = %v, want %q", name, err, message)
		}
		if got := readDeactivationSchema(t, pool); got != want {
			t.Fatalf("%s: after the refused rollback %+v, want %+v", name, got, want)
		}
		if got := activatedRows(t, pool); got != kept {
			t.Fatalf("%s: rows = %q, want %q", name, got, kept)
		}
	}
	intact := deactivationSchema{version: 8, versionColumn: true}

	exec(t, pool, `INSERT INTO subscription_deactivations (operation_id, fingerprint, operator_account_id, account_id, tenant_id, canvas_id, deactivated, version, accepted_at)
		VALUES ('op-migration-deactivate', decode(repeat('ab', 32), 'hex'), $1, $2, 'streamer-tenant', 'streamer-canvas', true, 2, now())`, operatorAccount, streamerAccount)
	withRecord := intact
	withRecord.records = 1
	refuse("deactivation record", "00008_deactivation down refused: 1 deactivation record(s)", withRecord)

	exec(t, pool, "INSERT INTO access_cuts (tenant_id, version, created_at) VALUES ('streamer-tenant', 2, now())")
	withBoth := withRecord
	withBoth.cuts = 1
	refuse("record and cut", "00008_deactivation down refused: 1 deactivation record(s)", withBoth)
	exec(t, pool, "DELETE FROM subscription_deactivations")
	withCut := intact
	withCut.cuts = 1
	refuse("access cut", "00008_deactivation down refused: 1 access cut(s)", withCut)

	// Without records and cuts the rollback runs and keeps the activated rows.
	exec(t, pool, "DELETE FROM access_cuts")
	if _, err := provider.DownTo(ctx, 7); err != nil {
		t.Fatalf("rollback without records and cuts: %v", err)
	}
	if got, want := readDeactivationSchema(t, pool), (deactivationSchema{version: 7}); got != want {
		t.Fatalf("after the rollback %+v, want %+v", got, want)
	}
	if got := activatedRows(t, pool); got != kept {
		t.Fatalf("rows after the rollback = %q, want %q", got, kept)
	}

	// Migrating up again restarts every tenant at access version 1.
	if _, err := provider.UpTo(ctx, 8); err != nil {
		t.Fatalf("UpTo(8) after the rollback: %v", err)
	}
	if got := readDeactivationSchema(t, pool); got != intact {
		t.Fatalf("after re-applying %+v, want %+v", got, intact)
	}
	if n := count(t, pool, "SELECT count(*) FROM tenants WHERE access_version = 1"); n != 1 {
		t.Fatal("re-applied migration did not default the access version")
	}
	if got := activatedRows(t, pool); got != kept {
		t.Fatalf("rows after re-applying = %q, want %q", got, kept)
	}
	for _, bad := range []string{
		"UPDATE tenants SET access_version = 0",
		"INSERT INTO access_cuts (tenant_id, version, created_at) VALUES ('streamer-tenant', 0, now())",
	} {
		if _, err := pool.Exec(ctx, bad); err == nil {
			t.Fatalf("%s: accepted, want a check violation", bad)
		}
	}
	exec(t, pool, "INSERT INTO access_cuts (tenant_id, version, created_at) VALUES ('streamer-tenant', 2, now())")
	if _, err := pool.Exec(ctx, "INSERT INTO access_cuts (tenant_id, version, created_at) VALUES ('streamer-tenant', 2, now())"); err == nil {
		t.Fatal("a second cut of the same tenant version was accepted")
	}
}

func TestInvitationRollbackRefusesToLoseInvitations(t *testing.T) {
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
	// The test exercises 00009 as the latest applied migration.
	if _, err := provider.DownTo(ctx, 9); err != nil {
		t.Fatalf("DownTo(9): %v", err)
	}

	exec(t, pool, "INSERT INTO accounts (account_id, email, password_hash, created_at) VALUES ($1, $1 || '@example.com', 'stub', now())", streamerAccount)
	exec(t, pool, "INSERT INTO tenants (tenant_id, subscription_status, owner_account_id, canvas_id) VALUES ('invited-tenant', 'active', $1, 'invited-canvas')", streamerAccount)
	exec(t, pool, `INSERT INTO invitations (invitation_id, tenant_id, token_hash, created_by, created_at, expires_at)
		VALUES ('0123456789abcdef0123456789abcdef', 'invited-tenant', sha256('pci_rollback'), $1, now(), now() + interval '168 hours')`, streamerAccount)
	intact := invitationSchema{version: 9, invitationsTable: true, operationsTable: true, invitations: 1}

	_, err = provider.DownTo(ctx, 8)
	if err == nil || !strings.Contains(err.Error(), "00009_invitations down refused: 1 invitation(s)") {
		t.Fatalf("rollback error = %v, want the refusal", err)
	}
	if got := readInvitationSchema(t, pool); got != intact {
		t.Fatalf("after the refused rollback %+v, want %+v", got, intact)
	}

	exec(t, pool, "DELETE FROM invitations")
	if _, err := provider.DownTo(ctx, 8); err != nil {
		t.Fatalf("rollback without invitations: %v", err)
	}
	if got, want := readInvitationSchema(t, pool), (invitationSchema{version: 8}); got != want {
		t.Fatalf("after the rollback %+v, want %+v", got, want)
	}
	if _, err := provider.UpTo(ctx, 9); err != nil {
		t.Fatalf("UpTo(9) after the rollback: %v", err)
	}
	if got, want := readInvitationSchema(t, pool), (invitationSchema{version: 9, invitationsTable: true, operationsTable: true}); got != want {
		t.Fatalf("after re-applying %+v, want %+v", got, want)
	}
}

// acceptanceSchema reports what 00010 adds and the operation records it keeps.
type acceptanceSchema struct {
	version      int64
	canvasColumn bool
	records      string
}

func readAcceptanceSchema(t *testing.T, pool *pgxpool.Pool) acceptanceSchema {
	t.Helper()
	s := acceptanceSchema{version: schemaVersion(t, pool)}
	err := pool.QueryRow(context.Background(), `SELECT
		EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'invitation_operations' AND column_name = 'canvas_id'),
		coalesce((SELECT string_agg(operation_id || ':' || kind, ',' ORDER BY operation_id) FROM invitation_operations), '')`).
		Scan(&s.canvasColumn, &s.records)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	return s
}

func TestInvitationAcceptanceMigrationRollsBackAndReapplies(t *testing.T) {
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
	// The test exercises 00010 as the latest applied migration.
	if _, err := provider.DownTo(ctx, 10); err != nil {
		t.Fatalf("DownTo(10): %v", err)
	}

	for _, account := range []string{streamerAccount, legacyAccount} {
		exec(t, pool, "INSERT INTO accounts (account_id, email, password_hash, created_at) VALUES ($1, $1 || '@example.com', 'stub', now())", account)
	}
	exec(t, pool, "INSERT INTO tenants (tenant_id, subscription_status, owner_account_id, canvas_id) VALUES ('invited-tenant', 'active', $1, 'invited-canvas')", streamerAccount)
	for _, id := range []string{"0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"} {
		exec(t, pool, `INSERT INTO invitations (invitation_id, tenant_id, token_hash, created_by, created_at, expires_at)
			VALUES ($1, 'invited-tenant', sha256(convert_to($1, 'UTF8')), $2, now(), now() + interval '168 hours')`, id, streamerAccount)
	}
	exec(t, pool, `UPDATE invitations SET accepted_at = now(), accepted_by = $1 WHERE invitation_id = 'fedcba9876543210fedcba9876543210'`, legacyAccount)
	exec(t, pool, "INSERT INTO memberships (tenant_id, account_id, role) VALUES ('invited-tenant', $1, 'moderator')", legacyAccount)
	insert := `INSERT INTO invitation_operations (operation_id, kind, owner_account_id, fingerprint, tenant_id, invitation_id, created_at, expires_at, status, canvas_id, accepted_at)
		VALUES ($1, $2, $3, decode(repeat('ab', 32), 'hex'), 'invited-tenant', $4, $5, $6, $7, $8, now())`
	created, expires := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	exec(t, pool, insert, "op-migration-create", "create", streamerAccount, "0123456789abcdef0123456789abcdef", created, expires, nil, nil)
	exec(t, pool, insert, "op-migration-revoke", "revoke", streamerAccount, "fedcba9876543210fedcba9876543210", nil, nil, "accepted", nil)
	exec(t, pool, insert, "op-migration-accept", "accept", legacyAccount, "fedcba9876543210fedcba9876543210", nil, nil, "accepted", "invited-canvas")

	// Each kind keeps exactly its own answer columns.
	for _, bad := range []struct {
		name                     string
		kind                     string
		created, expires, status any
		canvas                   any
	}{
		{"create with a canvas", "create", created, expires, nil, "invited-canvas"},
		{"revoke with a canvas", "revoke", nil, nil, "revoked", "invited-canvas"},
		{"accept without a canvas", "accept", nil, nil, "accepted", nil},
		{"accept with status revoked", "accept", nil, nil, "revoked", "invited-canvas"},
		{"accept without a status", "accept", nil, nil, nil, "invited-canvas"},
		{"accept with dates", "accept", created, expires, "accepted", "invited-canvas"},
		{"accept with an empty canvas", "accept", nil, nil, "accepted", ""},
		{"unknown kind", "delete", nil, nil, "accepted", "invited-canvas"},
	} {
		_, err := pool.Exec(ctx, insert, "op-migration-bad-0001", bad.kind, legacyAccount, "fedcba9876543210fedcba9876543210", bad.created, bad.expires, bad.status, bad.canvas)
		if err == nil || !strings.Contains(err.Error(), "23514") {
			t.Fatalf("%s: error = %v, want check_violation", bad.name, err)
		}
	}
	intact := acceptanceSchema{version: 10, canvasColumn: true, records: "op-migration-accept:accept,op-migration-create:create,op-migration-revoke:revoke"}
	if got := readAcceptanceSchema(t, pool); got != intact {
		t.Fatalf("schema = %+v, want %+v", got, intact)
	}
	kept := activatedRows(t, pool)

	// Rolling back drops only the acceptance replay records; invitations and
	// memberships stay.
	if _, err := provider.DownTo(ctx, 9); err != nil {
		t.Fatalf("DownTo(9): %v", err)
	}
	want := acceptanceSchema{version: 9, records: "op-migration-create:create,op-migration-revoke:revoke"}
	if got := readAcceptanceSchema(t, pool); got != want {
		t.Fatalf("after the rollback %+v, want %+v", got, want)
	}
	if got := activatedRows(t, pool); got != kept {
		t.Fatalf("rows after the rollback = %q, want %q", got, kept)
	}
	if n := count(t, pool, "SELECT count(*) FROM invitations i JOIN memberships m ON m.account_id = i.accepted_by AND m.tenant_id = i.tenant_id"); n != 1 {
		t.Fatal("the rollback lost the accepted invitation or its membership")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO invitation_operations (operation_id, kind, owner_account_id, fingerprint, tenant_id, invitation_id, status, accepted_at)
		VALUES ('op-migration-accept', 'accept', $1, decode(repeat('ab', 32), 'hex'), 'invited-tenant', 'fedcba9876543210fedcba9876543210', 'accepted', now())`, legacyAccount); err == nil {
		t.Fatal("00009 accepted an accept record")
	}

	if _, err := provider.UpTo(ctx, 10); err != nil {
		t.Fatalf("UpTo(10) after the rollback: %v", err)
	}
	want = acceptanceSchema{version: 10, canvasColumn: true, records: "op-migration-create:create,op-migration-revoke:revoke"}
	if got := readAcceptanceSchema(t, pool); got != want {
		t.Fatalf("after re-applying %+v, want %+v", got, want)
	}
	exec(t, pool, insert, "op-migration-accept", "accept", legacyAccount, "fedcba9876543210fedcba9876543210", nil, nil, "accepted", "invited-canvas")
}
