-- See the Postgres migration of the same name for the argument.

-- +goose Up
CREATE INDEX jobs_earlier_rank_idx ON jobs (digest, site_rank)
    WHERE state IN ('pending', 'blocked', 'leased') AND NOT paused;

-- +goose Down
DROP INDEX jobs_earlier_rank_idx;
