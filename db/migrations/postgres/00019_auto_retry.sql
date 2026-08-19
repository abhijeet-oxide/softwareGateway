-- How many times this transfer has been retried BY THE SYSTEM.
--
-- A retryable failure is one a second attempt could plausibly fix: a timeout, a
-- reset connection, a 503 from a registry having a bad minute. Leaving those
-- for a person to notice and press a button means a download that could have
-- finished by itself sits failed overnight - and the person, when they arrive,
-- does exactly what the system could have done unattended.
--
-- The counter is what keeps that from becoming a loop. Automatic retries are
-- bounded and spaced; a transfer that has used them up stays failed and waits
-- for a human, because at that point the failure is telling us something a
-- fourth attempt will not change.
--
-- Reset to zero by a MANUAL retry, which is somebody saying they have dealt
-- with the cause - and by a successful one, so a transfer that recovers is not
-- carrying its history into the next outage.

-- +goose Up
ALTER TABLE transfers ADD COLUMN auto_retries SMALLINT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE transfers DROP COLUMN auto_retries;
