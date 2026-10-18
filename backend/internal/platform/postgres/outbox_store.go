package postgres

import (
	"context"
	"fmt"

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
		record, err := toRecord(row.ID, canvas.Key{TenantID: row.TenantID, CanvasID: row.CanvasID}, row)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func toRecord(id int64, key canvas.Key, row db.ListPendingOutboxRow) (outbox.Record, error) {
	mutation, err := outbox.DecodeMutation(row.Kind, row.Mutation)
	if err != nil {
		return outbox.Record{}, fmt.Errorf("postgres: outbox record %d: %w", id, err)
	}
	return outbox.Record{
		ID: id,
		Event: resync.Event{
			Key:           key,
			Mutation:      mutation,
			Result:        canvas.Result{Revision: row.Revision, ObjectID: row.ObjectID, Z: row.Z},
			CorrelationID: row.CorrelationID.String,
		},
		CreatedAt:   row.CreatedAt.Time,
		Attempts:    int(row.Attempts),
		NextAttempt: row.NextAttemptAt.Time,
	}, nil
}

// pageSize clamps a requested page to [1, MaxOutboxPage].
func pageSize(limit int) int32 {
	return int32(min(max(limit, 1), MaxOutboxPage))
}
