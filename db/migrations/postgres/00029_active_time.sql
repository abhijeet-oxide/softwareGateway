-- How long this transfer was ACTUALLY DOWNLOADING, as opposed to how long ago
-- it started.
--
-- # The number this replaces
--
-- Elapsed was `completed_at - started_at`, which is wall clock. That is the
-- right answer only if the fleet was working for the whole of it, and the whole
-- point of a resumable queue is that it frequently was not: a worker that
-- crashed at midnight and came back at noon leaves a transfer reporting twelve
-- hours, of which perhaps twenty minutes were spent moving bytes. Somebody
-- reading that concludes the link is a hundredth of the speed it is.
--
-- # Why it cannot be derived from the jobs
--
-- The obvious reconstruction - merge the [started_at, completed_at] intervals
-- of the jobs and sum them - does not work, and it fails on exactly the case
-- this column exists for. `jobs.started_at` is written with COALESCE on the
-- FIRST lease and deliberately never reset, so a job leased at midnight,
-- orphaned by the crash, re-leased at noon and finished at 12:05 has an
-- interval spanning the whole outage. Merging those recovers wall clock again.
--
-- # What is accrued, and by whom
--
-- The reaper's sweep. Every pass it adds the time since the last pass to every
-- transfer that has a job LEASED at that moment, and re-anchors the ones that
-- do not. So the measure is "how long was there work of this transfer in the
-- hands of a worker", which is the honest reading of "spent downloading" - it
-- counts a single 8 GB blob that occupies one worker for half an hour, and it
-- counts none of the night the fleet was down.
--
-- A gap longer than a few sweeps is never accrued. The Coordinator being down
-- is the case: on restart the anchor is stale by the whole outage, and the
-- sweep re-anchors instead of believing it. Under-counting a period nobody
-- observed is the only honest option; adding it back is how wall clock got
-- here in the first place.
--
-- DOUBLE PRECISION rather than an integer count of seconds: a transfer that is
-- entirely deduplicated finishes in well under a second, and rounding that to
-- zero would report "no time at all" for work that did happen.

-- +goose Up
ALTER TABLE transfers ADD COLUMN active_seconds DOUBLE PRECISION NOT NULL DEFAULT 0;

-- The anchor the next accrual measures from, and NULL once the transfer has
-- settled and been accounted for. NULL is what stops a settled transfer being
-- accrued twice.
ALTER TABLE transfers ADD COLUMN last_active_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE transfers DROP COLUMN last_active_at;
ALTER TABLE transfers DROP COLUMN active_seconds;
