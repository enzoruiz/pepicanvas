package locking

import (
	"context"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
)

// ProtectedObject reports whether mutation needs the lease on an existing
// object, and which one. Moving, resizing, rotating, editing text, and
// deleting are protected; creating objects and resizing the canvas are not.
func ProtectedObject(mutation canvas.Mutation) (objectID string, protected bool) {
	switch m := mutation.(type) {
	case canvas.SetTransform:
		return m.ObjectID, true
	case canvas.SetContent:
		return m.ObjectID, true
	case canvas.DeleteObject:
		return m.ObjectID, true
	}
	return "", false
}

// Applier commits one canvas mutation; canvas.Store satisfies it.
type Applier interface {
	Apply(ctx context.Context, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) (canvas.Result, error)
}

// ApplyLocked commits mutation through store. An unprotected mutation
// commits without a lease, and session may be nil. A protected one commits
// only through session.Protect on its object, so the session must hold
// that lease in key's tenant; otherwise ApplyLocked returns ErrNotHolder
// without calling store. An invalid mutation is rejected with
// canvas.ErrInvalid before it is classified, so no form of a protected
// mutation can pass as unprotected.
func ApplyLocked(ctx context.Context, session *Session, store Applier, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) (canvas.Result, error) {
	if err := canvas.Validate(key, operation, mutation); err != nil {
		return canvas.Result{}, err
	}
	objectID, protected := ProtectedObject(mutation)
	if !protected {
		return store.Apply(ctx, key, operation, mutation)
	}
	if session == nil || session.holder.TenantID != key.TenantID {
		return canvas.Result{}, ErrNotHolder
	}
	var result canvas.Result
	err := session.Protect(ctx, key.CanvasID, objectID, func(ctx context.Context) error {
		var err error
		result, err = store.Apply(ctx, key, operation, mutation)
		return err
	})
	return result, err
}
