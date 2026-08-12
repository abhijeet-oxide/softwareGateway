-- A manifest waits for its OWN content, not for every blob in the transfer.
-- See the Postgres migration of the same name for the argument.
--
-- Nothing here needs the table rebuild that 00010 did: both changes are
-- additive, and SQLite adds a column and a table without touching the rest.

-- +goose Up
CREATE TABLE job_dependencies (
    job_id        INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    depends_on_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    PRIMARY KEY (job_id, depends_on_id)
);

CREATE INDEX job_dependencies_reverse_idx ON job_dependencies (depends_on_id);

ALTER TABLE jobs ADD COLUMN site_rank INTEGER NOT NULL DEFAULT 0;

DROP INDEX IF EXISTS jobs_dequeue_idx;
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, site_rank, size_bytes DESC, id)
    WHERE state = 'pending' AND NOT paused;

-- +goose Down
DROP INDEX IF EXISTS jobs_dequeue_idx;
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, next_visible_at, id)
    WHERE state = 'pending' AND NOT paused;
ALTER TABLE jobs DROP COLUMN site_rank;
DROP TABLE job_dependencies;
