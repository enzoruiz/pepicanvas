-- name: GetMemberGrant :one
-- The membership, its tenant's subscription, and the tenant's confirmed
-- access version come from one snapshot (US-020).
SELECT m.role, t.subscription_status, t.access_version
FROM memberships m
JOIN tenants t ON t.tenant_id = m.tenant_id
WHERE m.account_id = $1 AND m.tenant_id = $2;
