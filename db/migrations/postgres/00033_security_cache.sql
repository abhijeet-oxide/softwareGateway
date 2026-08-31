-- The security cache stops being a set of timers and becomes a managed store.
--
-- # What was wrong with expiry
--
-- Every tier carried `expires_at`, and every read filtered on it. That is a
-- correct cache and the wrong thing for this data, and the difference showed up
-- as a page that emptied itself: a release synced on Monday still said 90,808
-- on its listing row on Wednesday, because that number lives in
-- `package_security` and never expires - and its own security tab showed
-- nothing, because the rows behind the number had aged out. The reader is then
-- looking at a release that has been scanned, has counts, and has no findings,
-- and there is no sentence the interface can honestly put on that screen.
--
-- Worse, the expiry did not do the job it was there for. It was argued as
-- "Xray is the system of record, so do not become a second one" - a good
-- argument - but silently DELETING an answer is not how you avoid quoting a
-- stale one. It is how you lose the ability to say how stale it is.
--
-- # What replaces it
--
-- The same rows, kept until something needs the space, and stamped with when
-- they were fetched and last read. `expires_at` becomes `evictable_at`: the
-- point after which a row MAY be reclaimed, not the point at which it stops
-- being true. Reads no longer filter on it - they serve the row and its age,
-- and the interface says "from the sync 3 days ago" where it used to say
-- nothing at all.
--
-- The sweeper then does what a cache manager does rather than what a timer
-- does:
--
--   1. Rows whose package or scan no longer exists go immediately. They are
--      unreferenced, and unreferenced is the one thing that makes a row
--      genuinely useless.
--   2. Everything else is kept while the store is inside its byte budget.
--   3. Over budget, the least recently READ evictable rows go first, until it
--      is back inside. Heavy tiers before light ones: a raw scanner payload is
--      megabytes and is regenerable from one request, an index row is bytes and
--      is what every listing reads.
--
-- So: nothing is deleted until deleting is required, which is what an operator
-- means when they ask for a cache that survives a restart.
--
-- # security_documents, and why raw payloads are worth keeping at all
--
-- Because they are what somebody hands to a customer. An export that has to
-- ship "the actual scanner output for this image" cannot reconstruct it from
-- the normalized model - normalization is lossy on purpose - and re-fetching
-- 157 of them at download time turns a click into fifteen minutes of Xray.
--
-- Stored gzipped, because they are JSON and JSON gzips to about a tenth. The
-- codec travels in a column rather than being assumed, so a payload that does
-- not compress (or a future one that arrives already compressed) can be stored
-- as it is without a second table.
--
-- One row per (scope, artifact, KIND): the vulnerability response, the SBOM,
-- the policy violations and the malware findings are four different documents
-- about one image, fetched at different times and evicted independently. Kind
-- is part of the key rather than four nullable columns because three of them
-- are absent for most artifacts and a row of nulls is a row that lies about
-- having been fetched.

-- +goose Up

ALTER TABLE security_scans   RENAME COLUMN expires_at TO evictable_at;
ALTER TABLE security_details RENAME COLUMN expires_at TO evictable_at;

ALTER TABLE security_scans   ADD COLUMN last_used_at TIMESTAMPTZ;
ALTER TABLE security_details ADD COLUMN last_used_at TIMESTAMPTZ;

-- How the payload is encoded, and how much room it takes.
--
-- bytes is what is STORED and is what the budget is spent from; source_bytes is
-- what it decodes to, which is the number that says whether compression is
-- earning its keep. Both, because an operator asked to raise a budget deserves
-- to know which one they are raising.
ALTER TABLE security_details ADD COLUMN codec TEXT NOT NULL DEFAULT 'json';
ALTER TABLE security_details ADD COLUMN bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE security_details ADD COLUMN source_bytes INTEGER NOT NULL DEFAULT 0;

ALTER INDEX security_scans_expiry_idx   RENAME TO security_scans_evict_idx;
ALTER INDEX security_details_expiry_idx RENAME TO security_details_evict_idx;

-- The LRU order the eviction walks. Without it, reclaiming space from a
-- catalogue-sized cache is a full scan of the table it is trying to shrink.
CREATE INDEX security_details_used_idx ON security_details (last_used_at);

-- The rows that predate the accounting, measured now.
--
-- Eviction spends a byte budget, and a row reporting zero bytes frees nothing
-- when it is deleted - so a store full of them would evict its whole detail
-- tier to get back inside a budget it never left. These rows are uncompressed
-- JSON written before there was a column to say so, which is what codec 'json'
-- means and why the two sizes are the same.
UPDATE security_details
   SET bytes = octet_length(payload), source_bytes = octet_length(payload)
 WHERE bytes = 0;

CREATE TABLE security_documents (
    product      TEXT NOT NULL,
    repository   TEXT NOT NULL,
    provider     TEXT NOT NULL,
    artifact_ref TEXT NOT NULL,
    -- vulnerabilities | sbom | policy | malware
    kind         TEXT NOT NULL,

    payload      BYTEA NOT NULL,
    -- gzip | raw
    codec        TEXT NOT NULL DEFAULT 'gzip',
    content_type TEXT NOT NULL DEFAULT 'application/json',
    bytes        INTEGER NOT NULL DEFAULT 0,
    source_bytes INTEGER NOT NULL DEFAULT 0,
    -- fingerprint is over the DECODED payload, so an unchanged re-fetch can be
    -- recognised without decompressing what is already stored.
    fingerprint  TEXT NOT NULL DEFAULT '',

    fetched_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    evictable_at TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (product, repository, provider, artifact_ref, kind)
);
CREATE INDEX security_documents_evict_idx ON security_documents (evictable_at);
CREATE INDEX security_documents_used_idx  ON security_documents (last_used_at);
CREATE INDEX security_documents_kind_idx  ON security_documents (product, kind);

-- One row per scanner that contributed to a release.
--
-- # Why not a column on package_security
--
-- Because "how many did Xray find" and "how many did Anchore find" are the same
-- question asked of two answerers, and the shape that holds two answerers is a
-- row each. Columns would mean a migration per scanner and a listing query that
-- names them all - which is the shape that makes the third scanner a schema
-- change rather than a configuration one.
--
-- only_cves is the number the comparison exists for: advisories THIS scanner
-- reported and no other did. Stored rather than derived, because the listing
-- that most wants it is the one rendering twenty releases without any of their
-- findings loaded.
CREATE TABLE package_security_sources (
    package_id    BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,

    total         INTEGER NOT NULL DEFAULT 0,
    fixable       INTEGER NOT NULL DEFAULT 0,
    critical      INTEGER NOT NULL DEFAULT 0,
    high          INTEGER NOT NULL DEFAULT 0,
    medium        INTEGER NOT NULL DEFAULT 0,
    low           INTEGER NOT NULL DEFAULT 0,
    unknown       INTEGER NOT NULL DEFAULT 0,

    distinct_cves INTEGER NOT NULL DEFAULT 0,
    only_cves     INTEGER NOT NULL DEFAULT 0,
    artifacts     INTEGER NOT NULL DEFAULT 0,

    PRIMARY KEY (package_id, provider)
);

-- +goose Down
DROP TABLE IF EXISTS package_security_sources;
DROP TABLE IF EXISTS security_documents;
DROP INDEX IF EXISTS security_details_used_idx;
ALTER INDEX security_details_evict_idx RENAME TO security_details_expiry_idx;
ALTER INDEX security_scans_evict_idx   RENAME TO security_scans_expiry_idx;
ALTER TABLE security_details DROP COLUMN source_bytes;
ALTER TABLE security_details DROP COLUMN bytes;
ALTER TABLE security_details DROP COLUMN codec;
ALTER TABLE security_details DROP COLUMN last_used_at;
ALTER TABLE security_scans   DROP COLUMN last_used_at;
ALTER TABLE security_details RENAME COLUMN evictable_at TO expires_at;
ALTER TABLE security_scans   RENAME COLUMN evictable_at TO expires_at;
