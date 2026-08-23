-- See the Postgres migration of the same name for the argument.

-- +goose Up
ALTER TABLE package_security ADD COLUMN claimed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE package_security ADD COLUMN heartbeat_at TEXT;

-- +goose Down
ALTER TABLE package_security DROP COLUMN heartbeat_at;
ALTER TABLE package_security DROP COLUMN claimed_by;
