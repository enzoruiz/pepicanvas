-- +goose Up
CREATE TABLE accounts (
    account_id text PRIMARY KEY CHECK (account_id ~ '^[0-9a-f]{32}$'),
    email text NOT NULL CHECK (email <> '' AND octet_length(email) <= 254 AND email = lower(email)),
    password_hash text NOT NULL CHECK (password_hash <> '' AND octet_length(password_hash) <= 512),
    created_at timestamptz NOT NULL,
    CONSTRAINT accounts_email_key UNIQUE (email)
);

-- Registration operation IDs replay their account while retained (CTRL-IDEM-01).
-- Only the SHA-256 of the normalized email is kept, never a password digest.
CREATE TABLE account_registrations (
    operation_id text PRIMARY KEY CHECK (operation_id ~ '^[A-Za-z0-9_-]{16,128}$'),
    email_digest bytea NOT NULL CHECK (octet_length(email_digest) = 32),
    account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    accepted_at timestamptz NOT NULL
);

CREATE INDEX account_registrations_accepted_at ON account_registrations (accepted_at);

-- Sessions are stored by the SHA-256 of their token; the token itself never is.
CREATE TABLE sessions (
    token_hash bytea PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL CHECK (last_seen_at >= created_at)
);

CREATE INDEX sessions_account_id ON sessions (account_id);

-- Access facts read by the canvas request check. US-002 owns activation,
-- US-005 owns accepted moderation; a membership row means an accepted role.
CREATE TABLE tenants (
    tenant_id text PRIMARY KEY CHECK (tenant_id <> '' AND octet_length(tenant_id) <= 128),
    subscription_status text NOT NULL CHECK (subscription_status IN ('active', 'inactive'))
);

CREATE TABLE memberships (
    tenant_id text NOT NULL REFERENCES tenants (tenant_id) ON DELETE CASCADE,
    account_id text NOT NULL REFERENCES accounts (account_id) ON DELETE CASCADE,
    role text NOT NULL CHECK (role IN ('owner', 'moderator')),
    PRIMARY KEY (tenant_id, account_id)
);

CREATE INDEX memberships_account_id ON memberships (account_id);

-- +goose Down
DROP TABLE memberships;
DROP TABLE tenants;
DROP TABLE sessions;
DROP TABLE account_registrations;
DROP TABLE accounts;
