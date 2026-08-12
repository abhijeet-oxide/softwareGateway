-- A blob job that must not believe the destination. See the Postgres migration
-- of the same name for the argument.

-- +goose Up
ALTER TABLE jobs ADD COLUMN force_upload INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN force_upload;
