-- A vulnerability sync is a JOB, and a job with no log is a job you cannot ask
-- anything about after it finishes.
--
-- # What was missing
--
-- The sync reported its position - stage, counts, a few notes - from memory,
-- and only while the replica running it was asked. The moment it finished, all
-- of it was gone. What remained was one `error` sentence on a failure and
-- nothing at all on a success, so a release that came back with 100 artifacts
-- the scanner refused and 150 it had never indexed could say how MANY of each,
-- from the coverage columns, and never once say why, when, or which.
--
-- # Why a column rather than a table
--
-- One log per release, bounded, written once at the end of the run and read
-- whole. There is no query that wants a row of it - "the log of this sync" is
-- the only question anybody asks - and a table would buy an index, a retention
-- sweep and a join to answer it.
--
-- Bounded in the writer rather than by the schema: entries are capped and each
-- one truncated, so this is kilobytes and stays kilobytes. `synced_at` dates
-- it; a log without its run's outcome beside it is a transcript with no ending.

-- +goose Up
ALTER TABLE package_security ADD COLUMN log TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE package_security DROP COLUMN log;
