-- A promotion the REGISTRY carries out, and the record of what it did.
--
-- # Why `promoting` is a state and not a flavour of `syncing`
--
-- Both are "no jobs, waiting on somebody else's registry", and that similarity
-- is exactly why they must not share a name. `syncing` means a mirror is
-- pulling from an upstream on its own schedule, and its distinctive outcome is
-- `diverged`: the tag we asked for moved, and what arrived is not what was
-- requested. That is normal for a mirror and nobody should be paged.
--
-- A promotion is a copy WE asked a registry to make between two of its own
-- repositories. There is no upstream and no schedule, so there is nothing for
-- it to diverge from: if the destination ends up holding a different digest,
-- something is wrong and somebody should look. Sharing the state would mean
-- sharing the settle path, and the settle path is where that difference lives.
--
-- # Why `relocate` is a strategy
--
-- `strategy` already answers "how did the content get here" for a settled
-- transfer with no jobs and no bytes, which is otherwise indistinguishable
-- from one that failed to plan. A native promotion is a fourth answer to that
-- same question - the registry relocated it internally - so it belongs in the
-- same column rather than in a boolean beside it.
--
-- # Why the promotion has a row of its own
--
-- The transfer says WHETHER it worked. This says WHO did it, HOW MANY names
-- moved, and what went wrong on the way, which is what an operator asks after
-- the fact and what a retry needs in order to be a retry rather than a fresh
-- request. Same argument as mirror_syncs in 00016.

-- +goose Up
ALTER TABLE transfers DROP CONSTRAINT transfers_state_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_state_check
    CHECK (state IN ('waiting','pending','planning','ready','running','paused','syncing',
                     'promoting','verifying','succeeded','diverged','skipped','failed',
                     'cancelling','cancelled'));

ALTER TABLE transfers DROP CONSTRAINT transfers_strategy_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_strategy_check
    CHECK (strategy IN ('copy', 'mirror', 'proxy', 'relocate'));

CREATE TABLE promotions (
    id              BIGSERIAL PRIMARY KEY,
    transfer_id     TEXT    NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,

    -- promoter is the plugin that claimed the hop: `jfrog` today. Recorded
    -- rather than derived, because the plugin that ran is a fact about this
    -- promotion and the configuration it was derived from can change.
    promoter        TEXT    NOT NULL,

    state           TEXT    NOT NULL
        CHECK (state IN ('requested', 'running', 'succeeded', 'failed')),

    -- What the hop has to move, and how far it got. Names rather than bytes:
    -- a native promotion moves no bytes by construction, so a byte counter
    -- would be a column that is always zero and always misread as a failure.
    names_total     INTEGER NOT NULL DEFAULT 0,
    names_done      INTEGER NOT NULL DEFAULT 0,

    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT    NOT NULL DEFAULT '',

    -- claimed_by and heartbeat_at are the same claim 00027 gave a security
    -- sync, and for the same reason: a Coordinator killed mid-promotion must
    -- not leave a row that reads as running forever. A heartbeat that stopped
    -- is the one honest signal that the holder is gone.
    claimed_by      TEXT    NOT NULL DEFAULT '',
    heartbeat_at    TIMESTAMPTZ,

    requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One promotion per transfer. A retry re-opens THIS row rather than
    -- appending a second, so "what happened to this promotion" has one answer
    -- and the attempt count is where the history lives.
    UNIQUE (transfer_id)
);

CREATE INDEX promotions_open_idx ON promotions (state, requested_at)
    WHERE state IN ('requested', 'running');

-- The NAMES one promotion publishes, and how far it got.
--
-- A child table rather than a JSON column on `promotions`, for one reason that
-- pays for the join: a promotion interrupted half way through can be RESUMED
-- at the exact name rather than restarted. A native promotion is idempotent,
-- so a restart would be correct - but on a 260-name release it would re-issue
-- two hundred calls to discover they were already done, every time a
-- Coordinator was rolled.
--
-- It is also what makes progress honest. A native promotion moves no bytes by
-- construction, so the only true denominator it has is names, and a table of
-- them is where that number comes from rather than a counter somebody has to
-- remember to keep in step.
--
-- Recorded when the promotion is OPENED rather than derived when it runs.
-- What has to arrive at the destination was decided by the tree as it stood
-- when somebody asked; re-deriving it later would let a release re-analysed in
-- between silently change what a promotion means.
CREATE TABLE promotion_names (
    promotion_id    BIGINT  NOT NULL REFERENCES promotions(id) ON DELETE CASCADE,
    position        INTEGER NOT NULL,

    -- repository is RELATIVE to each end's configured base path, so one value
    -- re-bases under the origin's prefix to say where to read and under the
    -- destination's to say where to write. Storing either end's absolute path
    -- would bake one of them into both.
    repository      TEXT    NOT NULL,
    tag             TEXT    NOT NULL,
    digest          TEXT    NOT NULL,

    state           TEXT    NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'promoted', 'failed')),
    last_error      TEXT    NOT NULL DEFAULT '',
    promoted_at     TIMESTAMPTZ,

    PRIMARY KEY (promotion_id, position)
);

CREATE INDEX promotion_names_pending_idx ON promotion_names (promotion_id, position)
    WHERE state <> 'promoted';

-- +goose Down
DROP TABLE promotion_names;
DROP TABLE promotions;

UPDATE transfers SET strategy = 'copy' WHERE strategy = 'relocate';
UPDATE transfers SET state = 'failed', failure_reason = 'downgraded from promoting'
 WHERE state = 'promoting';

ALTER TABLE transfers DROP CONSTRAINT transfers_strategy_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_strategy_check
    CHECK (strategy IN ('copy', 'mirror', 'proxy'));

ALTER TABLE transfers DROP CONSTRAINT transfers_state_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_state_check
    CHECK (state IN ('waiting','pending','planning','ready','running','paused','syncing',
                     'verifying','succeeded','diverged','skipped','failed',
                     'cancelling','cancelled'));
