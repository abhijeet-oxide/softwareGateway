-- The run's transcript, kept after the run.
--
-- While a check is going, the panel shows a timeline: what each chart produced,
-- which ones refused and why, how long each stage cost. It is the most useful
-- thing on the screen and it lived only in the Coordinator's memory, so the
-- moment the run finished it was gone - and the question it answers, "why did
-- this take nine minutes and come back with eleven charts missing", is a
-- question people ask AFTER the run, not during it.
--
-- The vulnerability sync has kept its log on the row since it shipped, and the
-- two features are read by the same people on the same release. See
-- internal/compliance/progress.go for the bounded ring this stores: sixty
-- events, failures kept ahead of routine progress, so the column is kilobytes
-- rather than a transcript of ninety-five charts.

-- +goose Up

ALTER TABLE compliance_runs
    ADD COLUMN log TEXT;

-- +goose Down

ALTER TABLE compliance_runs DROP COLUMN log;
