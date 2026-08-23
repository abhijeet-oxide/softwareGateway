-- See the Postgres migration of the same name for the argument.

-- +goose Up
ALTER TABLE package_security ADD COLUMN log TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE package_security DROP COLUMN log;
