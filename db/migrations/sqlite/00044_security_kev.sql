-- Known-exploited vulnerabilities, stored so they can be counted and sorted
-- without reading a payload.
--
-- # Why a column rather than a derived read
--
-- Because it is the DEFAULT SORT KEY and the headline number. A release page
-- opens on "4 known-exploited vulnerabilities" and a table ordered with those
-- four at the top, and both have to be answerable from the index tier - which
-- is the tier that survives when the prose has been evicted, and the only one
-- a listing of twenty releases reads. Deriving the flag would mean
-- decompressing every stored payload to draw a badge.
--
-- # Why it is on three tables
--
-- The same three places every other security number lives, for the same
-- reasons:
--
--   security_findings.kev    per ROW, so a table can sort and filter on it and
--                            a search can ask "which releases carry an
--                            exploited CVE".
--   security_scans.kev       per ARTIFACT, so an image's badge does not need
--                            its findings loaded.
--   package_security.kevs    per RELEASE and DISTINCT, which is the number the
--                            listing prints. Distinct because a KEV in a base
--                            image carried by forty images is one advisory to
--                            chase and forty places it lands, and "40
--                            known-exploited vulnerabilities" reads as forty
--                            problems.
--
-- package_security_sources.kevs is the same per scanner, and it is the number
-- that decides whether a second scanner earned its licence: four thousand
-- extra lows nobody will read and two exploited CVEs nobody else saw look
-- identical in only_cves.
--
-- # Why false is not "not exploited"
--
-- It is "no scanner that answered said so". A deployment with only Xray - which
-- has no KEV feed in the versions this platform has seen - stores false on
-- every row, and the interface must not draw "0 known-exploited" as a clean
-- bill of health on a release nothing with a KEV feed has looked at. That
-- distinction lives in the reading code; this schema only has to be able to
-- carry the fact.

-- +goose Up

ALTER TABLE security_findings ADD COLUMN kev BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE security_scans ADD COLUMN kev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE security_scans ADD COLUMN kev_fixable INTEGER NOT NULL DEFAULT 0;

ALTER TABLE package_security ADD COLUMN kev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN kevs INTEGER NOT NULL DEFAULT 0;
ALTER TABLE package_security ADD COLUMN kev_fixable INTEGER NOT NULL DEFAULT 0;

ALTER TABLE package_security_sources ADD COLUMN kevs INTEGER NOT NULL DEFAULT 0;

-- The one query the KEV segment makes that would otherwise be a full scan:
-- "the exploited findings of this scan, worst first". Partial, because the
-- exploited ones are a small fraction of a release's rows and an index over
-- the other ninety thousand earns nothing.
CREATE INDEX security_findings_kev_idx ON security_findings (scan_id, severity) WHERE kev;

-- +goose Down

DROP INDEX IF EXISTS security_findings_kev_idx;
ALTER TABLE package_security_sources DROP COLUMN kevs;
ALTER TABLE package_security DROP COLUMN kev_fixable;
ALTER TABLE package_security DROP COLUMN kevs;
ALTER TABLE package_security DROP COLUMN kev;
ALTER TABLE security_scans DROP COLUMN kev_fixable;
ALTER TABLE security_scans DROP COLUMN kev;
ALTER TABLE security_findings DROP COLUMN kev;
