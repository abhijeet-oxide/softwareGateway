-- The four things a reader needs before they can do anything with a finding.
--
-- A severity says how much we care. It does not say who changes something, how
-- much work it is, when the consequence actually arrives, or how firmly the
-- tool knows what it is asserting - and a report without those is one that gets
-- forwarded rather than acted on. The columns are on the RESULT rather than
-- joined from the catalogue for the same reason the title and the remediation
-- are: a spreadsheet sent to a vendor has to still say what it said the day it
-- was exported, after the check behind it has been edited.
--
-- fix_example is the corrected YAML. It is the largest of the five and the one
-- most worth the bytes: prose describing a fix and the four lines that ARE the
-- fix are not the same artifact, and only one of them gets applied.

-- +goose Up

ALTER TABLE compliance_results ADD COLUMN confidence    TEXT NOT NULL DEFAULT '';
ALTER TABLE compliance_results ADD COLUMN when_it_bites TEXT NOT NULL DEFAULT '';
ALTER TABLE compliance_results ADD COLUMN fix_owner     TEXT NOT NULL DEFAULT '';
ALTER TABLE compliance_results ADD COLUMN fix_effort    TEXT NOT NULL DEFAULT '';
ALTER TABLE compliance_results ADD COLUMN fix_example   TEXT NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE compliance_results DROP COLUMN confidence;
ALTER TABLE compliance_results DROP COLUMN when_it_bites;
ALTER TABLE compliance_results DROP COLUMN fix_owner;
ALTER TABLE compliance_results DROP COLUMN fix_effort;
ALTER TABLE compliance_results DROP COLUMN fix_example;
