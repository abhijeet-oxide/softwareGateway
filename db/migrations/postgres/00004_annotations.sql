-- Keep what a manifest says about itself.
--
-- Discovery already fetches the tag's own manifest, and an index carries
-- annotations describing the release: when it was built, who published it,
-- what each child artifact is. We were parsing the structure and discarding
-- all of it.
--
-- Two columns, for two different kinds of fact.
--
-- `published_at` comes from org.opencontainers.image.created, a STANDARD OCI
-- annotation rather than any one vendor's — which is what makes it safe to
-- promote to a column. It is nullable because the spec makes it optional: a
-- registry that sets none is fully conformant. It is also VENDOR-DECLARED,
-- and deliberately separate from discovered_at, which is a fact about us. One
-- is a claim, the other an observation, and merging them would lose the
-- ability to say which.
--
-- `annotations` keeps the whole map verbatim, so a vendor's own keys —
-- com.nokia.ncd.orb.type, com.nokia.ncd.orb.rb.version — are queryable
-- without this project knowing they exist. That is the alternative to adding
-- a column per vendor, which does not end.

-- +goose Up
ALTER TABLE packages ADD COLUMN published_at TIMESTAMPTZ;
ALTER TABLE package_artifacts ADD COLUMN annotations JSONB;
CREATE INDEX packages_published_idx ON packages (product_id, published_at DESC NULLS LAST);

-- +goose Down
DROP INDEX packages_published_idx;
ALTER TABLE package_artifacts DROP COLUMN annotations;
ALTER TABLE packages DROP COLUMN published_at;
