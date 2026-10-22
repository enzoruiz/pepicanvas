-- +goose Up
-- Moderator invitations (US-004). Only the SHA-256 of the pci_ token is kept,
-- never the token. An invitation expires exactly seven days (168 hours, so no
-- time zone shifts it) after its creation and ends revoked or accepted, never
-- both.
CREATE TABLE invitations (
    invitation_id text PRIMARY KEY CHECK (invitation_id ~ '^[0-9a-f]{32}$'),
    tenant_id text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    created_by text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    accepted_at timestamptz,
    accepted_by text REFERENCES accounts (account_id) ON DELETE CASCADE,
    CONSTRAINT invitations_token_hash_key UNIQUE (token_hash),
    CONSTRAINT invitations_expiry_check CHECK (expires_at = created_at + interval '168 hours'),
    CONSTRAINT invitations_outcome_check CHECK (NOT (revoked_at IS NOT NULL AND accepted_at IS NOT NULL)),
    CONSTRAINT invitations_accepted_by_check CHECK ((accepted_at IS NULL) = (accepted_by IS NULL))
);

CREATE INDEX invitations_tenant_id ON invitations (tenant_id);

-- Create and revoke operation IDs replay their answer while retained
-- (CTRL-IDEM-01). The fingerprint is the SHA-256 of the kind, the owner ID,
-- and the payload. A create keeps the invitation's times, a revoke its status.
CREATE TABLE invitation_operations (
    operation_id text PRIMARY KEY CHECK (operation_id ~ '^[A-Za-z0-9_-]{16,128}$'),
    kind text NOT NULL CHECK (kind IN ('create', 'revoke')),
    owner_account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    tenant_id text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    invitation_id text NOT NULL REFERENCES invitations (invitation_id) ON DELETE CASCADE,
    created_at timestamptz,
    expires_at timestamptz,
    status text CHECK (status IN ('revoked', 'accepted')),
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    accepted_at timestamptz NOT NULL,
    CONSTRAINT invitation_operations_answer_check CHECK (CASE kind
        WHEN 'create' THEN created_at IS NOT NULL AND expires_at IS NOT NULL AND status IS NULL
        ELSE created_at IS NULL AND expires_at IS NULL AND status IS NOT NULL
    END)
);

CREATE INDEX invitation_operations_accepted_at ON invitation_operations (accepted_at);

-- +goose Down
-- Rolling back would lose every invitation and the hash of its link, so it is
-- refused while any exist. Operation records cannot outlive their invitation.
-- +goose StatementBegin
DO $$
DECLARE
    stored bigint;
BEGIN
    SELECT count(*) INTO stored FROM invitations;
    IF stored > 0 THEN
        RAISE EXCEPTION '00009_invitations down refused: % invitation(s) would be lost', stored;
    END IF;
END
$$;
-- +goose StatementEnd
DROP TABLE invitation_operations;
DROP TABLE invitations;
