-- Compliance: does a release follow this organization's own Kubernetes and CNF
-- standards.
--
-- # Why the results are denormalized
--
-- A result copies the check's title, severity, category and remediation rather
-- than joining to a checks table. Two reasons, and both are about the report
-- outliving the run.
--
-- A vendor receives an export and works through it over weeks. In that time the
-- policy pack is edited - a severity is lowered, a title is reworded, a check
-- is retired. A joined report would silently change under them, or lose rows
-- entirely, and the conversation would be about which version of the tool
-- somebody ran. A copied one says what was asserted at the moment it was
-- asserted, which is the only thing a finding can honestly claim.
--
-- Second: there is no checks table to join to. The catalogue lives in YAML on
-- disk, is rebuilt on every load, and is identified by the bundle digest
-- recorded on the run. Persisting it would create a second source of truth for
-- something Git already owns.
--
-- # Why every result is stored, not only the failures
--
-- Rule 2 of the compliance model. "40 workloads, all compliant" and "the
-- traversal never reached them" are the same empty list, and only one of them
-- is a reason to ship. The passes are the denominator, and a report without a
-- denominator is a number nobody can act on.

-- +goose Up

CREATE TABLE compliance_runs (
    id              UUID PRIMARY KEY,
    package_id      BIGINT      NOT NULL REFERENCES packages(id) ON DELETE CASCADE,

    -- '' never run | running | complete | failed | cancelled
    state           TEXT        NOT NULL DEFAULT ''
                      CHECK (state IN ('','running','complete','failed','cancelled')),
    error           TEXT,

    -- pass | conditional | fail | inconclusive. Empty while running.
    verdict         TEXT        NOT NULL DEFAULT '',

    -- WHAT PRODUCED THIS. Rule 5: reproducible, or it is an opinion. Every
    -- column here is something that could change a result, so a report that
    -- cannot state them cannot be re-derived a year later - and re-deriving it
    -- is exactly what happens when a vendor disputes a finding.
    bundle_digest   TEXT        NOT NULL DEFAULT '',
    helm_version    TEXT        NOT NULL DEFAULT '',
    kube_version    TEXT        NOT NULL DEFAULT '',
    checks          INTEGER     NOT NULL DEFAULT 0,

    pass_count      INTEGER     NOT NULL DEFAULT 0,
    fail_count      INTEGER     NOT NULL DEFAULT 0,
    skip_count      INTEGER     NOT NULL DEFAULT 0,
    error_count     INTEGER     NOT NULL DEFAULT 0,
    waived_count    INTEGER     NOT NULL DEFAULT 0,
    blocking_count  INTEGER     NOT NULL DEFAULT 0,
    warning_count   INTEGER     NOT NULL DEFAULT 0,
    info_count      INTEGER     NOT NULL DEFAULT 0,

    -- A truncated report LOOKS complete, which is worse than one that failed.
    truncated       BOOLEAN     NOT NULL DEFAULT FALSE,

    -- Who asked. A run started by a person and one started by the scheduler
    -- are read differently when somebody is working out why a release was
    -- blocked at 3am.
    trigger         TEXT        NOT NULL DEFAULT 'manual'
                      CHECK (trigger IN ('manual','auto','api')),

    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    -- Stale means the Coordinator that claimed this run died. The sweeper
    -- releases the claim; without it a release would be stuck "running"
    -- forever and could never be checked again.
    heartbeat_at    TIMESTAMPTZ
);

CREATE INDEX compliance_runs_package ON compliance_runs (package_id, started_at DESC);
CREATE INDEX compliance_runs_running ON compliance_runs (heartbeat_at) WHERE state = 'running';

-- One row per chart, so a reader sees the DENOMINATOR of the run and not only
-- its findings. A release where three of ninety-seven charts did not render is
-- a different release from one where all ninety-seven did, and the finding
-- count alone cannot tell them apart.
CREATE TABLE compliance_charts (
    run_id          UUID    NOT NULL REFERENCES compliance_runs(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    version         TEXT    NOT NULL DEFAULT '',
    artifact_digest TEXT    NOT NULL DEFAULT '',
    artifact_ref    TEXT    NOT NULL DEFAULT '',
    -- ok | failed | skipped
    status          TEXT    NOT NULL DEFAULT 'ok',
    error           TEXT,
    resources       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, name, version, artifact_digest)
);

CREATE TABLE compliance_results (
    run_id          UUID    NOT NULL REFERENCES compliance_runs(id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,

    check_id        TEXT    NOT NULL,
    -- Copied from the check at evaluation time. See the note above.
    check_title     TEXT    NOT NULL DEFAULT '',
    severity        TEXT    NOT NULL DEFAULT '',
    tier            SMALLINT NOT NULL DEFAULT 1,
    category        TEXT    NOT NULL DEFAULT '',
    pack            TEXT    NOT NULL DEFAULT '',
    remediation     TEXT    NOT NULL DEFAULT '',
    reference       TEXT    NOT NULL DEFAULT '',

    outcome         TEXT    NOT NULL
                      CHECK (outcome IN ('pass','fail','skip','error','waived')),
    -- fixed | configurable | unknown | na. What a values file can still change,
    -- which is what tells a vendor's defect from a site's decision.
    determinacy     TEXT    NOT NULL DEFAULT 'na',

    -- THE ADDRESS. Every column is one step of the path a vendor engineer walks
    -- to open the file, and the ones they could derive are still here because
    -- deriving them needs the release, which they do not have.
    chart           TEXT    NOT NULL DEFAULT '',
    chart_version   TEXT    NOT NULL DEFAULT '',
    subchart_path   TEXT    NOT NULL DEFAULT '',
    artifact_digest TEXT    NOT NULL DEFAULT '',
    artifact_ref    TEXT    NOT NULL DEFAULT '',
    source_file     TEXT    NOT NULL DEFAULT '',
    rendered_line   INTEGER NOT NULL DEFAULT 0,
    api_version     TEXT    NOT NULL DEFAULT '',
    kind            TEXT    NOT NULL DEFAULT '',
    namespace       TEXT    NOT NULL DEFAULT '',
    name            TEXT    NOT NULL DEFAULT '',
    container       TEXT    NOT NULL DEFAULT '',
    container_type  TEXT    NOT NULL DEFAULT '',
    locus           TEXT    NOT NULL DEFAULT '',

    observed        TEXT    NOT NULL DEFAULT '',
    expected        TEXT    NOT NULL DEFAULT '',
    message         TEXT    NOT NULL DEFAULT '',
    error           TEXT    NOT NULL DEFAULT '',

    waiver          TEXT    NOT NULL DEFAULT '',
    waiver_expires  TIMESTAMPTZ,

    -- The identity of a finding ACROSS releases. Chart version and release tag
    -- are deliberately excluded: a vendor who ships the same missing memory
    -- limit twice has one unfixed defect, and a fingerprint including the
    -- version would report it as one fixed and one new - which makes every
    -- release look like a complete turnover and the comparison useless.
    fingerprint     TEXT    NOT NULL DEFAULT '',

    PRIMARY KEY (run_id, seq)
);

-- The listing filter: failures of a run, worst first. Partial, because the
-- passes are the bulk of the rows and are read only when somebody opens the
-- coverage view.
CREATE INDEX compliance_results_findings
    ON compliance_results (run_id, severity, check_id)
    WHERE outcome IN ('fail','error');

CREATE INDEX compliance_results_chart       ON compliance_results (run_id, chart, source_file);
CREATE INDEX compliance_results_check       ON compliance_results (run_id, check_id);
CREATE INDEX compliance_results_fingerprint ON compliance_results (fingerprint);

-- The listing summary, one row per package.
--
-- Separate from compliance_runs for the same reason package_security is
-- separate from its syncs: the Software listing renders a column for every
-- release on the page, and doing that through a correlated subquery over runs
-- is the query that gets slow first and is noticed last.
CREATE TABLE package_compliance (
    package_id      BIGINT PRIMARY KEY REFERENCES packages(id) ON DELETE CASCADE,
    run_id          UUID REFERENCES compliance_runs(id) ON DELETE SET NULL,
    state           TEXT        NOT NULL DEFAULT ''
                      CHECK (state IN ('','running','complete','failed','cancelled')),
    verdict         TEXT        NOT NULL DEFAULT '',
    blocking_count  INTEGER     NOT NULL DEFAULT 0,
    warning_count   INTEGER     NOT NULL DEFAULT 0,
    error_count     INTEGER     NOT NULL DEFAULT 0,
    pass_count      INTEGER     NOT NULL DEFAULT 0,
    checked_at      TIMESTAMPTZ
);

-- +goose Down

DROP TABLE IF EXISTS package_compliance;
DROP TABLE IF EXISTS compliance_results;
DROP TABLE IF EXISTS compliance_charts;
DROP TABLE IF EXISTS compliance_runs;
