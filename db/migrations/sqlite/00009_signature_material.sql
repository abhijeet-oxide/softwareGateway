-- Record WHAT a signature actually is, not merely that one exists. See the
-- Postgres migration of the same name for the argument.

-- +goose Up
ALTER TABLE package_relations ADD COLUMN blob_digest     TEXT;
ALTER TABLE package_relations ADD COLUMN blob_media_type TEXT;
ALTER TABLE package_relations ADD COLUMN blob_size       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_relations ADD COLUMN annotations     TEXT;
ALTER TABLE package_relations ADD COLUMN resolved_at     TEXT;

-- +goose Down
ALTER TABLE package_relations DROP COLUMN resolved_at;
ALTER TABLE package_relations DROP COLUMN annotations;
ALTER TABLE package_relations DROP COLUMN blob_size;
ALTER TABLE package_relations DROP COLUMN blob_media_type;
ALTER TABLE package_relations DROP COLUMN blob_digest;
