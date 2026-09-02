-- See the Postgres migration of the same name for the argument.
--
-- Deviations, as elsewhere in this directory: TEXT timestamps.
--
-- Rendered charts, reused across runs and across releases.
--
-- # Why a cache is safe here, which is the only interesting question
--
-- A cache that can return a different answer from the work it replaces is not a
-- cache, it is a bug with a hit rate. The key names every input that can change
-- what `helm template` produces and nothing else: the chart by its LAYER DIGEST
-- (which content-addresses the bytes), helm's own version, the pinned
-- kubeVersion and apiVersions, the fixed release name and namespace, and which
-- of the two renders a run performs it is.
--
-- Those are exactly the fields a run already records as its provenance, because
-- rule 5 requires a finding to be re-derivable from them. That is not a
-- coincidence - the set of things that make a run reproducible and the set that
-- make its render reusable are the same set.
--
-- # Why the digest and not the chart's name and version
--
-- A vendor who republishes 4.2.1 with a fixed template has shipped different
-- bytes under the same version. A cache keyed by name and version would serve
-- the old answer forever; one keyed by digest cannot, because different bytes
-- are a different key and the stale row is simply never asked for again.
--
-- # What a hit avoids
--
-- The helm subprocess, and - because the layer digest is known from the
-- release's recorded contents before anything is fetched - the registry round
-- trip and the unpack as well. Most charts are unchanged between two releases
-- of a product, so the second check of an orb is mostly cache and the vendor's
-- registry sees almost no traffic for it.
--
-- # Why this is evictable and everything else about a run is not
--
-- It is derived data with a deterministic recipe: an evicted row costs one
-- render to rebuild and can never be WRONG. That is the same argument that
-- makes a package's manifest bodies evictable while what a package IS is kept
-- forever (see internal/store/manifestcache.go), and it is swept the same way:
-- an LRU trim to a byte budget, plus a TTL.

-- +goose Up

CREATE TABLE compliance_render_cache (
    -- sha256 over the chart digest, the variant and the render inputs.
    cache_key       TEXT PRIMARY KEY,

    -- The parts of the key, kept as columns so a human can see WHY a row is
    -- there. Nothing joins on them; they are for the operator reading the table
    -- when a cache is suspected, which is the moment a hash-only row is useless.
    chart_digest    TEXT      NOT NULL,
    variant         TEXT      NOT NULL CHECK (variant IN ('base','probe')),
    inputs_digest   TEXT      NOT NULL,

    -- Chart metadata, because a hit has to reproduce everything loading the
    -- chart produced. The name and version are on every finding's address, and
    -- a cache that returned only the manifests would address findings to an
    -- empty chart.
    chart_name      TEXT      NOT NULL DEFAULT '',
    chart_version   TEXT      NOT NULL DEFAULT '',
    app_version     TEXT      NOT NULL DEFAULT '',
    subchart_path   TEXT      NOT NULL DEFAULT '',
    -- The chart's own values.yaml as shipped. Stored because a check can read
    -- `chart.values`, and an entry that dropped them would make such a check
    -- behave differently on a hit than on a miss.
    values_yaml     TEXT      NOT NULL DEFAULT '',

    manifests       TEXT      NOT NULL,
    bytes           INTEGER   NOT NULL DEFAULT 0,

    created_at      TEXT        NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    -- The LRU hand. Touched on every hit, which is what makes the sweep evict
    -- the charts a vendor has stopped shipping rather than the ones every run
    -- uses.
    last_used_at    TEXT        NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX compliance_render_cache_lru   ON compliance_render_cache (last_used_at);
CREATE INDEX compliance_render_cache_chart ON compliance_render_cache (chart_digest);

-- +goose Down

DROP TABLE IF EXISTS compliance_render_cache;
