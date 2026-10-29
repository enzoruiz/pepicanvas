package media

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strconv"
)

// QuotaBytes is the inclusive storage quota of one canvas, in decimal bytes.
const QuotaBytes int64 = 1_000_000_000

// MaxIdentifierBytes bounds every tenant, canvas, asset, reference, and
// operation identifier.
const MaxIdentifierBytes = 128

// MaxObjectSide bounds each side of a Complete object's natural size. It
// equals canvas.MaxObjectSize, which this package does not import.
const MaxObjectSide = 32768

// Errors returned by ValidateCommand and by Store implementations.
var (
	// ErrInvalid reports a command rejected before it reaches storage.
	ErrInvalid = errors.New("media: invalid command")
	// ErrNotFound reports a missing canvas, asset, or reference, or an asset
	// that is not in the state the command needs. It never says whether the
	// resource exists in another tenant or in an unusable state.
	ErrNotFound = errors.New("media: not found")
	// ErrQuotaExceeded reports a reservation that does not fit the quota.
	ErrQuotaExceeded = errors.New("media: storage quota exceeded")
	// ErrReferenceExists reports a reference ID that already points to an asset.
	ErrReferenceExists = errors.New("media: reference already exists")
	// ErrOperationConflict reports an operation ID reused for a different
	// command. A new intent requires a new operation ID.
	ErrOperationConflict = errors.New("media: operation id reused with a different command")
	// ErrInactive reports a command for a canvas whose tenant's subscription
	// is not active. Nothing is written.
	ErrInactive = errors.New("media: the tenant's subscription is not active")
)

// State is the lifecycle state of a stored asset. Only StateReady is
// referenced, served, or counted as used.
type State string

const (
	// StatePending holds a reservation while the bytes are stored and inspected.
	StatePending State = "pending"
	// StateReady is a validated asset that consumes quota.
	StateReady State = "ready"
	// StateQuarantined is an asset rejected by inspection, isolated until its
	// stored bytes are deleted.
	StateQuarantined State = "quarantined"
	// StateDeleting is a failed, cancelled, expired, or released asset whose
	// stored bytes are still being deleted.
	StateDeleting State = "deleting"
)

// Valid reports whether state belongs to the closed lifecycle vocabulary.
func (state State) Valid() bool {
	switch state {
	case StatePending, StateReady, StateQuarantined, StateDeleting:
		return true
	default:
		return false
	}
}

// Usable reports whether an asset in state may be referenced or served.
func (state State) Usable() bool {
	return state == StateReady
}

// CanvasKey identifies the canvas that owns a quota inside one tenant.
type CanvasKey struct {
	TenantID string
	CanvasID string
}

// Operation identifies one client intent. Retrying the same command with the
// same ID returns the original result without a second effect. AccountID is
// the acting account, whose membership the store rechecks at the commit; it
// is empty for system work and is not part of the intent's fingerprint.
type Operation struct {
	ID            string
	CorrelationID string
	AccountID     string
}

// Usage is the committed accounting of one canvas. UsedBytes counts ready
// assets; ReservedBytes counts pending uploads.
type Usage struct {
	QuotaBytes    int64
	UsedBytes     int64
	ReservedBytes int64
}

// Available returns the bytes a new reservation may still take.
func (usage Usage) Available() int64 {
	return max(usage.QuotaBytes-usage.UsedBytes-usage.ReservedBytes, 0)
}

// Admits reports whether a reservation of bytes keeps the total within the
// inclusive quota.
func (usage Usage) Admits(bytes int64) bool {
	return bytes > 0 && bytes <= usage.Available()
}

// Command is one durable change applied atomically to a canvas quota.
type Command interface {
	validate() error
	// fields lists the command kind and every field, in a fixed order.
	fields() []string
}

// Reserve holds DeclaredBytes of quota for a new pending asset. With
// ReplacesReference, completion moves that reference to the new asset.
// The reservation needs the full declared size even when it replaces an asset,
// because the old asset is released only after the replacement succeeds.
type Reserve struct {
	DeclaredBytes     int64
	ReplacesReference string
}

// Complete turns a pending asset into a ready one. Its reservation becomes
// used quota, and ReferenceID is attached to it. For a replacing reservation
// ReferenceID must be the replaced reference, which moves to the new asset;
// the old asset is released when that was its last reference.
//
// With Object, the same transaction also creates the canvas image object that
// shows the asset: its ID is the asset ID, its content ReferenceID, and it is
// placed from the natural size ObjectWidth x ObjectHeight. The size is set
// exactly when Object is. The fingerprint includes Object but not the size:
// the size is decoded from the same stored bytes as the rest of the command,
// so it adds no intent, and a retry that replays a ready asset cannot read it.
type Complete struct {
	AssetID      string
	ReferenceID  string
	Kind         Kind
	Object       bool
	ObjectWidth  int
	ObjectHeight int
}

// AbandonReason says why a pending asset will never become ready.
type AbandonReason string

const (
	// AbandonCancelled is a client cancellation.
	AbandonCancelled AbandonReason = "cancelled"
	// AbandonFailed is a storage or processing failure.
	AbandonFailed AbandonReason = "failed"
	// AbandonRejected is an inspection rejection; the asset is quarantined.
	AbandonRejected AbandonReason = "rejected"
)

// Abandon releases the reservation of a pending asset. Rejection is the
// inspection rejection code: required exactly when Reason is AbandonRejected.
// The committed Abandon is retained with its operation, so a retry of the
// upload can repeat the same response.
type Abandon struct {
	AssetID   string
	Reason    AbandonReason
	Rejection RejectionCode
}

// Attach adds a reference to a ready asset.
type Attach struct {
	AssetID     string
	ReferenceID string
}

// Detach removes a reference. Removing the last reference of an asset releases
// its used quota at once.
type Detach struct {
	ReferenceID string
}

// Asset is the committed record of one upload.
type Asset struct {
	ID            string
	State         State
	Kind          Kind
	DeclaredBytes int64
	// StorageKey locates the bytes in object storage. It is random, internal,
	// and never shown to clients.
	StorageKey string
	References int
}

// Result is the committed outcome of one command. StorageKey is set by
// Reserve, and ObjectID, equal to AssetID, by a Complete with Object. Usage is
// the canvas accounting right after the command committed.
type Result struct {
	AssetID    string
	ObjectID   string
	StorageKey string
	Usage      Usage
}

// Garbage is a quarantined or deleting asset whose stored bytes must go.
type Garbage struct {
	Key        CanvasKey
	AssetID    string
	StorageKey string
}

// Store persists quotas and assets. Each Apply commits in a single
// transaction or not at all, and no committed state exceeds the quota.
type Store interface {
	// Usage returns the committed accounting of a canvas.
	Usage(ctx context.Context, key CanvasKey) (Usage, error)
	// Apply validates and commits one command.
	Apply(ctx context.Context, key CanvasKey, operation Operation, command Command) (Result, error)
	// Ready returns a ready asset, or ErrNotFound for any other state.
	Ready(ctx context.Context, key CanvasKey, assetID string) (Asset, error)
	// Collect releases expired reservations and returns up to limit assets
	// whose stored bytes must be deleted.
	Collect(ctx context.Context, limit int) ([]Garbage, error)
	// Purge forgets a quarantined or deleting asset once its bytes are gone.
	// Purging an asset that is already gone succeeds.
	Purge(ctx context.Context, key CanvasKey, assetID string) error
	// Abandoned returns the Abandon committed under operationID while that
	// operation is retained, or ErrNotFound when no such Abandon is retained.
	Abandoned(ctx context.Context, key CanvasKey, operationID string) (Abandon, error)
	// Inventory counts the assets of every canvas of a tenant, one entry per
	// state in InventoryStates order, aged by the time since each asset
	// entered its state.
	Inventory(ctx context.Context, tenantID string) ([]StateInventory, error)
	// HeldKeys returns which of up to MaxScanKeys storage keys an asset of
	// any tenant, in any state, still holds.
	HeldKeys(ctx context.Context, storageKeys []string) (map[string]bool, error)
	// AssetExists reports whether the owner's asset exists in any state.
	AssetExists(ctx context.Context, owner Owner) (bool, error)
	// ReadyPage returns up to limit ready assets of every tenant, ordered by
	// tenant, canvas, and asset, strictly after the owner after. The zero
	// Owner starts from the beginning.
	ReadyPage(ctx context.Context, after Owner, limit int) ([]StoredAsset, error)
}

// Fingerprint identifies the intent of a command, so a retry under the same
// operation ID can be told apart from a different command. The operation and
// its correlation ID are excluded.
func Fingerprint(command Command) [sha256.Size]byte {
	hash := sha256.New()
	for _, part := range command.fields() {
		_ = binary.Write(hash, binary.BigEndian, uint64(len(part)))
		hash.Write([]byte(part))
	}
	var sum [sha256.Size]byte
	hash.Sum(sum[:0])
	return sum
}

// ValidateKey checks a canvas key.
func ValidateKey(key CanvasKey) error {
	if err := validateIdentifier("tenant", key.TenantID); err != nil {
		return err
	}
	return validateIdentifier("canvas", key.CanvasID)
}

// ValidateCommand checks a command and its identity before it reaches storage.
func ValidateCommand(key CanvasKey, operation Operation, command Command) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := validateIdentifier("operation", operation.ID); err != nil {
		return err
	}
	if operation.CorrelationID != "" {
		if err := validateIdentifier("correlation", operation.CorrelationID); err != nil {
			return err
		}
	}
	// Pointer commands are rejected, so a typed nil never reaches validate and
	// a pointer never fingerprints like its value.
	if command == nil || reflect.ValueOf(command).Kind() == reflect.Pointer {
		return fmt.Errorf("%w: command must be a non-nil value", ErrInvalid)
	}
	return command.validate()
}

func (c Reserve) validate() error {
	if c.DeclaredBytes <= 0 || c.DeclaredBytes > QuotaBytes {
		return fmt.Errorf("%w: declared bytes must be 1 to %d", ErrInvalid, QuotaBytes)
	}
	if c.ReplacesReference != "" {
		return validateIdentifier("reference", c.ReplacesReference)
	}
	return nil
}

func (c Complete) validate() error {
	if err := validateIdentifier("asset", c.AssetID); err != nil {
		return err
	}
	if err := validateIdentifier("reference", c.ReferenceID); err != nil {
		return err
	}
	if !c.Kind.Valid() {
		return fmt.Errorf("%w: unsupported media kind", ErrInvalid)
	}
	if !c.Object {
		if c.ObjectWidth != 0 || c.ObjectHeight != 0 {
			return fmt.Errorf("%w: only a completion with an object has an object size", ErrInvalid)
		}
		return nil
	}
	if c.ObjectWidth < 1 || c.ObjectWidth > MaxObjectSide || c.ObjectHeight < 1 || c.ObjectHeight > MaxObjectSide {
		return fmt.Errorf("%w: object size must be within 1 and %d", ErrInvalid, MaxObjectSide)
	}
	return nil
}

func (c Abandon) validate() error {
	if err := validateIdentifier("asset", c.AssetID); err != nil {
		return err
	}
	switch c.Reason {
	case AbandonRejected:
		if _, known := rejectionMessages[c.Rejection]; !known {
			return fmt.Errorf("%w: a rejected abandon needs a known rejection code", ErrInvalid)
		}
		return nil
	case AbandonCancelled, AbandonFailed:
		if c.Rejection != "" {
			return fmt.Errorf("%w: only a rejected abandon has a rejection code", ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown abandon reason", ErrInvalid)
	}
}

func (c Attach) validate() error {
	if err := validateIdentifier("asset", c.AssetID); err != nil {
		return err
	}
	return validateIdentifier("reference", c.ReferenceID)
}

func (c Detach) validate() error {
	return validateIdentifier("reference", c.ReferenceID)
}

func (c Reserve) fields() []string {
	return []string{"reserve", strconv.FormatInt(c.DeclaredBytes, 10), c.ReplacesReference}
}

// A Complete without Object keeps the fields it had before objects existed,
// so its retained operations still replay across the upgrade.
func (c Complete) fields() []string {
	fields := []string{"complete", c.AssetID, c.ReferenceID, string(c.Kind)}
	if c.Object {
		fields = append(fields, "object")
	}
	return fields
}

func (c Abandon) fields() []string {
	return []string{"abandon", c.AssetID, string(c.Reason), string(c.Rejection)}
}

func (c Attach) fields() []string {
	return []string{"attach", c.AssetID, c.ReferenceID}
}

func (c Detach) fields() []string {
	return []string{"detach", c.ReferenceID}
}

// validateIdentifier accepts bounded printable ASCII without spaces.
func validateIdentifier(name, value string) error {
	if value == "" || len(value) > MaxIdentifierBytes {
		return fmt.Errorf("%w: %s identifier must have 1 to %d bytes", ErrInvalid, name, MaxIdentifierBytes)
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] > '~' {
			return fmt.Errorf("%w: malformed %s identifier", ErrInvalid, name)
		}
	}
	return nil
}
