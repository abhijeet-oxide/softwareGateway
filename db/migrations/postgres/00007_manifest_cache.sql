-- Separate what must be KEPT from what is only CACHED, and give the cache a way
-- to be reclaimed.
--
-- # The problem
--
-- `package_artifacts.raw` holds a manifest verbatim, and it has to: a manifest
-- must be pushed byte-for-byte identical, because its digest — and every
-- signature over it — is the hash of those exact bytes. Inspecting a package
-- writes one such row per manifest in its tree.
--
-- That is fine for one package and untenable for a catalogue. A NEAR release
-- bundle indexes sixty-odd artifacts at a few kilobytes of JSON each; a vendor
-- ships several releases a month across dozens of repositories; the rows are
-- never revisited and nothing ever removes them. Multiply it out over a couple
-- of years and the manifest bytes are hundreds of times the size of everything
-- else in the database — and they are the one part of it that is RECOVERABLE,
-- because the tree under a digest is immutable and can be re-fetched exactly.
--
-- # The split
--
-- What a package IS — its artifacts, their digests, sizes, media types,
-- platforms, the blobs they reference and the totals derived from them — is
-- small, is what every listing and every plan reads, and stays forever. A
-- fully-inspected sixty-artifact package costs a few kilobytes of it.
--
-- The manifest BYTES are a cache in front of the source registry. They are
-- large, are read only when something pushes them, and can be reclaimed at any
-- time without losing a fact.
--
-- The schema could not express that split before this migration, because
-- "have we fetched this manifest?" was answered by `raw IS NOT NULL`. Evicting
-- the bytes would therefore have unlearned the fetch, and the next inspect
-- would walk the whole tree again — the cache would have been unreclaimable in
-- practice. `fetched_at` records the FACT, `raw` carries the bytes, and the two
-- are now independent.
--
-- # What the sweeper needs
--
-- raw_bytes so a budget can be summed without reading the blobs themselves, and
-- raw_used_at so eviction can be least-recently-used rather than arbitrary. The
-- partial index keeps the sweep proportional to what is CACHED rather than to
-- how many artifacts exist.

-- +goose Up
ALTER TABLE package_artifacts ADD COLUMN fetched_at  TIMESTAMPTZ;
ALTER TABLE package_artifacts ADD COLUMN raw_bytes   BIGINT NOT NULL DEFAULT 0;
ALTER TABLE package_artifacts ADD COLUMN raw_used_at TIMESTAMPTZ;

-- Every row that currently holds bytes was, by definition, fetched. Its
-- created_at is the closest honest answer for when.
UPDATE package_artifacts
   SET fetched_at  = created_at,
       raw_bytes   = octet_length(raw),
       raw_used_at = created_at
 WHERE raw IS NOT NULL;

CREATE INDEX package_artifacts_cache_idx
    ON package_artifacts (raw_used_at)
    WHERE raw IS NOT NULL;

-- When the whole tree was last known to be walked.
--
-- Distinct from "every artifact has fetched_at", which is the same fact
-- assembled per row. This is the one `packages describe` prints, and having it
-- on the package row keeps that a single-row read.
ALTER TABLE packages ADD COLUMN expanded_at TIMESTAMPTZ;

UPDATE packages
   SET expanded_at = updated_at
 WHERE EXISTS (SELECT 1 FROM package_artifacts a WHERE a.package_id = packages.id)
   AND NOT EXISTS (SELECT 1 FROM package_artifacts a
                    WHERE a.package_id = packages.id AND a.raw IS NULL);

-- The repository path with a vendor's structural noise removed —
-- `cfx-5000-k8s` for NEAR's `orbs/cfx-5000-k8s`.
--
-- A stored column for the same two reasons display_tag is one (migration
-- 00006): the transform belongs to the vendor plugin and nothing else may know
-- it, and the shortened form has to RESOLVE as input or the abbreviation is a
-- trap. NULL means no shortening applies, which is what any conformant registry
-- gets — and what every registry got before this column existed, when the CLI
-- was inferring a prefix from whatever a page of rows happened to share.
ALTER TABLE repositories ADD COLUMN display_path TEXT;
CREATE INDEX repositories_display_path_idx ON repositories (product_id, display_path);

-- +goose Down
DROP INDEX repositories_display_path_idx;
ALTER TABLE repositories DROP COLUMN display_path;
ALTER TABLE packages DROP COLUMN expanded_at;
DROP INDEX package_artifacts_cache_idx;
ALTER TABLE package_artifacts DROP COLUMN raw_used_at;
ALTER TABLE package_artifacts DROP COLUMN raw_bytes;
-- An artifact whose bytes were evicted has no way back to the pre-migration
-- meaning of `raw IS NOT NULL`, so the row is removed rather than left claiming
-- a fetch it cannot evidence. The next inspect re-records it.
DELETE FROM package_artifacts WHERE fetched_at IS NOT NULL AND raw IS NULL;
ALTER TABLE package_artifacts DROP COLUMN fetched_at;
