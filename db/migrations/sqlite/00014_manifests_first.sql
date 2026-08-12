-- A runnable manifest goes before blobs, not behind them. See the Postgres
-- migration of the same name for the argument.

-- +goose Up
DROP INDEX IF EXISTS jobs_dequeue_idx;
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, kind DESC, site_rank, size_bytes DESC, id)
    WHERE state = 'pending' AND NOT paused;

-- +goose Down
DROP INDEX IF EXISTS jobs_dequeue_idx;
CREATE INDEX jobs_dequeue_idx
    ON jobs (priority DESC, site_rank, size_bytes DESC, id)
    WHERE state = 'pending' AND NOT paused;
