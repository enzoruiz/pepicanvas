-- name: LockOwnedTenantShare :one
-- The tenant stays share-locked until commit, so its status cannot change
-- under an owner action.
SELECT tenant_id, subscription_status
FROM tenants
WHERE owner_account_id = $1
FOR SHARE;

-- name: LockTenantShare :one
SELECT tenant_id, owner_account_id, canvas_id, subscription_status
FROM tenants
WHERE tenant_id = $1
FOR SHARE;

-- name: GetInvitationOperation :one
SELECT kind, owner_account_id, fingerprint, tenant_id, invitation_id, created_at, expires_at, status, canvas_id, correlation_id, accepted_at
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
INSERT INTO invitation_operations (operation_id, kind, owner_account_id, fingerprint, tenant_id, invitation_id, created_at, expires_at, status, canvas_id, correlation_id, accepted_at)
SELECT sqlc.arg(operation_id), sqlc.arg(kind), sqlc.arg(owner_account_id), sqlc.arg(fingerprint), i.tenant_id, i.invitation_id,
    sqlc.narg(created_at), sqlc.narg(expires_at), sqlc.narg(status), sqlc.narg(canvas_id), sqlc.narg(correlation_id), sqlc.arg(accepted_at)
FROM invitations i
WHERE i.invitation_id = sqlc.arg(invitation_id);

-- name: ListModeratedCanvases :many
-- Every moderator membership of the account with its tenant's canvas and
-- status; the caller filters by status.
SELECT t.tenant_id, t.canvas_id, t.subscription_status
FROM memberships m
JOIN tenants t ON t.tenant_id = m.tenant_id
WHERE m.account_id = $1 AND m.role = 'moderator' AND t.canvas_id IS NOT NULL
ORDER BY t.tenant_id;

-- name: LockOwnedTenantUpdate :one
-- The row lock conflicts with the FOR SHARE read of every mutation of the
-- tenant, so mutations and membership revocations commit one at a time.
SELECT tenant_id, subscription_status
FROM tenants
WHERE owner_account_id = $1
FOR UPDATE;

-- name: ListTenantModerators :many
-- Each moderator with its email and the latest acceptance by the account of
-- an invitation of the tenant; NULL when none records one.
SELECT m.account_id, a.email, i.accepted_at
FROM memberships m
JOIN accounts a ON a.account_id = m.account_id
LEFT JOIN LATERAL (
    SELECT inv.accepted_at FROM invitations inv
    WHERE inv.tenant_id = m.tenant_id AND inv.accepted_by = m.account_id
    ORDER BY inv.accepted_at DESC
    LIMIT 1
) i ON true
WHERE m.tenant_id = $1 AND m.role = 'moderator'
ORDER BY i.accepted_at NULLS FIRST, m.account_id;

-- name: GetMembershipRevocation :one
SELECT owner_account_id, fingerprint, tenant_id, account_id, version, correlation_id, accepted_at
FROM membership_revocations
WHERE operation_id = $1 AND accepted_at > sqlc.arg(not_before);

-- name: LockMembership :one
SELECT role FROM memberships
WHERE tenant_id = $1 AND account_id = $2
FOR UPDATE;

-- name: DeleteModeratorMembership :execrows
DELETE FROM memberships
WHERE tenant_id = $1 AND account_id = $2 AND role = 'moderator';

-- name: IncrementAccessVersion :one
UPDATE tenants SET access_version = access_version + 1
WHERE tenant_id = $1
RETURNING access_version;

-- name: InsertRevocationCut :exec
INSERT INTO access_cuts (tenant_id, kind, account_id, version, correlation_id, created_at)
VALUES ($1, 'revocation', $2, $3, $4, $5);

-- name: DeleteExpiredMembershipRevocation :exec
DELETE FROM membership_revocations
WHERE operation_id = $1 AND accepted_at <= sqlc.arg(expired_at);

-- name: PurgeExpiredMembershipRevocations :exec
-- Rows locked by a concurrent purge are left for the next one.
DELETE FROM membership_revocations
WHERE operation_id IN (
    SELECT r.operation_id FROM membership_revocations r
    WHERE r.accepted_at <= sqlc.arg(expired_at)
    FOR UPDATE SKIP LOCKED
);

-- name: InsertMembershipRevocation :exec
INSERT INTO membership_revocations (operation_id, fingerprint, owner_account_id, tenant_id, account_id, version, correlation_id, accepted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: GetMemberAccess :one
-- The account's role with its tenant's status and access version, read in
-- one statement so the grant's version matches the facts it was read with.
SELECT m.role, t.subscription_status, t.access_version
FROM memberships m
JOIN tenants t ON t.tenant_id = m.tenant_id
WHERE m.tenant_id = $1 AND m.account_id = $2;
