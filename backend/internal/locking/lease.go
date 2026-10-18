// Package locking grants exclusive, temporary leases on canvas objects. A
// lease names its holder, is renewed by the holder's heartbeat, and expires
// exactly TTL after the last accepted heartbeat. Every lease belongs to one
// connection session and ends with it, so a reconnection never restores one.
package locking

import (
	"cmp"
	"errors"
	"fmt"
	"time"
)

// MaxIdentifierBytes bounds every identifier in a holder or request.
const MaxIdentifierBytes = 128

var (
	// ErrInvalidConfig reports a manager configuration that cannot fail closed.
	ErrInvalidConfig = errors.New("locking: invalid config")
	// ErrInvalidRequest reports a holder or request rejected before it has
	// any effect.
	ErrInvalidRequest = errors.New("locking: invalid request")
	// ErrDetached reports work on a session whose connection ended. Its
	// leases are already released.
	ErrDetached = errors.New("locking: session detached")
	// ErrNotHolder reports a release of a lease the session does not hold.
	ErrNotHolder = errors.New("locking: not the lease holder")
)

// Key identifies one object of one canvas inside one tenant. Equal canvas
// and object IDs in different tenants are distinct leases.
type Key struct {
	TenantID string
	CanvasID string
	ObjectID string
}

// Holder identifies who holds a lease: the account other clients see as the
// owner, and the connection session the lease belongs to.
type Holder struct {
	TenantID  string
	AccountID string
	SessionID string
}

// Lease is the current state of one held lease.
type Lease struct {
	Key     Key
	Holder  Holder
	Expires time.Time
}

// Reason explains why a lease ended.
type Reason string

const (
	// ReasonFinished ends a lease after the holder completed its edit.
	ReasonFinished Reason = "finished"
	// ReasonCancelled ends a lease after the holder abandoned its edit.
	ReasonCancelled Reason = "cancelled"
	// ReasonExpired ends a lease that saw no accepted heartbeat for TTL.
	ReasonExpired Reason = "expired"
	// ReasonUnauthorized ends a lease whose holder lost access to it.
	ReasonUnauthorized Reason = "unauthorized"
	// ReasonDetached ends every lease of a session whose connection ended.
	ReasonDetached Reason = "detached"
	// ReasonShutdown ends every lease when the manager closes.
	ReasonShutdown Reason = "shutdown"
)

// Request asks to acquire or release the lease on one object of the
// session's tenant. Reason is required to release and must be
// ReasonFinished or ReasonCancelled.
type Request struct {
	OperationID string
	CanvasID    string
	ObjectID    string
	Reason      Reason
}

// Outcome reports what an acquire or release did.
type Outcome string

const (
	OutcomeGranted  Outcome = "granted"
	OutcomeDenied   Outcome = "denied"
	OutcomeReleased Outcome = "released"
)

// Result is the answer to one acquire or release. Lease names the holder:
// the caller when granted or released, the current owner when denied.
type Result struct {
	OperationID string
	Outcome     Outcome
	Reason      Reason
	Lease       Lease
}

// Loss reports a lease the session lost without releasing it.
type Loss struct {
	Key    Key
	Reason Reason
}

// Renewal is the answer to one heartbeat.
type Renewal struct {
	Renewed []Lease
	Lost    []Loss
}

func (h Holder) validate() error {
	return validIdentifiers(map[string]string{"tenant": h.TenantID, "account": h.AccountID, "session": h.SessionID})
}

func (r Request) validate() error {
	return validIdentifiers(map[string]string{"operation": r.OperationID, "canvas": r.CanvasID, "object": r.ObjectID})
}

func (r Request) validateRelease() error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.Reason != ReasonFinished && r.Reason != ReasonCancelled {
		return fmt.Errorf("%w: release reason must be finished or cancelled", ErrInvalidRequest)
	}
	return nil
}

func validIdentifiers(values map[string]string) error {
	for name, value := range values {
		if value == "" {
			return fmt.Errorf("%w: missing %s identifier", ErrInvalidRequest, name)
		}
		if !validIdentifier(value) {
			return fmt.Errorf("%w: malformed %s identifier", ErrInvalidRequest, name)
		}
	}
	return nil
}

// validIdentifier accepts bounded printable ASCII without spaces.
func validIdentifier(value string) bool {
	if len(value) > MaxIdentifierBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return false
		}
	}
	return true
}

func compareKeys(a, b Key) int {
	return cmp.Or(cmp.Compare(a.TenantID, b.TenantID), cmp.Compare(a.CanvasID, b.CanvasID), cmp.Compare(a.ObjectID, b.ObjectID))
}
