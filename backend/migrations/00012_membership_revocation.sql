-- +goose Up
-- A membership revocation (US-006) cuts one account in one tenant: its
-- access cut names the account. Deactivation cuts (US-003) name none, and
-- existing rows take the default kind. The relay maps each kind to its
-- registry target and leaves a kind it does not know pending, so a
-- revocation cut is never applied to the whole tenant.
ALTER TABLE access_cuts
    ADD COLUMN kind text NOT NULL DEFAULT 'deactivation' CONSTRAINT access_cuts_kind_check CHECK (kind IN ('deactivation', 'revocation')),
    ADD COLUMN account_id text REFERENCES accounts (account_id) ON DELETE CASCADE,
    ADD CONSTRAINT access_cuts_account_check CHECK ((kind = 'revocation') = (account_id IS NOT NULL));

-- Membership revocation operation IDs replay their answer while retained
-- (CTRL-IDEM-01). The fingerprint is the SHA-256 of 'revoke-member', the
-- owner ID, and the revoked account ID; version is the tenant's access
-- version after the revocation.
CREATE TABLE membership_revocations (
    operation_id text PRIMARY KEY CHECK (operation_id ~ '^[A-Za-z0-9_-]{16,128}$'),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    owner_account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    tenant_id text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    version bigint NOT NULL CHECK (version >= 1),
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    accepted_at timestamptz NOT NULL
);

CREATE INDEX membership_revocations_accepted_at ON membership_revocations (accepted_at);

-- +goose Down
-- Rolling back would lose revocation answers and the account of pending
-- revocation cuts, which keep a revoked moderator's stale grants from
-- reconnecting, so it is refused while any exist.
-- +goose StatementBegin
DO $$
DECLARE
    records bigint;
    cuts bigint;
BEGIN
    SELECT count(*) INTO records FROM membership_revocations;
    IF records > 0 THEN
        RAISE EXCEPTION '00012_membership_revocation down refused: % revocation record(s) would be lost', records;
    END IF;
    SELECT count(*) INTO cuts FROM access_cuts WHERE kind = 'revocation';
    IF cuts > 0 THEN
        RAISE EXCEPTION '00012_membership_revocation down refused: % revocation cut(s) would be lost', cuts;
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE membership_revocations;
ALTER TABLE access_cuts
    DROP CONSTRAINT access_cuts_account_check,
    DROP COLUMN account_id,
    DROP COLUMN kind;
