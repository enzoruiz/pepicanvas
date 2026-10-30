package invitation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

func TestEditorGrantNamesTheMemberAtTheTenantsAccessVersion(t *testing.T) {
	service, _, _ := fixture(t)
	for _, tc := range []struct {
		account access.AccountID
		tenant  string
	}{{ownerA, tenantA}, {moderatorA, tenantA}, {ownerB, tenantB}} {
		got, err := service.EditorGrant(context.Background(), tc.account, "session-1", tc.tenant)
		want := revocation.Grant{TenantID: tc.tenant, AccountID: string(tc.account), SessionID: "session-1", Client: revocation.ClientEditor, Version: 1}
		if err != nil || got != want {
			t.Fatalf("EditorGrant(%s in %s) = %+v, %v; want %+v", tc.account, tc.tenant, got, err, want)
		}
	}
}

func TestEditorGrantDeniesNonMembersRevokedModeratorsAndInactiveTenants(t *testing.T) {
	service, store, _, _ := memberFixture(t)
	if _, err := service.RevokeMember(context.Background(), revokeMember(revokeOpID, moderatorA)); err != nil {
		t.Fatal(err)
	}
	store.AddMember(tenantC, guest, access.RoleModerator)
	before := revocationState(store)
	for _, tc := range []struct {
		name    string
		account access.AccountID
		tenant  string
	}{
		{"revoked moderator", moderatorA, tenantA},
		{"stranger", stranger, tenantA},
		{"owner of another tenant", ownerB, tenantA},
		{"unknown account", unknownActor, tenantA},
		{"unknown tenant", ownerA, unknownID},
		{"owner of an inactive tenant", inactiveOwner, tenantC},
		{"moderator of an inactive tenant", guest, tenantC},
	} {
		got, err := service.EditorGrant(context.Background(), tc.account, "session-1", tc.tenant)
		if !errors.Is(err, access.ErrAccessDenied) || got != (revocation.Grant{}) {
			t.Fatalf("%s: EditorGrant = %+v, %v; want ErrAccessDenied and no grant", tc.name, got, err)
		}
	}
	if after := revocationState(store); after != before {
		t.Fatalf("EditorGrant wrote: %s, want %s", after, before)
	}
	// The owner's grant carries the version the revocation set.
	if got, err := service.EditorGrant(context.Background(), ownerA, "session-1", tenantA); err != nil || got.Version != 2 {
		t.Fatalf("owner grant = %+v, %v; want version 2", got, err)
	}
}

func TestEditorGrantReportsAStoreFailure(t *testing.T) {
	service, store, _ := fixture(t)
	injected := errors.New("store down")
	store.FailOnce("MemberAccess", injected)
	if got, err := service.EditorGrant(context.Background(), ownerA, "session-1", tenantA); !errors.Is(err, injected) || got != (revocation.Grant{}) {
		t.Fatalf("EditorGrant = %+v, %v; want the store error and no grant", got, err)
	}
}
