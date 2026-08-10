-- Separate what must be KEPT from what is only CACHED. See the Postgres
-- migration of the same name for the argument.
--
-- Only two things differ here. SQLite spells the byte length of a BLOB
-- `length()` rather than `octet_length()`, and its timestamps are the ISO-8601
-- strings the rest of this schema uses rather than TIMESTAMPTZ.

-- +goose Up
ALTER TABLE package_artifacts ADD COLUMN fetched_at  TEXT;
ALTER TABLE package_artifacts ADD COLUMN raw_bytes   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_artifacts ADD COLUMN raw_used_at TEXT;

UPDATE package_artifacts
   SET fetched_at  = created_at,
       raw_bytes   = length(raw),
       raw_used_at = created_at
 WHERE raw IS NOT NULL;

CREATE INDEX package_artifacts_cache_idx
    ON package_artifacts (raw_used_at)
    WHERE raw IS NOT NULL;

ALTER TABLE packages ADD COLUMN expanded_at TEXT;

UPDATE packages
   SET expanded_at = updated_at
 WHERE EXISTS (SELECT 1 FROM package_artifacts a WHERE a.package_id = packages.id)
   AND NOT EXISTS (SELECT 1 FROM package_artifacts a
                    WHERE a.package_id = packages.id AND a.raw IS NULL);

ALTER TABLE repositories ADD COLUMN display_path TEXT;
CREATE INDEX repositories_display_path_idx ON repositories (product_id, display_path);

-- +goose Down
DROP INDEX repositories_display_path_idx;
ALTER TABLE repositories DROP COLUMN display_path;
ALTER TABLE packages DROP COLUMN expanded_at;
DROP INDEX package_artifacts_cache_idx;
ALTER TABLE package_artifacts DROP COLUMN raw_used_at;
ALTER TABLE package_artifacts DROP COLUMN raw_bytes;
DELETE FROM package_artifacts WHERE fetched_at IS NOT NULL AND raw IS NULL;
ALTER TABLE package_artifacts DROP COLUMN fetched_at;
