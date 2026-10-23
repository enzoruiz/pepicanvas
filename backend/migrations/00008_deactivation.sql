-- +goose Up
-- The confirmed version of a tenant's access facts (US-003). Every
-- deactivation increments it; live connections hold grants read at a version.
ALTER TABLE tenants
    ADD COLUMN access_version bigint NOT NULL DEFAULT 1 CONSTRAINT tenants_access_version_check CHECK (access_version >= 1);

-- Deactivation operation IDs replay their answer while retained (CTRL-IDEM-01).
-- The fingerprint is the SHA-256 of the action, the operator ID, and the
-- normalized email.
CREATE TABLE subscription_deactivations (
    operation_id text PRIMARY KEY CHECK (operation_id ~ '^[A-Za-z0-9_-]{16,128}$'),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    operator_account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    tenant_id text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    canvas_id text NOT NULL CHECK (canvas_id <> '' AND octet_length(canvas_id) <= 128),
    deactivated boolean NOT NULL,
    version bigint NOT NULL CHECK (version >= 1),
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    accepted_at timestamptz NOT NULL
);

CREATE INDEX subscription_deactivations_accepted_at ON subscription_deactivations (accepted_at);

-- Access cuts confirmed in the transaction that changes the access facts
-- (CTRL-CONS-01). A relay applies each pending cut to the live connections
-- and sets applied_at; a cut is never lost when the process stops first.
CREATE TABLE access_cuts (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    version bigint NOT NULL CHECK (version >= 1),
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    created_at timestamptz NOT NULL,
    applied_at timestamptz,
    CONSTRAINT access_cuts_tenant_version_key UNIQUE (tenant_id, version)
);

CREATE INDEX access_cuts_pending ON access_cuts (id) WHERE applied_at IS NULL;

-- +goose Down
-- Rolling back would lose deactivation answers and access cuts, which keep
-- stale grants from reconnecting, so it is refused while any exist.
-- +goose StatementBegin
DO $$
DECLARE
    records bigint;
    cuts bigint;
BEGIN
    SELECT count(*) INTO records FROM subscription_deactivations;
    IF records > 0 THEN
        RAISE EXCEPTION '00008_deactivation down refused: % deactivation record(s) would be lost', records;
    END IF;
    SELECT count(*) INTO cuts FROM access_cuts;
    IF cuts > 0 THEN
        RAISE EXCEPTION '00008_deactivation down refused: % access cut(s) would be lost', cuts;
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE access_cuts;
DROP TABLE subscription_deactivations;
ALTER TABLE tenants
    DROP CONSTRAINT tenants_access_version_check,
    DROP COLUMN access_version;
