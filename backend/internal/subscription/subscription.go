// Package subscription activates streamers manually (US-002): an operator
// turns an account into the owner of one tenant with exactly one canvas. An
// operator also deactivates a tenant (US-003), which cuts its access at the
// commit and keeps its canvas for a later reactivation.
package subscription

import (
	"context"
	"errors"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
)

// Errors returned by the activation service.
var (
	// ErrInvalidActivation reports a request rejected before storage.
	ErrInvalidActivation = errors.New("subscription: invalid activation")
	// ErrInvalidDeactivation reports a deactivation rejected before storage.
	ErrInvalidDeactivation = errors.New("subscription: invalid deactivation")
	// ErrAccountNotFound reports that no account has the requested email or,
	// for a deactivation, that the account owns no tenant.
	ErrAccountNotFound = errors.New("subscription: account not found")
	// ErrOperationConflict reports an operation ID reused for a different
	// activation or deactivation. A new intent requires a new operation ID.
	ErrOperationConflict = errors.New("subscription: operation id reused with a different activation")
	// ErrOperationRetained is returned by Tx.RecordActivation and
	// Tx.RecordDeactivation when another operation already retains the ID.
	ErrOperationRetained = errors.New("subscription: operation id retained")
)

// ActivationRetention is how long an activation or deactivation operation ID
// replays its answer. An operation accepted at T is retained while now < T + ttl.
const ActivationRetention = 10 * time.Minute

// ActivateRequest is one operator's request to activate the account of email.
type ActivateRequest struct {
	OperatorID  access.AccountID
	OperationID string
	Email       string
	// CorrelationID is the request correlation (CTRL-TRACE-01 primitive), or empty.
	CorrelationID string
}

// Activation is the answer to an activation. Created reports whether this
// operation created the tenant; a replay returns the same answer.
type Activation struct {
	AccountID access.AccountID
	TenantID  string
	CanvasID  string
	Created   bool
}

// DeactivateRequest is one operator's request to deactivate the tenant owned
// by the account of email.
type DeactivateRequest struct {
	OperatorID  access.AccountID
	OperationID string
	Email       string
	// CorrelationID is the request correlation (CTRL-TRACE-01 primitive), or empty.
	CorrelationID string
}

// Deactivation is the answer to a deactivation. Deactivated reports whether
// this operation cut an active tenant; Version is the tenant's access version
// after it. A replay returns the same answer.
type Deactivation struct {
	AccountID   access.AccountID
	TenantID    string
	CanvasID    string
	Deactivated bool
	Version     uint64
}

// Tenant is a streamer: one owner account and its only canvas. Version is the
// confirmed version of the tenant's access facts; it starts at 1 and every
// deactivation increments it.
type Tenant struct {
	ID       string
	Owner    access.AccountID
	CanvasID string
	Status   access.SubscriptionStatus
	Version  uint64
}

// AccessCut is the durable record of one confirmed access cut, written in the
// transaction that confirms it, so live connections are cut even when the
// process stops before it applies the cut (CTRL-CONS-01).
type AccessCut struct {
	TenantID      string
	Version       uint64
	CorrelationID string
	CreatedAt     time.Time
}

// CutKind names the confirmed change an access cut applies.
type CutKind string

const (
	// CutDeactivation cuts every connection of a tenant (US-003).
	CutDeactivation CutKind = "deactivation"
	// CutRevocation cuts the connections of one account in a tenant whose
	// membership was revoked (US-006).
	CutRevocation CutKind = "revocation"
)

// PendingCut is an access cut not yet applied to the live connections.
// AccountID names the revoked account of a revocation cut and is empty
// otherwise.
type PendingCut struct {
	ID        int64
	Kind      CutKind
	AccountID access.AccountID
	AccessCut
}

// Record is the retained answer of one activation operation. Fingerprint is
// the SHA-256 of the operator ID and the normalized email.
type Record struct {
	OperationID   string
	OperatorID    access.AccountID
	Fingerprint   [32]byte
	Result        Activation
	CorrelationID string
	AcceptedAt    time.Time
}

// DeactivationRecord is the retained answer of one deactivation operation.
// Fingerprint is the SHA-256 of the action, the operator ID, and the
// normalized email.
type DeactivationRecord struct {
	OperationID   string
	OperatorID    access.AccountID
	Fingerprint   [32]byte
	Result        Deactivation
	CorrelationID string
	AcceptedAt    time.Time
}

// Tx is one activation or deactivation transaction. Its reads hold their
// locks until commit.
type Tx interface {
	// IsOperator reads the operator grant and holds it until commit.
	IsOperator(ctx context.Context, account access.AccountID) (bool, error)
	// LockOperation holds the operation ID until commit, so activations and
	// deactivations under one ID run one at a time and each sees the other's
	// record.
	LockOperation(ctx context.Context, operationID string) error
	// Activation returns the record of operationID accepted after notBefore.
	Activation(ctx context.Context, operationID string, notBefore time.Time) (Record, bool, error)
	// LockAccount finds the account of a normalized email and locks it, so
	// activations of one account run one at a time.
	LockAccount(ctx context.Context, email string) (access.AccountID, bool, error)
	TenantOwnedBy(ctx context.Context, account access.AccountID) (Tenant, bool, error)
	// CreateTenant stores an active tenant, its owner membership, and its
	// canvas with the default dimensions.
	CreateTenant(ctx context.Context, tenant Tenant) error
	// ActivateTenant sets the tenant's subscription to active.
	ActivateTenant(ctx context.Context, tenantID string) error
	// RecordActivation drops records accepted at or before purgeBefore and
	// retains record. It returns ErrOperationRetained for a retained ID.
	RecordActivation(ctx context.Context, record Record, purgeBefore time.Time) error
	// Deactivation returns the deactivation record of operationID accepted
	// after notBefore.
	Deactivation(ctx context.Context, operationID string, notBefore time.Time) (DeactivationRecord, bool, error)
	// DeactivateTenant sets an active tenant's subscription to inactive,
	// increments its access version, and returns the new version.
	DeactivateTenant(ctx context.Context, tenantID string) (uint64, error)
	// RecordCut stores cut as pending.
	RecordCut(ctx context.Context, cut AccessCut) error
	// RecordDeactivation drops deactivation records accepted at or before
	// purgeBefore and retains record. It returns ErrOperationRetained for a
	// retained ID.
	RecordDeactivation(ctx context.Context, record DeactivationRecord, purgeBefore time.Time) error
}

// Store runs activation transactions and reads current subscription facts.
type Store interface {
	// InTx runs fn in one transaction and commits only if fn returns nil.
	InTx(ctx context.Context, fn func(Tx) error) error
	// IsOperator reads the current operator grant.
	IsOperator(ctx context.Context, account access.AccountID) (bool, error)
	// OwnedTenant returns the tenant the account owns.
	OwnedTenant(ctx context.Context, account access.AccountID) (Tenant, bool, error)
}

// CutStore reads the access cuts not yet applied to live connections and
// marks them applied.
type CutStore interface {
	// PendingCuts returns up to limit pending cuts, oldest first.
	PendingCuts(ctx context.Context, limit int) ([]PendingCut, error)
	// SettleCut marks the cut applied at at. Settling an applied or unknown
	// cut changes nothing.
	SettleCut(ctx context.Context, id int64, at time.Time) error
}
