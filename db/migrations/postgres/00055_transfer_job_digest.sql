-- See the SQLite migration of the same name for the argument.

-- +goose Up
CREATE INDEX jobs_transfer_digest_idx ON jobs (transfer_id, digest);

-- +goose Down
DROP INDEX jobs_transfer_digest_idx;