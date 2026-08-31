-- The index behind the dequeue's "wait for the earlier copy" clause.
--
-- # What it is for
--
-- A component published under its own name as well as inside a bundle has one
-- digest and two destination repositories, so it becomes two jobs: site_rank 0
-- and site_rank 1. The rank-1 job is not leasable while its rank-0 sibling is
-- still outstanding, because letting both run at once is how a blob gets
-- streamed across the WAN twice for content the registry could have relocated
-- internally (see leaseCandidatePredicate).
--
-- Establishing that is a lookup of `jobs` BY DIGEST, and there was no index on
-- digest. The only one that mentions it is UNIQUE (transfer_id, kind, digest),
-- which cannot be searched by digest alone, and jobs_inflight_blob_idx, which
-- is restricted to leased rows.
--
-- So SQLite built a transient index for the subquery on every lease:
--
--	SEARCH earlier USING AUTOMATIC PARTIAL COVERING INDEX (digest=?)
--
-- "AUTOMATIC" is the planner saying it had to construct one. Measured over
-- 50,000 pending jobs with half of them at rank 1 - an ordinary bundle-heavy
-- estate - that was 3.5ms per lease against 2.1ms without any rank-1 rows, and
-- the cost grows with the queue because the index is built over the matching
-- rows each time.
--
-- It matters more than that number suggests. The dequeue is the hottest write
-- path in the system, and the worker now refills as capacity frees rather than
-- once a second, so it is called far more often than it used to be.
--
-- # Why it is partial
--
-- The clause only ever looks for OUTSTANDING siblings - a settled job is not
-- something to wait for - and settled jobs are the overwhelming majority of the
-- table on any estate that has been running a while. Restricting the index to
-- the three live states keeps it proportional to the work in flight rather than
-- to everything ever transferred.

-- +goose Up
CREATE INDEX jobs_earlier_rank_idx ON jobs (digest, site_rank)
    WHERE state IN ('pending', 'blocked', 'leased') AND NOT paused;

-- +goose Down
DROP INDEX jobs_earlier_rank_idx;
