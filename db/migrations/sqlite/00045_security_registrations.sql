-- Whether a release has been replicated to a scanner that has to be TOLD about
-- it, and what that scanner holds for it.
--
-- # Why this is stored at all, when the scanner already knows
--
-- Because the page has to draw the answer without a round trip. "Anchore is
-- configured for this release and its images have not been sent yet" is a
-- notice with a button on it, rendered on every open of a release's Security
-- tab - and asking Anchore three questions to decide whether to draw a button
-- would put a third system's availability on the critical path of a page that
-- otherwise reads one database.
--
-- It is a RECORD OF WHAT WE DID, not a mirror of the scanner. The scanner stays
-- the authority: a reader who suspects the two have diverged - somebody deleted
-- the application in Anchore, an image was removed from the version - presses
-- Replicate, which re-reads and re-writes this row. That the two can disagree
-- is the reason the button exists rather than a flaw in keeping this.
--
-- # Why it is per (release, scanner) and not a column on package_security
--
-- Same argument package_security_sources makes. "Has this been replicated to
-- Anchore" and "has this been replicated to whatever comes next" are one
-- question asked of two answerers, and the shape that holds two answerers is a
-- row each. Columns would mean a migration per scanner.
--
-- Xray never has a row here and never needs one: it indexes a repository, so
-- there is nothing to register and the notice is never drawn for it.
--
-- # The counts, and why three of them
--
--   expected    artifacts this release wanted registered
--   submitted   what the last run told the scanner about
--   associated  what the scanner's own grouping holds, READ BACK
--
-- Associated is the one that decides whether the release is done, and it is
-- read back rather than assumed because a successful write is not evidence of
-- the final state. Submitted is kept because a second press of the button
-- should visibly do nothing: "submitted 0, already known 157" is how a reader
-- sees that it ran and had nothing to do, where a silent no-op reads as a
-- button that is broken.

-- +goose Up

CREATE TABLE security_registrations (
    package_id  BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    provider    TEXT NOT NULL,

    -- '' never | registering | registered | partial | failed
    --
    -- `partial` is its own state rather than a failure, because it is the
    -- ordinary outcome of replicating a release that is still transferring:
    -- the images that have landed are registered and answerable, and the rest
    -- need the button pressing again once they arrive. Recording that as
    -- failed would hide the half that worked.
    state       TEXT NOT NULL DEFAULT ''
                  CHECK (state IN ('','registering','registered','partial','failed')),
    error       TEXT,

    expected     INTEGER NOT NULL DEFAULT 0,
    submitted    INTEGER NOT NULL DEFAULT 0,
    already_known INTEGER NOT NULL DEFAULT 0,
    associated   INTEGER NOT NULL DEFAULT 0,
    -- analysed is how many the scanner had FINISHED with when this was written.
    -- It goes stale immediately and that is fine: it is the number that answers
    -- "why has the sync not found anything yet", and being an hour old does not
    -- make it a wrong answer to that.
    analysed     INTEGER NOT NULL DEFAULT 0,

    -- The scanner's own identity for this release, so the page can link to it
    -- and so a later run reuses the same application rather than creating a
    -- second one beside it.
    application     TEXT NOT NULL DEFAULT '',
    application_id  TEXT NOT NULL DEFAULT '',
    version         TEXT NOT NULL DEFAULT '',
    version_id      TEXT NOT NULL DEFAULT '',
    url             TEXT NOT NULL DEFAULT '',

    -- started_at is what makes a claim RECOVERABLE: a row still marked
    -- registering long after anything could plausibly still be running was
    -- claimed by a process that is no longer here.
    --
    -- No heartbeat column, deliberately, where the sync has one. A sync is
    -- minutes and its duration is set by a scanner; this is seconds and its
    -- duration is set by our own request count, so a stale claim is a rare
    -- event with a short window - and every operation behind it is idempotent,
    -- so the cost of a double run is a wasted request rather than a wrong row.
    started_at     TIMESTAMPTZ,
    registered_at  TIMESTAMPTZ,

    -- log is the run's transcript, stored with the result so "what happened
    -- when I pressed Replicate" survives the process that answered it.
    log         TEXT,

    PRIMARY KEY (package_id, provider)
);

-- +goose Down
DROP TABLE IF EXISTS security_registrations;
