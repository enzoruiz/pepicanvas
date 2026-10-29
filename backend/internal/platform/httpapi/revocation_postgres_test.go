package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/invitation"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/httpapi"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres"
)

// US-006 S2, S4, S5, S8, S9: a moderator revocation confirmed by the real
// service after the route checks orders every later effect. A media command
// already reauthorized is rejected at its commit with nothing written, a read
// already loaded is not delivered, and the owner keeps reading the same
// canvas and usage. A command confirmed before the revocation persists.
func TestPostgresMemberRevocationAfterTheRouteCheck(t *testing.T) {
	m := newMediaEnv(t, httpapi.DefaultLimits)
	alice := m.streamer(m.op, "alice1", "alice@example.com")
	modID, mod := m.moderatorOf(alice, "mod001", "mod@example.com")
	var ownerID string
	if err := m.pool.QueryRow(context.Background(), "SELECT owner_account_id FROM tenants WHERE tenant_id = $1", alice.tenantID).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	service := invitation.NewService(postgres.NewInvitationStore(m.pool))
	revocations := 0
	revoke := func() {
		revocations++
		got, err := service.RevokeMember(context.Background(), invitation.RevokeMemberRequest{
			OwnerID: access.AccountID(ownerID), OperationID: fmt.Sprintf("op-pg-revoke-member-%d", revocations), AccountID: modID,
		})
		if err != nil || got.AccountID != modID {
			t.Errorf("RevokeMember = %+v, %v", got, err)
		}
	}
	restore := func() {
		if _, err := m.pool.Exec(context.Background(), "INSERT INTO memberships (tenant_id, account_id, role) VALUES ($1, $2, 'moderator')", alice.tenantID, string(modID)); err != nil {
			t.Fatalf("restore moderator: %v", err)
		}
	}
	// hooked serves with revoke running after the given membership read of
	// alice's tenant: 1 is the route check, 2 the reauthorization.
	hooked := func(afterCalls int) {
		h := &deactivatingMemberships{tenantID: alice.tenantID, afterCalls: afterCalls, deactivate: revoke}
		m.memberships = func(store access.MembershipStore) access.MembershipStore {
			h.MembershipStore = store
			return h
		}
		m.serve(m.canvases)
	}
	getCanvas := func(s signedIn) sent {
		return m.serveRaw(context.Background(), http.MethodGet, alice.path, "", nil, 0, withSession(s.token))
	}

	data := randomBytes(t, 3000)
	up := wantUploaded(t, m.upload(alice.signedIn, alice.path, "op-rev-01", "ref-01", false, data), 3000, 0)
	// Confirmed before the revocation: it persists.
	strictBody[attachJSON](t, m.attach(mod, alice.path, "op-rev-02", up.AssetID, "ref-mod"), http.StatusOK)
	canvasBefore := getCanvas(alice.signedIn)
	usageBefore := m.getUsage(alice.signedIn, alice.path)
	if canvasBefore.status != http.StatusOK || usageBefore.status != http.StatusOK {
		t.Fatalf("owner reads = %d, %d", canvasBefore.status, usageBefore.status)
	}
	operations := m.count("SELECT count(*) FROM media_operations WHERE tenant_id = $1", alice.tenantID)
	outbox := m.count("SELECT count(*) FROM canvas_outbox WHERE tenant_id = $1", alice.tenantID)
	unchanged := func(t *testing.T) {
		t.Helper()
		m.wantUsage(alice, 3000, 0)
		m.wantBucket()
		if n := m.count("SELECT count(*) FROM media_references WHERE tenant_id = $1", alice.tenantID); n != 2 {
			t.Fatalf("references = %d, want 2", n)
		}
		if n := m.count("SELECT count(*) FROM media_operations WHERE tenant_id = $1", alice.tenantID); n != operations {
			t.Fatalf("media operations = %d, want %d", n, operations)
		}
		if n := m.count("SELECT count(*) FROM canvas_outbox WHERE tenant_id = $1", alice.tenantID); n != outbox {
			t.Fatalf("outbox records = %d, want %d", n, outbox)
		}
	}

	t.Run("attach reauthorized before the revocation", func(t *testing.T) {
		hooked(2)
		defer restore()
		wantMediaError(t, m.attach(mod, alice.path, "op-rev-03", up.AssetID, "ref-late"), http.StatusForbidden, "forbidden", "")
		unchanged(t)
	})
	t.Run("detach reauthorized before the revocation", func(t *testing.T) {
		hooked(2)
		defer restore()
		wantMediaError(t, m.detach(mod, alice.path, "op-rev-04", "ref-01"), http.StatusForbidden, "forbidden", "")
		unchanged(t)
	})
	t.Run("upload reauthorized before the revocation", func(t *testing.T) {
		hooked(2)
		defer restore()
		wantMediaError(t, m.upload(mod, alice.path, "op-rev-05", "ref-up", false, randomBytes(t, 500)), http.StatusForbidden, "forbidden", "")
		unchanged(t)
	})
	for name, read := range map[string]func() sent{
		"canvas": func() sent { return getCanvas(mod) },
		"usage":  func() sent { return m.getUsage(mod, alice.path) },
	} {
		t.Run(name+" loaded before the revocation", func(t *testing.T) {
			hooked(1)
			defer restore()
			got := read()
			wantMediaError(t, got, http.StatusForbidden, "forbidden", "")
			if strings.Contains(got.body, alice.canvasID) {
				t.Fatalf("forbidden body names the canvas: %s", got.body)
			}
		})
	}

	// After the last revocation, retries are refused and the owner reads the
	// same content.
	hooked(-1)
	revoke()
	wantMediaError(t, m.attach(mod, alice.path, "op-rev-02", up.AssetID, "ref-mod"), http.StatusForbidden, "forbidden", "")
	wantMediaError(t, getCanvas(mod), http.StatusForbidden, "forbidden", "")
	if got := getCanvas(alice.signedIn); got.status != http.StatusOK || got.body != canvasBefore.body {
		t.Fatalf("owner canvas after the revocation = %d %s, want %s", got.status, got.body, canvasBefore.body)
	}
	if got := m.getUsage(alice.signedIn, alice.path); got.status != http.StatusOK || got.body != usageBefore.body {
		t.Fatalf("owner usage after the revocation = %d %s, want %s", got.status, got.body, usageBefore.body)
	}
	unchanged(t)
	m.wantServed(alice, up.AssetID, data)
}
