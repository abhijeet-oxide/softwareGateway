-- Per-image replication outcomes make partial runs actionable after completion.
-- +goose Up
ALTER TABLE security_registrations ADD COLUMN IF NOT EXISTS outcomes TEXT;

-- +goose Down
ALTER TABLE security_registrations DROP COLUMN IF EXISTS outcomes;