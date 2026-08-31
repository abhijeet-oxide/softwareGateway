-- See the Postgres migration of the same name for the argument.
--
-- SQLite renames a column in place (3.25+) and renames an index by dropping and
-- recreating it, which is what the two DROP/CREATE pairs below are.

-- +goose Up

ALTER TABLE security_scans   RENAME COLUMN expires_at TO evictable_at;
ALTER TABLE security_details RENAME COLUMN expires_at TO evictable_at;

ALTER TABLE security_scans   ADD COLUMN last_used_at TEXT;
ALTER TABLE security_details ADD COLUMN last_used_at TEXT;

ALTER TABLE security_details ADD COLUMN codec TEXT NOT NULL DEFAULT 'json';
ALTER TABLE security_details ADD COLUMN bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE security_details ADD COLUMN source_bytes INTEGER NOT NULL DEFAULT 0;

DROP INDEX IF EXISTS security_scans_expiry_idx;
DROP INDEX IF EXISTS security_details_expiry_idx;
CREATE INDEX security_scans_evict_idx   ON security_scans (evictable_at);
CREATE INDEX security_details_evict_idx ON security_details (evictable_at);
CREATE INDEX security_details_used_idx  ON security_details (last_used_at);

-- The rows that predate the accounting, measured now.
--
-- Eviction spends a byte budget, and a row reporting zero bytes frees nothing
-- when it is deleted - so a store full of them would evict its whole detail
-- tier to get back inside a budget it never left. These rows are uncompressed
-- JSON written before there was a column to say so, which is what codec 'json'
-- means and why the two sizes are the same.
UPDATE security_details
   SET bytes = length(payload), source_bytes = length(payload)
 WHERE bytes = 0;

CREATE TABLE security_documents (
    product      TEXT NOT NULL,
    repository   TEXT NOT NULL,
    provider     TEXT NOT NULL,
    artifact_ref TEXT NOT NULL,
    kind         TEXT NOT NULL,

    payload      BLOB NOT NULL,
    codec        TEXT NOT NULL DEFAULT 'gzip',
    content_type TEXT NOT NULL DEFAULT 'application/json',
    bytes        INTEGER NOT NULL DEFAULT 0,
    source_bytes INTEGER NOT NULL DEFAULT 0,
    fingerprint  TEXT NOT NULL DEFAULT '',

    fetched_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    last_used_at TEXT,
    evictable_at TEXT NOT NULL,

    PRIMARY KEY (product, repository, provider, artifact_ref, kind)
);
CREATE INDEX security_documents_evict_idx ON security_documents (evictable_at);
CREATE INDEX security_documents_used_idx  ON security_documents (last_used_at);
CREATE INDEX security_documents_kind_idx  ON security_documents (product, kind);

CREATE TABLE package_security_sources (
    package_id    INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
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
DROP INDEX IF EXISTS security_details_evict_idx;
DROP INDEX IF EXISTS security_scans_evict_idx;
CREATE INDEX security_scans_expiry_idx   ON security_scans (evictable_at);
CREATE INDEX security_details_expiry_idx ON security_details (evictable_at);
ALTER TABLE security_details DROP COLUMN source_bytes;
ALTER TABLE security_details DROP COLUMN bytes;
ALTER TABLE security_details DROP COLUMN codec;
ALTER TABLE security_details DROP COLUMN last_used_at;
ALTER TABLE security_scans   DROP COLUMN last_used_at;
ALTER TABLE security_details RENAME COLUMN evictable_at TO expires_at;
ALTER TABLE security_scans   RENAME COLUMN evictable_at TO expires_at;
