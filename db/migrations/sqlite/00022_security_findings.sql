-- See the Postgres migration of the same name for the argument.
--
-- One deviation: the partial index on `cve` is written as a full index, because
-- the tables this runs against in development are small enough that the
-- distinction buys nothing and the simpler statement reads better beside its
-- Postgres twin.

-- +goose Up

CREATE TABLE security_scans (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,

    product       TEXT NOT NULL,
    repository    TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'source',
    provider      TEXT NOT NULL,

    artifact_ref  TEXT NOT NULL,
    artifact_key  TEXT NOT NULL,
    artifact_tag  TEXT,
    artifact_kind TEXT,
    artifact_repo TEXT,

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

    scanned_at    TEXT,
    retrieved_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    fingerprint   TEXT NOT NULL DEFAULT '',
    expires_at    TEXT NOT NULL,

    UNIQUE (product, repository, provider, artifact_ref)
);
CREATE INDEX security_scans_expiry_idx ON security_scans (expires_at);
CREATE INDEX security_scans_key_idx ON security_scans (product, artifact_key);

CREATE TABLE security_findings (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    scan_id       INTEGER NOT NULL REFERENCES security_scans(id) ON DELETE CASCADE,

    cve           TEXT NOT NULL DEFAULT '',
    issue_id      TEXT NOT NULL DEFAULT '',
    severity      TEXT NOT NULL
                    CHECK (severity IN ('critical','high','medium','low','unknown')),
    fixable       INTEGER NOT NULL DEFAULT 0,

    component_id      TEXT NOT NULL DEFAULT '',
    component_name    TEXT NOT NULL DEFAULT '',
    component_version TEXT NOT NULL DEFAULT '',
    component_type    TEXT NOT NULL DEFAULT '',
    fixed_in      TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL DEFAULT '',

    UNIQUE (scan_id, cve, issue_id, component_id)
);
CREATE INDEX security_findings_cve_idx ON security_findings (cve);
CREATE INDEX security_findings_component_idx ON security_findings (component_name);
CREATE INDEX security_findings_scan_idx ON security_findings (scan_id, severity);

CREATE TABLE security_details (
    product      TEXT NOT NULL,
    repository   TEXT NOT NULL,
    provider     TEXT NOT NULL,
    artifact_ref TEXT NOT NULL,

    payload      BLOB NOT NULL,
    fingerprint  TEXT NOT NULL DEFAULT '',
    retrieved_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    expires_at   TEXT NOT NULL,

    PRIMARY KEY (product, repository, provider, artifact_ref)
);
CREATE INDEX security_details_expiry_idx ON security_details (expires_at);

-- +goose Down
DROP TABLE IF EXISTS security_details;
DROP TABLE IF EXISTS security_findings;
DROP TABLE IF EXISTS security_scans;
