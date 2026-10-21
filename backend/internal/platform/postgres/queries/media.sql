-- name: EnsureMediaUsage :exec
INSERT INTO media_usage (tenant_id, canvas_id)
SELECT c.tenant_id, c.canvas_id
FROM canvases c
WHERE c.tenant_id = $1 AND c.canvas_id = $2
ON CONFLICT (tenant_id, canvas_id) DO NOTHING;

-- name: LockMediaUsage :one
SELECT quota_bytes, used_bytes, reserved_bytes
FROM media_usage
WHERE tenant_id = $1 AND canvas_id = $2
FOR UPDATE;

-- name: SetMediaUsage :exec
UPDATE media_usage
SET used_bytes = $3, reserved_bytes = $4
WHERE tenant_id = $1 AND canvas_id = $2;

-- name: ExpireReservations :one
WITH expired AS (
    UPDATE media_assets
    SET state = 'deleting'
    WHERE tenant_id = $1 AND canvas_id = $2 AND state = 'pending' AND reserved_at <= sqlc.arg(expired_at)
    RETURNING declared_bytes
)
SELECT COALESCE(SUM(declared_bytes), 0)::bigint AS released FROM expired;

-- name: CanvasesWithExpiredReservations :many
SELECT DISTINCT tenant_id, canvas_id
FROM media_assets
WHERE state = 'pending' AND reserved_at <= sqlc.arg(expired_at)
LIMIT sqlc.arg(max_canvases);

-- name: InsertPendingAsset :exec
INSERT INTO media_assets (tenant_id, canvas_id, asset_id, state, declared_bytes, storage_key, replaces_reference, reserved_at)
VALUES ($1, $2, $3, 'pending', $4, $5, $6, $7);

-- name: LockAsset :one
SELECT state, kind, declared_bytes, storage_key, replaces_reference
FROM media_assets
WHERE tenant_id = $1 AND canvas_id = $2 AND asset_id = $3
FOR UPDATE;

-- name: SetAssetState :exec
UPDATE media_assets
SET state = $4, kind = COALESCE(sqlc.narg(kind), kind)
WHERE tenant_id = $1 AND canvas_id = $2 AND asset_id = $3;

-- name: GetReadyAsset :one
SELECT a.kind, a.declared_bytes, a.storage_key,
    (SELECT count(*) FROM media_references r
     WHERE r.tenant_id = a.tenant_id AND r.canvas_id = a.canvas_id AND r.asset_id = a.asset_id)::bigint AS refs
FROM media_assets a
WHERE a.tenant_id = $1 AND a.canvas_id = $2 AND a.asset_id = $3 AND a.state = 'ready';

-- name: AssetExists :one
SELECT EXISTS (
    SELECT 1 FROM media_assets WHERE tenant_id = $1 AND canvas_id = $2 AND asset_id = $3
);

-- name: ListGarbage :many
SELECT tenant_id, canvas_id, asset_id, storage_key
FROM media_assets
WHERE state IN ('quarantined', 'deleting')
ORDER BY tenant_id, canvas_id, asset_id
LIMIT sqlc.arg(max_assets);

-- name: PurgeAsset :execrows
DELETE FROM media_assets
WHERE tenant_id = $1 AND canvas_id = $2 AND asset_id = $3 AND state IN ('quarantined', 'deleting');

-- name: GetReference :one
SELECT asset_id
FROM media_references
WHERE tenant_id = $1 AND canvas_id = $2 AND reference_id = $3;

-- name: InsertReference :execrows
INSERT INTO media_references (tenant_id, canvas_id, reference_id, asset_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (tenant_id, canvas_id, reference_id) DO NOTHING;

-- name: MoveReference :exec
UPDATE media_references
SET asset_id = $4
WHERE tenant_id = $1 AND canvas_id = $2 AND reference_id = $3;

-- name: DeleteReference :one
DELETE FROM media_references
WHERE tenant_id = $1 AND canvas_id = $2 AND reference_id = $3
RETURNING asset_id;

-- name: CountReferences :one
SELECT count(*)::bigint
FROM media_references
WHERE tenant_id = $1 AND canvas_id = $2 AND asset_id = $3;

-- name: PurgeExpiredMediaOperations :exec
DELETE FROM media_operations
WHERE tenant_id = $1 AND canvas_id = $2 AND accepted_at <= sqlc.arg(expired_at);

-- name: GetMediaOperation :one
SELECT fingerprint, asset_id, storage_key, quota_bytes, used_bytes, reserved_bytes
FROM media_operations
WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3;

-- name: InsertMediaOperation :exec
INSERT INTO media_operations (tenant_id, canvas_id, operation_id, fingerprint, asset_id, storage_key, quota_bytes, used_bytes, reserved_bytes, correlation_id, accepted_at, abandon_reason, rejection_code)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: GetAbandonOperation :one
SELECT asset_id, abandon_reason, rejection_code
FROM media_operations
WHERE tenant_id = $1 AND canvas_id = $2 AND operation_id = $3
    AND abandon_reason IS NOT NULL AND accepted_at > sqlc.arg(expired_at);

-- name: CanvasExists :one
SELECT EXISTS (
    SELECT 1 FROM canvases WHERE tenant_id = $1 AND canvas_id = $2
);
