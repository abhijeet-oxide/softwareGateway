-- The indexes behind the queue gauges.
--
-- See the Postgres migration of the same name for the argument. Identical on
-- purpose: the gauges are read the same way on both dialects, and an index
-- that exists on only one of them is a development environment that cannot
-- reproduce a production plan.

-- +goose Up
CREATE INDEX jobs_live_state_idx ON jobs (state, created_at)
    WHERE state IN ('blocked', 'pending', 'leased');

CREATE INDEX transfers_live_state_idx ON transfers (state)
    WHERE state IN ('waiting', 'pending', 'planning', 'ready', 'running',
                    'paused', 'syncing', 'promoting', 'verifying', 'cancelling');

-- +goose Down
DROP INDEX transfers_live_state_idx;
DROP INDEX jobs_live_state_idx;
