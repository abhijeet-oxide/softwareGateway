-- The service's own record of when it was serving.
--
-- # The question this answers
--
-- "Was the gateway up, and how often has it not been?" - asked on the Overview
-- page, which is where somebody looks first when a colleague says the thing
-- was broken this morning. Until this existed the honest answer was a shrug: a
-- metrics stack holds `up` for anybody who has a dashboard open and knows what
-- to type into it, and nothing in the product itself remembered a thing. Worse,
-- the one availability statement the interface DID make came from a browser
-- deciding an endpoint's error meant the backend was gone, which is how an
-- application with no outage at all came to report a flapping one.
--
-- # Why intervals rather than heartbeats
--
-- A row per beat is the obvious shape and the wrong one: a fifteen-second beat
-- is five and a half thousand rows a day per replica, all of them saying the
-- same thing, and answering "when was it down" then means scanning them to
-- find the gaps. So a beat EXTENDS the run it is part of - one UPDATE of
-- `until_at` - and only starts a new row when the previous beat is too old to
-- be continuous with this one, or when the status changed. A replica that runs
-- for a month is one row; a restart is two. The gaps BETWEEN rows are the
-- outages, which makes the question a read of a handful of rows rather than an
-- aggregation over a hundred thousand.
--
-- # Why this is honest about what it does not know
--
-- Nothing writes here while the process is not running, which is exactly the
-- property that makes a gap meaningful: the record cannot claim to have been
-- up during a crash, a failed deployment or a database outage, because writing
-- it requires the very things that were not working. The cost is the other
-- side of the same coin - a gap is also what "nobody had this deployed yet"
-- looks like - so a reader is told when the record STARTS and no window is
-- ever reported as down before its first row.
--
-- # One row per replica
--
-- Each process records its own runs under its own `instance_id`, and the API
-- unions them. Two replicas mean the service was available whenever EITHER was
-- serving, which is the fact a reader cares about, and a rolling restart that
-- never dropped a request correctly shows no outage at all.

-- +goose Up
CREATE TABLE service_availability (
    id          BIGSERIAL   PRIMARY KEY,
    component   TEXT        NOT NULL,
    instance_id TEXT        NOT NULL,
    -- 'healthy' or 'degraded'. A process that cannot serve at all does not get
    -- to write 'down' here - it writes nothing, and the gap says it.
    status      TEXT        NOT NULL CHECK (status IN ('healthy','degraded')),
    version     TEXT        NOT NULL DEFAULT '',
    began_at    TIMESTAMPTZ NOT NULL,
    until_at    TIMESTAMPTZ NOT NULL
);

-- The only read: runs that touch a window, newest first. Leading on
-- `until_at` because every query is "since when", and a month of history is a
-- range scan from one end of it.
CREATE INDEX service_availability_window_idx
    ON service_availability (component, until_at DESC);

-- The write path's own lookup: the run this process is currently extending.
CREATE INDEX service_availability_instance_idx
    ON service_availability (component, instance_id, until_at DESC);

-- +goose Down
DROP TABLE service_availability;
