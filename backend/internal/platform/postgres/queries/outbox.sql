-- name: InsertOutboxEvent :exec
INSERT INTO canvas_outbox (tenant_id, canvas_id, revision, operation_id, correlation_id, kind, mutation, object_id, z, created_at, next_attempt_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10);

-- name: ListPendingOutbox :many
SELECT id, tenant_id, canvas_id, revision, correlation_id, kind, mutation, object_id, z, created_at, attempts, next_attempt_at, last_error
FROM canvas_outbox
WHERE tenant_id = $1 AND canvas_id = $2 AND settled_at IS NULL
ORDER BY id
LIMIT sqlc.arg(max_records);

-- name: ListOutboxHeads :many
SELECT h.id, h.tenant_id, h.canvas_id, h.revision, h.correlation_id, h.kind, h.mutation, h.object_id, h.z, h.created_at, h.attempts, h.next_attempt_at, h.last_error
FROM (
    SELECT DISTINCT ON (tenant_id, canvas_id) id, tenant_id, canvas_id, revision, correlation_id, kind, mutation, object_id, z, created_at, attempts, next_attempt_at, last_error
    FROM canvas_outbox
    WHERE settled_at IS NULL
    ORDER BY tenant_id, canvas_id, id
) h
ORDER BY h.next_attempt_at, h.id
LIMIT sqlc.arg(max_records);

-- name: SettleOutbox :execrows
UPDATE canvas_outbox
SET settled_at = sqlc.arg(settled_at), outcome = sqlc.arg(outcome),
    last_error = COALESCE(NULLIF(sqlc.arg(reason)::text, ''), last_error)
WHERE id = sqlc.arg(id) AND settled_at IS NULL;

-- name: RetryOutbox :execrows
UPDATE canvas_outbox
SET attempts = sqlc.arg(attempts), next_attempt_at = sqlc.arg(next_attempt_at),
    last_error = NULLIF(sqlc.arg(reason)::text, '')
WHERE id = sqlc.arg(id) AND settled_at IS NULL;

-- name: PurgeSettledOutbox :execrows
DELETE FROM canvas_outbox
WHERE id IN (
    SELECT s.id FROM canvas_outbox s
    WHERE s.settled_at IS NOT NULL AND s.settled_at <= sqlc.arg(cutoff)
    ORDER BY s.settled_at, s.id
    LIMIT sqlc.arg(max_records)
);

-- name: TenantOutboxStats :one
SELECT count(*) FILTER (WHERE settled_at IS NULL) AS pending,
    (min(created_at) FILTER (WHERE settled_at IS NULL))::timestamptz AS oldest_pending,
    count(*) FILTER (WHERE settled_at IS NULL AND attempts > 0) AS retrying,
    (COALESCE(sum(attempts) FILTER (WHERE settled_at IS NULL), 0))::bigint AS pending_attempts,
    count(*) FILTER (WHERE outcome = 'rejected') AS rejected
FROM canvas_outbox
WHERE tenant_id = sqlc.arg(tenant_id);

-- name: AllOutboxStats :one
SELECT count(*) FILTER (WHERE settled_at IS NULL) AS pending,
    (min(created_at) FILTER (WHERE settled_at IS NULL))::timestamptz AS oldest_pending,
    count(*) FILTER (WHERE settled_at IS NULL AND attempts > 0) AS retrying,
    (COALESCE(sum(attempts) FILTER (WHERE settled_at IS NULL), 0))::bigint AS pending_attempts,
    count(*) FILTER (WHERE outcome = 'rejected') AS rejected
FROM canvas_outbox;
