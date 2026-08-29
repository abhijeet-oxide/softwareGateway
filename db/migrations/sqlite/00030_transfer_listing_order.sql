-- See the Postgres migration of the same name for the argument.

-- +goose Up
CREATE INDEX transfers_recent_idx ON transfers (created_at DESC, id DESC);

-- +goose Down
DROP INDEX transfers_recent_idx;
