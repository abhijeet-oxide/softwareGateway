-- The manifests a compliance run judged, kept so a finding can be SHOWN.
--
-- # Why a report keeps its inputs at all
--
-- Rule 5 - reproducible or it is an opinion - was served until now by recording
-- the inputs a run used: the chart digest, the helm version, the pinned
-- Kubernetes version, the rulebook digest. That is the right guarantee and it
-- is not the one a vendor engineer needs at the moment they are reading a row.
-- Theirs is smaller: show me.
--
-- Answering "is this true?" from the inputs alone means pulling the chart out
-- of the registry, installing the same helm and rendering it again. Everybody
-- skips that, which means a disputed finding is settled by whether the vendor
-- trusts the tool. The rendered manifest settles it in one glance, and settles
-- it the same way for both sides of the conversation because it is one
-- artifact.
--
-- # Why the rendered text and not a re-render on demand
--
-- A re-render can differ from what was judged - a chart that reads the clock, a
-- helm upgraded since, a values default changed in a later chart version - and
-- evidence that can differ from what was judged is not evidence. These are the
-- exact bytes the checks were evaluated against.
--
-- # Why one row per CHART and not per object
--
-- A result's rendered_line is an offset into the stream `helm template`
-- produced for the whole chart. Splitting that stream per object would make
-- every one of those offsets wrong, and the offsets are the entire point: they
-- are what turns "this chart has a finding" into "line 412".
--
-- # Why only the latest run keeps its evidence
--
-- This is the one part of a run whose size the vendor sets, and every run of
-- every release would keep its own copy. The interface reads the latest run of
-- a release and nothing else does, so a completed run drops the evidence of the
-- runs it supersedes - see FinishComplianceRun. The RESULTS of older runs are
-- kept; it is only the manifests behind them that are reclaimed, and a run
-- whose evidence has gone says so rather than showing an empty document.

-- +goose Up

CREATE TABLE compliance_rendered (
    run_id          UUID    NOT NULL REFERENCES compliance_runs(id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,

    -- A chart's document, or a manifest the release ships as-is. Exactly one of
    -- these is set, and together they are how a request names a document.
    chart           TEXT    NOT NULL DEFAULT '',
    chart_version   TEXT    NOT NULL DEFAULT '',
    source_file     TEXT    NOT NULL DEFAULT '',

    content         TEXT    NOT NULL,
    line_count      INTEGER NOT NULL DEFAULT 0,
    byte_count      INTEGER NOT NULL DEFAULT 0,

    -- Cut at the evidence budget. Recorded because line numbers past the cut do
    -- not exist, and an excerpt that silently showed the wrong lines would be
    -- worse than no excerpt at all.
    truncated       BOOLEAN NOT NULL DEFAULT FALSE,

    PRIMARY KEY (run_id, seq)
);

-- The lookup a result performs: this run, this chart (or this file).
CREATE INDEX compliance_rendered_key ON compliance_rendered (run_id, chart, source_file);

-- +goose Down

DROP TABLE IF EXISTS compliance_rendered;
