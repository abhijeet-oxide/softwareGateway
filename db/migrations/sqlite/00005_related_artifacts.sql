-- A package's related artifacts, and whether it is signed. See the Postgres
-- migration of the same name for why the role is stored and the vendor is not,
-- and for why signature_status has three values rather than two.

-- +goose Up
ALTER TABLE packages ADD COLUMN signature_status TEXT NOT NULL DEFAULT 'unknown'
    CHECK (signature_status IN ('signed','unsigned','unknown'));
ALTER TABLE packages ADD COLUMN transfer_root_digest TEXT;
ALTER TABLE packages ADD COLUMN transfer_root_tag TEXT;

CREATE TABLE package_relations (
    package_id  INTEGER NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    role        TEXT    NOT NULL CHECK (role IN ('signature','sbom','attestation','wrapper')),
    digest      TEXT    NOT NULL,
    tag         TEXT,
    media_type  TEXT,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY (package_id, role, digest)
);

CREATE INDEX package_relations_package_idx ON package_relations (package_id);

-- +goose Down
DROP INDEX package_relations_package_idx;
DROP TABLE package_relations;
ALTER TABLE packages DROP COLUMN transfer_root_tag;
ALTER TABLE packages DROP COLUMN transfer_root_digest;
ALTER TABLE packages DROP COLUMN signature_status;
