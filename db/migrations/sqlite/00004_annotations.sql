-- Keep what a manifest says about itself. See the Postgres migration of the
-- same name for why these are two columns rather than one.

-- +goose Up
ALTER TABLE packages ADD COLUMN published_at TEXT;
ALTER TABLE package_artifacts ADD COLUMN annotations TEXT;
CREATE INDEX packages_published_idx ON packages (product_id, published_at DESC);

-- +goose Down
DROP INDEX packages_published_idx;
ALTER TABLE package_artifacts DROP COLUMN annotations;
ALTER TABLE packages DROP COLUMN published_at;
