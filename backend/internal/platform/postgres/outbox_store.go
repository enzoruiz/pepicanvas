package postgres

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/enzoruiz/pepicanvas/backend/internal/canvas"
	"github.com/enzoruiz/pepicanvas/backend/internal/outbox"
	"github.com/enzoruiz/pepicanvas/backend/internal/platform/postgres/db"
	"github.com/enzoruiz/pepicanvas/backend/internal/resync"
)

// MaxOutboxPage bounds the records one outbox query returns.
const MaxOutboxPage = 1000

// OutboxStore reads and settles the outbox records CanvasStore writes.
type OutboxStore struct {
	pool *pgxpool.Pool
}

var _ outbox.Store = (*OutboxStore)(nil)

// NewOutboxStore returns an outbox store over a migrated pool.
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

// Pending returns up to limit undelivered records of one canvas, oldest
// first. Records of other tenants are never returned.
func (s *OutboxStore) Pending(ctx context.Context, key canvas.Key, limit int) ([]outbox.Record, error) {
	if err := canvas.ValidateKey(key); err != nil {
		return nil, err
	}
	rows, err := db.New(s.pool).ListPendingOutbox(ctx, db.ListPendingOutboxParams{
		TenantID: key.TenantID, CanvasID: key.CanvasID, MaxRecords: pageSize(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list pending outbox: %w", err)
	}
	records := make([]outbox.Record, 0, len(rows))
	for _, row := range rows {
		record, err := toRecord(row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// Heads returns the oldest pending record of up to limit canvases, the
// records due soonest first. A canvas whose head backs off never hides the
// heads of other canvases.
func (s *OutboxStore) Heads(ctx context.Context, limit int) ([]outbox.Record, error) {
	rows, err := db.New(s.pool).ListOutboxHeads(ctx, pageSize(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: list outbox heads: %w", err)
	}
	records := make([]outbox.Record, 0, len(rows))
	for _, row := range rows {
		record, err := toRecord(db.ListPendingOutboxRow(row))
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

// Settle marks a pending record delivered or rejected at at. A non-empty
// reason replaces the last error. Settled records are left unchanged.
func (s *OutboxStore) Settle(ctx context.Context, id int64, outcome outbox.Outcome, at time.Time, reason string) error {
	if outcome != outbox.OutcomeDelivered && outcome != outbox.OutcomeRejected {
		return fmt.Errorf("postgres: settle outbox record %d: unknown outcome %q", id, outcome)
	}
	_, err := db.New(s.pool).SettleOutbox(ctx, db.SettleOutboxParams{
		SettledAt: pgtype.Timestamptz{Time: at, Valid: true},
		Outcome:   pgtype.Text{String: string(outcome), Valid: true},
		Reason:    reason,
		ID:        id,
	})
	if err != nil {
		return fmt.Errorf("postgres: settle outbox record %d: %w", id, err)
	}
	return nil
}

// Retry records a failed delivery of a pending record: its attempt count,
// the earliest next attempt, and the error. Settled records are left
// unchanged.
func (s *OutboxStore) Retry(ctx context.Context, id int64, attempts int, next time.Time, reason string) error {
	if attempts < 0 || attempts > math.MaxInt32 {
		return fmt.Errorf("postgres: retry outbox record %d: attempts %d out of range", id, attempts)
	}
	_, err := db.New(s.pool).RetryOutbox(ctx, db.RetryOutboxParams{
		Attempts:      int32(attempts),
		NextAttemptAt: pgtype.Timestamptz{Time: next, Valid: true},
		Reason:        reason,
		ID:            id,
	})
	if err != nil {
		return fmt.Errorf("postgres: retry outbox record %d: %w", id, err)
	}
	return nil
}

// Purge deletes up to limit records settled at or before cutoff, the oldest
// settled first, and returns how many it deleted. Pending records are never
// deleted.
func (s *OutboxStore) Purge(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	deleted, err := db.New(s.pool).PurgeSettledOutbox(ctx, db.PurgeSettledOutboxParams{
		Cutoff:     pgtype.Timestamptz{Time: cutoff, Valid: true},
		MaxRecords: pageSize(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("postgres: purge outbox: %w", err)
	}
	return int(deleted), nil
}

// Stats describes the outbox records of one tenant. The tenant ID follows
// the identifier rules of canvas keys; an invalid one is rejected with
// canvas.ErrInvalid.
func (s *OutboxStore) Stats(ctx context.Context, tenantID string) (outbox.Stats, error) {
	// The placeholder canvas ID is valid, so only the tenant ID is checked.
	if err := canvas.ValidateKey(canvas.Key{TenantID: tenantID, CanvasID: "c"}); err != nil {
		return outbox.Stats{}, err
	}
	row, err := db.New(s.pool).TenantOutboxStats(ctx, tenantID)
	if err != nil {
		return outbox.Stats{}, fmt.Errorf("postgres: tenant outbox stats: %w", err)
	}
	return toStats(db.AllOutboxStatsRow(row)), nil
}

// AllStats describes the outbox records of every tenant, for operators.
func (s *OutboxStore) AllStats(ctx context.Context) (outbox.Stats, error) {
	row, err := db.New(s.pool).AllOutboxStats(ctx)
	if err != nil {
		return outbox.Stats{}, fmt.Errorf("postgres: outbox stats: %w", err)
	}
	return toStats(row), nil
}

func toStats(row db.AllOutboxStatsRow) outbox.Stats {
	stats := outbox.Stats{
		Pending:         int(row.Pending),
		Retrying:        int(row.Retrying),
		PendingAttempts: int(row.PendingAttempts),
		Rejected:        int(row.Rejected),
	}
	if row.OldestPending.Valid {
		stats.OldestPending = row.OldestPending.Time
	}
	return stats
}

// toRecord decodes a stored record. Head rows convert to the pending row
// type, so both queries must keep the same columns.
func toRecord(row db.ListPendingOutboxRow) (outbox.Record, error) {
	mutation, err := outbox.DecodeMutation(row.Kind, row.Mutation)
	if err != nil {
		return outbox.Record{}, fmt.Errorf("postgres: outbox record %d: %w", row.ID, err)
	}
	return outbox.Record{
		ID: row.ID,
		Event: resync.Event{
			Key:           canvas.Key{TenantID: row.TenantID, CanvasID: row.CanvasID},
			Mutation:      mutation,
			Result:        canvas.Result{Revision: row.Revision, ObjectID: row.ObjectID, Z: row.Z},
			CorrelationID: row.CorrelationID.String,
		},
		CreatedAt:   row.CreatedAt.Time,
		Attempts:    int(row.Attempts),
		NextAttempt: row.NextAttemptAt.Time,
		LastError:   row.LastError.String,
	}, nil
}

// pageSize clamps a requested page to [1, MaxOutboxPage].
func pageSize(limit int) int32 {
	return int32(min(max(limit, 1), MaxOutboxPage))
}
