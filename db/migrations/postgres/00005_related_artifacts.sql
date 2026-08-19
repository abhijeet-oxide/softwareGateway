-- A package's related artifacts, and whether it is signed.
--
-- The concept is the standard one. OCI 1.1 formalises it with `subject` and
-- the referrers API: a signature, an SBOM or an attestation is a separate
-- artifact that BELONGS to a package without living inside its manifest tree.
-- Vendors differ only in how the relationship is discovered - referrers,
-- cosign's `sha256-<hex>.sig` tag, or (our first vendor) a third tag whose
-- index bundles the payload and the signature as siblings.
--
-- So the ROLE is stored, never the mechanism and never the vendor: `signature`,
-- not `nokia_signature`. A vendor's naming stays in the plugin that produced
-- the row. Adding a column per vendor is the alternative, and it does not end.
--
-- Two columns on `packages` alongside it.
--
-- `signature_status` is THREE-valued, and the third value is the point.
-- "We looked and found nothing" and "nobody has looked" are the same value in
-- a boolean and completely different facts when someone is deciding whether to
-- trust a package. Vendors do not sign everything - older releases and hotfixes
-- routinely go unsigned - so both states occur in real data.
--
-- `transfer_root_digest` is what a transfer plans from, which is not always the
-- package's own manifest. Where a vendor bundles the payload and its signature
-- under a wrapper index, only the wrapper reaches both; planning from the
-- payload alone would move the bytes and LEAVE THE SIGNATURE BEHIND, after
-- which destination-side verification is impossible for good. NULL means "use
-- manifest_digest", which is the ordinary case.

-- +goose Up
ALTER TABLE packages ADD COLUMN signature_status TEXT NOT NULL DEFAULT 'unknown'
    CHECK (signature_status IN ('signed','unsigned','unknown'));
ALTER TABLE packages ADD COLUMN transfer_root_digest TEXT;
ALTER TABLE packages ADD COLUMN transfer_root_tag TEXT;

CREATE TABLE package_relations (
    package_id  BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    role        TEXT   NOT NULL CHECK (role IN ('signature','sbom','attestation','wrapper')),
    digest      TEXT   NOT NULL,
    tag         TEXT,
    media_type  TEXT,
    size_bytes  BIGINT NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- One artifact per (package, role, digest). A re-scan that re-derives the
    -- same relationship is a no-op, which is what makes grouping idempotent.
    PRIMARY KEY (package_id, role, digest)
);

CREATE INDEX package_relations_package_idx ON package_relations (package_id);

-- +goose Down
DROP INDEX package_relations_package_idx;
DROP TABLE package_relations;
ALTER TABLE packages DROP COLUMN transfer_root_tag;
ALTER TABLE packages DROP COLUMN transfer_root_digest;
ALTER TABLE packages DROP COLUMN signature_status;
