-- name: LockOwnedTenantShare :one
-- The tenant stays share-locked until commit, so its status cannot change
-- under an owner action.
SELECT tenant_id, subscription_status
FROM tenants
WHERE owner_account_id = $1
FOR SHARE;

-- name: LockTenantShare :one
SELECT tenant_id, owner_account_id, subscription_status
FROM tenants
WHERE tenant_id = $1
FOR SHARE;

-- name: GetInvitationOperation :one
SELECT kind, owner_account_id, fingerprint, tenant_id, invitation_id, created_at, expires_at, status, correlation_id, accepted_at
FROM invitation_operations
WHERE operation_id = $1 AND accepted_at > sqlc.arg(not_before);

-- name: InsertInvitation :exec
INSERT INTO invitations (invitation_id, tenant_id, token_hash, created_by, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListPendingInvitations :many
SELECT * FROM invitations
WHERE tenant_id = $1 AND revoked_at IS NULL AND accepted_at IS NULL AND expires_at > sqlc.arg(now)
ORDER BY created_at, invitation_id;

-- name: LockInvitation :one
SELECT * FROM invitations
WHERE tenant_id = $1 AND invitation_id = $2
FOR UPDATE;

-- name: LockInvitationByToken :one
SELECT * FROM invitations
WHERE token_hash = $1
FOR UPDATE;

-- name: RevokeInvitation :execrows
UPDATE invitations SET revoked_at = sqlc.arg(revoked_at)
WHERE invitation_id = $1 AND revoked_at IS NULL AND accepted_at IS NULL;

-- name: MarkInvitationAccepted :execrows
UPDATE invitations SET accepted_at = sqlc.arg(accepted_at), accepted_by = sqlc.arg(accepted_by)
WHERE invitation_id = $1 AND revoked_at IS NULL AND accepted_at IS NULL;

-- name: IsTenantMember :one
SELECT EXISTS (SELECT 1 FROM memberships WHERE tenant_id = $1 AND account_id = $2);

-- name: DeleteExpiredInvitationOperation :exec
DELETE FROM invitation_operations
WHERE operation_id = $1 AND accepted_at <= sqlc.arg(expired_at);

-- name: PurgeExpiredInvitationOperations :exec
-- Rows locked by a concurrent purge are left for the next one.
DELETE FROM invitation_operations
WHERE operation_id IN (
    SELECT o.operation_id FROM invitation_operations o
    WHERE o.accepted_at <= sqlc.arg(expired_at)
    FOR UPDATE SKIP LOCKED
);

-- name: InsertInvitationOperation :execrows
-- The tenant is the invitation's, so a record never names another tenant.
INSERT INTO invitation_operations (operation_id, kind, owner_account_id, fingerprint, tenant_id, invitation_id, created_at, expires_at, status, correlation_id, accepted_at)
SELECT sqlc.arg(operation_id), sqlc.arg(kind), sqlc.arg(owner_account_id), sqlc.arg(fingerprint), i.tenant_id, i.invitation_id,
    sqlc.narg(created_at), sqlc.narg(expires_at), sqlc.narg(status), sqlc.narg(correlation_id), sqlc.arg(accepted_at)
FROM invitations i
WHERE i.invitation_id = sqlc.arg(invitation_id);
