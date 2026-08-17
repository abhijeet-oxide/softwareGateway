-- What we last WROTE to a registry's own configuration, and what it did next.
--
-- Two tables for two different kinds of fact, and keeping them apart is the
-- point of the design rather than an accident of normalisation.
--
-- target_replication is the record of an APPLY: the mode, the fingerprint of
-- the configuration we sent, and the fingerprint of the credentials we sent
-- with it. It exists because drift has to be computed against what we SENT,
-- not against what the registry will return. Quay never returns a stored
-- password; comparing our desired state against its response would report
-- permanent, unfixable drift on every credential field, which is the classic
-- reconciler bug that teaches operators to ignore the drift signal entirely.
-- Hashing what we sent means a ROTATED credential shows as drift and a
-- redacted response does not.
--
-- mirror_syncs is the record of an OBSERVATION: what Quay says happened. It is
-- deliberately not an assertion. When Quay reports that a sync succeeded we do
-- not know what it did, so the terminal step of a mirror transfer walks the
-- destination and records the digest it actually found. That is why
-- observed_digest is here and why `diverged` is a distinct outcome from
-- `succeeded`: a mirror faithfully following an upstream tag that moved is a
-- FACT worth recording, not a failure.
--
-- Neither table holds a byte count, and that absence is deliberate. We do not
-- move those bytes and cannot count them; a column that looked like throughput
-- but was derived from elapsed time would be worse than no column at all.

-- +goose Up
CREATE TABLE target_replication (
    id                  BIGSERIAL PRIMARY KEY,
    product_id          BIGINT      NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    -- The target's configured NAME rather than a repository row id: a target
    -- may be reconfigured to a different path while remaining the same target,
    -- and the apply history should survive that.
    target_name         TEXT        NOT NULL,

    mode                TEXT        NOT NULL
        CHECK (mode IN ('copy', 'mirror', 'proxy')),

    -- The fingerprint of the non-secret configuration we last wrote, and of
    -- the credential values that went with it. Hashes, never values: nothing
    -- in this table can leak a password even to somebody holding a dump.
    applied_config_hash TEXT        NOT NULL DEFAULT '',
    applied_secret_hash TEXT        NOT NULL DEFAULT '',
    applied_at          TIMESTAMPTZ,
    applied_by          TEXT        NOT NULL DEFAULT '',

    -- The last observation, refreshed by any read. Kept beside the apply so a
    -- listing can answer "is this in sync" without a network call.
    last_observed_at    TIMESTAMPTZ,
    drift_detected      BOOLEAN     NOT NULL DEFAULT FALSE,
    drift_summary       TEXT        NOT NULL DEFAULT '',

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (product_id, target_name)
);

CREATE INDEX target_replication_drift_idx
    ON target_replication (drift_detected, product_id)
    WHERE drift_detected;

CREATE TABLE mirror_syncs (
    id              BIGSERIAL PRIMARY KEY,
    product_id      BIGINT      NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    target_name     TEXT        NOT NULL,

    -- The transfer this sync was requested for, when it was. NULL for a sync
    -- Quay ran on its own schedule, which is the normal case and the whole
    -- point of mirroring: convergence keeps working while we are down.
    transfer_id     BIGINT      REFERENCES transfers(id) ON DELETE SET NULL,

    requested_at    TIMESTAMPTZ,
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,

    -- OUR classification of the outcome, which is not Quay's status.
    --   requested  we asked; Quay has not started
    --   running    Quay says it is syncing
    --   succeeded  Quay finished AND the destination holds the digest we expected
    --   diverged   Quay finished and the destination holds a DIFFERENT digest
    --   failed     Quay failed, or the tag is absent because the glob missed it
    --   unknown    Quay reported a status this build does not recognise
    status          TEXT        NOT NULL
        CHECK (status IN ('requested', 'running', 'succeeded', 'diverged', 'failed', 'unknown')),

    -- Quay's own word, kept verbatim beside our classification. An enum that
    -- has grown across versions must not be flattened into ours: `unknown`
    -- plus the raw value is honest, and mapping an unrecognised status onto
    -- SUCCESS or FAIL would not be.
    quay_status     TEXT        NOT NULL DEFAULT '',

    -- What we FOUND at the destination, from a walk after the sync. Empty when
    -- we have not looked yet.
    observed_digest TEXT        NOT NULL DEFAULT '',
    expected_digest TEXT        NOT NULL DEFAULT '',

    message         TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX mirror_syncs_target_idx
    ON mirror_syncs (product_id, target_name, requested_at DESC);

CREATE INDEX mirror_syncs_transfer_idx
    ON mirror_syncs (transfer_id)
    WHERE transfer_id IS NOT NULL;

-- Which strategy performed a transfer, so a year-old record still says HOW it
-- was done. Without it, a transfer with no jobs and no bytes is indisinguishable
-- from one that failed to plan.
ALTER TABLE transfers ADD COLUMN strategy TEXT NOT NULL DEFAULT 'copy'
    CHECK (strategy IN ('copy', 'mirror', 'proxy'));

-- +goose Down
ALTER TABLE transfers DROP COLUMN strategy;
DROP TABLE mirror_syncs;
DROP TABLE target_replication;
