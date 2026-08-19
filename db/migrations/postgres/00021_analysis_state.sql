-- Whether anybody is walking this release RIGHT NOW, and how it went last time.
--
-- # Why a state and not just `expanded_at`
--
-- `expanded_at` answers "has this been walked", which has two answers and needs
-- three. A release nobody has touched, a release being walked this minute, and
-- a release whose walk failed are different situations with different things to
-- say and different things to do — and with only a timestamp, all three look
-- identical: not analysed.
--
-- That is what made the interface offer "Analyze package" on a release already
-- being analysed by discovery, and what made a crash leave a release looking
-- untouched forever while the process that claimed it was gone.
--
-- # Why the claim is in the database
--
-- Because the process holding it can die. `analysis_started_at` is what makes a
-- claim RECOVERABLE: a row still marked analyzing long after anything could
-- plausibly still be running was claimed by a process that is no longer here,
-- and is released rather than left to sit. In-memory bookkeeping cannot survive
-- the event it exists to handle.
--
-- Empty string rather than NULL for the state, so `WHERE analysis_state = ''`
-- needs no COALESCE in the queue query that runs on every scan.

-- +goose Up
ALTER TABLE packages ADD COLUMN analysis_state TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN analysis_started_at TIMESTAMPTZ;
ALTER TABLE packages ADD COLUMN analysis_error TEXT;

-- The queue query: recent releases in one repository that nobody is walking.
-- Partial, because the interesting rows are a tiny minority of a catalogue.
CREATE INDEX idx_packages_analysis_state ON packages (source_repo_id, analysis_state)
  WHERE analysis_state <> '';

-- +goose Down
DROP INDEX IF EXISTS idx_packages_analysis_state;
ALTER TABLE packages DROP COLUMN analysis_error;
ALTER TABLE packages DROP COLUMN analysis_started_at;
ALTER TABLE packages DROP COLUMN analysis_state;
