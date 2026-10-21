package postgres

import "context"

// SetBeforeCommit injects a fault after a mutation ran and before its
// transaction commits. A nil hook removes it.
func SetBeforeCommit(store *CanvasStore, hook func(context.Context) error) {
	store.beforeCommit = hook
}

// SetMediaBeforeCommit injects a fault after a media command ran and before
// its transaction commits. A nil hook removes it.
func SetMediaBeforeCommit(store *MediaStore, hook func(context.Context) error) {
	store.beforeCommit = hook
}

// SetSubscriptionFault injects a fault after an activation step
// ("CreateTenant") or before its transaction commits ("commit"). A nil hook
// removes it.
func SetSubscriptionFault(store *SubscriptionStore, hook func(ctx context.Context, step string) error) {
	store.fault = hook
}
