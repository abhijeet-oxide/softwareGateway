-- Xray's answers, kept in two tiers, and one of them on purpose does not last.
--
-- # Why two tables and not one
--
-- The platform is a security CHECKPOINT, not a system of record for security
-- findings. Xray is the system of record: it re-grades issues, learns new
-- fixed versions and re-scans continuously, and a copy of its detailed output
-- that this platform kept indefinitely would be an unsynchronised replica of a
-- database that changes underneath it. The first time somebody made a release
-- decision from a six-week-old cached finding, this design would have caused it.
--
-- So findings are split by what they cost and how long they stay true:
--
--   security_scans        one row per artifact: status, counts, severities,
--                         fixability, when it was scanned. Kilobytes per
--                         release, read by every listing and dashboard, kept
--                         long. Cheap to keep, cheap to be slightly stale.
--
--   security_findings     one row per (artifact, CVE, component): identifiers
--                         only, no prose. This is what makes "which images
--                         contain CVE-2024-3094" answerable without asking
--                         Xray about 157 artifacts, and what a relationship
--                         navigation walks. Still lightweight - an identifier,
--                         a severity and a version string - and kept long.
--
--   security_details      the COMPLETE normalized response, as JSON, with a
--                         short expiry. Everything needed to reopen a finding,
--                         repeat a comparison, navigate relationships and
--                         generate an export without a second round trip - and
--                         nothing that outlives the sitting it was fetched for.
--
-- # Why every row carries a scope
--
-- product, repository and provider are on all three tables and are part of
-- every unique key, and that is an authorization boundary rather than a filing
-- convention. Findings were retrieved with ONE repository's credential under
-- THAT repository's Xray permissions. Two products pointing at the same digest
-- get two rows even though the bytes are identical, because serving one
-- product's cached findings to the other would disclose the security posture
-- of something the asker was never entitled to see. A cache keyed by digest
-- alone would be a cross-tenant leak with good performance.
--
-- # Why expires_at is a column rather than a policy
--
-- Retention is per repository (`xray.detailTtl`), so the expiry has to travel
-- with the row: one product may cache details for five minutes and another for
-- an hour, and a sweeper reading a global constant would be wrong for both.

-- # Why the registry type constraint is untouched
--
-- A product may now declare `type: jfrog`, and nothing here admits that value.
-- That is deliberate: the two spellings select ONE backend, and the catalog
-- stores the canonical one (product.RegistryType.Canonical). Widening the
-- constraint would mean recreating the table on SQLite, which cannot alter a
-- CHECK in place - carrying every column and index added since the initial
-- schema, and losing the listing the first time one is missed.

-- +goose Up

CREATE TABLE security_scans (
    id            BIGSERIAL PRIMARY KEY,

    -- The scope. See the header: this is an authorization boundary.
    product       TEXT NOT NULL,
    repository    TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'source',
    provider      TEXT NOT NULL,

    -- Which artifact. artifact_ref is the concrete identity within a release
    -- (a digest); artifact_key is the name two releases are ALIGNED on, and
    -- they are different on purpose - digests are what change between releases.
    artifact_ref  TEXT NOT NULL,
    artifact_key  TEXT NOT NULL,
    artifact_tag  TEXT,
    artifact_kind TEXT,
    artifact_repo TEXT,

    -- Status is the field everything else defers to. 'scanned' with zero
    -- findings is a clean artifact; 'not_scanned' with zero findings is an
    -- artifact nobody looked at, and the two must never be read as one.
    status        TEXT NOT NULL
                    CHECK (status IN ('scanned','not_scanned','unsupported','disabled','unavailable')),
    message       TEXT,

    total         INTEGER NOT NULL DEFAULT 0,
    fixable       INTEGER NOT NULL DEFAULT 0,
    critical      INTEGER NOT NULL DEFAULT 0,
    high          INTEGER NOT NULL DEFAULT 0,
    medium        INTEGER NOT NULL DEFAULT 0,
    low           INTEGER NOT NULL DEFAULT 0,
    unknown       INTEGER NOT NULL DEFAULT 0,
    fix_critical  INTEGER NOT NULL DEFAULT 0,
    fix_high      INTEGER NOT NULL DEFAULT 0,
    fix_medium    INTEGER NOT NULL DEFAULT 0,
    fix_low       INTEGER NOT NULL DEFAULT 0,
    fix_unknown   INTEGER NOT NULL DEFAULT 0,

    -- scanned_at is when XRAY produced the result; retrieved_at is when we
    -- fetched it. Two different facts, and the gap between them is how stale a
    -- cached answer is allowed to look before somebody asks for a refresh.
    scanned_at    TIMESTAMPTZ,
    retrieved_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- fingerprint is a stable hash of the findings, served as an ETag so a
    -- browser holding last minute's answer is told "unchanged" rather than
    -- sent two megabytes to discover it.
    fingerprint   TEXT NOT NULL DEFAULT '',
    expires_at    TIMESTAMPTZ NOT NULL,

    UNIQUE (product, repository, provider, artifact_ref)
);
CREATE INDEX security_scans_expiry_idx ON security_scans (expires_at);
CREATE INDEX security_scans_key_idx ON security_scans (product, artifact_key);

CREATE TABLE security_findings (
    id            BIGSERIAL PRIMARY KEY,
    scan_id       BIGINT NOT NULL REFERENCES security_scans(id) ON DELETE CASCADE,

    -- cve is the public identifier, upper-cased; issue_id is the scanner's own
    -- ("XRAY-123456"). Both, because a finding without a CVE still needs an
    -- identity and a support case is opened against the scanner's.
    cve           TEXT NOT NULL DEFAULT '',
    issue_id      TEXT NOT NULL DEFAULT '',
    severity      TEXT NOT NULL
                    CHECK (severity IN ('critical','high','medium','low','unknown')),
    fixable       BOOLEAN NOT NULL DEFAULT FALSE,

    -- component_id deliberately EXCLUDES the version - `deb://openssl`, not
    -- `deb://openssl:1.1.1n`. It is the key two releases are compared on, and a
    -- version in it would make every package bump read as one finding resolved
    -- and one introduced: a patch release reporting a fix it did not make.
    component_id      TEXT NOT NULL DEFAULT '',
    component_name    TEXT NOT NULL DEFAULT '',
    component_version TEXT NOT NULL DEFAULT '',
    component_type    TEXT NOT NULL DEFAULT '',
    -- fixed_in is the cleanest fixed version, for the listing. The full list
    -- lives in the detail tier.
    fixed_in      TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL DEFAULT '',

    UNIQUE (scan_id, cve, issue_id, component_id)
);
-- The three searches the interface offers, one index each. Partial on cve
-- because findings without one are a small minority and never searched by it.
CREATE INDEX security_findings_cve_idx ON security_findings (cve) WHERE cve <> '';
CREATE INDEX security_findings_component_idx ON security_findings (component_name);
CREATE INDEX security_findings_scan_idx ON security_findings (scan_id, severity);

CREATE TABLE security_details (
    product      TEXT NOT NULL,
    repository   TEXT NOT NULL,
    provider     TEXT NOT NULL,
    artifact_ref TEXT NOT NULL,

    -- The complete normalized report, as JSON. Normalized rather than Xray's
    -- own body: the raw body is a vendor payload nobody above the plugin can
    -- read, and storing it would put Xray's schema in the database that the
    -- provider boundary exists to keep it out of.
    payload      BYTEA NOT NULL,
    fingerprint  TEXT NOT NULL DEFAULT '',
    retrieved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (product, repository, provider, artifact_ref)
);
CREATE INDEX security_details_expiry_idx ON security_details (expires_at);

-- +goose Down
DROP TABLE IF EXISTS security_details;
DROP TABLE IF EXISTS security_findings;
DROP TABLE IF EXISTS security_scans;
