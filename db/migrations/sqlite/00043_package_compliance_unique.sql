-- The DISTINCT checks behind a release's compliance counts.
--
-- A release breaks five rules in a hundred and seventy-one places. "171" is how
-- much editing there is to do; "5" is how many conversations, and it is the
-- number somebody means when they ask how many problems a release has.
--
-- On this table because it is the number the Software listing and the release
-- page's tab label show, and both read this row rather than the run. The tab
-- label said 618 over a band that said 40 - the same finding counted once per
-- place it fires, beside the count of rules - which is two answers to one
-- question, ten pixels apart.
--
-- Written from the results the run is inserting in the same transaction, so
-- there is no second pass over the rows. See internal/store/compliance.go.

-- +goose Up

ALTER TABLE package_compliance
    ADD COLUMN unique_blocking INTEGER NOT NULL DEFAULT 0;

ALTER TABLE package_compliance
    ADD COLUMN unique_warning INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE package_compliance DROP COLUMN unique_blocking;
ALTER TABLE package_compliance DROP COLUMN unique_warning;
