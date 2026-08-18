-- See the Postgres migration of the same name for the argument.
--
-- A plain ADD COLUMN on both sides: nothing here changes a CHECK, so SQLite
-- needs none of the table-rebuild ceremony of 00017 and 00018.

-- +goose Up
ALTER TABLE transfers ADD COLUMN auto_retries INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE transfers DROP COLUMN auto_retries;
