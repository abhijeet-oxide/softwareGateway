-- Transfer content accounting asks for one digest inside one transfer.
--
-- The jobs uniqueness index starts (transfer_id, kind, digest, target_repo_id),
-- so digest is not searchable unless kind is also known. The Downloads listing
-- does not know kind: manifests and blobs are both content. Without this index,
-- each of its correlated digest lookups reads every job in the transfer.

-- +goose Up
CREATE INDEX jobs_transfer_digest_idx ON jobs (transfer_id, digest);

-- +goose Down
DROP INDEX jobs_transfer_digest_idx;