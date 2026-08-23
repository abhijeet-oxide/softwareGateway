-- A claim that says WHO holds it, and proves it is still alive.
--
-- `started_at` alone cannot tell a running sync from an abandoned one. A
-- Coordinator killed mid-sync leaves the row marked `syncing` with a start time
-- that only gets older, and every reader of that row is told the sync is
-- running on another Coordinator - for the thirty minutes it takes the stale
-- sweep to notice. On a restarted single-Coordinator deployment that sentence
-- is simply false, and the release cannot be synced again until it expires.
--
-- So the sync beats while it runs. A heartbeat that has stopped is the one
-- honest signal that the process holding the claim is gone, and it is worth
-- seconds rather than half an hour: the reader is told the sync was
-- interrupted, and the next claim takes the row rather than being refused.
--
-- `claimed_by` names the process, which is what lets a Coordinator recognise
-- its own abandoned claims after a restart and what makes a stopped sync
-- attributable in a log.

-- +goose Up
ALTER TABLE package_security ADD COLUMN claimed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE package_security ADD COLUMN heartbeat_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE package_security DROP COLUMN heartbeat_at;
ALTER TABLE package_security DROP COLUMN claimed_by;
