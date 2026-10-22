-- name: CreateCanvas :exec
INSERT INTO canvases (tenant_id, canvas_id)
VALUES ($1, $2)
ON CONFLICT (tenant_id, canvas_id) DO NOTHING;

-- name: GetCanvas :one
SELECT width, height, revision
FROM canvases
WHERE tenant_id = $1 AND canvas_id = $2;

-- name: LockCanvas :one
SELECT revision
FROM canvases
WHERE tenant_id = $1 AND canvas_id = $2
FOR UPDATE;

-- name: ListObjects :many
SELECT object_id, kind, content, x, y, width, height, rotation, z
FROM canvas_objects
WHERE tenant_id = $1 AND canvas_id = $2
ORDER BY z;

-- name: SetCanvasDimensions :one
UPDATE canvases
SET width = $3, height = $4, revision = revision + 1
WHERE tenant_id = $1 AND canvas_id = $2
RETURNING revision;

-- name: BumpRevision :one
UPDATE canvases
SET revision = revision + 1
WHERE tenant_id = $1 AND canvas_id = $2
RETURNING revision;

-- name: TakeTopZ :one
UPDATE canvases
SET revision = revision + 1, next_z = next_z + 1
WHERE tenant_id = $1 AND canvas_id = $2
RETURNING revision, (next_z - 1)::bigint AS z;

-- name: InsertObject :exec
INSERT INTO canvas_objects (tenant_id, canvas_id, object_id, kind, content, x, y, width, height, rotation, z)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: ObjectExists :one
SELECT EXISTS (
    SELECT 1 FROM canvas_objects
    WHERE tenant_id = $1 AND canvas_id = $2 AND object_id = $3
);

-- name: UpdateObjectContent :exec
UPDATE canvas_objects
SET content = $4
WHERE tenant_id = $1 AND canvas_id = $2 AND object_id = $3;

-- name: UpdateObjectTransform :exec
UPDATE canvas_objects
SET x = $4, y = $5, width = $6, height = $7, rotation = $8, z = $9
WHERE tenant_id = $1 AND canvas_id = $2 AND object_id = $3;

-- name: DeleteObject :exec
DELETE FROM canvas_objects
WHERE tenant_id = $1 AND canvas_id = $2 AND object_id = $3;

-- name: LockTenantStatus :one
-- Read after the canvas lock and held until commit, so a deactivation of the
-- tenant waits for the mutation, and a mutation after it sees it.
SELECT subscription_status FROM tenants WHERE tenant_id = $1 FOR SHARE;
