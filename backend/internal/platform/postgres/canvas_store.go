package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/access"
	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/telemetry"
)

// DefaultOperationRetention is how long an accepted operation ID replays its
// result, matching the realtime retention.
const DefaultOperationRetention = 10 * time.Minute

// CanvasStore implements canvas.Store. Every mutation locks its canvas row,
// so mutations of one canvas commit one at a time in revision order. Each
// accepted mutation retains its operation ID and result, and writes its
// outbox record, in the same transaction, so a retry returns the committed
// result without a second effect or a second record.
type CanvasStore struct {
	pool      *pgxpool.Pool
	now       func() time.Time
	retention time.Duration
	committed func(canvas.Key)
	// beforeCommit, when set by tests, runs after the mutation and before commit.
	beforeCommit func(context.Context) error
}

var _ canvas.Store = (*CanvasStore)(nil)

// CanvasStoreOption configures a CanvasStore.
type CanvasStoreOption func(*CanvasStore)

// WithClock sets the time source used for operation retention.
func WithClock(now func() time.Time) CanvasStoreOption {
	return func(s *CanvasStore) { s.now = now }
}

// WithRetention sets how long an operation ID replays its result. An operation
// accepted at T is retained while now < T + ttl.
func WithRetention(ttl time.Duration) CanvasStoreOption {
	return func(s *CanvasStore) { s.retention = ttl }
}

// WithCommitted sets a hook that runs after each commit that wrote an outbox
// record, so a publisher can deliver it without waiting for its next poll.
// The hook must not block.
func WithCommitted(hook func(canvas.Key)) CanvasStoreOption {
	return func(s *CanvasStore) { s.committed = hook }
}

// NewCanvasStore returns a store over a migrated pool.
func NewCanvasStore(pool *pgxpool.Pool, options ...CanvasStoreOption) *CanvasStore {
	s := &CanvasStore{pool: pool, now: time.Now, retention: DefaultOperationRetention}
	for _, option := range options {
		option(s)
	}
	return s
}

// Create makes the canvas with default dimensions, or returns the existing one.
func (s *CanvasStore) Create(ctx context.Context, key canvas.Key) (canvas.Canvas, error) {
	if err := canvas.ValidateKey(key); err != nil {
		return canvas.Canvas{}, err
	}
	if err := db.New(s.pool).CreateCanvas(ctx, db.CreateCanvasParams{TenantID: key.TenantID, CanvasID: key.CanvasID}); err != nil {
		return canvas.Canvas{}, fmt.Errorf("postgres: create canvas: %w", err)
	}
	return s.Load(ctx, key)
}

// Load reads the canvas and its objects from one consistent snapshot.
func (s *CanvasStore) Load(ctx context.Context, key canvas.Key) (canvas.Canvas, error) {
	if err := canvas.ValidateKey(key); err != nil {
		return canvas.Canvas{}, err
	}
	var loaded canvas.Canvas
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.GetCanvas(ctx, db.GetCanvasParams{TenantID: key.TenantID, CanvasID: key.CanvasID})
		if errors.Is(err, pgx.ErrNoRows) {
			return canvas.ErrNotFound
		}
		if err != nil {
			return err
		}
		objects, err := q.ListObjects(ctx, db.ListObjectsParams{TenantID: key.TenantID, CanvasID: key.CanvasID})
		if err != nil {
			return err
		}
		loaded = canvas.Canvas{
			Key:        key,
			Dimensions: canvas.Dimensions{Width: int(row.Width), Height: int(row.Height)},
			Revision:   row.Revision,
			Objects:    make([]canvas.Object, 0, len(objects)),
		}
		for _, object := range objects {
			loaded.Objects = append(loaded.Objects, canvas.Object{
				ID:      object.ObjectID,
				Kind:    canvas.Kind(object.Kind),
				Content: object.Content,
				Transform: canvas.Transform{
					X: int(object.X), Y: int(object.Y),
					Width: int(object.Width), Height: int(object.Height),
					Rotation: object.Rotation,
				},
				Z: object.Z,
			})
		}
		return nil
	})
	if err != nil {
		return canvas.Canvas{}, wrap("load canvas", err)
	}
	return loaded, nil
}

// Apply validates one mutation and commits it in a single transaction. A
// retained operation ID replays its result when the mutation is equal and
// returns canvas.ErrOperationConflict when it differs. A new mutation of a
// tenant whose subscription is not active returns canvas.ErrInactive and
// writes nothing; an equal retry of a mutation committed before the
// deactivation still replays its result, since a replay has no effect. It
// runs in a postgres span of the caller's trace.
func (s *CanvasStore) Apply(ctx context.Context, key canvas.Key, operation canvas.Operation, mutation canvas.Mutation) (_ canvas.Result, err error) {
	var replay bool
	ctx, span := telemetry.Start(ctx, telemetry.SpanPostgres, telemetry.Attr("operation", "canvas.apply"))
	defer func() { endApply(span, replay, err) }()
	if err := canvas.Validate(key, operation, mutation); err != nil {
		return canvas.Result{}, err
	}
	fingerprint := canvas.Fingerprint(mutation)
	kind, payload, err := outbox.EncodeMutation(mutation)
	if err != nil {
		return canvas.Result{}, err
	}
	var result canvas.Result
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.LockCanvas(ctx, db.LockCanvasParams{TenantID: key.TenantID, CanvasID: key.CanvasID}); errors.Is(err, pgx.ErrNoRows) {
			return canvas.ErrNotFound
		} else if err != nil {
			return err
		}
		// The tenant row stays shared-locked until commit, so a deactivation
		// commits either before this read, and the mutation is rejected, or
		// after this commit. Canvases without a tenant row predate US-002.
		status, err := q.LockTenantStatus(ctx, key.TenantID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		active := errors.Is(err, pgx.ErrNoRows) || status == string(access.SubscriptionActive)
		// Read the clock after the lock wait, so retention starts at acceptance.
		now := s.now()
		replayed, found, err := retained(ctx, q, key, operation, fingerprint, now.Add(-s.retention), active)
		if !active && (errors.Is(err, canvas.ErrOperationConflict) || (err == nil && !found)) {
			return canvas.ErrInactive
		}
		if err != nil || found {
			result, replay = replayed, found
			return err
		}
		if result, err = mutate(ctx, q, key, mutation); err != nil {
			return err
		}
		if err := q.InsertOperation(ctx, db.InsertOperationParams{
			TenantID: key.TenantID, CanvasID: key.CanvasID, OperationID: operation.ID,
			Fingerprint: fingerprint[:],
			Revision:    result.Revision, ObjectID: result.ObjectID, Z: result.Z,
			CorrelationID: pgtype.Text{String: operation.CorrelationID, Valid: operation.CorrelationID != ""},
			AcceptedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		}); err != nil {
			return err
		}
		if err := insertMutationRecord(ctx, q, key, operation, kind, payload, result, now); err != nil {
			return err
		}
		if s.beforeCommit != nil {
			return s.beforeCommit(ctx)
		}
		return nil
	})
	if err != nil {
		return canvas.Result{}, wrap("apply mutation", err)
	}
	if !replay && s.committed != nil {
		s.committed(key)
	}
	return result, nil
}

// endApply ends the span of a store transaction with its closed outcome:
// committed, replayed, or error. The error text is never exported.
func endApply(span *telemetry.Span, replay bool, err error) {
	switch {
	case err != nil:
		span.SetAttr("outcome", "error")
	case replay:
		span.SetAttr("outcome", "replayed")
	default:
		span.SetAttr("outcome", "committed")
	}
	span.End(err)
}

// retained returns the result of an equal retry of an operation accepted
// after expiredAt, or canvas.ErrOperationConflict. With purge it first deletes
// the canvas operations accepted at or before expiredAt; without it nothing
// is written.
func retained(ctx context.Context, q *db.Queries, key canvas.Key, operation canvas.Operation, fingerprint [32]byte, expiredAt time.Time, purge bool) (canvas.Result, bool, error) {
	if purge {
		if err := q.PurgeExpiredOperations(ctx, db.PurgeExpiredOperationsParams{
			TenantID: key.TenantID, CanvasID: key.CanvasID, ExpiredAt: pgtype.Timestamptz{Time: expiredAt, Valid: true},
		}); err != nil {
			return canvas.Result{}, false, err
		}
	}
	row, err := q.GetOperation(ctx, db.GetOperationParams{TenantID: key.TenantID, CanvasID: key.CanvasID, OperationID: operation.ID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !row.AcceptedAt.Time.After(expiredAt)) {
		return canvas.Result{}, false, nil
	}
	if err != nil {
		return canvas.Result{}, false, err
	}
	if !bytes.Equal(row.Fingerprint, fingerprint[:]) {
		return canvas.Result{}, false, canvas.ErrOperationConflict
	}
	return canvas.Result{Revision: row.Revision, ObjectID: row.ObjectID, Z: row.Z}, true, nil
}

// mutate runs one mutation inside the transaction that holds the canvas lock.
func mutate(ctx context.Context, q *db.Queries, key canvas.Key, mutation canvas.Mutation) (canvas.Result, error) {
	tenant, canvasID := key.TenantID, key.CanvasID
	switch m := mutation.(type) {
	case canvas.SetDimensions:
		revision, err := q.SetCanvasDimensions(ctx, db.SetCanvasDimensionsParams{
			TenantID: tenant, CanvasID: canvasID,
			Width: int32(m.Dimensions.Width), Height: int32(m.Dimensions.Height),
		})
		return canvas.Result{Revision: revision}, err

	case canvas.CreateObject:
		id, err := newObjectID()
		if err != nil {
			return canvas.Result{}, err
		}
		return createObject(ctx, q, key, id, m)

	case canvas.SetContent:
		if err := requireObject(ctx, q, key, m.ObjectID); err != nil {
			return canvas.Result{}, err
		}
		if err := q.UpdateObjectContent(ctx, db.UpdateObjectContentParams{
			TenantID: tenant, CanvasID: canvasID, ObjectID: m.ObjectID, Content: m.Content,
		}); err != nil {
			return canvas.Result{}, err
		}
		revision, err := q.BumpRevision(ctx, db.BumpRevisionParams{TenantID: tenant, CanvasID: canvasID})
		return canvas.Result{Revision: revision, ObjectID: m.ObjectID}, err

	case canvas.SetTransform:
		if err := requireObject(ctx, q, key, m.ObjectID); err != nil {
			return canvas.Result{}, err
		}
		top, err := q.TakeTopZ(ctx, db.TakeTopZParams{TenantID: tenant, CanvasID: canvasID})
		if err != nil {
			return canvas.Result{}, err
		}
		t := m.Transform
		err = q.UpdateObjectTransform(ctx, db.UpdateObjectTransformParams{
			TenantID: tenant, CanvasID: canvasID, ObjectID: m.ObjectID,
			X: int32(t.X), Y: int32(t.Y), Width: int32(t.Width), Height: int32(t.Height),
			Rotation: t.Rotation, Z: top.Z,
		})
		return canvas.Result{Revision: top.Revision, ObjectID: m.ObjectID, Z: top.Z}, err

	case canvas.DeleteObject:
		if err := requireObject(ctx, q, key, m.ObjectID); err != nil {
			return canvas.Result{}, err
		}
		if err := q.DeleteObject(ctx, db.DeleteObjectParams{TenantID: tenant, CanvasID: canvasID, ObjectID: m.ObjectID}); err != nil {
			return canvas.Result{}, err
		}
		revision, err := q.BumpRevision(ctx, db.BumpRevisionParams{TenantID: tenant, CanvasID: canvasID})
		return canvas.Result{Revision: revision, ObjectID: m.ObjectID}, err

	default:
		return canvas.Result{}, fmt.Errorf("%w: unsupported mutation %T", canvas.ErrInvalid, mutation)
	}
}

// createObject inserts object id above every object of the canvas and bumps
// its revision, inside the transaction that holds the canvas lock.
func createObject(ctx context.Context, q *db.Queries, key canvas.Key, id string, m canvas.CreateObject) (canvas.Result, error) {
	top, err := q.TakeTopZ(ctx, db.TakeTopZParams{TenantID: key.TenantID, CanvasID: key.CanvasID})
	if err != nil {
		return canvas.Result{}, err
	}
	t := m.Transform
	err = q.InsertObject(ctx, db.InsertObjectParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID, ObjectID: id,
		Kind: string(m.Kind), Content: m.Content,
		X: int32(t.X), Y: int32(t.Y), Width: int32(t.Width), Height: int32(t.Height),
		Rotation: t.Rotation, Z: top.Z,
	})
	return canvas.Result{Revision: top.Revision, ObjectID: id, Z: top.Z}, err
}

// insertMutationRecord writes the outbox record of a committed mutation, in
// the transaction that committed it.
func insertMutationRecord(ctx context.Context, q *db.Queries, key canvas.Key, operation canvas.Operation, kind string, payload []byte, result canvas.Result, now time.Time) error {
	return q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID, Revision: result.Revision,
		OperationID:   operation.ID,
		CorrelationID: pgtype.Text{String: operation.CorrelationID, Valid: operation.CorrelationID != ""},
		Kind:          kind, Mutation: payload,
		ObjectID: result.ObjectID, Z: result.Z,
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	})
}

// requireObject reports ErrNotFound for an object that is missing from this
// canvas, including one that exists only in another tenant.
func requireObject(ctx context.Context, q *db.Queries, key canvas.Key, objectID string) error {
	exists, err := q.ObjectExists(ctx, db.ObjectExistsParams{TenantID: key.TenantID, CanvasID: key.CanvasID, ObjectID: objectID})
	if err != nil {
		return err
	}
	if !exists {
		return canvas.ErrNotFound
	}
	return nil
}

func newObjectID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

// wrap keeps domain errors comparable and labels storage failures.
func wrap(action string, err error) error {
	for _, domain := range []error{canvas.ErrNotFound, canvas.ErrInvalid, canvas.ErrOperationConflict, canvas.ErrInactive} {
		if errors.Is(err, domain) {
			return err
		}
	}
	return fmt.Errorf("postgres: %s: %w", action, err)
}
