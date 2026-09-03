-- Making a replication a JOB, and making its position survive the page.
--
-- 00045 argued that a replication needs no heartbeat because it is seconds
-- rather than minutes. That was true of its duration and wrong about its
-- shape: it ran on the HTTP request, so nothing outside that request could
-- see where it had got to. A reader who pressed Replicate and reloaded the
-- page was shown "registering" with no position, no transcript and no way to
-- tell a live run from a Coordinator that had died holding the claim - which
-- is the one question the state is read to answer.
--
-- Two columns fix both halves:
--
--   heartbeat_at  the run saying it is still alive, every few seconds. A row
--                 marked registering whose heartbeat has stopped is an
--                 abandoned claim, and can be told from a live one WITHOUT
--                 waiting for the whole stale window to expire.
--   progress      the run's position and transcript, as JSON, rewritten on
--                 every beat. This is what makes the panel survive a refresh,
--                 a navigation, and a Coordinator restart: the position lives
--                 in the database rather than in the memory of whichever
--                 replica happens to be running the work.
--
-- Writing progress on the beat rather than on every event is deliberate. The
-- interesting resolution here is seconds - a person watching a bar cannot use
-- more - and an UPDATE per submitted image would put a hundred and fifty
-- writes into an operation whose whole point is that it is quick.

-- +goose Up

ALTER TABLE security_registrations ADD COLUMN heartbeat_at TIMESTAMPTZ;
ALTER TABLE security_registrations ADD COLUMN progress TEXT;

-- The sweep that releases abandoned claims reads exactly this pair.
CREATE INDEX IF NOT EXISTS security_registrations_running
    ON security_registrations(state, heartbeat_at);

-- +goose Down

DROP INDEX IF EXISTS security_registrations_running;
ALTER TABLE security_registrations DROP COLUMN progress;
ALTER TABLE security_registrations DROP COLUMN heartbeat_at;
