package media

import (
	"fmt"
	"time"
)

// Age buckets of StateInventory.ByAge, by time since an asset entered its
// current state.
const (
	// AgeUnderHour counts assets that entered their state less than an hour ago.
	AgeUnderHour = iota
	// AgeUnderDay counts assets aged from one hour up to, not including, a day.
	AgeUnderDay
	// AgeDayOrMore counts assets aged a day or more.
	AgeDayOrMore
)

// MaxScanKeys bounds the storage keys of one HeldKeys call.
const MaxScanKeys = 1000

// MaxStorageKeyBytes bounds a storage key.
const MaxStorageKeyBytes = 256

// InventoryStates lists the states of an inventory, in its order.
var InventoryStates = [...]State{StatePending, StateReady, StateQuarantined, StateDeleting}

// StateInventory counts the assets of one tenant in one state. Bytes is the
// sum of their declared bytes, ByAge counts them by age bucket, and Oldest is
// the largest age, zero without assets. It never names an asset or an object.
type StateInventory struct {
	State  State
	Assets int64
	Bytes  int64
	ByAge  [3]int64
	Oldest time.Duration
}

// StoredAsset is a committed asset and the storage key of its bytes, for
// reconciliation against object storage.
type StoredAsset struct {
	Owner      Owner
	StorageKey string
}

// ValidateTenant checks a tenant identifier.
func ValidateTenant(tenantID string) error {
	return validateIdentifier("tenant", tenantID)
}

// ValidateOwner checks every identifier of an owner.
func ValidateOwner(owner Owner) error {
	if err := ValidateKey(owner.Key); err != nil {
		return err
	}
	return validateIdentifier("asset", owner.AssetID)
}

// ValidateStorageKeys checks the keys of one HeldKeys call.
func ValidateStorageKeys(keys []string) error {
	if len(keys) > MaxScanKeys {
		return fmt.Errorf("%w: at most %d storage keys per scan", ErrInvalid, MaxScanKeys)
	}
	for _, key := range keys {
		if key == "" || len(key) > MaxStorageKeyBytes {
			return fmt.Errorf("%w: storage key must have 1 to %d bytes", ErrInvalid, MaxStorageKeyBytes)
		}
	}
	return nil
}
