-- The technical index over a report written in plain language.
--
-- Titles and messages are deliberately written so somebody who is not a
-- Kubernetes engineer can act on them, which means they say "the rule that
-- tells the platform how many copies must stay running" rather than
-- "PodDisruptionBudget". That is right for the person deciding whether to ship
-- and useless for the person fixing it: an engineer types `toleration`,
-- `maxUnavailable` or `RWX` into the search box, and the plainer the prose gets
-- the fewer of those words survive anywhere in the report.
--
-- subcategory names the mechanism the finding is about, so results group by the
-- thing they concern rather than by the section of the standard they came from.
-- keywords is the vocabulary the finding is searchable by, carried deliberately
-- instead of left as a side effect of how the sentences happen to be worded.
-- Both are denormalized onto the result for the same reason the title is: the
-- search runs over stored results, and an exported report has to keep saying
-- what it said.

-- +goose Up

ALTER TABLE compliance_results ADD COLUMN subcategory TEXT NOT NULL DEFAULT '';
ALTER TABLE compliance_results ADD COLUMN keywords    TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS compliance_results_subcategory_idx
    ON compliance_results (run_id, subcategory);

-- +goose Down

DROP INDEX IF EXISTS compliance_results_subcategory_idx;
ALTER TABLE compliance_results DROP COLUMN subcategory;
ALTER TABLE compliance_results DROP COLUMN keywords;
