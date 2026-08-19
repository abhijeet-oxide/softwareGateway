-- Repair in two stages, because the first one threw away the mount.
--
-- # What 00012 got wrong
--
-- `force_upload` was a boolean: take no fast path at all, stream the bytes.
-- That is correct and far more expensive than it needs to be, and the reason is
-- the ORDER of the fast-path ladder (docs/design/05 §4):
--
--   1. placement record   0 bytes
--   2. HEAD at target     0 bytes   <- the one Artifactory answers wrongly
--   3. cross-repo mount   0 bytes
--   4. stream             N bytes
--
-- The lying HEAD sits at step 2, ABOVE the mount. So for the copy of a
-- component published under its own name - the same digest already sitting in a
-- sibling repository of the same registry - step 2 answered "already there" and
-- step 3 was never reached. The mount was not declined; it was never attempted.
--
-- Repairing that by disabling the whole ladder streams the blob across the WAN
-- for content the destination registry is holding and could relocate internally
-- in one request. On this transfer that is about 1.5 GiB. On the SECOND
-- transfer of a product line it is the whole package: every blob is a placement
-- hit, so every blob is skipped, so every manifest fails, so every blob is
-- force-uploaded - turning the case deduplication exists to make free into a
-- full re-upload.
--
-- # The levels
--
--   0  ordinary. The full ladder.
--   1  distrust the CACHE: skip the placement record and the HEAD, but still
--      try the mount. Zero bytes when the registry relocates it, a stream when
--      it declines. This is the right answer for a stale placement, which is
--      what the repair is for.
--   2  distrust EVERYTHING. Stream, no questions. Reached only when a level-1
--      repair did not fix the manifest push, which means the mount reported
--      success without materialising the blob.
--
-- Escalation is per blob and stops at 2, so a repair that never helps costs at
-- most one mount attempt and one upload before the manifest is allowed to fail
-- on its own attempt budget.

-- +goose Up
ALTER TABLE jobs ADD COLUMN repair_level SMALLINT NOT NULL DEFAULT 0;

-- Anything already force-uploaded has been through the strongest form, so it
-- carries the level that means "do not try anything cheaper".
UPDATE jobs SET repair_level = 2 WHERE force_upload;

ALTER TABLE jobs DROP COLUMN force_upload;

-- +goose Down
ALTER TABLE jobs ADD COLUMN force_upload BOOLEAN NOT NULL DEFAULT FALSE;
UPDATE jobs SET force_upload = TRUE WHERE repair_level > 0;
ALTER TABLE jobs DROP COLUMN repair_level;
