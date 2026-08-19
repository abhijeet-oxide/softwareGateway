-- See the Postgres migration of the same name for the argument.
--
-- Plain ADD COLUMNs and a full index rather than a partial one: SQLite supports
-- partial indexes, but the tables this runs against in development are small
-- enough that the distinction buys nothing and the simpler statement is easier
-- to read alongside its Postgres twin.

-- +goose Up
ALTER TABLE packages ADD COLUMN analysis_state TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN analysis_started_at TEXT;
ALTER TABLE packages ADD COLUMN analysis_error TEXT;

CREATE INDEX idx_packages_analysis_state ON packages (source_repo_id, analysis_state);

-- +goose Down
DROP INDEX IF EXISTS idx_packages_analysis_state;
ALTER TABLE packages DROP COLUMN analysis_error;
ALTER TABLE packages DROP COLUMN analysis_started_at;
ALTER TABLE packages DROP COLUMN analysis_state;
