package subscription

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/revocation"
)

// Service activates streamers and resolves each streamer's canvas.
type Service struct {
	store     Store
	now       func() time.Time
	committed func()
}

// Option configures a Service.
type Option func(*Service)

// WithClock sets the time source used for operation retention.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithCommitted sets a hook that runs after each commit that wrote an access
// cut, such as CutRelay.Notify. It must not block: the deactivation answers
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

// Activate gives the account of request.Email exactly one active tenant and
// one canvas. Activating an active account returns its existing tenant and
// canvas; activating an inactive one enables them again. A retry under the
// same operation ID returns the first answer while it is retained.
func (s *Service) Activate(ctx context.Context, request ActivateRequest) (Activation, error) {
	email, ok := access.NormalizeEmail(request.Email)
	if !ok || !access.ValidOperationID(request.OperationID) {
		return Activation{}, ErrInvalidActivation
	}
	fingerprint := sha256.Sum256([]byte(string(request.OperatorID) + "\x00" + email))

	result, err := s.activate(ctx, request, email, fingerprint)
	if errors.Is(err, ErrOperationRetained) {
		// A concurrent activation committed the same operation ID first:
		// replay it, or reject a different payload.
		result, err = s.activate(ctx, request, email, fingerprint)
		if errors.Is(err, ErrOperationRetained) {
			return Activation{}, ErrOperationConflict
		}
	}
	return result, err
}

func (s *Service) activate(ctx context.Context, request ActivateRequest, email string, fingerprint [32]byte) (Activation, error) {
	tenantID, err := randomHex(16)
	if err != nil {
		return Activation{}, err
	}
	canvasID, err := randomHex(16)
	if err != nil {
		return Activation{}, err
	}
	now := s.now()
	notBefore := now.Add(-ActivationRetention)

	var result Activation
	err = s.store.InTx(ctx, func(tx Tx) error {
		// The grant stays locked until commit, so the decision still holds
		// when the activation is confirmed.
		operator, err := tx.IsOperator(ctx, request.OperatorID)
		if err != nil {
			return fmt.Errorf("subscription: read operator: %w", err)
		}
		if err := access.Check(access.OperatorRequest(operator, tenantID, access.ActionActivate)); err != nil {
			return err
		}

		record, found, err := tx.Activation(ctx, request.OperationID, notBefore)
		if err != nil {
			return fmt.Errorf("subscription: find activation: %w", err)
		}
		if found {
			if subtle.ConstantTimeCompare(record.Fingerprint[:], fingerprint[:]) != 1 {
				return ErrOperationConflict
			}
			result = record.Result
			return nil
		}

		account, found, err := tx.LockAccount(ctx, email)
		if err != nil {
			return fmt.Errorf("subscription: lock account: %w", err)
		}
		if !found {
			return ErrAccountNotFound
		}
		tenant, found, err := tx.TenantOwnedBy(ctx, account)
		if err != nil {
			return fmt.Errorf("subscription: find tenant: %w", err)
		}
		created := !found
		switch {
		case created:
			tenant = Tenant{ID: tenantID, Owner: account, CanvasID: canvasID, Status: access.SubscriptionActive, Version: 1}
			if err := tx.CreateTenant(ctx, tenant); err != nil {
				return fmt.Errorf("subscription: create tenant: %w", err)
			}
		case tenant.Status != access.SubscriptionActive:
			if err := tx.ActivateTenant(ctx, tenant.ID); err != nil {
				return fmt.Errorf("subscription: activate tenant: %w", err)
			}
		}
		if err := access.Check(access.OperatorRequest(operator, tenant.ID, access.ActionActivate)); err != nil {
			return err
		}

		result = Activation{AccountID: account, TenantID: tenant.ID, CanvasID: tenant.CanvasID, Created: created}
		err = tx.RecordActivation(ctx, Record{
			OperationID:   request.OperationID,
			OperatorID:    request.OperatorID,
			Fingerprint:   fingerprint,
			Result:        result,
			CorrelationID: request.CorrelationID,
			AcceptedAt:    now,
		}, notBefore)
		if err != nil && !errors.Is(err, ErrOperationRetained) {
			return fmt.Errorf("subscription: record activation: %w", err)
		}
		return err
	})
	if err != nil {
		return Activation{}, err
	}
	return result, nil
}

// anyTenant names the target of the operator check made before the tenant is
// known. Operators act platform-wide, so any tenant ID gives the same decision.
const anyTenant = "any-tenant"

// Deactivate cuts the access of the tenant owned by the account of
// request.Email. Deactivating an active tenant sets it inactive, increments
// its access version, and stores one pending access cut in the same
// transaction; deactivating an inactive tenant changes nothing. A retry under
// the same operation ID returns the first answer while it is retained.
//
// The access cut is effective at the commit: from then on every new read and
// mutation is rejected by the stored status. Live connections are cut by the
// relay the WithCommitted hook wakes; the answer does not wait for it, and a
// cut the relay has not applied stays pending until one does.
func (s *Service) Deactivate(ctx context.Context, request DeactivateRequest) (Deactivation, error) {
	email, ok := access.NormalizeEmail(request.Email)
	if !ok || !access.ValidOperationID(request.OperationID) {
		return Deactivation{}, ErrInvalidDeactivation
	}
	fingerprint := sha256.Sum256([]byte(string(access.ActionDeactivate) + "\x00" + string(request.OperatorID) + "\x00" + email))

	result, err := s.deactivate(ctx, request, email, fingerprint)
	if errors.Is(err, ErrOperationRetained) {
		// A concurrent deactivation committed the same operation ID first:
		// replay it, or reject a different payload.
		result, err = s.deactivate(ctx, request, email, fingerprint)
		if errors.Is(err, ErrOperationRetained) {
			return Deactivation{}, ErrOperationConflict
		}
	}
	return result, err
}

func (s *Service) deactivate(ctx context.Context, request DeactivateRequest, email string, fingerprint [32]byte) (Deactivation, error) {
	now := s.now()
	notBefore := now.Add(-ActivationRetention)

	var result Deactivation
	wroteCut := false
	err := s.store.InTx(ctx, func(tx Tx) error {
		wroteCut = false
		// The grant stays locked until commit, so the decision still holds
		// when the deactivation is confirmed.
		operator, err := tx.IsOperator(ctx, request.OperatorID)
		if err != nil {
			return fmt.Errorf("subscription: read operator: %w", err)
		}
		if err := access.Check(access.OperatorRequest(operator, anyTenant, access.ActionDeactivate)); err != nil {
			return err
		}

		record, found, err := tx.Deactivation(ctx, request.OperationID, notBefore)
		if err != nil {
			return fmt.Errorf("subscription: find deactivation: %w", err)
		}
		if found {
			if subtle.ConstantTimeCompare(record.Fingerprint[:], fingerprint[:]) != 1 {
				return ErrOperationConflict
			}
			result = record.Result
			return nil
		}

		account, found, err := tx.LockAccount(ctx, email)
		if err != nil {
			return fmt.Errorf("subscription: lock account: %w", err)
		}
		if !found {
			return ErrAccountNotFound
		}
		tenant, found, err := tx.TenantOwnedBy(ctx, account)
		if err != nil {
			return fmt.Errorf("subscription: find tenant: %w", err)
		}
		if !found {
			return ErrAccountNotFound
		}
		deactivated := tenant.Status == access.SubscriptionActive
		version := tenant.Version
		if deactivated {
			if version, err = tx.DeactivateTenant(ctx, tenant.ID); err != nil {
				return fmt.Errorf("subscription: deactivate tenant: %w", err)
			}
			cut := AccessCut{TenantID: tenant.ID, Version: version, CorrelationID: request.CorrelationID, CreatedAt: now}
			if err := tx.RecordCut(ctx, cut); err != nil {
				return fmt.Errorf("subscription: record cut: %w", err)
			}
			wroteCut = true
		}
		if err := access.Check(access.OperatorRequest(operator, tenant.ID, access.ActionDeactivate)); err != nil {
			return err
		}

		result = Deactivation{AccountID: account, TenantID: tenant.ID, CanvasID: tenant.CanvasID, Deactivated: deactivated, Version: version}
		err = tx.RecordDeactivation(ctx, DeactivationRecord{
			OperationID:   request.OperationID,
			OperatorID:    request.OperatorID,
			Fingerprint:   fingerprint,
			Result:        result,
			CorrelationID: request.CorrelationID,
			AcceptedAt:    now,
		}, notBefore)
		if err != nil && !errors.Is(err, ErrOperationRetained) {
			return fmt.Errorf("subscription: record deactivation: %w", err)
		}
		return err
	})
	if err != nil {
		return Deactivation{}, err
	}
	if wroteCut && s.committed != nil {
		s.committed()
	}
	return result, nil
}

// CanvasOf returns the canvas of the tenant the account owns. An account
// without an active subscription gets the shared access denial.
func (s *Service) CanvasOf(ctx context.Context, account access.AccountID) (canvas.Key, error) {
	tenant, found, err := s.store.OwnedTenant(ctx, account)
	if err != nil {
		return canvas.Key{}, fmt.Errorf("subscription: find tenant: %w", err)
	}
	membership := access.Membership{TenantID: tenant.ID, Role: access.RoleOwner, Subscription: tenant.Status}
	if err := access.Check(access.CanvasRequest(membership, found, tenant.ID, access.ActionView)); err != nil {
		return canvas.Key{}, err
	}
	return canvas.Key{TenantID: tenant.ID, CanvasID: tenant.CanvasID}, nil
}

// EditorGrant returns the access facts an editor connection of the account's
// session is authorized with: the tenant the account owns, read at its access
// version. An account without an active subscription gets the shared access
// denial, so a grant is never read for an inactive tenant.
func (s *Service) EditorGrant(ctx context.Context, account access.AccountID, sessionID string) (revocation.Grant, error) {
	tenant, found, err := s.store.OwnedTenant(ctx, account)
	if err != nil {
		return revocation.Grant{}, fmt.Errorf("subscription: find tenant: %w", err)
	}
	membership := access.Membership{TenantID: tenant.ID, Role: access.RoleOwner, Subscription: tenant.Status}
	if err := access.Check(access.CanvasRequest(membership, found, tenant.ID, access.ActionView)); err != nil {
		return revocation.Grant{}, err
	}
	return revocation.Grant{
		TenantID:  tenant.ID,
		AccountID: string(account),
		SessionID: sessionID,
		Client:    revocation.ClientEditor,
		Version:   tenant.Version,
	}, nil
}

// IsOperator reports whether the account currently holds the operator role.
func (s *Service) IsOperator(ctx context.Context, account access.AccountID) (bool, error) {
	operator, err := s.store.IsOperator(ctx, account)
	if err != nil {
		return false, fmt.Errorf("subscription: read operator: %w", err)
	}
	return operator, nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("subscription: read random bytes: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
