// Package subscription activates streamers manually (US-002): an operator
// turns an account into the owner of one tenant with exactly one canvas.
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
	// ErrAccountNotFound reports that no account has the requested email.
	ErrAccountNotFound = errors.New("subscription: account not found")
	// ErrOperationConflict reports an operation ID reused for a different
	// activation. A new intent requires a new operation ID.
	ErrOperationConflict = errors.New("subscription: operation id reused with a different activation")
	// ErrOperationRetained is returned by Tx.RecordActivation when another
	// activation already retains the operation ID.
	ErrOperationRetained = errors.New("subscription: operation id retained")
)

// ActivationRetention is how long an activation operation ID replays its
// answer. An operation accepted at T is retained while now < T + ttl.
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

// Tenant is a streamer: one owner account and its only canvas.
type Tenant struct {
	ID       string
	Owner    access.AccountID
	CanvasID string
	Status   access.SubscriptionStatus
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

// Tx is one activation transaction. Its reads hold their locks until commit.
type Tx interface {
	// IsOperator reads the operator grant and holds it until commit.
	IsOperator(ctx context.Context, account access.AccountID) (bool, error)
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
