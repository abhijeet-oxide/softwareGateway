-- Xray answers "Artifact doesn't exist or not indexed/cached in Xray" for two
-- situations that have nothing in common: an image it has not looked at, and an
-- image that is not in the repository at all.
--
-- The first is a scan waiting to happen. The second is a TRANSFER waiting to
-- happen, and no amount of scanning will ever produce a result for it. Reported
-- as one line - "142 images have no scan result" - the second kind reads as an
-- accusation against the scanner and sends somebody to the wrong team.
--
-- A column rather than a new `status` value, because status carries a CHECK
-- constraint that SQLite cannot alter without rebuilding the table, and this is
-- a second fact about a not-scanned row rather than a third kind of row. The
-- API composes the two into the status the interface shows.

-- +goose Up
ALTER TABLE security_scans ADD COLUMN missing INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE security_scans DROP COLUMN missing;
