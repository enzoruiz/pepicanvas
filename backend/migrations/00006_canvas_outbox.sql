-- +goose Up
-- Version 00004 is reserved for the US-002 activation migration.
CREATE TABLE canvas_outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id text NOT NULL,
    canvas_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision >= 1),
    operation_id text NOT NULL CHECK (operation_id <> '' AND octet_length(operation_id) <= 128),
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    kind text NOT NULL CHECK (kind IN ('set_dimensions', 'create_object', 'set_content', 'set_transform', 'delete_object')),
    mutation jsonb NOT NULL,
    object_id text NOT NULL CHECK (octet_length(object_id) <= 128),
    z bigint NOT NULL CHECK (z >= 0),
    created_at timestamptz NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL,
    last_error text CHECK (octet_length(last_error) <= 512),
    settled_at timestamptz,
    outcome text CHECK (outcome IN ('delivered', 'rejected')),
    CHECK ((settled_at IS NULL) = (outcome IS NULL)),
    UNIQUE (tenant_id, canvas_id, revision),
    FOREIGN KEY (tenant_id, canvas_id) REFERENCES canvases (tenant_id, canvas_id) ON DELETE CASCADE
);

-- Pending records, oldest first per canvas.
CREATE INDEX canvas_outbox_pending ON canvas_outbox (tenant_id, canvas_id, id) WHERE settled_at IS NULL;
-- Settled records, oldest first, for the bounded purge.
CREATE INDEX canvas_outbox_settled ON canvas_outbox (settled_at) WHERE settled_at IS NOT NULL;

-- +goose Down
DROP TABLE canvas_outbox;
