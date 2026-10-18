// Package revocation applies server-confirmed access cuts to live
// connections: revocation, deactivation, and credential rotation close every
// affected editor and OBS connection within a fixed budget and keep stale
// grants from reconnecting.
package revocation

import (
	"errors"
	"fmt"
)

// MaxIdentifierBytes bounds every identifier in a grant or cut.
const MaxIdentifierBytes = 128

var (
	// ErrInvalidCut reports a cut rejected before it has any effect.
	ErrInvalidCut = errors.New("revocation: invalid cut")
	// ErrUnconfirmedCut reports a cut without a server confirmation. It wraps
	// ErrInvalidCut.
	ErrUnconfirmedCut = fmt.Errorf("%w: unconfirmed", ErrInvalidCut)
	// ErrInvalidGrant reports a registration rejected before it has any effect.
	ErrInvalidGrant = errors.New("revocation: invalid grant")
)

// Client identifies the kind of connection a grant authorizes.
type Client string

const (
	ClientEditor Client = "editor"
	ClientOBS    Client = "obs"
)

// Grant holds the access facts a connection was authorized with. Version is
// the confirmed version at which those facts were read; zero is unconfirmed.
// Editor grants name an account and session; OBS grants name a credential.
type Grant struct {
	TenantID     string
	AccountID    string
	SessionID    string
	CredentialID string
	Client       Client
	Version      uint64
}

// Type identifies why access is cut.
type Type string

const (
	// TypeRevocation removes an account's membership or a session.
	TypeRevocation Type = "revocation"
	// TypeDeactivation disables a whole tenant's subscription.
	TypeDeactivation Type = "deactivation"
	// TypeRotation replaces an OBS credential.
	TypeRotation Type = "rotation"
)

// TargetKind identifies which grants a cut affects.
type TargetKind string

const (
	TargetTenant     TargetKind = "tenant"
	TargetAccount    TargetKind = "account"
	TargetSession    TargetKind = "session"
	TargetCredential TargetKind = "credential"
)

// Target names the grants a cut affects inside one tenant. A tenant target
// has no ID.
type Target struct {
	Kind     TargetKind
	TenantID string
	ID       string
}

// Cut is one server-confirmed access change. Version is the confirmed
// version of the change; it affects only grants read before it.
type Cut struct {
	OperationID   string
	Type          Type
	Target        Target
	Version       uint64
	CorrelationID string
}

// Reason tells the transport why its connection closes.
type Reason string

const (
	ReasonRevoked     Reason = "access_revoked"
	ReasonDeactivated Reason = "subscription_inactive"
	ReasonRotated     Reason = "credential_rotated"
	// ReasonShutdown closes connections on server shutdown; the client may
	// reconnect with its current grant.
	ReasonShutdown Reason = "server_shutdown"
)

// Retryable reports whether the client may reconnect without new access.
func (r Reason) Retryable() bool { return r == ReasonShutdown }

func (c Cut) validate() error {
	if c.Version == 0 {
		return ErrUnconfirmedCut
	}
	if !validIdentifier(c.OperationID) || !validIdentifier(c.Target.TenantID) {
		return fmt.Errorf("%w: malformed operation or tenant identifier", ErrInvalidCut)
	}
	if c.CorrelationID != "" && !validIdentifier(c.CorrelationID) {
		return fmt.Errorf("%w: malformed correlation identifier", ErrInvalidCut)
	}
	var kinds []TargetKind
	switch c.Type {
	case TypeRevocation:
		kinds = []TargetKind{TargetAccount, TargetSession}
	case TypeDeactivation:
		kinds = []TargetKind{TargetTenant}
	case TypeRotation:
		kinds = []TargetKind{TargetCredential}
	default:
		return fmt.Errorf("%w: unknown type", ErrInvalidCut)
	}
	allowed := false
	for _, kind := range kinds {
		allowed = allowed || kind == c.Target.Kind
	}
	if !allowed {
		return fmt.Errorf("%w: %s cannot target %s", ErrInvalidCut, c.Type, c.Target.Kind)
	}
	if c.Target.Kind == TargetTenant {
		if c.Target.ID != "" {
			return fmt.Errorf("%w: tenant target carries an id", ErrInvalidCut)
		}
	} else if !validIdentifier(c.Target.ID) {
		return fmt.Errorf("%w: malformed target identifier", ErrInvalidCut)
	}
	return nil
}

func (c Cut) reason() Reason {
	switch c.Type {
	case TypeDeactivation:
		return ReasonDeactivated
	case TypeRotation:
		return ReasonRotated
	default:
		return ReasonRevoked
	}
}

func (g Grant) validate() error {
	if g.Version == 0 {
		return fmt.Errorf("%w: unconfirmed", ErrInvalidGrant)
	}
	required := []string{g.TenantID}
	switch g.Client {
	case ClientEditor:
		required = append(required, g.AccountID, g.SessionID)
	case ClientOBS:
		required = append(required, g.CredentialID)
	default:
		return fmt.Errorf("%w: unknown client", ErrInvalidGrant)
	}
	for _, value := range required {
		if !validIdentifier(value) {
			return fmt.Errorf("%w: missing or malformed identifier", ErrInvalidGrant)
		}
	}
	for _, value := range []string{g.AccountID, g.SessionID, g.CredentialID} {
		if value != "" && !validIdentifier(value) {
			return fmt.Errorf("%w: malformed identifier", ErrInvalidGrant)
		}
	}
	return nil
}

// targets lists every target that can cut this grant.
func (g Grant) targets() []Target {
	targets := []Target{{Kind: TargetTenant, TenantID: g.TenantID}}
	for kind, id := range map[TargetKind]string{
		TargetAccount:    g.AccountID,
		TargetSession:    g.SessionID,
		TargetCredential: g.CredentialID,
	} {
		if id != "" {
			targets = append(targets, Target{Kind: kind, TenantID: g.TenantID, ID: id})
		}
	}
	return targets
}

func (t Target) matches(g Grant) bool {
	for _, candidate := range g.targets() {
		if candidate == t {
			return true
		}
	}
	return false
}

// validIdentifier accepts nonempty bounded printable ASCII without spaces.
func validIdentifier(value string) bool {
	if value == "" || len(value) > MaxIdentifierBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return false
		}
	}
	return true
}
