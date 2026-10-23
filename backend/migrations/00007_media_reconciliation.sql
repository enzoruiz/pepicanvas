-- +goose Up
-- When an asset entered its current state, for the inventory by state and
-- age. Existing assets entered their state no earlier than their reservation.
ALTER TABLE media_assets ADD COLUMN state_changed_at timestamptz;
UPDATE media_assets SET state_changed_at = reserved_at;
ALTER TABLE media_assets ALTER COLUMN state_changed_at SET NOT NULL;

CREATE INDEX media_assets_inventory ON media_assets (tenant_id, state, state_changed_at);

-- +goose Down
DROP INDEX media_assets_inventory;
ALTER TABLE media_assets DROP COLUMN state_changed_at;
