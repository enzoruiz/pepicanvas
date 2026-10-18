-- name: GrantOperator :exec
INSERT INTO operators (account_id, granted_at)
VALUES ($1, $2)
ON CONFLICT (account_id) DO NOTHING;

-- name: GetOperator :one
SELECT account_id FROM operators WHERE account_id = $1;

-- name: LockOperator :one
-- The grant stays locked until commit, so a revocation waits for the activation.
SELECT account_id FROM operators WHERE account_id = $1 FOR SHARE;

-- name: LockAccountByEmail :one
SELECT account_id FROM accounts WHERE email = $1 FOR UPDATE;

-- name: GetOwnedTenant :one
SELECT tenant_id, canvas_id, subscription_status
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
