-- The service's own record of when it was serving.
--
-- See the Postgres migration of the same name for the argument. Identical on
-- purpose: `task run` is where most of this is exercised, and an availability
-- record that only existed in production would be a screen nobody could
-- develop against.

-- +goose Up
CREATE TABLE service_availability (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    component   TEXT    NOT NULL,
    instance_id TEXT    NOT NULL,
    status      TEXT    NOT NULL CHECK (status IN ('healthy','degraded')),
    version     TEXT    NOT NULL DEFAULT '',
    began_at    TEXT    NOT NULL,
    until_at    TEXT    NOT NULL
);

CREATE INDEX service_availability_window_idx
    ON service_availability (component, until_at DESC);

CREATE INDEX service_availability_instance_idx
    ON service_availability (component, instance_id, until_at DESC);

-- +goose Down
DROP TABLE service_availability;
