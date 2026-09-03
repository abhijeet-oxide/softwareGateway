-- Making a replication a JOB, and making its position survive the page.
--
-- See db/migrations/sqlite/00046_replication_progress.sql for the argument.
-- The short version: a replication used to run on the HTTP request, so its
-- position lived only in that request. Reloading the page lost it. These two
-- columns move the position into the database, where a refresh, a navigation
-- and a Coordinator restart can all find it again.

-- +goose Up

ALTER TABLE security_registrations ADD COLUMN IF NOT EXISTS heartbeat_at TIMESTAMPTZ;
ALTER TABLE security_registrations ADD COLUMN IF NOT EXISTS progress TEXT;

CREATE INDEX IF NOT EXISTS security_registrations_running
    ON security_registrations(state, heartbeat_at);

-- +goose Down

DROP INDEX IF EXISTS security_registrations_running;
ALTER TABLE security_registrations DROP COLUMN IF EXISTS progress;
ALTER TABLE security_registrations DROP COLUMN IF EXISTS heartbeat_at;
