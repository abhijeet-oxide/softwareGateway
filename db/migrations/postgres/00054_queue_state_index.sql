-- The indexes behind the queue gauges.
--
-- # What they are for
--
-- The Coordinator samples queue depth on a timer and publishes it as
-- Prometheus gauges - queue depth by state, bytes outstanding, the age of the
-- oldest job nobody has started. That makes these reads some of the most
-- frequently executed statements in the system, so they have to be
-- proportional to the WORK OUTSTANDING rather than to everything ever
-- transferred.
--
-- Counting `jobs` grouped by state without an index is a scan of the whole
-- table. On an estate with ten million settled rows that is a multi-second
-- read taken several times a minute, which is a strange way to find out
-- whether the queue is busy: the measurement becomes the load.
--
-- # Why they are partial
--
-- The gauges only ever ask about live work. A settled job is history, and
-- history is what a counter is for (jobs_completed_total), not a gauge - so
-- neither index carries a settled row, and both stay proportional to the
-- backlog rather than to the archive.
--
-- # Why an existing index will not do
--
--   jobs_dequeue_idx        pending AND NOT paused only, so it cannot see
--                           blocked or leased rows, nor paused ones - and a
--                           queue paused for six hours is exactly what a
--                           depth gauge exists to show
--   jobs_lease_expiry_idx   leased only
--   jobs_transfer_state_idx leads on transfer_id, so grouping by state alone
--                           means reading all of it, settled rows included
--   jobs_earlier_rank_idx   leads on digest, and excludes paused rows
--   transfers_active_idx    ready/running/paused only, so a transfer wedged
--                           in `planning` or `verifying` - the states worth
--                           noticing - is invisible to it
--
-- # Why created_at is in the jobs index
--
-- So that "how long has the oldest unstarted job been waiting" is a seek to
-- the first entry of a range rather than a scan to find a minimum. That single
-- number is the difference between a queue that is deep because it is busy and
-- one that is deep because it is stuck, and it should not cost a scan to ask.

-- +goose Up
CREATE INDEX jobs_live_state_idx ON jobs (state, created_at)
    WHERE state IN ('blocked', 'pending', 'leased');

CREATE INDEX transfers_live_state_idx ON transfers (state)
    WHERE state IN ('waiting', 'pending', 'planning', 'ready', 'running',
                    'paused', 'syncing', 'promoting', 'verifying', 'cancelling');

-- +goose Down
DROP INDEX transfers_live_state_idx;
DROP INDEX jobs_live_state_idx;
