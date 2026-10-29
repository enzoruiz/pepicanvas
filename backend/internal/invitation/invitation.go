// Package invitation lets the owner of an active tenant create, list, and
// revoke single-use moderator invitations (US-004), lets an authenticated
// account accept one to become moderator of exactly that tenant's canvas,
// lists the canvases an account moderates (US-005), and lets the owner list
// and revoke the tenant's moderators (US-006).
package invitation

import (
	"context"
	"errors"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/secret"
)

// Errors returned by the invitation service.
var (
	// ErrInvalid reports a request rejected before storage.
	ErrInvalid = errors.New("invitation: invalid request")
	// ErrNotFound reports an invitation ID that is unknown or belongs to
	// another tenant, or a revoked account that is unknown, not a moderator
	// of the owner's tenant, or the owner. The cases are indistinguishable.
	ErrNotFound = errors.New("invitation: not found")
	// ErrUnavailable is the only acceptance failure. It never says whether
	// the token is unknown, revoked, accepted, or expired.
	ErrUnavailable = errors.New("invitation: unavailable")
	// ErrOperationConflict reports an operation ID reused for a different
	// request, or a token already stored under another operation.
	ErrOperationConflict = errors.New("invitation: operation id or token reused with a different request")
	// ErrOperationRetained is returned by Tx.RecordOperation when another
	// operation already retains the operation ID.
	ErrOperationRetained = errors.New("invitation: operation id retained")
	// ErrTokenTaken is returned by Tx.InsertInvitation when an invitation
	// with the same token hash is already stored.
	ErrTokenTaken = errors.New("invitation: token already stored")
	// ErrMemberTaken is returned by Tx.AddModerator when the account already
	// has a membership in the tenant, including by a concurrent commit.
	ErrMemberTaken = errors.New("invitation: membership already stored")
)

const (
	// Lifetime is how long an invitation stays usable. It is expired at
	// exactly CreatedAt + Lifetime.
	Lifetime = 7 * 24 * time.Hour
	// OperationRetention is how long an operation ID replays its answer. An
	// operation accepted at T is retained while now < T + OperationRetention.
	OperationRetention = 10 * time.Minute
)

// CreateRequest is one owner's request to store a client-generated token.
type CreateRequest struct {
	OwnerID     access.AccountID
	OperationID string
	// Token is the secret of kind secret.KindInvitationToken.
	Token secret.Value
	// CorrelationID is the request correlation (CTRL-TRACE-01 primitive), or empty.
	CorrelationID string
}

// RevokeRequest is one owner's request to revoke an invitation.
type RevokeRequest struct {
	OwnerID       access.AccountID
	OperationID   string
	InvitationID  string
	CorrelationID string
}

// AcceptRequest is one account's request to accept the invitation of Token.
type AcceptRequest struct {
	AccountID   access.AccountID
	OperationID string
	// Token is the secret of kind secret.KindInvitationToken.
	Token         secret.Value
	CorrelationID string
}

// RevokeMemberRequest is one owner's request to revoke the moderator
// membership of AccountID in the owner's tenant.
type RevokeMemberRequest struct {
	OwnerID       access.AccountID
	OperationID   string
	AccountID     access.AccountID
	CorrelationID string
}

// Member is a moderator of the owner's tenant as the owner sees it (US-006
// L2). AcceptedAt is the latest acceptance by the account of an invitation of
// the tenant, or the zero time when no invitation records one.
type Member struct {
	AccountID  access.AccountID
	Email      string
	AcceptedAt time.Time
}

// MemberRevocation is the answer to a confirmed membership revocation.
type MemberRevocation struct {
	TenantID  string
	AccountID access.AccountID
}

// RevocationCut is the pending access cut of one account in one tenant,
// written in the transaction that removes its membership (CTRL-CONS-01).
// Version is the tenant's access version after the revocation.
type RevocationCut struct {
	TenantID      string
	AccountID     access.AccountID
	Version       uint64
	CorrelationID string
	CreatedAt     time.Time
}

// MemberRevocationRecord is the retained answer of one membership
// revocation. Fingerprint is the SHA-256 of OperationRevokeMember, the owner
// ID, and the revoked account ID.
type MemberRevocationRecord struct {
	OperationID   string
	OwnerID       access.AccountID
	Fingerprint   [32]byte
	Result        MemberRevocation
	Version       uint64
	CorrelationID string
	AcceptedAt    time.Time
}

// Invitation is what the owner sees of an invitation. It never carries the token.
type Invitation struct {
	ID        string
	TenantID  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Revocation is the answer to a revocation: InvitationRevoked, or
// InvitationAccepted when an acceptance was confirmed first.
type Revocation struct {
	InvitationID string
	Status       access.InvitationStatus
}

// Acceptance is the answer to a confirmed acceptance: the tenant and canvas
// the account now moderates.
type Acceptance struct {
	TenantID     string
	CanvasID     string
	InvitationID string
}

// Moderation is a canvas the account moderates in an active tenant.
type Moderation struct {
	TenantID string
	CanvasID string
	Role     access.Role
}

// ModeratedCanvas is a moderator membership with its tenant's canvas and
// subscription status, whatever that status is.
type ModeratedCanvas struct {
	Membership access.Membership
	CanvasID   string
}

// Tenant is a streamer tenant and its one canvas.
type Tenant struct {
	ID       string
	Owner    access.AccountID
	CanvasID string
	Status   access.SubscriptionStatus
}

// Stored is an invitation row. Zero times mean the event did not happen.
type Stored struct {
	ID         string
	TenantID   string
	TokenHash  [32]byte
	CreatedBy  access.AccountID
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  time.Time
	AcceptedAt time.Time
	AcceptedBy access.AccountID
}

// Pending reports whether the invitation is neither revoked nor accepted and
// not yet expired at now.
func (s Stored) Pending(now time.Time) bool {
	return s.RevokedAt.IsZero() && s.AcceptedAt.IsZero() && now.Before(s.ExpiresAt)
}

// Invitation returns the owner's view of the row.
func (s Stored) Invitation() Invitation {
	return Invitation{ID: s.ID, TenantID: s.TenantID, CreatedAt: s.CreatedAt, ExpiresAt: s.ExpiresAt}
}

// OperationKind names the action an operation record retains.
type OperationKind string

const (
	OperationCreate OperationKind = "create"
	OperationRevoke OperationKind = "revoke"
	OperationAccept OperationKind = "accept"
	// OperationRevokeMember names membership revocations. Their records are
	// MemberRevocationRecord, kept apart from Record.
	OperationRevokeMember OperationKind = "revoke-member"
)

// Record is the retained answer of one operation. OwnerID is the acting
// account: the owner for create and revoke, the accepting account for
// accept. Fingerprint is the SHA-256 of the kind, that account ID, and the
// payload (the token hash or the invitation ID). Invitation holds a create
// answer, Revocation a revoke one, and Acceptance an accept one.
type Record struct {
	OperationID   string
	Kind          OperationKind
	OwnerID       access.AccountID
	Fingerprint   [32]byte
	Invitation    Invitation
	Revocation    Revocation
	Acceptance    Acceptance
	CorrelationID string
	AcceptedAt    time.Time
}

// Tx is one invitation transaction. Its locks are held until commit.
type Tx interface {
	// LockOwnedTenant reads the tenant the account owns FOR SHARE.
	LockOwnedTenant(ctx context.Context, owner access.AccountID) (Tenant, bool, error)
	// LockTenant reads a tenant FOR SHARE.
	LockTenant(ctx context.Context, tenantID string) (Tenant, bool, error)
	// Operation returns the record of operationID accepted after notBefore.
	Operation(ctx context.Context, operationID string, notBefore time.Time) (Record, bool, error)
	// InsertInvitation stores a new invitation. It returns ErrTokenTaken when
	// the token hash is already stored, including by a concurrent commit.
	InsertInvitation(ctx context.Context, invitation Stored) error
	// PendingInvitations returns the tenant's invitations pending at now,
	// oldest first.
	PendingInvitations(ctx context.Context, tenantID string, now time.Time) ([]Stored, error)
	// LockInvitation reads the tenant's invitation FOR UPDATE. An invitation
	// of another tenant is not found.
	LockInvitation(ctx context.Context, tenantID, invitationID string) (Stored, bool, error)
	// LockInvitationByToken reads the invitation of a token hash FOR UPDATE.
	LockInvitationByToken(ctx context.Context, tokenHash [32]byte) (Stored, bool, error)
	// RevokeInvitation sets the invitation's revoked_at.
	RevokeInvitation(ctx context.Context, invitationID string, at time.Time) error
	// IsMember reports whether the account has any role in the tenant.
	IsMember(ctx context.Context, tenantID string, account access.AccountID) (bool, error)
	// AddModerator stores a moderator membership. It returns ErrMemberTaken
	// when the account already has one in the tenant, including by a
	// concurrent commit.
	AddModerator(ctx context.Context, tenantID string, account access.AccountID) error
	// MarkAccepted sets the invitation's accepted_at and accepted_by.
	MarkAccepted(ctx context.Context, invitationID string, account access.AccountID, at time.Time) error
	// RecordOperation drops records accepted at or before purgeBefore and
	// retains record. It returns ErrOperationRetained for a retained ID.
	RecordOperation(ctx context.Context, record Record, purgeBefore time.Time) error
	// ModeratedCanvases returns the account's moderator memberships with
	// their tenant's canvas and status, by tenant ID.
	ModeratedCanvases(ctx context.Context, account access.AccountID) ([]ModeratedCanvas, error)
	// LockOwnedTenantForUpdate reads the tenant the account owns FOR UPDATE,
	// so its memberships and access version change one revocation at a time
	// and never under a share-locked mutation.
	LockOwnedTenantForUpdate(ctx context.Context, owner access.AccountID) (Tenant, bool, error)
	// Members returns the tenant's moderators with their email and latest
	// acceptance, oldest acceptance first, then by account ID.
	Members(ctx context.Context, tenantID string) ([]Member, error)
	// MemberRevocation returns the membership revocation record of
	// operationID accepted after notBefore.
	MemberRevocation(ctx context.Context, operationID string, notBefore time.Time) (MemberRevocationRecord, bool, error)
	// LockMembership reads the account's role in the tenant FOR UPDATE.
	LockMembership(ctx context.Context, tenantID string, account access.AccountID) (access.Role, bool, error)
	// RemoveModerator deletes the account's moderator membership.
	RemoveModerator(ctx context.Context, tenantID string, account access.AccountID) error
	// IncrementAccessVersion increments the tenant's access version and
	// returns the new one.
	IncrementAccessVersion(ctx context.Context, tenantID string) (uint64, error)
	// RecordCut stores one pending revocation cut.
	RecordCut(ctx context.Context, cut RevocationCut) error
	// RecordMemberRevocation drops records accepted at or before purgeBefore
	// and retains record. It returns ErrOperationRetained for a retained ID.
	RecordMemberRevocation(ctx context.Context, record MemberRevocationRecord, purgeBefore time.Time) error
}

// Store runs invitation transactions.
type Store interface {
	// InTx runs fn in one transaction and commits only if fn returns nil.
	InTx(ctx context.Context, fn func(Tx) error) error
}
