-- The tag with a vendor's structural noise removed. See the Postgres migration
-- of the same name for why this is a stored column rather than a render-time
-- transform.

-- +goose Up
ALTER TABLE packages ADD COLUMN display_tag TEXT;
CREATE INDEX packages_display_tag_idx ON packages (source_repo_id, display_tag);

-- +goose Down
DROP INDEX packages_display_tag_idx;
ALTER TABLE packages DROP COLUMN display_tag;
