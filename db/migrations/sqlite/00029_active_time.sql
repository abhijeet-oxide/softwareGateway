-- See the Postgres migration of the same name for the argument.
--
-- A plain ADD COLUMN on both sides: nothing here changes a CHECK, so SQLite
-- needs none of the table-rebuild ceremony of 00017 and 00018.

-- +goose Up
ALTER TABLE transfers ADD COLUMN active_seconds REAL NOT NULL DEFAULT 0;
ALTER TABLE transfers ADD COLUMN last_active_at TEXT;

-- +goose Down
ALTER TABLE transfers DROP COLUMN last_active_at;
ALTER TABLE transfers DROP COLUMN active_seconds;
