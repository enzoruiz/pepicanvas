-- name: InsertOutboxEvent :exec
INSERT INTO canvas_outbox (tenant_id, canvas_id, revision, operation_id, correlation_id, kind, mutation, object_id, z, created_at, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10);

-- name: ListPendingOutbox :many
SELECT id, tenant_id, canvas_id, revision, correlation_id, kind, mutation, object_id, z, created_at, attempts, next_attempt_at
FROM canvas_outbox
WHERE tenant_id = $1 AND canvas_id = $2 AND settled_at IS NULL
ORDER BY id
LIMIT sqlc.arg(max_records);
