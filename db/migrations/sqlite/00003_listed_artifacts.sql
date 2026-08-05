-- Discovery no longer traverses a package's manifest tree. See the Postgres
-- migration of the same name for why.
--
-- SQLite cannot drop a NOT NULL constraint in place, so the table is rebuilt.
-- Done inside the transaction goose already opens, with foreign keys deferred:
-- package_artifacts is referenced by artifact_blobs and by itself.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE package_artifacts_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    package_id    INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    parent_id     INTEGER REFERENCES package_artifacts(id) ON DELETE CASCADE,
    digest        TEXT    NOT NULL,
    media_type    TEXT    NOT NULL,
    artifact_type TEXT,
    size_bytes    INTEGER NOT NULL,
    platform      TEXT,
    depth         INTEGER NOT NULL DEFAULT 0,
    -- NULL means listed by a parent index and not fetched.
    raw           BLOB,
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (package_id, digest)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO package_artifacts_new
    (id, package_id, parent_id, digest, media_type, artifact_type,
     size_bytes, platform, depth, raw, created_at)
SELECT id, package_id, parent_id, digest, media_type, artifact_type,
       size_bytes, platform, depth, raw, created_at
  FROM package_artifacts;
-- +goose StatementEnd

DROP TABLE package_artifacts;
ALTER TABLE package_artifacts_new RENAME TO package_artifacts;
CREATE INDEX package_artifacts_parent_idx ON package_artifacts (parent_id);

-- +goose Down
DELETE FROM package_artifacts WHERE raw IS NULL;
