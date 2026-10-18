-- name: DeleteExpiredRegistration :exec
DELETE FROM account_registrations
WHERE operation_id = $1 AND accepted_at <= sqlc.arg(expired_at);

-- name: PurgeExpiredRegistrations :exec
-- Rows locked by a concurrent purge are left for the next one.
DELETE FROM account_registrations
WHERE operation_id IN (
    SELECT r.operation_id FROM account_registrations r
    WHERE r.accepted_at <= sqlc.arg(expired_at)
    FOR UPDATE SKIP LOCKED
);

-- name: GetRegistration :one
SELECT operation_id, email_digest, account_id, accepted_at
FROM account_registrations
WHERE operation_id = $1 AND accepted_at > sqlc.arg(not_before);

-- name: InsertAccount :exec
INSERT INTO accounts (account_id, email, password_hash, created_at)
VALUES ($1, $2, $3, $4);

-- name: InsertRegistration :exec
INSERT INTO account_registrations (operation_id, email_digest, account_id, correlation_id, accepted_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetAccountByEmail :one
SELECT account_id, email, password_hash FROM accounts WHERE email = $1;

-- name: GetAccountByID :one
SELECT account_id, email, password_hash FROM accounts WHERE account_id = $1;

-- name: PurgeExpiredSessions :exec
DELETE FROM sessions
WHERE account_id = $1
  AND (last_seen_at <= sqlc.arg(idle_before) OR created_at <= sqlc.arg(created_before));

-- name: InsertSession :exec
INSERT INTO sessions (token_hash, account_id, created_at, last_seen_at)
VALUES ($1, $2, $3, $4);

-- name: GetSession :one
SELECT token_hash, account_id, created_at, last_seen_at FROM sessions WHERE token_hash = $1;

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = sqlc.arg(seen_at)
WHERE token_hash = $1 AND last_seen_at < sqlc.arg(seen_at);

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: GetMembership :one
SELECT m.role, t.subscription_status
FROM memberships m
JOIN tenants t ON t.tenant_id = m.tenant_id
WHERE m.account_id = $1 AND m.tenant_id = $2;
