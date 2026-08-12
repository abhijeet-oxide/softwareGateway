-- Repair in two stages, because the first one threw away the mount. See the
-- Postgres migration of the same name for the argument.

-- +goose Up
ALTER TABLE jobs ADD COLUMN repair_level INTEGER NOT NULL DEFAULT 0;

UPDATE jobs SET repair_level = 2 WHERE force_upload <> 0;

ALTER TABLE jobs DROP COLUMN force_upload;

-- +goose Down
ALTER TABLE jobs ADD COLUMN force_upload INTEGER NOT NULL DEFAULT 0;
UPDATE jobs SET force_upload = 1 WHERE repair_level > 0;
ALTER TABLE jobs DROP COLUMN repair_level;
