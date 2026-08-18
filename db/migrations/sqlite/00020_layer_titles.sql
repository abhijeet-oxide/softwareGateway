-- See the Postgres migration of the same name for the argument.
--
-- A plain ADD COLUMN on both sides: no CHECK changes, so none of the
-- table-rebuild ceremony of 00017 and 00018.

-- +goose Up
ALTER TABLE artifact_blobs ADD COLUMN title TEXT;

-- +goose Down
ALTER TABLE artifact_blobs DROP COLUMN title;
