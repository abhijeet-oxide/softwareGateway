-- See the Postgres migration of the same name for the argument.

-- +goose Up
ALTER TABLE security_scans ADD COLUMN missing INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE security_scans DROP COLUMN missing;
