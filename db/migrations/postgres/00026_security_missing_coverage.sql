-- "Not scanned" and "not in the repository" are two different jobs.
--
-- Xray answers both with one sentence - "Artifact doesn't exist or not
-- indexed/cached in Xray" - so a release with 122 images that had simply never
-- been transferred to JFrog reported 122 images waiting to be scanned. That is
-- a scanning problem on a page read by whoever owns scanning, and the fix was
-- a transfer owned by somebody else entirely.
--
-- The platform asks Artifactory which of the two it is and stores the answer
-- here, so the coverage a reader sees names the work that actually exists.

-- +goose Up
ALTER TABLE package_security ADD COLUMN missing INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE package_security DROP COLUMN missing;
