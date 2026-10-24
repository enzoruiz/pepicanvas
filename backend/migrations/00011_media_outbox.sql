-- +goose Up
-- Media usage changes reach the realtime actor through the outbox (US-016,
-- CTRL-CONS-01). Every committed change of a canvas's used or reserved bytes
-- bumps its usage version and writes one 'media_usage' record in the same
-- transaction, under the quota row lock. Such a record has no canvas
-- revision and names no object; its payload is the aggregate usage and its
-- version, which is unique per canvas.
ALTER TABLE media_usage ADD COLUMN version bigint NOT NULL DEFAULT 0 CHECK (version >= 0);

ALTER TABLE canvas_outbox ALTER COLUMN revision DROP NOT NULL;
ALTER TABLE canvas_outbox DROP CONSTRAINT canvas_outbox_kind_check;
ALTER TABLE canvas_outbox ADD CONSTRAINT canvas_outbox_kind_check
    CHECK (kind IN ('set_dimensions', 'create_object', 'set_content', 'set_transform', 'delete_object', 'media_usage'));
ALTER TABLE canvas_outbox ADD CONSTRAINT canvas_outbox_media_usage_check CHECK (CASE
    WHEN kind = 'media_usage' THEN revision IS NULL AND object_id = '' AND z = 0
    ELSE revision IS NOT NULL
END);

CREATE UNIQUE INDEX canvas_outbox_media_usage_version
    ON canvas_outbox (tenant_id, canvas_id, ((mutation ->> 'version')::bigint))
    WHERE kind = 'media_usage';

-- +goose Down
-- Rolling back would lose the undelivered usage records and the usage
-- versions consumers compare against, so it is refused while any exist.
-- +goose StatementBegin
DO $$
DECLARE
    stored bigint;
BEGIN
    SELECT count(*) INTO stored FROM canvas_outbox WHERE kind = 'media_usage';
    IF stored > 0 THEN
        RAISE EXCEPTION '00011_media_outbox down refused: % media usage record(s) would be lost', stored;
    END IF;
    SELECT count(*) INTO stored FROM media_usage WHERE version > 0;
    IF stored > 0 THEN
        RAISE EXCEPTION '00011_media_outbox down refused: % canvas usage version(s) would be lost', stored;
    END IF;
END
$$;
-- +goose StatementEnd
DROP INDEX canvas_outbox_media_usage_version;
ALTER TABLE canvas_outbox DROP CONSTRAINT canvas_outbox_media_usage_check;
ALTER TABLE canvas_outbox DROP CONSTRAINT canvas_outbox_kind_check;
ALTER TABLE canvas_outbox ADD CONSTRAINT canvas_outbox_kind_check
    CHECK (kind IN ('set_dimensions', 'create_object', 'set_content', 'set_transform', 'delete_object'));
ALTER TABLE canvas_outbox ALTER COLUMN revision SET NOT NULL;
ALTER TABLE media_usage DROP COLUMN version;
