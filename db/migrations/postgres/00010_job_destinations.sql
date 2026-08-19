-- A bundle does not land in one repository, and its components keep their names.
--
-- An ORB is an index over container images, Helm charts and generic artifacts,
-- and each of those has its own repository path and its own tag in the source
-- registry. Until now every job in a transfer pointed at the ONE repository
-- configured as the target, and only the package's root manifest was tagged.
--
-- That preserved the content and destroyed the names. Every blob is
-- content-addressed so nothing was lost, but a consumer who could pull
-- `orbs/CFX-5000-k8s/nginx:1.2.3` from the vendor was left with a digest at the
-- destination.
--
-- Two changes follow from letting one transfer write to several repositories.
--
-- 1. The job uniqueness key gains target_repo_id.
--
--    A base layer shared by two components that land in DIFFERENT destination
--    repositories must be pushed to both - the registry stores blobs per
--    repository, so "already uploaded" is only true within one. Under the old
--    key the second job was silently dropped by ON CONFLICT DO NOTHING, and the
--    manifest referencing it would have failed with BLOB_UNKNOWN.
--
--    Planning stays idempotent: the same blob to the same repository is still
--    one job, which is the property the key exists to guarantee.
--
-- 2. Manifest jobs carry the tags to apply.
--
--    Tags were derived at lease time from the package row, which could only
--    ever produce the one tag a person asked for. A component's own tag lives
--    in its `org.opencontainers.image.ref.name` annotation, is resolved at
--    planning time, and has to travel with the job that pushes it. JSON rather
--    than a side table because it is read exactly once, by the worker holding
--    the job, and an array of two strings does not earn a join.

-- +goose Up
ALTER TABLE jobs ADD COLUMN target_tags JSONB;

-- The destination path, denormalised for diagnostics. The authoritative value
-- is repositories.repository_path via target_repo_id; this is what makes
-- `transferctl transfers jobs` able to say where a job is going without a join,
-- and what makes a stuck job legible in a raw SQL session.
ALTER TABLE jobs ADD COLUMN target_repository TEXT;

ALTER TABLE jobs DROP CONSTRAINT jobs_transfer_id_kind_digest_key;
ALTER TABLE jobs ADD CONSTRAINT jobs_transfer_kind_digest_target_key
    UNIQUE (transfer_id, kind, digest, target_repo_id);

-- +goose Down
ALTER TABLE jobs DROP CONSTRAINT jobs_transfer_kind_digest_target_key;
ALTER TABLE jobs ADD CONSTRAINT jobs_transfer_id_kind_digest_key
    UNIQUE (transfer_id, kind, digest);
ALTER TABLE jobs DROP COLUMN target_repository;
ALTER TABLE jobs DROP COLUMN target_tags;
