-- The index a listing needs to carry each release's transfer history.
--
-- See the Postgres migration of the same name for the argument.

-- +goose Up
CREATE INDEX transfers_by_package_idx ON transfers (package_id, created_at DESC);

-- +goose Down
DROP INDEX transfers_by_package_idx;
