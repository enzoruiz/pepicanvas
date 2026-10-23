-- name: PurgeExpiredOperations :exec
DELETE FROM canvas_operations
WHERE tenant_id = $1 AND canvas_id = $2 AND accepted_at <= sqlc.arg(expired_at);

-- name: GetOperation :one
SELECT fingerprint, revision, object_id, z, accepted_at
FROM canvas_operations
WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3;

-- name: InsertOperation :exec
INSERT INTO canvas_operations (tenant_id, canvas_id, operation_id, fingerprint, revision, object_id, z, correlation_id, accepted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);
