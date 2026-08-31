-- See the Postgres migration of the same name for the argument.
--
-- SQLite cannot drop an inline UNIQUE, so the table is rebuilt. The rows are
-- CARRIED rather than dropped and left to the next sync to refill: a findings
-- index that empties itself on upgrade is a security page that silently goes
-- blank for every release nobody re-syncs, and "re-sync everything" is not a
-- migration step anyone should have to be told about.
--
-- The carried rows are the collapsed ones - the duplicates this key exists to
-- keep were never written - so a release still reports its old total until its
-- next sync. That is the honest state: the stored rows are what the last sync
-- managed to store.

-- +goose Up

CREATE TABLE security_findings_new (
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

    UNIQUE (scan_id, cve, issue_id, component_id, component_version)
);

INSERT INTO security_findings_new (
    id, scan_id, cve, issue_id, severity, fixable,
    component_id, component_name, component_version, component_type, fixed_in, summary)
SELECT id, scan_id, cve, issue_id, severity, fixable,
       component_id, component_name, component_version, component_type, fixed_in, summary
  FROM security_findings;

DROP TABLE security_findings;
ALTER TABLE security_findings_new RENAME TO security_findings;

CREATE INDEX security_findings_cve_idx ON security_findings (cve);
CREATE INDEX security_findings_component_idx ON security_findings (component_name);
CREATE INDEX security_findings_scan_idx ON security_findings (scan_id, severity);

ALTER TABLE package_security ADD COLUMN distinct_cves INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE package_security DROP COLUMN distinct_cves;
