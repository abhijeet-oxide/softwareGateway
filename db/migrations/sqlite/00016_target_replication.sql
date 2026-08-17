-- What we last WROTE to a registry's own configuration, and what it did next.
-- See the Postgres migration of the same name for the argument.

-- +goose Up
CREATE TABLE target_replication (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id          INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    target_name         TEXT    NOT NULL,

    mode                TEXT    NOT NULL
        CHECK (mode IN ('copy', 'mirror', 'proxy')),

    applied_config_hash TEXT    NOT NULL DEFAULT '',
    applied_secret_hash TEXT    NOT NULL DEFAULT '',
    applied_at          TEXT,
    applied_by          TEXT    NOT NULL DEFAULT '',

    last_observed_at    TEXT,
    drift_detected      INTEGER NOT NULL DEFAULT 0,
    drift_summary       TEXT    NOT NULL DEFAULT '',

    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE (product_id, target_name)
);

CREATE INDEX target_replication_drift_idx
    ON target_replication (drift_detected, product_id);

CREATE TABLE mirror_syncs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    product_id      INTEGER NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    target_name     TEXT    NOT NULL,
    transfer_id     INTEGER REFERENCES transfers(id) ON DELETE SET NULL,

    requested_at    TEXT,
    started_at      TEXT,
    finished_at     TEXT,

    status          TEXT    NOT NULL
        CHECK (status IN ('requested', 'running', 'succeeded', 'diverged', 'failed', 'unknown')),
    quay_status     TEXT    NOT NULL DEFAULT '',

    observed_digest TEXT    NOT NULL DEFAULT '',
    expected_digest TEXT    NOT NULL DEFAULT '',

    message         TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    updated_at      TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX mirror_syncs_target_idx
    ON mirror_syncs (product_id, target_name, requested_at DESC);

CREATE INDEX mirror_syncs_transfer_idx
    ON mirror_syncs (transfer_id);

ALTER TABLE transfers ADD COLUMN strategy TEXT NOT NULL DEFAULT 'copy'
    CHECK (strategy IN ('copy', 'mirror', 'proxy'));

-- +goose Down
DROP TABLE mirror_syncs;
DROP TABLE target_replication;
