package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/media"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
)

// DefaultReservationTTL is how long a pending upload holds its reservation
// before it expires and is released.
const DefaultReservationTTL = 30 * time.Minute

// expiryOperationID is the stored operation of a media usage record written
// by a reservation expiry, which no client operation caused.
const expiryOperationID = "reservation-expiry"

// MediaStore implements media.Store. Every command locks the quota row of its
// canvas, so the accounting of one canvas changes one transaction at a time,
// and the database check keeps every committed state within the quota. Each
// accepted command retains its operation ID and result in the same
// transaction, so a retry returns the committed result without a second effect.
// Every transaction that changes the used or reserved bytes of a canvas also
// bumps its usage version and writes one media usage outbox record, so the
// realtime actor learns every committed usage (CTRL-CONS-01).
type MediaStore struct {
	pool           *pgxpool.Pool
	now            func() time.Time
	retention      time.Duration
	reservationTTL time.Duration
	// beforeCommit, when set by tests, runs after the command and before commit.
	beforeCommit func(context.Context) error
	committed    func(media.CanvasKey)
}

var _ media.Store = (*MediaStore)(nil)

// MediaStoreOption configures a MediaStore.
type MediaStoreOption func(*MediaStore)

// WithMediaClock sets the time source used for retention and reservation expiry.
func WithMediaClock(now func() time.Time) MediaStoreOption {
	return func(s *MediaStore) { s.now = now }
}

// WithMediaRetention sets how long an operation ID replays its result. An
// operation accepted at T is retained while now < T + ttl.
func WithMediaRetention(ttl time.Duration) MediaStoreOption {
	return func(s *MediaStore) { s.retention = ttl }
}

// WithReservationTTL sets how long a pending upload holds its reservation. A
// reservation made at T expires once now >= T + ttl.
func WithReservationTTL(ttl time.Duration) MediaStoreOption {
	return func(s *MediaStore) { s.reservationTTL = ttl }
}

// WithMediaCommitted sets a hook that runs after each commit that wrote a
// media usage outbox record, for example to wake the outbox publisher. It
// must not block.
func WithMediaCommitted(hook func(media.CanvasKey)) MediaStoreOption {
	return func(s *MediaStore) { s.committed = hook }
}

// NewMediaStore returns a store over a migrated pool.
func NewMediaStore(pool *pgxpool.Pool, options ...MediaStoreOption) *MediaStore {
	s := &MediaStore{pool: pool, now: time.Now, retention: DefaultOperationRetention, reservationTTL: DefaultReservationTTL}
	for _, option := range options {
		option(s)
	}
	return s
}

// Usage returns the committed accounting of a canvas after releasing its
// expired reservations.
func (s *MediaStore) Usage(ctx context.Context, key media.CanvasKey) (media.Usage, error) {
	if err := media.ValidateKey(key); err != nil {
		return media.Usage{}, err
	}
	var usage media.Usage
	var written bool
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := db.New(tx)
		locked, now, err := s.lock(ctx, q, key)
		if err != nil {
			return err
		}
		if usage, err = s.expire(ctx, q, key, locked, now); err != nil {
			return err
		}
		written, err = commitUsage(ctx, q, key, locked, usage, media.Operation{}, now)
		return err
	})
	if err != nil {
		return media.Usage{}, wrapMedia("read media usage", err)
	}
	s.notify(key, written)
	return usage, nil
}

// Apply validates one command and commits it in a single transaction. A
// retained operation ID replays its result when the command is equal and
// returns media.ErrOperationConflict when it differs. A new command for a
// tenant whose subscription is not active returns media.ErrInactive and
// writes nothing, except Abandon: a failed upload always releases its
// reservation. An equal retry of a command committed before the deactivation
// still replays its result, since a replay has no effect. A command with an
// acting account that is no longer a member of the tenant returns
// access.ErrAccessDenied and writes nothing, without a replay either (US-006);
// Abandon and system work, without an actor, are not checked.
func (s *MediaStore) Apply(ctx context.Context, key media.CanvasKey, operation media.Operation, command media.Command) (media.Result, error) {
	if err := media.ValidateCommand(key, operation, command); err != nil {
		return media.Result{}, err
	}
	fingerprint := media.Fingerprint(command)
	var result media.Result
	var written bool
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		written = false
		q := db.New(tx)
		locked, now, err := s.lock(ctx, q, key)
		if err != nil {
			return err
		}
		// The tenant row stays shared-locked until commit, so a deactivation
		// commits either before this read, and the command is rejected, or
		// after this commit. Canvases without a tenant row predate US-002.
		status, err := q.LockTenantStatus(ctx, key.TenantID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, abandon := command.(media.Abandon)
		accepted := abandon || errors.Is(err, pgx.ErrNoRows) || status == string(access.SubscriptionActive)
		// The actor's membership stays shared-locked until commit, so a
		// revocation commits either before this read, and the command is
		// rejected, or after this commit. The order media usage, tenant FOR
		// SHARE, membership FOR SHARE cannot deadlock with a revocation, which
		// takes the tenant FOR UPDATE before the membership FOR UPDATE: whoever
		// holds the tenant row first finishes before the other reaches the
		// membership.
		if operation.AccountID != "" && !abandon {
			_, err := q.LockActorMembership(ctx, db.LockActorMembershipParams{TenantID: key.TenantID, AccountID: operation.AccountID})
			if errors.Is(err, pgx.ErrNoRows) {
				return access.ErrAccessDenied
			}
			if err != nil {
				return err
			}
		}
		// As in CanvasStore.Apply, the replay check runs after the status read:
		// a rejected tenant still replays an equal retry, and without purging
		// expired operations, so the replay writes nothing.
		replayed, found, err := retainedMedia(ctx, q, key, operation, fingerprint, now.Add(-s.retention), accepted)
		if !accepted && (errors.Is(err, media.ErrOperationConflict) || (err == nil && !found)) {
			return media.ErrInactive
		}
		if err != nil || found {
			result = replayed
			return err
		}
		usage, err := s.expire(ctx, q, key, locked, now)
		if err != nil {
			return err
		}
		if result, err = applyMedia(ctx, q, key, usage, command, now); err != nil {
			return err
		}
		if written, err = commitUsage(ctx, q, key, locked, result.Usage, operation, now); err != nil {
			return err
		}
		// An Abandon retains its outcome, so a retry repeats its response.
		var reason, rejection pgtype.Text
		if abandon, ok := command.(media.Abandon); ok {
			reason = pgtype.Text{String: string(abandon.Reason), Valid: true}
			rejection = pgtype.Text{String: string(abandon.Rejection), Valid: abandon.Rejection != ""}
		}
		if err := q.InsertMediaOperation(ctx, db.InsertMediaOperationParams{
			TenantID: key.TenantID, CanvasID: key.CanvasID, OperationID: operation.ID,
			Fingerprint: fingerprint[:],
			AssetID:     result.AssetID, StorageKey: result.StorageKey,
			QuotaBytes: result.Usage.QuotaBytes, UsedBytes: result.Usage.UsedBytes, ReservedBytes: result.Usage.ReservedBytes,
			CorrelationID: pgtype.Text{String: operation.CorrelationID, Valid: operation.CorrelationID != ""},
			AcceptedAt:    pgtype.Timestamptz{Time: now, Valid: true},
			AbandonReason: reason, RejectionCode: rejection,
		}); err != nil {
			return err
		}
		if s.beforeCommit != nil {
			return s.beforeCommit(ctx)
		}
		return nil
	})
	if err != nil {
		return media.Result{}, wrapMedia("apply media command", err)
	}
	s.notify(key, written)
	return result, nil
}

// Ready returns a ready asset, or media.ErrNotFound for any other state.
func (s *MediaStore) Ready(ctx context.Context, key media.CanvasKey, assetID string) (media.Asset, error) {
	if err := media.ValidateKey(key); err != nil {
		return media.Asset{}, err
	}
	if assetID == "" || len(assetID) > media.MaxIdentifierBytes {
		return media.Asset{}, fmt.Errorf("%w: asset identifier must have 1 to %d bytes", media.ErrInvalid, media.MaxIdentifierBytes)
	}
	row, err := db.New(s.pool).GetReadyAsset(ctx, db.GetReadyAssetParams{TenantID: key.TenantID, CanvasID: key.CanvasID, AssetID: assetID})
	if errors.Is(err, pgx.ErrNoRows) {
		return media.Asset{}, media.ErrNotFound
	}
	if err != nil {
		return media.Asset{}, wrapMedia("read ready asset", err)
	}
	return media.Asset{
		ID:            assetID,
		State:         media.StateReady,
		Kind:          media.Kind(row.Kind.String),
		DeclaredBytes: row.DeclaredBytes,
		StorageKey:    row.StorageKey,
		References:    int(row.Refs),
	}, nil
}

// Collect releases the expired reservations of up to limit canvases, one
// canvas transaction at a time, and returns up to limit assets whose stored
// bytes must be deleted.
func (s *MediaStore) Collect(ctx context.Context, limit int) ([]media.Garbage, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: collect limit must be positive", media.ErrInvalid)
	}
	bound := int32(min(limit, math.MaxInt32))
	q := db.New(s.pool)
	canvases, err := q.CanvasesWithExpiredReservations(ctx, db.CanvasesWithExpiredReservationsParams{
		ExpiredAt: pgtype.Timestamptz{Time: s.now().Add(-s.reservationTTL), Valid: true}, MaxCanvases: bound,
	})
	if err != nil {
		return nil, wrapMedia("list expired reservations", err)
	}
	for _, row := range canvases {
		key := media.CanvasKey{TenantID: row.TenantID, CanvasID: row.CanvasID}
		var written bool
		err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			q := db.New(tx)
			locked, now, err := s.lock(ctx, q, key)
			if err != nil {
				return err
			}
			usage, err := s.expire(ctx, q, key, locked, now)
			if err != nil {
				return err
			}
			written, err = commitUsage(ctx, q, key, locked, usage, media.Operation{}, now)
			return err
		})
		if err != nil {
			return nil, wrapMedia("expire reservations", err)
		}
		s.notify(key, written)
	}
	rows, err := q.ListGarbage(ctx, bound)
	if err != nil {
		return nil, wrapMedia("list media garbage", err)
	}
	garbage := make([]media.Garbage, 0, len(rows))
	for _, row := range rows {
		garbage = append(garbage, media.Garbage{
			Key:     media.CanvasKey{TenantID: row.TenantID, CanvasID: row.CanvasID},
			AssetID: row.AssetID, StorageKey: row.StorageKey,
		})
	}
	return garbage, nil
}

// Purge forgets a quarantined or deleting asset. An absent asset succeeds; a
// pending or ready asset returns media.ErrNotFound.
func (s *MediaStore) Purge(ctx context.Context, key media.CanvasKey, assetID string) error {
	if err := media.ValidateKey(key); err != nil {
		return err
	}
	if assetID == "" || len(assetID) > media.MaxIdentifierBytes {
		return fmt.Errorf("%w: asset identifier must have 1 to %d bytes", media.ErrInvalid, media.MaxIdentifierBytes)
	}
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := db.New(tx)
		params := db.PurgeAssetParams{TenantID: key.TenantID, CanvasID: key.CanvasID, AssetID: assetID}
		purged, err := q.PurgeAsset(ctx, params)
		if err != nil || purged > 0 {
			return err
		}
		exists, err := q.AssetExists(ctx, db.AssetExistsParams(params))
		if err != nil {
			return err
		}
		if exists {
			return media.ErrNotFound
		}
		return nil
	})
	return wrapMedia("purge asset", err)
}

// Abandoned returns the Abandon committed under operationID while that
// operation is retained, with the same cutoff as replay. It purges nothing:
// expired rows are purged by the next Apply on the canvas.
func (s *MediaStore) Abandoned(ctx context.Context, key media.CanvasKey, operationID string) (media.Abandon, error) {
	if err := media.ValidateKey(key); err != nil {
		return media.Abandon{}, err
	}
	if operationID == "" || len(operationID) > media.MaxIdentifierBytes {
		return media.Abandon{}, fmt.Errorf("%w: operation identifier must have 1 to %d bytes", media.ErrInvalid, media.MaxIdentifierBytes)
	}
	row, err := db.New(s.pool).GetAbandonOperation(ctx, db.GetAbandonOperationParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID, OperationID: operationID,
		ExpiredAt: pgtype.Timestamptz{Time: s.now().Add(-s.retention), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return media.Abandon{}, media.ErrNotFound
	}
	if err != nil {
		return media.Abandon{}, wrapMedia("read abandoned upload", err)
	}
	return media.Abandon{
		AssetID:   row.AssetID,
		Reason:    media.AbandonReason(row.AbandonReason.String),
		Rejection: media.RejectionCode(row.RejectionCode.String),
	}, nil
}

// Inventory counts the assets of every canvas of a tenant by state, aged by
// the store clock since each asset entered its state. It names no asset.
func (s *MediaStore) Inventory(ctx context.Context, tenantID string) ([]media.StateInventory, error) {
	if err := media.ValidateTenant(tenantID); err != nil {
		return nil, err
	}
	now := s.now()
	rows, err := db.New(s.pool).MediaInventory(ctx, db.MediaInventoryParams{
		TenantID: tenantID,
		HourAgo:  pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		DayAgo:   pgtype.Timestamptz{Time: now.Add(-24 * time.Hour), Valid: true},
	})
	if err != nil {
		return nil, wrapMedia("read media inventory", err)
	}
	inventory := make([]media.StateInventory, len(media.InventoryStates))
	for i, state := range media.InventoryStates {
		inventory[i].State = state
		for _, row := range rows {
			if media.State(row.State) != state {
				continue
			}
			inventory[i].Assets, inventory[i].Bytes = row.Assets, row.Bytes
			inventory[i].ByAge = [3]int64{
				media.AgeUnderHour: row.UnderHour,
				media.AgeUnderDay:  row.UnderDay,
				media.AgeDayOrMore: row.Assets - row.UnderHour - row.UnderDay,
			}
			inventory[i].Oldest = max(now.Sub(row.OldestChangedAt.Time), 0)
		}
	}
	return inventory, nil
}

// HeldKeys returns which storage keys an asset of any tenant, in any state,
// still holds.
func (s *MediaStore) HeldKeys(ctx context.Context, storageKeys []string) (map[string]bool, error) {
	if err := media.ValidateStorageKeys(storageKeys); err != nil {
		return nil, err
	}
	held := make(map[string]bool)
	if len(storageKeys) == 0 {
		return held, nil
	}
	keys, err := db.New(s.pool).HeldStorageKeys(ctx, storageKeys)
	if err != nil {
		return nil, wrapMedia("read held storage keys", err)
	}
	for _, key := range keys {
		held[key] = true
	}
	return held, nil
}

// AssetExists reports whether the owner's asset exists in any state.
func (s *MediaStore) AssetExists(ctx context.Context, owner media.Owner) (bool, error) {
	if err := media.ValidateOwner(owner); err != nil {
		return false, err
	}
	exists, err := db.New(s.pool).AssetExists(ctx, db.AssetExistsParams{TenantID: owner.Key.TenantID, CanvasID: owner.Key.CanvasID, AssetID: owner.AssetID})
	if err != nil {
		return false, wrapMedia("read asset existence", err)
	}
	return exists, nil
}

// ReadyPage returns up to limit ready assets of every tenant, ordered by
// tenant, canvas, and asset, strictly after the owner after. The zero Owner
// starts from the beginning; any other cursor must be a valid owner.
func (s *MediaStore) ReadyPage(ctx context.Context, after media.Owner, limit int) ([]media.StoredAsset, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: page limit must be positive", media.ErrInvalid)
	}
	if after != (media.Owner{}) {
		if err := media.ValidateOwner(after); err != nil {
			return nil, err
		}
	}
	rows, err := db.New(s.pool).ReadyAssetsAfter(ctx, db.ReadyAssetsAfterParams{
		AfterTenant: after.Key.TenantID, AfterCanvas: after.Key.CanvasID, AfterAsset: after.AssetID,
		MaxAssets: int32(min(limit, math.MaxInt32)),
	})
	if err != nil {
		return nil, wrapMedia("list ready assets", err)
	}
	page := make([]media.StoredAsset, 0, len(rows))
	for _, row := range rows {
		page = append(page, media.StoredAsset{
			Owner:      media.Owner{Key: media.CanvasKey{TenantID: row.TenantID, CanvasID: row.CanvasID}, AssetID: row.AssetID},
			StorageKey: row.StorageKey,
		})
	}
	return page, nil
}

// lock creates the quota row of an existing canvas, locks it, and returns its
// usage with the time read after the lock wait, so retention and expiry start
// at acceptance.
func (s *MediaStore) lock(ctx context.Context, q *db.Queries, key media.CanvasKey) (media.Usage, time.Time, error) {
	if err := q.EnsureMediaUsage(ctx, db.EnsureMediaUsageParams{TenantID: key.TenantID, CanvasID: key.CanvasID}); err != nil {
		return media.Usage{}, time.Time{}, err
	}
	row, err := q.LockMediaUsage(ctx, db.LockMediaUsageParams{TenantID: key.TenantID, CanvasID: key.CanvasID})
	if errors.Is(err, pgx.ErrNoRows) {
		return media.Usage{}, time.Time{}, media.ErrNotFound
	}
	if err != nil {
		return media.Usage{}, time.Time{}, err
	}
	return media.Usage{QuotaBytes: row.QuotaBytes, UsedBytes: row.UsedBytes, ReservedBytes: row.ReservedBytes}, s.now(), nil
}

// expire marks the pending assets reserved at or before now - ttl as deleting
// at now and returns the locked usage without their reservations.
func (s *MediaStore) expire(ctx context.Context, q *db.Queries, key media.CanvasKey, usage media.Usage, now time.Time) (media.Usage, error) {
	released, err := q.ExpireReservations(ctx, db.ExpireReservationsParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID,
		ChangedAt: pgtype.Timestamptz{Time: now, Valid: true},
		ExpiredAt: pgtype.Timestamptz{Time: now.Add(-s.reservationTTL), Valid: true},
	})
	usage.ReservedBytes -= released
	return usage, err
}

// commitUsage stores the usage after a command or an expiry, inside the
// transaction that holds the quota lock. A changed usage bumps the usage
// version and writes one media usage outbox record carrying only the
// aggregate usage, the version, and the operation's correlation ID; an
// unchanged usage writes nothing. It reports whether a record was written.
func commitUsage(ctx context.Context, q *db.Queries, key media.CanvasKey, before, after media.Usage, operation media.Operation, now time.Time) (bool, error) {
	if after == before {
		return false, nil
	}
	version, err := q.SetMediaUsage(ctx, db.SetMediaUsageParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID, UsedBytes: after.UsedBytes, ReservedBytes: after.ReservedBytes,
	})
	if err != nil {
		return false, err
	}
	payload, err := outbox.EncodeMediaUsage(outbox.MediaUsage{
		Version: version, QuotaBytes: after.QuotaBytes, UsedBytes: after.UsedBytes, ReservedBytes: after.ReservedBytes,
	})
	if err != nil {
		return false, err
	}
	operationID := operation.ID
	if operationID == "" {
		operationID = expiryOperationID
	}
	if err := q.InsertMediaUsageOutbox(ctx, db.InsertMediaUsageOutboxParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID, OperationID: operationID,
		CorrelationID: pgtype.Text{String: operation.CorrelationID, Valid: operation.CorrelationID != ""},
		Usage:         payload,
		CreatedAt:     pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return false, err
	}
	return true, nil
}

// notify runs the commit hook after a commit that wrote a usage record.
func (s *MediaStore) notify(key media.CanvasKey, written bool) {
	if written && s.committed != nil {
		s.committed(key)
	}
}

// retainedMedia returns the result of an equal retry of an operation accepted
// after expiredAt, or media.ErrOperationConflict. With purge it first deletes
// the canvas operations accepted at or before expiredAt; without it nothing is
// written.
func retainedMedia(ctx context.Context, q *db.Queries, key media.CanvasKey, operation media.Operation, fingerprint [32]byte, expiredAt time.Time, purge bool) (media.Result, bool, error) {
	if purge {
		if err := q.PurgeExpiredMediaOperations(ctx, db.PurgeExpiredMediaOperationsParams{
			TenantID: key.TenantID, CanvasID: key.CanvasID, ExpiredAt: pgtype.Timestamptz{Time: expiredAt, Valid: true},
		}); err != nil {
			return media.Result{}, false, err
		}
	}
	row, err := q.GetMediaOperation(ctx, db.GetMediaOperationParams{TenantID: key.TenantID, CanvasID: key.CanvasID, OperationID: operation.ID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.AcceptedAt.Time.After(expiredAt)) {
		return media.Result{}, false, nil
	}
	if err != nil {
		return media.Result{}, false, err
	}
	if !bytes.Equal(row.Fingerprint, fingerprint[:]) {
		return media.Result{}, false, media.ErrOperationConflict
	}
	return media.Result{
		AssetID: row.AssetID, StorageKey: row.StorageKey,
		Usage: media.Usage{QuotaBytes: row.QuotaBytes, UsedBytes: row.UsedBytes, ReservedBytes: row.ReservedBytes},
	}, true, nil
}

// applyMedia runs one command inside the transaction that holds the quota
// lock and returns its result with the usage after the command.
func applyMedia(ctx context.Context, q *db.Queries, key media.CanvasKey, usage media.Usage, command media.Command, now time.Time) (media.Result, error) {
	tenant, canvasID := key.TenantID, key.CanvasID
	switch c := command.(type) {
	case media.Reserve:
		if !usage.Admits(c.DeclaredBytes) {
			return media.Result{}, media.ErrQuotaExceeded
		}
		if c.ReplacesReference != "" {
			if _, err := referencedAsset(ctx, q, key, c.ReplacesReference); err != nil {
				return media.Result{}, err
			}
		}
		assetID, err := newObjectID()
		if err != nil {
			return media.Result{}, err
		}
		storageID, err := newObjectID()
		if err != nil {
			return media.Result{}, err
		}
		storageKey := "assets/" + storageID
		if err := q.InsertPendingAsset(ctx, db.InsertPendingAssetParams{
			TenantID: tenant, CanvasID: canvasID, AssetID: assetID,
			DeclaredBytes: c.DeclaredBytes, StorageKey: storageKey,
			ReplacesReference: pgtype.Text{String: c.ReplacesReference, Valid: c.ReplacesReference != ""},
			ReservedAt:        pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return media.Result{}, err
		}
		usage.ReservedBytes += c.DeclaredBytes
		return media.Result{AssetID: assetID, StorageKey: storageKey, Usage: usage}, nil

	case media.Complete:
		asset, err := lockAssetIn(ctx, q, key, c.AssetID, media.StatePending)
		if err != nil {
			return media.Result{}, err
		}
		if asset.ReplacesReference.Valid {
			if c.ReferenceID != asset.ReplacesReference.String {
				return media.Result{}, fmt.Errorf("%w: a replacement completes the reference it replaces", media.ErrInvalid)
			}
			old, err := referencedAsset(ctx, q, key, c.ReferenceID)
			if err != nil {
				return media.Result{}, err
			}
			if err := q.MoveReference(ctx, db.MoveReferenceParams{TenantID: tenant, CanvasID: canvasID, ReferenceID: c.ReferenceID, AssetID: c.AssetID}); err != nil {
				return media.Result{}, err
			}
			if usage, err = releaseIfUnreferenced(ctx, q, key, old, usage, now); err != nil {
				return media.Result{}, err
			}
		} else if err := insertReference(ctx, q, key, c.ReferenceID, c.AssetID); err != nil {
			return media.Result{}, err
		}
		if err := q.SetAssetState(ctx, db.SetAssetStateParams{
			TenantID: tenant, CanvasID: canvasID, AssetID: c.AssetID,
			State: string(media.StateReady), Kind: pgtype.Text{String: string(c.Kind), Valid: true},
			ChangedAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return media.Result{}, err
		}
		usage.ReservedBytes -= asset.DeclaredBytes
		usage.UsedBytes += asset.DeclaredBytes
		return media.Result{AssetID: c.AssetID, Usage: usage}, nil

	case media.Abandon:
		asset, err := lockAssetIn(ctx, q, key, c.AssetID, media.StatePending)
		if err != nil {
			return media.Result{}, err
		}
		state := media.StateDeleting
		if c.Reason == media.AbandonRejected {
			state = media.StateQuarantined
		}
		if err := q.SetAssetState(ctx, db.SetAssetStateParams{
			TenantID: tenant, CanvasID: canvasID, AssetID: c.AssetID, State: string(state),
			ChangedAt: pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return media.Result{}, err
		}
		usage.ReservedBytes -= asset.DeclaredBytes
		return media.Result{AssetID: c.AssetID, Usage: usage}, nil

	case media.Attach:
		if _, err := lockAssetIn(ctx, q, key, c.AssetID, media.StateReady); err != nil {
			return media.Result{}, err
		}
		if err := insertReference(ctx, q, key, c.ReferenceID, c.AssetID); err != nil {
			return media.Result{}, err
		}
		return media.Result{AssetID: c.AssetID, Usage: usage}, nil

	case media.Detach:
		assetID, err := q.DeleteReference(ctx, db.DeleteReferenceParams{TenantID: tenant, CanvasID: canvasID, ReferenceID: c.ReferenceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return media.Result{}, media.ErrNotFound
		}
		if err != nil {
			return media.Result{}, err
		}
		if usage, err = releaseIfUnreferenced(ctx, q, key, assetID, usage, now); err != nil {
			return media.Result{}, err
		}
		return media.Result{AssetID: assetID, Usage: usage}, nil

	default:
		return media.Result{}, fmt.Errorf("%w: unsupported command %T", media.ErrInvalid, command)
	}
}

// lockAssetIn locks an asset of this canvas and reports media.ErrNotFound when
// it is missing or not in state.
func lockAssetIn(ctx context.Context, q *db.Queries, key media.CanvasKey, assetID string, state media.State) (db.LockAssetRow, error) {
	asset, err := q.LockAsset(ctx, db.LockAssetParams{TenantID: key.TenantID, CanvasID: key.CanvasID, AssetID: assetID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.LockAssetRow{}, media.ErrNotFound
	}
	if err != nil {
		return db.LockAssetRow{}, err
	}
	if media.State(asset.State) != state {
		return db.LockAssetRow{}, media.ErrNotFound
	}
	return asset, nil
}

// referencedAsset returns the ready asset a reference points to, or
// media.ErrNotFound.
func referencedAsset(ctx context.Context, q *db.Queries, key media.CanvasKey, referenceID string) (string, error) {
	assetID, err := q.GetReference(ctx, db.GetReferenceParams{TenantID: key.TenantID, CanvasID: key.CanvasID, ReferenceID: referenceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", media.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err := lockAssetIn(ctx, q, key, assetID, media.StateReady); err != nil {
		return "", err
	}
	return assetID, nil
}

func insertReference(ctx context.Context, q *db.Queries, key media.CanvasKey, referenceID, assetID string) error {
	inserted, err := q.InsertReference(ctx, db.InsertReferenceParams{TenantID: key.TenantID, CanvasID: key.CanvasID, ReferenceID: referenceID, AssetID: assetID})
	if err != nil {
		return err
	}
	if inserted == 0 {
		return media.ErrReferenceExists
	}
	return nil
}

// releaseIfUnreferenced marks a ready asset with no references left as
// deleting at now and releases its used quota at once.
func releaseIfUnreferenced(ctx context.Context, q *db.Queries, key media.CanvasKey, assetID string, usage media.Usage, now time.Time) (media.Usage, error) {
	references, err := q.CountReferences(ctx, db.CountReferencesParams{TenantID: key.TenantID, CanvasID: key.CanvasID, AssetID: assetID})
	if err != nil || references > 0 {
		return usage, err
	}
	asset, err := lockAssetIn(ctx, q, key, assetID, media.StateReady)
	if err != nil {
		return usage, err
	}
	if err := q.SetAssetState(ctx, db.SetAssetStateParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID, AssetID: assetID, State: string(media.StateDeleting),
		ChangedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}); err != nil {
		return usage, err
	}
	usage.UsedBytes -= asset.DeclaredBytes
	return usage, nil
}

// wrapMedia keeps media domain errors comparable and labels storage failures.
func wrapMedia(action string, err error) error {
	if err == nil {
		return nil
	}
	for _, domain := range []error{media.ErrNotFound, media.ErrInvalid, media.ErrQuotaExceeded, media.ErrReferenceExists, media.ErrOperationConflict, media.ErrInactive, access.ErrAccessDenied} {
		if errors.Is(err, domain) {
			return err
		}
	}
	return fmt.Errorf("postgres: %s: %w", action, err)
}
