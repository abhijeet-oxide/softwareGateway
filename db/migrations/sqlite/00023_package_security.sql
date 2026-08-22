-- See the Postgres migration of the same name for the argument.
--
-- Deviations: a full index rather than a partial one, and TEXT timestamps, as
-- everywhere else in the SQLite schema.
--
-- # Why this table exists when security_scans already holds counts
--
-- Because they answer different questions and have different lifetimes.
-- `security_scans` is per ARTIFACT and expires: it is the cache behind a
-- retrieval. This is per RELEASE and does not expire: it is the RESULT of one,
-- and it is what a listing of two hundred releases renders.
--
-- Without it, a package listing showing a vulnerability column had to query the
-- scanner once per row - twenty scanner-backed reads to draw one column - which
-- is why that column shipped behind a toggle nobody should have had to find.
-- With it, the column is a join and the toggle is deleted.
--
-- # Why syncing is a state and not a timestamp
--
-- `synced_at` answers "has this been synced", which has two answers and needs
-- four. A release nobody has synced, one being synced right now, one whose sync
-- failed, and one that is done are four different situations with four
-- different things to say and to offer - and with only a timestamp, three of
-- them look identical: never synced. That is the same argument migration 00021
-- makes for analysis_state, and it is the same mistake being avoided.
--
-- `started_at` is what makes a claim RECOVERABLE: a row still marked syncing
-- long after anything could plausibly still be running was claimed by a process
-- that is no longer here, and is released rather than left to sit forever
-- showing a spinner.
--
-- # Why the counts are denormalised here
--
-- A listing must not aggregate a thousand finding rows per page. These are the
-- numbers a human reads at a glance; the rows behind them stay in
-- security_scans and security_findings, and the two are written in one
-- transaction so they cannot disagree.

-- +goose Up

CREATE TABLE package_security (
    package_id    INTEGER PRIMARY KEY REFERENCES packages(id) ON DELETE CASCADE,

    -- '' never synced | syncing | synced | failed
    state         TEXT NOT NULL DEFAULT ''
                    CHECK (state IN ('','syncing','synced','failed')),
    error         TEXT,

    provider      TEXT NOT NULL DEFAULT '',
    -- The CONFIGURED repository that answered. Not the source repository: a
    -- release is published by a vendor and scanned where it LANDS, which in a
    -- typical estate is a JFrog target rather than the vendor's own registry.
    repository    TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'target',

    total         INTEGER NOT NULL DEFAULT 0,
    fixable       INTEGER NOT NULL DEFAULT 0,
    critical      INTEGER NOT NULL DEFAULT 0,
    high          INTEGER NOT NULL DEFAULT 0,
    medium        INTEGER NOT NULL DEFAULT 0,
    low           INTEGER NOT NULL DEFAULT 0,
    unknown       INTEGER NOT NULL DEFAULT 0,
    fix_critical  INTEGER NOT NULL DEFAULT 0,
    fix_high      INTEGER NOT NULL DEFAULT 0,
    fix_medium    INTEGER NOT NULL DEFAULT 0,
    fix_low       INTEGER NOT NULL DEFAULT 0,
    fix_unknown   INTEGER NOT NULL DEFAULT 0,
    -- distinct (CVE, component) across the release, which is the other honest
    -- number: the same base-image CVE in ten images is ten things to fix and
    -- also one problem, and quoting either alone misleads in a direction.
    distinct_total INTEGER NOT NULL DEFAULT 0,

    artifacts     INTEGER NOT NULL DEFAULT 0,
    scanned       INTEGER NOT NULL DEFAULT 0,
    not_scanned   INTEGER NOT NULL DEFAULT 0,
    unsupported   INTEGER NOT NULL DEFAULT 0,
    unavailable   INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,

    -- scanned_at is the OLDEST scan among contributing artifacts - the age of
    -- the weakest link. synced_at is when we last asked.
    scanned_at    TEXT,
    synced_at     TEXT,
    started_at    TEXT,
    fingerprint   TEXT NOT NULL DEFAULT ''
);

-- The listing's join, and the recovery sweep's scan. Partial, because a row
-- mid-sync is a tiny minority of a catalogue.
CREATE INDEX package_security_state_idx ON package_security (state);

-- +goose Down
DROP TABLE IF EXISTS package_security;
