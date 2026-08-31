-- The row identity that made one sync report two different totals.
--
-- # The symptom
--
-- A release's listing row said 90,808 findings. Its own security tab, from the
-- same sync, said 86,085. Neither page was wrong about its own arithmetic: the
-- listing quotes what the sync summed in memory, the tab counts the rows that
-- reached this table, and 4,723 findings did not survive the trip.
--
-- # The cause
--
-- This table's unique key was (scan, CVE, issue, component_id), and
-- component_id deliberately carries NO VERSION - `alpine://libcrypto3`, never
-- `alpine://libcrypto3:3.5.5-r0`. That is the right identity for comparing two
-- releases and the wrong one for a row: an image holding two builds of one
-- package, which any multi-stage build does routinely, has two things to
-- upgrade and wrote one row. `ON CONFLICT DO NOTHING` then discarded the second
-- silently, which is the worst way to lose a security finding.
--
-- # The fix, in two halves
--
-- The version joins the key here, so the row count can equal the counted total.
-- And the provider collapses exact duplicates before it hands findings over
-- (security.DedupeFindings), so the total and the rows are computed from ONE
-- list rather than from two that were expected to agree. A page can quote two
-- numbers for one sync only if two lists exist; after this, one does.
--
-- # Why a separate index rather than an altered constraint
--
-- Postgres names an inline UNIQUE after its columns, and the name is what has
-- to be dropped. Named explicitly here so the next person changing it does not
-- have to guess what Postgres called it.

-- +goose Up

ALTER TABLE security_findings
    DROP CONSTRAINT IF EXISTS security_findings_scan_id_cve_issue_id_component_id_key;

CREATE UNIQUE INDEX IF NOT EXISTS security_findings_row_idx
    ON security_findings (scan_id, cve, issue_id, component_id, component_version);

-- The number the page prints biggest, stored rather than recomputed.
--
-- distinct_total collapses (CVE, package) PAIRS - openssl and libssl3 carrying
-- one advisory are two - and the interface labelled it "unique CVEs", where a
-- reader counts one. Both numbers are worth having and they are different
-- questions; what was not worth having was one number wearing the other's name.
ALTER TABLE package_security ADD COLUMN distinct_cves INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE package_security DROP COLUMN distinct_cves;
DROP INDEX IF EXISTS security_findings_row_idx;
