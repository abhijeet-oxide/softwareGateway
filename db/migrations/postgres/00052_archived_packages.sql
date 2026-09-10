-- A release the vendor has taken down.
--
-- # What this records
--
-- Discovery lists the tags a repository has and compares them with what it
-- found last time. A tag that has gone is not an error and not a gap in the
-- scan: the vendor withdrew it. Until now nothing was written down, so the row
-- stayed exactly as it was and the interface went on offering to download
-- something that no longer exists - a request that can only fail, and fails
-- late, against the registry, with a 404 nobody can act on.
--
-- # Why a timestamp and not a state
--
-- `packages.state` carries a CHECK constraint, and SQLite cannot alter one in
-- place: adding 'archived' there means rebuilding the packages table, which
-- half the schema references. The twin migration would have to do that, and a
-- table rebuild to record one fact is a poor trade.
--
-- More to the point, this is not a lifecycle step. A release that was archived
-- still went through everything it went through; what changed is the SOURCE,
-- not the release. Keeping the state untouched means every existing query
-- still reads correctly, and the derived status a person sees can say
-- "Archived" by looking at one nullable column.
--
-- # Why it is reversible
--
-- A vendor can put a tag back, and a registry can lie by omission for one
-- scan - a proxy serving a truncated catalogue, a paging bug. Setting the
-- column on the way out and CLEARING it when the tag returns means neither
-- case needs anybody's attention: the next honest scan corrects it.
--
-- Only a release that was never downloaded is ever marked. One already in the
-- internal registries is not affected by the vendor withdrawing it upstream -
-- we HAVE it, and it can still be promoted and shipped.

-- +goose Up
ALTER TABLE packages ADD COLUMN archived_at TIMESTAMPTZ;

-- Partial: archived rows are the rare ones, and the question asked of this
-- column is always "which are archived", never "which are not".
CREATE INDEX idx_packages_archived
    ON packages (source_repo_id)
    WHERE archived_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_packages_archived;
ALTER TABLE packages DROP COLUMN archived_at;
