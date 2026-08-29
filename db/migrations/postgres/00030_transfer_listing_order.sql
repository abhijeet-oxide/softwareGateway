-- The index the transfer listing sorts by.
--
-- # What its absence cost
--
-- Every listing is `ORDER BY created_at DESC, id DESC LIMIT n`, and there was
-- no index in that order - so the planner read EVERY transfer, sorted the lot
-- in a temporary B-tree, and threw all but the page away.
--
-- On its own that is a sort of a few hundred narrow rows and would not be worth
-- an index. What made it expensive is what the projection does per row: a dozen
-- correlated subqueries over `jobs`, each seeking the jobs of that transfer.
-- Those were evaluated for every transfer in the table, because the LIMIT
-- cannot be applied until the sort has run.
--
-- Measured on an estate of sixty transfers of 2,500 jobs: 160ms for a listing,
-- and the SAME 160ms whether the page asked for 25 rows or 100, which is the
-- signature of work being done per-table rather than per-page. The interface
-- polls this every few seconds from three places while a download runs.
--
-- With the index the scan becomes an ordered walk that stops after the page, so
-- the subqueries run for the rows that are actually returned.
--
-- DESC on both columns to match the query exactly. Postgres can walk an index
-- backwards and would use an ASC index here, but the SQLite planner is fussier
-- about it and the two dialects are better kept identical than subtly different.

-- +goose Up
CREATE INDEX transfers_recent_idx ON transfers (created_at DESC, id DESC);

-- +goose Down
DROP INDEX transfers_recent_idx;
