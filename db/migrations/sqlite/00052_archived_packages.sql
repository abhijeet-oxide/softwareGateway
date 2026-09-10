-- See the Postgres migration of the same name for the argument.
--
-- A plain ADD COLUMN, which is the whole reason this is a timestamp rather
-- than a new `packages.state`: altering that CHECK constraint would mean
-- rebuilding a table half the schema has foreign keys into.

-- +goose Up
ALTER TABLE packages ADD COLUMN archived_at TEXT;

CREATE INDEX idx_packages_archived
    ON packages (source_repo_id)
    WHERE archived_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_packages_archived;
ALTER TABLE packages DROP COLUMN archived_at;
