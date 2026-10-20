-- +goose Up
-- One quota row per canvas. Every command locks it, so the accounting of a
-- canvas changes one transaction at a time, and the check keeps every
-- committed state within the inclusive quota.
CREATE TABLE media_usage (
    tenant_id text NOT NULL,
    canvas_id text NOT NULL,
    quota_bytes bigint NOT NULL DEFAULT 1000000000 CHECK (quota_bytes >= 0),
    used_bytes bigint NOT NULL DEFAULT 0 CHECK (used_bytes >= 0),
    reserved_bytes bigint NOT NULL DEFAULT 0 CHECK (reserved_bytes >= 0),
    CHECK (used_bytes + reserved_bytes <= quota_bytes),
    PRIMARY KEY (tenant_id, canvas_id),
    FOREIGN KEY (tenant_id, canvas_id) REFERENCES canvases (tenant_id, canvas_id)
);

-- Only 'ready' assets are referenced, served, or counted as used. 'pending'
-- assets hold a reservation; 'quarantined' and 'deleting' assets hold nothing
-- and wait for their stored bytes to be deleted.
CREATE TABLE media_assets (
    tenant_id text NOT NULL,
    canvas_id text NOT NULL,
    asset_id text NOT NULL CHECK (asset_id <> '' AND octet_length(asset_id) <= 128),
    state text NOT NULL CHECK (state IN ('pending', 'ready', 'quarantined', 'deleting')),
    kind text CHECK (kind IN ('image', 'gif', 'audio', 'video')),
    declared_bytes bigint NOT NULL CHECK (declared_bytes > 0),
    storage_key text NOT NULL UNIQUE CHECK (storage_key <> '' AND octet_length(storage_key) <= 256),
    replaces_reference text CHECK (replaces_reference <> '' AND octet_length(replaces_reference) <= 128),
    reserved_at timestamptz NOT NULL,
    CHECK (state <> 'ready' OR kind IS NOT NULL),
    PRIMARY KEY (tenant_id, canvas_id, asset_id),
    FOREIGN KEY (tenant_id, canvas_id) REFERENCES media_usage (tenant_id, canvas_id)
);

CREATE INDEX media_assets_pending ON media_assets (reserved_at) WHERE state = 'pending';
CREATE INDEX media_assets_garbage ON media_assets (tenant_id, canvas_id) WHERE state IN ('quarantined', 'deleting');

CREATE TABLE media_references (
    tenant_id text NOT NULL,
    canvas_id text NOT NULL,
    reference_id text NOT NULL CHECK (reference_id <> '' AND octet_length(reference_id) <= 128),
    asset_id text NOT NULL,
    PRIMARY KEY (tenant_id, canvas_id, reference_id),
    FOREIGN KEY (tenant_id, canvas_id, asset_id) REFERENCES media_assets (tenant_id, canvas_id, asset_id)
);

CREATE INDEX media_references_asset ON media_references (tenant_id, canvas_id, asset_id);

CREATE TABLE media_operations (
    tenant_id text NOT NULL,
    canvas_id text NOT NULL,
    operation_id text NOT NULL CHECK (operation_id <> '' AND octet_length(operation_id) <= 128),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    asset_id text NOT NULL CHECK (octet_length(asset_id) <= 128),
    storage_key text NOT NULL CHECK (octet_length(storage_key) <= 256),
    quota_bytes bigint NOT NULL,
    used_bytes bigint NOT NULL,
    reserved_bytes bigint NOT NULL,
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    accepted_at timestamptz NOT NULL,
    -- The outcome of an Abandon, so a retry of the upload repeats its response.
    abandon_reason text CHECK (abandon_reason IN ('cancelled', 'failed', 'rejected')),
    rejection_code text CHECK (rejection_code <> '' AND octet_length(rejection_code) <= 64),
    CHECK ((rejection_code IS NOT NULL) = (abandon_reason IS NOT DISTINCT FROM 'rejected')),
    PRIMARY KEY (tenant_id, canvas_id, operation_id),
    FOREIGN KEY (tenant_id, canvas_id) REFERENCES media_usage (tenant_id, canvas_id)
);

CREATE INDEX media_operations_accepted_at ON media_operations (tenant_id, canvas_id, accepted_at);

-- +goose Down
DROP TABLE media_operations;
DROP TABLE media_references;
DROP TABLE media_assets;
DROP TABLE media_usage;
