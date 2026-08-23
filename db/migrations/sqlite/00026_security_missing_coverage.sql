-- See the Postgres migration of the same name for the argument.

-- +goose Up
ALTER TABLE package_security ADD COLUMN missing INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE package_security DROP COLUMN missing;
