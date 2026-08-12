-- A blob job that must not believe the destination.
--
-- # What the destination said, and what was true
--
-- Artifactory answers `HEAD /v2/<path>/blobs/<digest>` from its checksum index,
-- which spans the whole Artifactory repository rather than the image path the
-- request named. A blob present anywhere under `apm0014228-oci-stage` therefore
-- reads as present under every path in it. The blob fast path takes that as an
-- answer, skips the upload, and records a placement.
--
-- The truth arrives at manifest push, where Artifactory tries to link the blob
-- into `<image>/<manifest-digest>/sha256__<hex>` and cannot:
--
--   HTTP 400: manifest invalid: map[description:Failed to copy blob sha256:… to
--   orbs/cfx-5000-…/sha256:…/sha256__dc82908b11cf…]
--
-- Every manifest of the bundle failed this way, permanently, while every blob
-- job reported success. The design anticipated exactly this — a placement is
-- strong evidence and not proof, with the manifest push as the backstop
-- (docs/design/11 §2.5) — and the backstop was never built. The class was
-- produced by the engine and consumed by nothing.
--
-- # Why a column and not a retry
--
-- Requeueing the blob is not enough on its own. The next attempt asks the same
-- HEAD and gets the same lie, skips the upload again, and the manifest fails
-- again — eight times, and then permanently. Repair needs a job that is
-- forbidden from taking any fast path: no placement record, no HEAD, no mount,
-- just the bytes.
--
-- Set by the repair and cleared when the job succeeds, so it costs one full
-- upload of the blobs of one manifest rather than of the transfer.

-- +goose Up
ALTER TABLE jobs ADD COLUMN force_upload BOOLEAN NOT NULL DEFAULT FALSE;

-- +goose Down
ALTER TABLE jobs DROP COLUMN force_upload;
