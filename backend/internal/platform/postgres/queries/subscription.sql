-- name: GrantOperator :exec
INSERT INTO operators (account_id, granted_at)
VALUES ($1, $2)
ON CONFLICT (account_id) DO NOTHING;

-- name: GetOperator :one
SELECT account_id FROM operators WHERE account_id = $1;

-- name: LockOperator :one
-- The grant stays locked until commit, so a revocation waits for the activation.
SELECT account_id FROM operators WHERE account_id = $1 FOR SHARE;

-- name: LockOperation :exec
-- Activations and deactivations share one operation ID space; the lock is
-- released at commit or rollback.
SELECT pg_advisory_xact_lock(hashtextextended('subscription-operation:' || sqlc.arg(operation_id)::text, 0));

-- name: LockAccountByEmail :one
SELECT account_id FROM accounts WHERE email = $1 FOR UPDATE;

-- name: GetOwnedTenant :one
SELECT tenant_id, canvas_id, subscription_status, access_version
FROM tenants
WHERE owner_account_id = $1;

-- name: InsertOwnedTenant :exec
INSERT INTO tenants (tenant_id, subscription_status, owner_account_id, canvas_id)
VALUES ($1, $2, $3, $4);

-- name: InsertMembership :exec
INSERT INTO memberships (tenant_id, account_id, role)
VALUES ($1, $2, $3);

-- name: SetTenantStatus :exec
UPDATE tenants SET subscription_status = $2 WHERE tenant_id = $1;

-- name: GetActivation :one
SELECT fingerprint, account_id, tenant_id, canvas_id, created, correlation_id, accepted_at
FROM subscription_activations
WHERE operation_id = $1 AND accepted_at > sqlc.arg(not_before);

-- name: DeleteExpiredActivation :exec
DELETE FROM subscription_activations
WHERE operation_id = $1 AND accepted_at <= sqlc.arg(expired_at);

-- name: PurgeExpiredActivations :exec
-- Rows locked by a concurrent purge are left for the next one.
DELETE FROM subscription_activations
WHERE operation_id IN (
    SELECT a.operation_id FROM subscription_activations a
    WHERE a.accepted_at <= sqlc.arg(expired_at)
    FOR UPDATE SKIP LOCKED
);

-- name: InsertActivation :exec
INSERT INTO subscription_activations (operation_id, fingerprint, operator_account_id, account_id, tenant_id, canvas_id, created, correlation_id, accepted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: DeactivateTenant :one
-- The row lock conflicts with the FOR SHARE read of every canvas mutation,
-- so mutations and deactivations of a tenant commit one at a time.
UPDATE tenants
SET subscription_status = 'inactive', access_version = access_version + 1
WHERE tenant_id = $1 AND subscription_status = 'active'
RETURNING access_version;

-- name: InsertAccessCut :exec
INSERT INTO access_cuts (tenant_id, version, correlation_id, created_at)
VALUES ($1, $2, $3, $4);

-- name: ListPendingCuts :many
SELECT id, tenant_id, version, correlation_id, created_at
FROM access_cuts
WHERE applied_at IS NULL
ORDER BY id
LIMIT $1;

-- name: SettleCut :exec
UPDATE access_cuts SET applied_at = sqlc.arg(applied_at)
WHERE id = $1 AND applied_at IS NULL;

-- name: GetDeactivation :one
SELECT fingerprint, operator_account_id, account_id, tenant_id, canvas_id, deactivated, version, correlation_id, accepted_at
FROM subscription_deactivations
WHERE operation_id = $1 AND accepted_at > sqlc.arg(not_before);

-- name: DeleteExpiredDeactivation :exec
DELETE FROM subscription_deactivations
WHERE operation_id = $1 AND accepted_at <= sqlc.arg(expired_at);

-- name: PurgeExpiredDeactivations :exec
-- Rows locked by a concurrent purge are left for the next one.
DELETE FROM subscription_deactivations
WHERE operation_id IN (
    SELECT d.operation_id FROM subscription_deactivations d
    WHERE d.accepted_at <= sqlc.arg(expired_at)
    FOR UPDATE SKIP LOCKED
);

-- name: InsertDeactivation :exec
INSERT INTO subscription_deactivations (operation_id, fingerprint, operator_account_id, account_id, tenant_id, canvas_id, deactivated, version, correlation_id, accepted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);
