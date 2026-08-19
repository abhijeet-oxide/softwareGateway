-- What a layer is CALLED, when the publisher says.
--
-- `org.opencontainers.image.title` is the ORAS convention for a single-file
-- layer: the publisher is saying "this layer IS this file". Every configuration
-- bundle, release note and script a vendor ships as an OCI artifact carries
-- one, and it is the difference between a release page that can list
-- `CONFIGURATION/nodes.json` and one that can only say `2 layers`.
--
-- Kept as a FACT rather than read from the manifest on demand, because the
-- manifest bodies are an evictable cache (migration 00007) and this is not: a
-- release whose bodies have been reclaimed must still be able to say what is
-- inside it. It is derived from bytes we verified against their digest, so it
-- is as durable as the artifact row it hangs off.
--
-- Nullable, because most layers have no title: an image layer is a tar of an
-- unknown number of paths and naming it would be inventing something.

-- +goose Up
ALTER TABLE artifact_blobs ADD COLUMN title TEXT;

-- +goose Down
ALTER TABLE artifact_blobs DROP COLUMN title;
