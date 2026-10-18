-- +goose Up
CREATE TABLE canvas_operations (
    tenant_id text NOT NULL,
    canvas_id text NOT NULL,
    operation_id text NOT NULL CHECK (operation_id <> '' AND octet_length(operation_id) <= 128),
    fingerprint bytea NOT NULL CHECK (octet_length(fingerprint) = 32),
    revision bigint NOT NULL CHECK (revision >= 1),
    object_id text NOT NULL CHECK (octet_length(object_id) <= 128),
    z bigint NOT NULL CHECK (z >= 0),
    correlation_id text CHECK (correlation_id <> '' AND octet_length(correlation_id) <= 128),
    accepted_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, canvas_id, operation_id),
    FOREIGN KEY (tenant_id, canvas_id) REFERENCES canvases (tenant_id, canvas_id) ON DELETE CASCADE
);

CREATE INDEX canvas_operations_accepted_at ON canvas_operations (tenant_id, canvas_id, accepted_at);

-- +goose Down
DROP TABLE canvas_operations;
