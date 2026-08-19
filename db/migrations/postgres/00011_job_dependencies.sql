-- A manifest waits for its OWN content, not for every blob in the transfer.
--
-- # What was wrong
--
-- Invariant I1 is a statement about one manifest: it may be pushed once every
-- blob and child manifest IT references is present. The implementation was a
-- global barrier - wave N+1 opened only when wave N had drained entirely - which
-- is a far stronger claim than the invariant makes.
--
-- Measured on a real 63.7 GiB bundle: at 1974 of 1976 blobs done, 517 manifests
-- whose content was already complete at the destination could not be written,
-- because two unrelated blobs had not landed. The cost is not bandwidth -
-- manifests are kilobytes and pushing them sooner moves no bytes sooner. The
-- cost is everything else:
--
--   * nothing appears at the destination until the very end, so a transfer 99%
--     done delivers exactly nothing;
--   * one stuck blob blocks every artifact rather than its own;
--   * an interrupted transfer leaves no durably finished artifact, only blobs
--     that are reusable through placement records.
--
-- # What replaces it
--
-- An explicit edge per dependency, written at plan time. A manifest job is
-- promoted from `blocked` to `pending` the moment none of its own dependencies
-- is outstanding. `wave` survives as ORDERING - it still decides what a
-- dequeue reaches first, and it is still what the progress rollup groups by -
-- but it is no longer a gate.
--
-- I1 is unchanged, and is in fact enforced more precisely than before: the edge
-- set IS the invariant, stated per manifest rather than approximated by a
-- global barrier.
--
-- # site_rank, and why ordering needs it
--
-- A bundle publishes each component twice - once inside the bundle, so the
-- index still resolves, and once under the component's own name. One digest
-- therefore becomes two jobs of identical size, and the SECOND one is nearly
-- free: the blob is already in a sibling repository of the same registry, so it
-- is a cross-repository mount rather than a transfer over the WAN.
--
-- That saving depended on the two jobs landing in different lease batches,
-- which depended on insertion order, which is not something the schema stated.
-- Ordering the dequeue largest-first broke it immediately: equal-sized jobs
-- became adjacent, both copies landed in one batch, and both streamed from the
-- vendor. Measured on the bundle fixture: 16 vendor GETs for 11 distinct blobs,
-- and no mounts at all.
--
-- site_rank states the thing that was implicit. Rank 0 is the copy that keeps
-- the bundle resolvable; rank 1 is the copy published under the component's own
-- name. Ordering by (site_rank, size DESC) runs every rank-0 job before any
-- rank-1 job, so the mount is guaranteed by the ordering rather than by
-- accident - and within a rank, the largest job starts first, which is the
-- makespan win that could not be had before.

-- +goose Up
CREATE TABLE job_dependencies (
    job_id        BIGINT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    depends_on_id BIGINT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    PRIMARY KEY (job_id, depends_on_id)
);

-- The reverse direction is the hot one: a job completes and asks "what was
-- waiting on me". The forward direction is served by the primary key.
CREATE INDEX job_dependencies_reverse_idx ON job_dependencies (depends_on_id);

ALTER TABLE jobs ADD COLUMN site_rank SMALLINT NOT NULL DEFAULT 0;

-- The dequeue index has to match the dequeue's ORDER BY or the planner sorts.
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
