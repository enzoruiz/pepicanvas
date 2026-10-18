-- +goose Up
CREATE TABLE canvases (
    tenant_id text NOT NULL CHECK (tenant_id <> '' AND octet_length(tenant_id) <= 128),
    canvas_id text NOT NULL CHECK (canvas_id <> '' AND octet_length(canvas_id) <= 128),
    width integer NOT NULL DEFAULT 1920 CHECK (width BETWEEN 320 AND 3840),
    height integer NOT NULL DEFAULT 1080 CHECK (height BETWEEN 180 AND 2160),
    revision bigint NOT NULL DEFAULT 0 CHECK (revision >= 0),
    next_z bigint NOT NULL DEFAULT 1 CHECK (next_z >= 1),
    PRIMARY KEY (tenant_id, canvas_id)
);

CREATE TABLE canvas_objects (
    tenant_id text NOT NULL,
    canvas_id text NOT NULL,
    object_id text NOT NULL CHECK (object_id <> '' AND octet_length(object_id) <= 128),
    kind text NOT NULL CHECK (kind IN ('text', 'image', 'video', 'audio')),
    content text NOT NULL CHECK (octet_length(content) <= 4096),
    x integer NOT NULL,
    y integer NOT NULL,
    width integer NOT NULL CHECK (width > 0),
    height integer NOT NULL CHECK (height > 0),
    rotation double precision NOT NULL CHECK (rotation >= 0 AND rotation < 360),
    z bigint NOT NULL CHECK (z >= 1),
    PRIMARY KEY (tenant_id, canvas_id, object_id),
    UNIQUE (tenant_id, canvas_id, z),
    FOREIGN KEY (tenant_id, canvas_id) REFERENCES canvases (tenant_id, canvas_id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE canvas_objects;
DROP TABLE canvases;
