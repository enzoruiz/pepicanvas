package invitation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

var (
	tokenPattern = regexp.MustCompile(`^pci_[A-Za-z0-9_-]{43}$`)
	idPattern    = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Service creates, lists, revokes, and accepts invitations.
type Service struct {
	store     Store
	now       func() time.Time
	committed func()
}

// Option configures a Service.
type Option func(*Service)

// WithClock sets the time source used for expiry and operation retention.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithCommitted sets a hook that runs after each commit that wrote an access
// cut, such as CutRelay.Notify. It must not block: the revocation answers
// once the hook returns.
func WithCommitted(hook func()) Option {
	return func(s *Service) { s.committed = hook }
}

// NewService builds the service.
func NewService(store Store, options ...Option) *Service {
	s := &Service{store: store, now: time.Now}
	for _, option := range options {
		option(s)
	}
	return s
}

// Create stores the hash of a client-generated token as an invitation to the
// owner's tenant that expires Lifetime later. A retry under the same
// operation ID returns the first answer while it is retained; the token is
// never returned.
func (s *Service) Create(ctx context.Context, request CreateRequest) (Invitation, error) {
	tokenHash, ok := hashToken(request.Token)
	if !ok || !access.ValidOperationID(request.OperationID) {
		return Invitation{}, ErrInvalid
	}
	fingerprint := operationFingerprint(OperationCreate, request.OwnerID, tokenHash[:])

	result, err := s.create(ctx, request, tokenHash, fingerprint)
	if errors.Is(err, ErrOperationRetained) || errors.Is(err, ErrTokenTaken) {
		// A concurrent creation committed the operation ID or the token
		// first: replay it, or reject a different payload.
		result, err = s.create(ctx, request, tokenHash, fingerprint)
		if errors.Is(err, ErrOperationRetained) || errors.Is(err, ErrTokenTaken) {
			return Invitation{}, ErrOperationConflict
		}
	}
	return result, err
}

func (s *Service) create(ctx context.Context, request CreateRequest, tokenHash, fingerprint [32]byte) (Invitation, error) {
	id, err := randomHex(16)
	if err != nil {
		return Invitation{}, err
	}
	now := s.now()
	notBefore := now.Add(-OperationRetention)

	var result Invitation
	err = s.store.InTx(ctx, func(tx Tx) error {
		tenant, err := lockAdministeredTenant(ctx, tx, request.OwnerID, access.ActionAdminister)
		if err != nil {
			return err
		}
		record, found, err := tx.Operation(ctx, request.OperationID, notBefore)
		if err != nil {
			return fmt.Errorf("invitation: find operation: %w", err)
		}
		if found {
			if subtle.ConstantTimeCompare(record.Fingerprint[:], fingerprint[:]) != 1 {
				return ErrOperationConflict
			}
			result = record.Invitation
			return nil
		}

		stored := Stored{
			ID:        id,
			TenantID:  tenant.ID,
			TokenHash: tokenHash,
			CreatedBy: request.OwnerID,
			CreatedAt: now,
			ExpiresAt: now.Add(Lifetime),
		}
		if err := tx.InsertInvitation(ctx, stored); err != nil {
			if errors.Is(err, ErrTokenTaken) {
				return err
			}
			return fmt.Errorf("invitation: insert invitation: %w", err)
		}
		result = stored.Invitation()
		return s.record(ctx, tx, Record{
			OperationID:   request.OperationID,
			Kind:          OperationCreate,
			OwnerID:       request.OwnerID,
			Fingerprint:   fingerprint,
			Invitation:    result,
			CorrelationID: request.CorrelationID,
			AcceptedAt:    now,
		}, notBefore)
	})
	if err != nil {
		return Invitation{}, err
	}
	return result, nil
}

// List returns the owner's pending invitations, oldest first.
func (s *Service) List(ctx context.Context, owner access.AccountID) ([]Invitation, error) {
	now := s.now()
	var result []Invitation
	err := s.store.InTx(ctx, func(tx Tx) error {
		tenant, err := lockAdministeredTenant(ctx, tx, owner, access.ActionView)
		if err != nil {
			return err
		}
		pending, err := tx.PendingInvitations(ctx, tenant.ID, now)
		if err != nil {
			return fmt.Errorf("invitation: list invitations: %w", err)
		}
		result = make([]Invitation, 0, len(pending))
		for _, stored := range pending {
			result = append(result, stored.Invitation())
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Revoke revokes a pending or expired invitation of the owner's tenant once.
// An already revoked invitation answers revoked without a write; an accepted
// one answers accepted and keeps its membership. A retry under the same
// operation ID returns the first answer while it is retained.
func (s *Service) Revoke(ctx context.Context, request RevokeRequest) (Revocation, error) {
	if !idPattern.MatchString(request.InvitationID) || !access.ValidOperationID(request.OperationID) {
		return Revocation{}, ErrInvalid
	}
	fingerprint := operationFingerprint(OperationRevoke, request.OwnerID, []byte(request.InvitationID))

	result, err := s.revoke(ctx, request, fingerprint)
	if errors.Is(err, ErrOperationRetained) {
		result, err = s.revoke(ctx, request, fingerprint)
		if errors.Is(err, ErrOperationRetained) {
			return Revocation{}, ErrOperationConflict
		}
	}
	return result, err
}

func (s *Service) revoke(ctx context.Context, request RevokeRequest, fingerprint [32]byte) (Revocation, error) {
	now := s.now()
	notBefore := now.Add(-OperationRetention)

	var result Revocation
	err := s.store.InTx(ctx, func(tx Tx) error {
		tenant, err := lockAdministeredTenant(ctx, tx, request.OwnerID, access.ActionAdminister)
		if err != nil {
			return err
		}
		record, found, err := tx.Operation(ctx, request.OperationID, notBefore)
		if err != nil {
			return fmt.Errorf("invitation: find operation: %w", err)
		}
		if found {
			if subtle.ConstantTimeCompare(record.Fingerprint[:], fingerprint[:]) != 1 {
				return ErrOperationConflict
			}
			result = record.Revocation
			return nil
		}

		// The row lock orders this revocation against acceptances and other
		// revocations: whichever commits first is what this one sees.
		stored, found, err := tx.LockInvitation(ctx, tenant.ID, request.InvitationID)
		if err != nil {
			return fmt.Errorf("invitation: lock invitation: %w", err)
		}
		if !found {
			return ErrNotFound
		}
		result = Revocation{InvitationID: stored.ID, Status: access.InvitationRevoked}
		switch {
		case !stored.AcceptedAt.IsZero():
			result.Status = access.InvitationAccepted
		case stored.RevokedAt.IsZero():
			if err := tx.RevokeInvitation(ctx, stored.ID, now); err != nil {
				return fmt.Errorf("invitation: revoke invitation: %w", err)
			}
		}
		return s.record(ctx, tx, Record{
			OperationID:   request.OperationID,
			Kind:          OperationRevoke,
			OwnerID:       request.OwnerID,
			Fingerprint:   fingerprint,
			Revocation:    result,
			CorrelationID: request.CorrelationID,
			AcceptedAt:    now,
		}, notBefore)
	})
	if err != nil {
		return Revocation{}, err
	}
	return result, nil
}

// Accept turns the pending invitation of request.Token into a moderator
// membership of its tenant and consumes the invitation. Every unusable token
// gets the one ErrUnavailable, so the answer reveals nothing about the
// invitation or its tenant. A retry under the same operation ID returns the
// first answer while it is retained, even after the link was consumed.
func (s *Service) Accept(ctx context.Context, request AcceptRequest) (Acceptance, error) {
	if !access.ValidOperationID(request.OperationID) {
		return Acceptance{}, ErrInvalid
	}
	tokenHash, ok := hashToken(request.Token)
	if !ok {
		return Acceptance{}, ErrUnavailable
	}
	fingerprint := operationFingerprint(OperationAccept, request.AccountID, tokenHash[:])

	result, err := s.accept(ctx, request, tokenHash, fingerprint)
	if errors.Is(err, ErrOperationRetained) || errors.Is(err, ErrMemberTaken) {
		// A concurrent acceptance committed the operation ID or a membership
		// of this account first: replay it, or reject the second effect.
		result, err = s.accept(ctx, request, tokenHash, fingerprint)
		if errors.Is(err, ErrOperationRetained) {
			return Acceptance{}, ErrOperationConflict
		}
		if errors.Is(err, ErrMemberTaken) {
			return Acceptance{}, ErrUnavailable
		}
	}
	return result, err
}

func (s *Service) accept(ctx context.Context, request AcceptRequest, tokenHash, fingerprint [32]byte) (Acceptance, error) {
	now := s.now()
	notBefore := now.Add(-OperationRetention)

	var result Acceptance
	err := s.store.InTx(ctx, func(tx Tx) error {
		// The row lock comes first: a concurrent acceptance of the same
		// link commits before this one reads its operation, so a retry
		// under the same operation ID replays it.
		stored, found, err := tx.LockInvitationByToken(ctx, tokenHash)
		if err != nil {
			return fmt.Errorf("invitation: lock invitation: %w", err)
		}
		record, retained, err := tx.Operation(ctx, request.OperationID, notBefore)
		if err != nil {
			return fmt.Errorf("invitation: find operation: %w", err)
		}
		if retained {
			if subtle.ConstantTimeCompare(record.Fingerprint[:], fingerprint[:]) != 1 {
				return ErrOperationConflict
			}
			result = record.Acceptance
			return nil
		}
		if !found || !stored.Pending(now) {
			return ErrUnavailable
		}
		tenant, found, err := tx.LockTenant(ctx, stored.TenantID)
		if err != nil {
			return fmt.Errorf("invitation: lock tenant: %w", err)
		}
		if !found || tenant.Status != access.SubscriptionActive {
			return ErrUnavailable
		}
		member, err := tx.IsMember(ctx, tenant.ID, request.AccountID)
		if err != nil {
			return fmt.Errorf("invitation: read membership: %w", err)
		}
		if member {
			return ErrUnavailable
		}
		if err := tx.AddModerator(ctx, tenant.ID, request.AccountID); err != nil {
			if errors.Is(err, ErrMemberTaken) {
				return err
			}
			return fmt.Errorf("invitation: add moderator: %w", err)
		}
		if err := tx.MarkAccepted(ctx, stored.ID, request.AccountID, now); err != nil {
			return fmt.Errorf("invitation: mark accepted: %w", err)
		}
		result = Acceptance{TenantID: tenant.ID, CanvasID: tenant.CanvasID, InvitationID: stored.ID}
		return s.record(ctx, tx, Record{
			OperationID:   request.OperationID,
			Kind:          OperationAccept,
			OwnerID:       request.AccountID,
			Fingerprint:   fingerprint,
			Acceptance:    result,
			CorrelationID: request.CorrelationID,
			AcceptedAt:    now,
		}, notBefore)
	})
	if err != nil {
		return Acceptance{}, err
	}
	return result, nil
}

// Moderations lists the canvases the account moderates, by tenant ID. Each
// membership passes TE-001 canvas view, so tenants without an active
// subscription are left out. Owner memberships are not listed.
func (s *Service) Moderations(ctx context.Context, account access.AccountID) ([]Moderation, error) {
	var result []Moderation
	err := s.store.InTx(ctx, func(tx Tx) error {
		moderated, err := tx.ModeratedCanvases(ctx, account)
		if err != nil {
			return fmt.Errorf("invitation: list moderated canvases: %w", err)
		}
		result = make([]Moderation, 0, len(moderated))
		for _, canvas := range moderated {
			membership := canvas.Membership
			if membership.Role != access.RoleModerator ||
				access.Check(access.CanvasRequest(membership, true, membership.TenantID, access.ActionView)) != nil {
				continue
			}
			result = append(result, Moderation{TenantID: membership.TenantID, CanvasID: canvas.CanvasID, Role: membership.Role})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(result, func(a, b Moderation) int { return strings.Compare(a.TenantID, b.TenantID) })
	return result, nil
}

// Members lists the moderators of the owner's tenant with their email and
// latest acceptance (US-006 L2). Only the owner of an active tenant sees them.
func (s *Service) Members(ctx context.Context, owner access.AccountID) ([]Member, error) {
	var result []Member
	err := s.store.InTx(ctx, func(tx Tx) error {
		tenant, found, err := tx.LockOwnedTenant(ctx, owner)
		if err != nil {
			return fmt.Errorf("invitation: lock tenant: %w", err)
		}
		if err := access.Check(membershipRequest(found, tenant, access.ActionView, "")); err != nil {
			return err
		}
		if result, err = tx.Members(ctx, tenant.ID); err != nil {
			return fmt.Errorf("invitation: list members: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result == nil {
		result = []Member{}
	}
	return result, nil
}

// RevokeMember removes the moderator membership of request.AccountID from
// the owner's tenant. In one transaction it increments the tenant's access
// version and stores one pending revocation cut, so the cut is effective at
// the commit; live connections are cut by the relay the WithCommitted hook
// wakes. Content is never touched. An unknown account, a non-member, and the
// owner are ErrNotFound. A retry under the same operation ID returns the
// first answer while it is retained.
func (s *Service) RevokeMember(ctx context.Context, request RevokeMemberRequest) (MemberRevocation, error) {
	if !idPattern.MatchString(string(request.AccountID)) || !access.ValidOperationID(request.OperationID) ||
		len(request.CorrelationID) > MaxCorrelationBytes {
		return MemberRevocation{}, ErrInvalid
	}
	fingerprint := operationFingerprint(OperationRevokeMember, request.OwnerID, []byte(request.AccountID))

	result, err := s.revokeMember(ctx, request, fingerprint)
	if errors.Is(err, ErrOperationRetained) {
		// A concurrent revocation committed the same operation ID first:
		// replay it, or reject a different payload.
		result, err = s.revokeMember(ctx, request, fingerprint)
		if errors.Is(err, ErrOperationRetained) {
			return MemberRevocation{}, ErrOperationConflict
		}
	}
	return result, err
}

func (s *Service) revokeMember(ctx context.Context, request RevokeMemberRequest, fingerprint [32]byte) (MemberRevocation, error) {
	now := s.now()
	notBefore := now.Add(-OperationRetention)

	var result MemberRevocation
	wroteCut := false
	err := s.store.InTx(ctx, func(tx Tx) error {
		wroteCut = false
		// The tenant row lock conflicts with the share lock of every
		// mutation of the tenant, so a mutation commits wholly before or
		// wholly after the revocation.
		tenant, found, err := tx.LockOwnedTenantForUpdate(ctx, request.OwnerID)
		if err != nil {
			return fmt.Errorf("invitation: lock tenant: %w", err)
		}
		if err := access.Check(membershipRequest(found, tenant, access.ActionAdminister, access.RoleModerator)); err != nil {
			return err
		}
		record, retained, err := tx.MemberRevocation(ctx, request.OperationID, notBefore)
		if err != nil {
			return fmt.Errorf("invitation: find revocation: %w", err)
		}
		if retained {
			if subtle.ConstantTimeCompare(record.Fingerprint[:], fingerprint[:]) != 1 {
				return ErrOperationConflict
			}
			result = record.Result
			return nil
		}

		role, found, err := tx.LockMembership(ctx, tenant.ID, request.AccountID)
		if err != nil {
			return fmt.Errorf("invitation: lock membership: %w", err)
		}
		if !found || role != access.RoleModerator {
			return ErrNotFound
		}
		if err := tx.RemoveModerator(ctx, tenant.ID, request.AccountID); err != nil {
			return fmt.Errorf("invitation: remove moderator: %w", err)
		}
		version, err := tx.IncrementAccessVersion(ctx, tenant.ID)
		if err != nil {
			return fmt.Errorf("invitation: increment access version: %w", err)
		}
		cut := RevocationCut{TenantID: tenant.ID, AccountID: request.AccountID, Version: version, CorrelationID: request.CorrelationID, CreatedAt: now}
		if err := tx.RecordCut(ctx, cut); err != nil {
			return fmt.Errorf("invitation: record cut: %w", err)
		}
		wroteCut = true

		result = MemberRevocation{TenantID: tenant.ID, AccountID: request.AccountID}
		err = tx.RecordMemberRevocation(ctx, MemberRevocationRecord{
			OperationID:   request.OperationID,
			OwnerID:       request.OwnerID,
			Fingerprint:   fingerprint,
			Result:        result,
			Version:       version,
			CorrelationID: request.CorrelationID,
			AcceptedAt:    now,
		}, notBefore)
		if err != nil && !errors.Is(err, ErrOperationRetained) {
			return fmt.Errorf("invitation: record revocation: %w", err)
		}
		return err
	})
	if err != nil {
		return MemberRevocation{}, err
	}
	if wroteCut && s.committed != nil {
		s.committed()
	}
	return result, nil
}

// EditorGrant returns the access facts an editor connection of the account's
// session to the tenant's canvas is authorized with: its membership, owner or
// moderator, read with the tenant's access version. A non-member, a revoked
// moderator, and a member of an inactive tenant get the TE-001 denial.
func (s *Service) EditorGrant(ctx context.Context, account access.AccountID, sessionID, tenantID string) (revocation.Grant, error) {
	var member MemberAccess
	var found bool
	err := s.store.InTx(ctx, func(tx Tx) error {
		var err error
		if member, found, err = tx.MemberAccess(ctx, tenantID, account); err != nil {
			return fmt.Errorf("invitation: read member access: %w", err)
		}
		return nil
	})
	if err != nil {
		return revocation.Grant{}, err
	}
	if err := access.Check(access.CanvasRequest(member.Membership, found, tenantID, access.ActionView)); err != nil {
		return revocation.Grant{}, err
	}
	return revocation.Grant{
		TenantID:  tenantID,
		AccountID: string(account),
		SessionID: sessionID,
		Client:    revocation.ClientEditor,
		Version:   member.Version,
	}, nil
}

// membershipRequest builds the TE-001 request for an account acting on the
// memberships of the tenant it owns, toward members of target role. Like
// invitations, only the owner acts on them, so a non-owner has no actor
// tenant and is denied like any other stranger.
func membershipRequest(owner bool, tenant Tenant, action access.Action, target access.Role) access.Request {
	request := access.InvitationRequest(owner, tenant.ID, tenant.Status, action)
	request.Resource = access.ResourceMembership
	request.TargetRole = target
	return request
}

// lockAdministeredTenant locks the tenant the account owns and checks TE-001
// before any invitation or operation is read, so moderators, strangers, and
// owners of an inactive subscription get the same denial.
func lockAdministeredTenant(ctx context.Context, tx Tx, owner access.AccountID, action access.Action) (Tenant, error) {
	tenant, found, err := tx.LockOwnedTenant(ctx, owner)
	if err != nil {
		return Tenant{}, fmt.Errorf("invitation: lock tenant: %w", err)
	}
	if err := access.Check(access.InvitationRequest(found, tenant.ID, tenant.Status, action)); err != nil {
		return Tenant{}, err
	}
	return tenant, nil
}

func (s *Service) record(ctx context.Context, tx Tx, record Record, purgeBefore time.Time) error {
	err := tx.RecordOperation(ctx, record, purgeBefore)
	if err != nil && !errors.Is(err, ErrOperationRetained) {
		return fmt.Errorf("invitation: record operation: %w", err)
	}
	return err
}

// hashToken returns the SHA-256 of a well-formed invitation token.
func hashToken(token secret.Value) ([32]byte, bool) {
	if token.Kind() != secret.KindInvitationToken || !tokenPattern.MatchString(token.Reveal()) {
		return [32]byte{}, false
	}
	return sha256.Sum256([]byte(token.Reveal())), true
}

func operationFingerprint(kind OperationKind, owner access.AccountID, payload []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(owner))
	h.Write([]byte{0})
	h.Write(payload)
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("invitation: read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
