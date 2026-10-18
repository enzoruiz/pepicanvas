package postgres

import "context"

// SetBeforeCommit injects a fault after a mutation ran and before its
// transaction commits. A nil hook removes it.
func SetBeforeCommit(store *CanvasStore, hook func(context.Context) error) {
	store.beforeCommit = hook
}
