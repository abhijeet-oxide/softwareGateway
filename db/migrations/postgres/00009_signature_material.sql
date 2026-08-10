-- Record WHAT a signature actually is, not merely that one exists.
--
-- # What was missing
--
-- `package_relations` records that a package has a signature, and names the
-- MANIFEST that carries it. For NEAR that row points at
-- `signature_orb_23.8.1076` — an image manifest whose single layer is the thing
-- a verifier actually needs:
--
--   config  application/vnd.com.nokia.orb.config.v1+json   2 bytes ("{}")
--   layer   application/pkcs7-signature                    3051 bytes
--
-- The bytes that get verified are that LAYER. Nothing recorded which blob it
-- was. After an inspection the blob is in `blobs` and linked in
-- `artifact_blobs` like any other — indistinguishable, from the outside, from a
-- Helm chart or an image layer sitting beside it.
--
-- So verification would have had to re-derive the vendor's layout from scratch
-- every time it ran: find the relation, find the artifact, parse its manifest,
-- decide which layer is the signature. That is the plugin's knowledge leaking
-- into the verifier — the exact coupling internal/vendors exists to prevent.
--
-- # What this records, and when
--
-- Resolved at INSPECT time, because inspect is what fetches the signature
-- manifest, and it fetches it anyway: the transfer root is the wrapper, so the
-- walk passes through the signature on its way down. No extra request.
--
-- The rule used is vendor-neutral — "a relation of role `signature` names a
-- manifest, and that manifest's layers are the signature material" — which
-- holds for any layout that publishes a signature as a manifest, including the
-- referrers-based ones NEAR will eventually move to.
--
-- `annotations` is stored verbatim rather than picked apart. A verifier will
-- want `com.nokia.ncd.orb.type` to know it is looking at a `generic_signature`,
-- and `com.nokia.rb.name` / `com.nokia.rb.version` to tie it back to the
-- release; a column per vendor key does not end. The same argument as
-- `package_artifacts.annotations` (migration 00004).
--
-- `resolved_at` distinguishes "we looked and this signature carries no blob"
-- from "nobody has inspected this package yet". Without it, an empty
-- blob_digest means both, and only one of them is worth acting on.

-- +goose Up
ALTER TABLE package_relations ADD COLUMN blob_digest     TEXT;
ALTER TABLE package_relations ADD COLUMN blob_media_type TEXT;
ALTER TABLE package_relations ADD COLUMN blob_size       BIGINT NOT NULL DEFAULT 0;
ALTER TABLE package_relations ADD COLUMN annotations     TEXT;
ALTER TABLE package_relations ADD COLUMN resolved_at     TIMESTAMPTZ;

-- +goose Down
ALTER TABLE package_relations DROP COLUMN resolved_at;
ALTER TABLE package_relations DROP COLUMN annotations;
ALTER TABLE package_relations DROP COLUMN blob_size;
ALTER TABLE package_relations DROP COLUMN blob_media_type;
ALTER TABLE package_relations DROP COLUMN blob_digest;
