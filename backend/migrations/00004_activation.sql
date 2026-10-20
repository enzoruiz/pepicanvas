-- +goose Up
-- Operators activate streamers platform-wide (US-002). A row is the grant.
CREATE TABLE operators (
    account_id text PRIMARY KEY REFERENCES accounts (account_id) ON DELETE CASCADE,
    granted_at timestamptz NOT NULL
);

-- A streamer is a tenant owned by one account with exactly one canvas. Tenants
-- created before US-002 keep both columns empty.
ALTER TABLE tenants
    ADD COLUMN owner_account_id text REFERENCES accounts (account_id) ON DELETE CASCADE,
    ADD COLUMN canvas_id text CHECK (canvas_id <> '' AND octet_length(canvas_id) <= 128),
    ADD CONSTRAINT tenants_owner_account_id_key UNIQUE (owner_account_id),
    ADD CONSTRAINT tenants_owner_canvas_check CHECK ((owner_account_id IS NULL) = (canvas_id IS NULL));

-- Activation operation IDs replay their answer while retained (CTRL-IDEM-01).
-- The fingerprint is the SHA-256 of the operator ID and the normalized email.
CREATE TABLE subscription_activations (
    operation_id text PRIMARY KEY CHECK (operation_id ~ '^[A-Za-z0-9_-]{16,128}$'),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    operator_account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    tenant_id text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    canvas_id text NOT NULL CHECK (canvas_id <> '' AND octet_length(canvas_id) <= 128),
    created boolean NOT NULL,
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    accepted_at timestamptz NOT NULL
);

CREATE INDEX subscription_activations_accepted_at ON subscription_activations (accepted_at);

-- +goose Down
-- Rolling back would drop the owner and canvas of activated tenants and every
-- operator grant, so it is refused while any exist; legacy rows are kept.
-- +goose StatementBegin
DO $$
DECLARE
    activated bigint;
    grants bigint;
BEGIN
    SELECT count(*) INTO activated FROM tenants WHERE owner_account_id IS NOT NULL;
    IF activated > 0 THEN
        RAISE EXCEPTION '00004_activation down refused: % activated tenant(s) would lose their owner and canvas', activated;
    END IF;
    SELECT count(*) INTO grants FROM operators;
    IF grants > 0 THEN
        RAISE EXCEPTION '00004_activation down refused: % operator grant(s) would be lost', grants;
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE subscription_activations;
ALTER TABLE tenants
    DROP CONSTRAINT tenants_owner_canvas_check,
    DROP CONSTRAINT tenants_owner_account_id_key,
    DROP COLUMN canvas_id,
    DROP COLUMN owner_account_id;
DROP TABLE operators;
