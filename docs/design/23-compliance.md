# 23 - Custom Software Compliance

> **Prerequisites:** [01 - Domain Model](01-domain-model.md), [02 - Configuration](02-configuration.md), [03 - Persistence](03-persistence.md), [09 - API](09-api.md)
> **Ground truth:** [compliance/00 - The Compliance Model](../compliance/00-compliance-model.md), [compliance/01 - Check Catalog](../compliance/01-check-catalog.md), [compliance/02 - Authoring Checks](../compliance/02-authoring-checks.md), [compliance/03 - Review of the Existing Policies](../compliance/03-sample-policy-review.md)
> **Consumed by:** [17 - Delivery Plan](17-delivery-plan.md), [19 - User Interface](19-user-interface.md), [20 - Downloads](20-download-rules.md)
>
> **Status: DESIGN. Not implemented.** Scheduled at [M11](17-delivery-plan.md).

---

## 1. The statement

**Every release this platform ingests is checked against the organization's own
Kubernetes and CNF standards, before the bytes move, and every finding names one
Kubernetes object inside one chart inside one release.**

The standards are [compliance/source-standards.md](../compliance/source-standards.md) -
118 assertions written from lab and production experience. This document is how
they become a machine that runs on every release, a screen that shows what
passed and what did not, and a spreadsheet a release engineer sends to a vendor.

What it is *not*: an admission controller, a linter with a score, or a second
copy of the vulnerability scanner. [compliance/00](../compliance/00-compliance-model.md) §8
states each non-goal and why.

### 1.1 What "tier 1" means here

The organization's phrasing is *tier 1 needs no values files, tier 2 does*. The
precise version, from [compliance/00](../compliance/00-compliance-model.md) §4:

- **Tier 1** decides from what the vendor shipped - chart archives, their own
  `values.yaml`, plain manifests, kpt packages, kustomize bases, and the OCI
  artifact tree. This is what this document builds.
- **Tier 2** needs a site's values or a cluster fact. Its checks are catalogued,
  and at tier 1 they report `skip` with a reason. They never report a pass.

The bridge between them is **determinacy** (§6): a tier-1 run establishes, per
finding, whether the value it judged is fixed by the template or merely
defaulted. That is what lets tier 1 make hard, blocking statements about a chart
without ever seeing a site's values file, and it is the single idea that makes
the tier split useful rather than an excuse.

## 2. Where it sits in the life of a release

```
   vendor publishes
         │
    ┌────▼─────┐
    │ discovery│  a tag becomes a package row                     [07]
    └────┬─────┘
    ┌────▼─────┐
    │  expand  │  the artifact tree is walked; charts, files,
    └────┬─────┘  images and their digests are known              [expand]
         │
    ┌────▼──────────┐
    │  VALIDATION   │  ◄── this document. Reads chart and file blobs
    │   tier 1      │      by digest, renders, evaluates, records.
    └────┬──────────┘      Runs BEFORE anything is downloaded.
         │
    ┌────▼─────┐
    │ download │  30-60 GB moves, optionally gated on the verdict [20]
    └────┬─────┘
    ┌────▼─────┐
    │ security │  Xray, on the images that landed                 [21]
    └────┬─────┘
    ┌────▼─────┐
    │ promote  │  lab → production                                [22]
    └──────────┘
```

**Compliance runs after expansion and before download, and that ordering is the
main practical argument for the feature.** Everything a tier-1 check needs is
known once the tree has been walked: the charts are a few hundred kilobytes and
they are addressable by digest. Learning that a release ships a `ClusterRole`
with `verbs: ["*"]` is worth much more before 40 GB crosses a WAN than after.

It can also run **after** a download, against what actually landed in a target -
a different question ("is what we received what the vendor published?") answered
by the same machinery pointed at a different repository. §12.2.

## 3. Components

Nothing new in the deployment topology. One package, one migration, one set of
routes, one page.

| Component | Where | Responsibility |
|---|---|---|
| **Catalog** | `internal/compliance` | The loaded packs and the checks they own. Rebuilt on change; hashed into a bundle digest |
| **Loader / watcher** | `internal/compliance` | Discovers policy directories on start and on change, compiles, fails closed per pack |
| **Source** | `internal/compliance/source` | Fetches chart and file blobs by digest, from the source repository, under a byte budget |
| **Renderer** | `internal/compliance/render` | `helm template`, `kustomize build`, plain YAML. Deterministic inputs, sandboxed, bounded |
| **Engine** | `internal/compliance` | Applicability, evaluation, derived passes, verdict |
| **Evaluators** | `internal/compliance/cel`, `internal/compliance/builtin` | Declarative YAML/CEL checks and Go checks, behind one interface |
| **Runner** | `internal/compliance` | One run: claim, heartbeat, progress, cancel, record. Modelled on `security.Syncer` |
| **Store** | `internal/store/compliance.go, rendercache.go` | `compliance_runs`, `compliance_results`, `compliance_charts`, `package_compliance` |
| **API** | `internal/api/compliance*.go` | Routes, wire types, export |
| **UI** | `web/src/pages/Policies.tsx`, `web/src/components/compliancepanel.tsx, complianceevidence.tsx,
                complianceprogress.tsx` | The catalogue, the results, the report |
| **Retention** | `internal/maintenance/compliance.go, rendercache.go` | Leader-gated sweep, budget-based like the security one |

## 4. Acquiring what is checked

> **Decision - the Coordinator reads chart and file blobs; it never reads image layers.**
>
> The system's founding invariant is that artifact bytes never traverse the
> Coordinator ([00](00-overview.md) §5). A compliance run has to read Helm
> charts, and a chart is bytes.
>
> *Why this is not a violation of the invariant:* the invariant exists because a
> 30-60 GB package through a control plane is a memory profile, a network
> bottleneck and a failure domain that the whole architecture is shaped to
> avoid. It is about *payload*. A Helm chart is 10 KB to 2 MB, it is metadata by
> volume, and the platform **already reads exactly this class of blob** - the
> file-content routes stream a named layer out of the source registry so a
> reader can look at what a vendor shipped (`internal/api/packages.go`, `cmd/coordinator/blobs.go`).
>
> *The bounds that keep it true:*
>
> | Bound | Value | Why |
> |---|---|---|
> | Only artifacts classified `chart` or `file` | `oci.Classify` | An image layer is never fetched. Images are judged by their *reference*, not their content |
> | Only digests already recorded as artifacts of this package | reuses `store.FileInPackage`'s property | The fetch is a lookup, not a proxy. Nothing can ask the Coordinator to fetch an arbitrary digest on a credentialed connection |
> | Only from the **source** repository | as `blobsImpl.ReadBlob` does | What the vendor published is what is being judged |
> | `maxArtifactBytes` | 32 MiB default | A chart above this is not a chart |
> | `maxRunBytes` | 512 MiB default | Bounds a release with 400 charts |
> | Written to a temp directory, deleted at run end | | Nothing persists; the database stores results, never content |
>
> *Alternative considered - do the rendering in a worker.* Architecturally
> tidier: workers are the tier that touches bytes, and it scales. Rejected for
> now because workers hold no database credentials and no policy bundle, so the
> feature would need a policy-distribution channel and a result-submission API
> before it could check anything - a milestone of plumbing before the first
> check runs. The `Runner` interface is shaped so a worker-side runner is an
> addition rather than a move: nothing in the domain knows where it executes.
>
> *What would change our mind:* a corpus where chart bytes per run routinely
> exceed the budget, or a security requirement that the control plane execute no
> vendor-supplied templates. §5.4 covers the second.

### 4.1 What is picked up

For each artifact in the package's tree:

| Classification | Treatment |
|---|---|
| `chart` | Fetched, unpacked, rendered (§5) |
| `file` whose title ends `.tgz`/`.tar.gz` | Fetched, unpacked, inspected: a `Chart.yaml` at the root makes it a chart; a `kustomization.yaml` makes it a kustomize base; a `Kptfile` makes it a kpt package; otherwise a directory of manifests |
| `file` whose title ends `.yaml`/`.yml` | Fetched, parsed as one or more manifests |
| `image` | **Not fetched.** Contributes its reference to `input.images` |
| `signature`, `index`, `artifact` | Not fetched; contributes to coverage as `not applicable` |

Anything skipped is recorded in coverage with a reason. A release where every
artifact was skipped is reported as `inconclusive`, never as a pass - the same
discipline the security feature applies to unscanned images
([21](21-security-posture.md)).

## 5. Rendering

### 5.1 Helm

> **Decision - the `helm` binary, invoked as a subprocess, not the Helm Go SDK.**
>
> *Alternative:* import `helm.sh/helm/v3` and render in-process. No external
> dependency, no subprocess, no PATH question.
>
> *Rejected because* the Helm SDK pulls in `k8s.io/client-go`, `k8s.io/api`,
> `k8s.io/apimachinery`, `k8s.io/cli-runtime` and their transitive closure -
> roughly a hundred modules into a binary that currently has thirty and that
> deliberately has no Kubernetes client at all ([02](02-configuration.md) §3 is
> explicit that avoiding client-go was a design goal). It also pins us to one
> Helm version at build time.
>
> *Chosen:* the binary, discovered on `PATH` or at a configured path, with its
> version recorded on every run. This is the shape the request asked for and it
> has a property the SDK does not: **the version that renders is the version the
> organization deploys with**, and it can be changed without rebuilding this
> platform.
>
> *The cost, stated plainly:* if `helm` is absent, chart rendering does not
> happen. §5.4 says exactly what the feature does then, and it is not "pass".

> **Re-examined once the run was measured, and upheld.**
>
> The obvious argument for the SDK is process overhead: 95 charts is 190 `fork`
> + `exec` + interpreter startups. That argument was worth re-testing and it
> does not survive contact with the numbers. Subprocess spawn is single-digit
> milliseconds; the cost of a render is template execution and YAML marshalling,
> which the SDK pays identically. The two changes that actually moved this were
> **rendering several charts at once** (§5.5) and **not rendering at all**
> (§5.4a) - 10.7s → 4.7s → 1.3s on 95 charts. Neither needed the SDK, and
> in-process rendering would have made the first one harder, not easier: a
> template loop that does not terminate is `SIGKILL` on a subprocess and an
> unkillable goroutine in-process, so the render timeout that bounds a hostile
> chart would have had to be abandoned.
>
> The dependency argument has also grown rather than shrunk: `helm.sh/helm/v3`
> still pulls `client-go` and its closure into a binary that deliberately has no
> Kubernetes client, and the render cache means the remaining subprocesses are a
> small and shrinking share of a run.
>
> *What would change this:* a requirement to render a chart whose values come
> from a live cluster, or per-template attribution that the `# Source:` markers
> cannot give. Neither is in scope, and §5.2 explains why the markers are
> sufficient.

Invocation, fixed for reproducibility:

```
helm template <releaseName> <chartDir>
     --namespace        <configured, default "sgw-compliance">
     --kube-version     <configured, e.g. 1.31.0>
     --api-versions     <configured list>
     --include-crds
     --dry-run
```

| Flag / choice | Why it is what it is |
|---|---|
| `releaseName` and `--namespace` fixed in config | `.Release.Name` and `.Release.Namespace` appear in rendered names, labels and selectors. A varying release name produces varying results, and [compliance/00](../compliance/00-compliance-model.md) Rule 5 forbids that |
| `--kube-version` pinned, **never read from a cluster** | `.Capabilities.KubeVersion` gates whole blocks of many charts. Taking it from a live cluster would make the answer depend on which cluster the Coordinator can see |
| `--include-crds` | UPG-07 and UPG-11 need the CRDs the chart ships |
| No `--dependency-update`, no `--repository-config` | It would reach the network. A chart whose dependencies are not vendored fails SUP-07, which is a finding, not an excuse to go and fetch them |
| Hooks are **not** suppressed | MTA-08 is about hooks. `--no-hooks` would hide the thing being checked |
| `--set` is never used | Tier 1 renders defaults. Anything else is tier 2 |

Environment: `HELM_CACHE_HOME`, `HELM_CONFIG_HOME`, `HELM_DATA_HOME`,
`HELM_REPOSITORY_CONFIG` and `HELM_REGISTRY_CONFIG` all point inside the run's
temp directory, `HOME` with them. A chart cannot reach the operator's Helm
configuration and a render cannot leave a trace outside the directory that is
deleted at the end.

### 5.2 Source attribution

`helm template` emits a marker above every document:

```yaml
---
# Source: mysvc/templates/deployment.yaml
apiVersion: apps/v1
```

That marker is how a finding gets its `sourceFile`, and it is exact. The **line
within the template** is not recoverable from helm's output. Rather than invent
one, a result carries the source file plus the line **in the rendered document**,
and the rendered document is kept in the evidence bundle so both ends of a vendor
conversation are reading the same text ([compliance/00](../compliance/00-compliance-model.md) §3).

Subchart documents are attributed to the subchart - `mysvc/charts/redis/templates/…` -
which is what makes "this finding is in a dependency, not in your chart" visible.

### 5.3 Kustomize, kpt, plain manifests

| Form | How it is detected | How it is rendered |
|---|---|---|
| Kustomize | `kustomization.yaml` at the root of an unpacked file layer | `kustomize build` if the binary is present; otherwise `error`, reason `kustomize unavailable` |
| kpt package | `Kptfile` present | Read as plain YAML. kpt packages are already-rendered manifests; running kpt functions would need the function images and the network, which is tier 2 |
| Plain manifests | `.yaml`/`.yml` with no marker file | Parsed directly. Multi-document streams split on `---` |

Charts whose `Chart.yaml` declares dependencies that are not vendored under
`charts/` are **not rendered**: SUP-07 fails, `renderStatus` is
`dependencies_missing`, and every check that needed the rendered output reports
`error` for that chart. This is deliberate. Fetching the dependency would mean
reaching the network from the control plane, and rendering without it produces
manifests that are not the ones the vendor ships.

### 5.4 When helm is not there

Discovered once at start and re-probed when config changes; the version is
recorded and shown in Settings.

| State | Behaviour |
|---|---|
| `helm` present | Everything runs |
| `helm` absent | `T1-C` checks - chart structure, `Chart.yaml`, `values.yaml`, template text, file presence - still run. Every `T1-R` check reports `outcome: error`, `reason: "helm is not available on this Coordinator"`. The run's verdict is **inconclusive** |
| `helm` present but a render fails | That chart's `T1-R` checks report `error` with helm's own stderr, truncated, in the reason. Other charts are unaffected |

**A missing renderer never produces a pass.** That is the whole point of `error`
being an outcome ([compliance/00](../compliance/00-compliance-model.md) Rule 2),
and it is the difference between a tool that degrades and a tool that lies.

### 5.5 Bounds on a hostile chart

A chart is vendor-supplied input and it is executed. Go templates cannot exec or
read outside the chart, so the risk is resource exhaustion rather than escape,
and it is bounded rather than trusted:

| Bound | Default |
|---|---|
| Wall-clock per render | 60 s |
| Rendered output size | 64 MiB, `SIGKILL` past it |
| Unpacked chart size and file count | 128 MiB / 20,000 files - a zip-bomb bound applied at unpack |
| Path traversal in the archive | Rejected at unpack; a `../` entry fails the artifact and is itself reported |
| Concurrent renders per run | Configurable, default 4 |

### 5.4a The render cache - not rendering at all

The output of `helm template` is a pure function of the chart's bytes and the
pinned render inputs. Rendering the same bytes twice under the same inputs
cannot produce a different answer, so it is not done twice.

The key is `sha256(chart layer digest ‖ variant ‖ render-inputs digest)`, where
the render inputs are helm's version, `kubeVersion`, `apiVersions`, the release
name and the namespace - **exactly the fields a run already records as its
provenance**, because [compliance/00](../compliance/00-compliance-model.md) §2
rule 5 requires a finding to be re-derivable from them. That is not a
coincidence: the set of things that make a run reproducible and the set that
make its render reusable are the same set. A new render input belongs in
`compliance.RenderInputs` and in the run's provenance in the same commit.

Two consequences, and the second is the larger:

- A re-check of an unchanged release renders nothing. 95 helm subprocesses
  become 95 map lookups.
- The key is the **layer digest**, which the release's own record carries before
  anything is fetched - so a hit also means the chart is never pulled from the
  vendor's registry and never unpacked. Most charts are unchanged between two
  releases of a product, so the second check of an orb is mostly cache and the
  vendor's registry sees almost no traffic for it.

Keyed by digest and not by chart name and version, because a vendor who
republishes 4.2.1 with a fixed template has shipped different bytes under the
same version. A cache keyed by name and version would serve the old answer
forever; one keyed by digest cannot.

A cache entry carries the chart's `values.yaml` as well as its manifests. No
shipped check reads `chart.values` today, and that is not a reason to build a
cache that would break the first one that does: a hit must reproduce
**everything** loading the chart produced, or it has changed an answer.

Evictable, and this is the only thing in the schema that is besides the manifest
bodies, for the same reason ([03](03-persistence.md)): it is derived data with a
deterministic recipe, so an evicted entry costs one render and **can never be
wrong**. Bounded by `renderCacheTTL` and `renderCacheBytes`, swept LRU by
`maintenance.RenderCacheSweeper`. `renderCacheBytes` below zero disables it, for
a deployment that will not hold rendered vendor manifests in its database.

Measured on the development estate, 95 charts: **5.2s cold, 1.3s warm** - and
the warm run makes no registry request at all, which is the part that dominates
against a real vendor registry. `internal/compliance/source/rendercache_test.go`
asserts the property the whole thing rests on: a hit produces byte-identical
resources, addresses, line numbers, chart metadata and values to a miss.

### 5.5 Concurrency, and why it cannot change an answer

A real orb is 95 charts. Fetched and rendered one after another that is minutes
of a Coordinator idle on registry round trips, and then minutes of one CPU
running `helm template` 190 times while the rest of the machine does nothing.
Both stages run several charts at once: `fetchConcurrency` (default 6, bounded
by politeness to somebody else's registry) and `renderConcurrency` (default 4-8,
bounded by this machine's cores, because template execution is CPU-bound).

Concurrency here is an optimisation, and **an optimisation that can change an
answer is a defect**. Three things make it inert:

- **Results are written by index and merged in chart order.** Nothing about a
  report may depend on which worker finished first - not the order of the
  coverage table, not a result's `seq`, not which chart's manifests the evidence
  budget runs out on.
- **The per-release byte budget is decided before anything is fetched**, in the
  release's order, against the layer sizes the release's record already carries.
  Accumulated as downloads landed it would refuse a different set of charts on
  every run, so a report's coverage would depend on network timing.
- **The evidence budget is applied during the merge**, not in the workers, for
  the same reason.

`internal/compliance/source/concurrency_test.go` holds these as tests: a
registry that answers in strictly reverse order behind a barrier (so a serial
fetch deadlocks rather than quietly passing), a budget asserted identical across
five runs, and the progress reporter written from every worker while a poller
reads it.

Measured on the development estate - 95 charts, a local registry, so the fetch
has almost no latency to hide - a run goes from ~10.7s to ~4.7s. Against a real
vendor registry the fetch stage is where most of the saving is, and it is larger.

## 6. Determinacy - the mechanism

The idea in [compliance/00](../compliance/00-compliance-model.md) Rule 4, made
concrete. It is what makes a tier-1 verdict defensible.

**Render twice.**

1. **Baseline render** - the chart's own `values.yaml`.
2. **Probe render** - a copy of `values.yaml` with every scalar leaf replaced by
   a type-preserving sentinel: integers become a distinctive number (`424242`),
   strings become `sgw-probe-<n>`, booleans are flipped, and `null` is left
   alone.

Then, per rendered field:

| Observation | Determinacy |
|---|---|
| The field exists in both renders with the **same** value | `fixed` - no values file changes it |
| The field exists in both with **different** values | `configurable` - the finding is about the defaults |
| The resource exists in one render only | `configurable`, and the finding says the resource's *existence* is gated by a value - which is usually the more interesting sentence ("this chart ships a PDB, disabled by default") |
| The probe render fails | `unknown` for that chart. Recorded on the run; never silently treated as `fixed` |

Matching between the two renders is by `(kind, name-after-normalization, container)`.
Names frequently contain a value (`{{ .Values.nameOverride }}`), so the
normalization replaces sentinel strings with a placeholder before matching. A
resource that cannot be matched is `unknown`, not guessed.

**Cost:** one extra `helm template` per chart, at most a second. **Value:** the
difference between "your chart is not highly available" (wrong, and the vendor
stops reading) and "your chart ships no PodDisruptionBudget template, so no
deployment of it can be protected" (right, blocking, and actionable).

Configurable: `determinacy: off | probe`. With `off`, every `T1-R` result is
`unknown`, which weakens the verdict to `conditional` at worst - it never
strengthens it.

## 7. The engine

One run, in order:

```
 1  claim         one active run per package, in the database, with a heartbeat
 2  plan          the artifact list, from the recorded tree: what will be
                  fetched, what will be skipped and why  (this is validateOnly)
 3  acquire       fetch chart and file blobs, under the byte budget
 4  render        helm / kustomize / plain, baseline and probe
 5  index         parse into resources; attach __address to each; build the
                  release-wide indexes: pod templates by label set, services,
                  PDBs, CRDs, image references
 6  applicability for each check, the set of resources it judges
 7  evaluate      CEL and builtin evaluators, per check
 8  reconcile     derived passes = applicable − reported; skips where applicable
                  is empty; determinacy attached from step 4
 9  waivers       applied, expiry checked against the run's own start time
10  verdict       counts, coverage, the four-state verdict
11  record        one transaction: run, results, chart rows, package summary
```

Steps 5 and 6 are where the design earns the "not vague" requirement. The
release-wide indexes are built **once** and shared by every check, so
`matchExpressions` evaluation, `IntOrString` handling, quantity parsing and OCI
reference parsing are each implemented once and tested once - which is exactly
what [compliance/03](../compliance/03-sample-policy-review.md) §3.3 shows going
wrong when every policy does it for itself.

### 7.1 The check interface

```go
// Check is one assertion. Both evaluators satisfy it, and nothing downstream
// can tell which produced a Result.
type Check interface {
    // Meta is the manifest's declaration: ID, title, description, rationale,
    // severity, tier, category, remediation, reference, appliesTo.
    Meta() CheckMeta
    // Evaluate reports what it found. It reports FAILURES only, unless
    // Meta().EmitsPasses is set. Passes are derived by the engine from
    // Meta().AppliesTo, which is what makes a pass impossible to forget and
    // impossible to over-claim.
    Evaluate(ctx context.Context, in *Release) ([]Finding, error)
}
```

An `error` return from `Evaluate` becomes one `error` result per applicable
resource - not a dropped check and not a silent pass. A check that panics is
recovered, recorded as `error`, and its pack is marked unhealthy in the
catalogue.

### 7.2 Volume

A large release: ~200 workloads, ~600 resources, 88 checks, most applying to
workloads only. Order 10,000-15,000 result rows per run. Small for Postgres,
large for a browser - so the API paginates and the UI's default view is failures
grouped by chart, with passes one click away. `maxResultsPerRun` (default
200,000) truncates rather than falling over, and a truncated run **says so** on
the run row, in the API and in the export.

## 8. Policy discovery and loading

The mechanism [compliance/02](../compliance/02-authoring-checks.md) §1 describes,
implemented the way product configuration already is
([02](02-configuration.md) §3): a mounted directory, `fsnotify`, no Kubernetes
API, and the same path against a plain directory in development.

```
start ──► scan policyPaths ──► parse each pack.yaml
                                   │
                                   ├─ prefix collision? reject THIS pack, keep the rest
                                   ├─ duplicate ID?     reject THIS pack
                                   ├─ expr compile err? reject THIS pack
                                   └─ ok → compile, register
                                        │
                                        ▼
                             bundle digest = sha256 over every
                             loaded pack file, sorted by path
```

- **Fail closed per pack, never per catalogue.** One broken pack does not take
  the baseline down, and it does not disappear either: it is listed as broken
  with its error, in the API and on the Policies page, and every check it owns
  reports `error`.
- **The built-in baseline is compiled in** and always present. It cannot be
  removed by deleting a directory, which is what stops a misconfigured mount
  turning every release green.
- **Reload is atomic.** A new catalogue is built and swapped; a run in flight
  keeps the one it started with, and its bundle digest still describes it.
- **The bundle digest is recorded on every run**, so "which rulebook produced
  this report" is answerable a year later.

## 9. Persistence

`db/migrations/{postgres,sqlite}/00035_compliance.sql
db/migrations/{postgres,sqlite}/00039_compliance_evidence.sql
db/migrations/{postgres,sqlite}/00040_compliance_render_cache.sql`. Postgres shown; the
SQLite dialect follows the conventions in [03](03-persistence.md) §4.

```sql
CREATE TABLE compliance_runs (
    id                    UUID PRIMARY KEY,
    package_id            BIGINT NOT NULL REFERENCES packages(id) ON DELETE CASCADE,
    product               TEXT   NOT NULL,
    state                 TEXT   NOT NULL
        CHECK (state IN ('pending','running','succeeded','failed','cancelled')),
    trigger               TEXT   NOT NULL
        CHECK (trigger IN ('manual','analysis','download','schedule')),
    actor                 TEXT   NOT NULL DEFAULT 'anonymous',
    tier                  SMALLINT NOT NULL DEFAULT 1,

    -- WHAT PRODUCED THIS ANSWER. Every column here can change a result, so a
    -- report that omitted them would not be reproducible and two runs that
    -- differ in any of them are not comparable. See compliance/00 Rule 5.
    policy_bundle_digest  TEXT   NOT NULL,
    engine_version        TEXT   NOT NULL,
    helm_version          TEXT,
    kustomize_version     TEXT,
    kube_version          TEXT,
    api_versions          TEXT,
    release_name          TEXT   NOT NULL,
    render_namespace      TEXT   NOT NULL,
    determinacy_mode      TEXT   NOT NULL,

    verdict               TEXT
        CHECK (verdict IN ('pass','conditional','fail','inconclusive')),
    counts                JSONB  NOT NULL DEFAULT '{}',
    coverage              JSONB  NOT NULL DEFAULT '{}',
    truncated             BOOLEAN NOT NULL DEFAULT FALSE,
    error                 TEXT,

    started_at            TIMESTAMPTZ NOT NULL,
    heartbeat_at          TIMESTAMPTZ,
    finished_at           TIMESTAMPTZ
);

-- One active run per release. The claim is IN THE DATABASE for the same reason
-- the analysis claim is (migration 00021): the process holding it can die, and
-- a release stuck "running" forever is a release nobody can ever check
-- again. heartbeat_at is what makes the claim recoverable.
CREATE UNIQUE INDEX compliance_runs_active
    ON compliance_runs (package_id) WHERE state IN ('pending','running');
CREATE INDEX compliance_runs_package ON compliance_runs (package_id, started_at DESC);

CREATE TABLE compliance_results (
    id             BIGSERIAL PRIMARY KEY,
    run_id         UUID NOT NULL REFERENCES compliance_runs(id) ON DELETE CASCADE,

    check_id       TEXT NOT NULL,
    pack           TEXT NOT NULL,
    outcome        TEXT NOT NULL CHECK (outcome  IN ('pass','fail','skip','error')),
    severity       TEXT NOT NULL CHECK (severity IN ('block','warn','info')),
    determinacy    TEXT NOT NULL CHECK (determinacy IN ('fixed','configurable','unknown','na')),

    -- The address. Denormalized on purpose: a result is read on its own, in a
    -- spreadsheet row, out of order, pasted into a vendor ticket. A row that
    -- needs four joins to say which chart it is about is a row nobody can act
    -- on, and it is exactly what a normalized schema produces here.
    artifact_id      BIGINT REFERENCES package_artifacts(id) ON DELETE SET NULL,
    artifact_digest  TEXT,
    artifact_ref     TEXT,
    chart_name       TEXT,
    chart_version    TEXT,
    source_file      TEXT,
    rendered_line    INTEGER,
    api_version      TEXT,
    resource_kind    TEXT,
    resource_ns      TEXT,
    resource_name    TEXT,
    container        TEXT,
    locus            TEXT,

    observed       TEXT,
    expected       TEXT,
    message        TEXT NOT NULL,
    reason         TEXT,

    -- Stable across releases: excludes chart version and release tag, so
    -- "has the vendor fixed this yet" and "how long has this been failing"
    -- are joins rather than guesses. It is also what a waiver keys on.
    fingerprint    TEXT NOT NULL,
    waiver_id      TEXT
);

CREATE INDEX compliance_results_run     ON compliance_results (run_id, outcome, severity);
CREATE INDEX compliance_results_chart   ON compliance_results (run_id, chart_name, resource_kind);
CREATE INDEX compliance_results_fprint  ON compliance_results (fingerprint);
CREATE INDEX compliance_results_check   ON compliance_results (run_id, check_id);

-- Per-chart coverage: what was rendered, what was not, and why. This is the
-- denominator. Without it a run with 3 of 97 charts rendered and no failures
-- reads exactly like a clean release.
CREATE TABLE compliance_charts (
    run_id          UUID NOT NULL REFERENCES compliance_runs(id) ON DELETE CASCADE,
    artifact_digest TEXT NOT NULL,
    artifact_ref    TEXT,
    chart_name      TEXT,
    chart_version   TEXT,
    kind            TEXT NOT NULL,     -- chart | kustomize | kpt | manifests
    render_status   TEXT NOT NULL,     -- ok | failed | skipped | dependencies_missing | too_large
    render_error    TEXT,
    resources       INTEGER NOT NULL DEFAULT 0,
    determinacy     TEXT NOT NULL DEFAULT 'unknown',
    PRIMARY KEY (run_id, artifact_digest)
);

-- The listing column. One row per release, overwritten by each run, so the
-- Software table can show a compliance pill without touching the result rows.
-- Same shape and same reasoning as package_security (migration 00023).
CREATE TABLE package_compliance (
    package_id      BIGINT PRIMARY KEY REFERENCES packages(id) ON DELETE CASCADE,
    run_id          UUID REFERENCES compliance_runs(id) ON DELETE SET NULL,
    verdict         TEXT,
    blocking_fails  INTEGER NOT NULL DEFAULT 0,
    warn_fails      INTEGER NOT NULL DEFAULT 0,
    passes          INTEGER NOT NULL DEFAULT 0,
    skips           INTEGER NOT NULL DEFAULT 0,
    errors          INTEGER NOT NULL DEFAULT 0,
    waived          INTEGER NOT NULL DEFAULT 0,
    coverage_complete BOOLEAN NOT NULL DEFAULT FALSE,
    evaluated_at    TIMESTAMPTZ
);
```

**Retention.** Run summaries and `package_compliance` are kept forever - they are
kilobytes and they are the history. Result rows are kept for the most recent `N`
runs per release (default 5) and past that become evictable under a byte budget,
swept by a leader-gated loop. The policy is the one the security store arrived at
after getting it wrong once ([21](21-security-posture.md) §7): **past its
retention a row is evictable, not deleted**, and nothing goes until the store is
over budget. A release with a verdict and no findings behind it is the failure
mode to avoid.

### 9.1 Lifecycle, and what is kept

Four tables with four different lifetimes, and the differences are deliberate.

| What | Where | Kept | Why that long |
|---|---|---|---|
| The listing summary - one row per release: verdict, counts, when | `package_compliance` | **Forever** | It is what the Software page reads. Losing it turns a checked release back into an unchecked one on screen, which is the one distinction this whole feature exists to preserve. |
| The run, its coverage and its results | `compliance_runs`, `compliance_charts`, `compliance_results` | The **newest `coordinator.gc.complianceRuns`** of each release, default **10** | A count and not an age, and that is the point: a release checked once eight months ago must keep that run - it is the only answer anybody has about it - while a release checked nightly by a schedule must not keep six thousand. Ten covers a fortnight of scheduled checks and a comparison against what a re-check replaced. |
| Cached chart renders | `compliance_render_cache` | Until the TTL or the byte budget evicts them | Derived data with a deterministic recipe, so an evicted entry costs one render and can never be wrong. Not scoped to a release or a run at all: the whole point is that two releases sharing a chart share its render. |
| The rendered manifests | `compliance_rendered` | The **latest run only** | The one part of a run whose size the vendor sets. A completed run reclaims what it supersedes, and nothing displays an older run, so nothing reads an older run's manifests. Also bounded per document and per release while it is being written - see [compliance/00](../compliance/00-compliance-model.md) §2 rule 5. |
| The working directory of unpacked charts | `/tmp` | The **duration of the run** | Removed by the run's own cleanup, on the success path and on every failure path. |

A run is deleted whole. Its charts, results and manifests hang off it with
`ON DELETE CASCADE`, and that is the only correct granularity: a run without its
results is a verdict nobody can look behind, and a run without its coverage is a
finding count with no denominator. Half a run is worse than no run.

Nothing is deleted on a schedule of its own - the sweep is a case in
`store.SweepRetention`, run by the hourly retention loop that already handles
transfers, worker logs and audit events, and it is leader-gated like every loop
that writes.

**Rough sizes**, from the 95-chart orb this is built against: ~1,200 result rows
and ~80 KB of rendered manifests per run. Ten runs of a hundred releases is
therefore a few hundred megabytes of results and, because only the latest run
keeps its manifests, under ten megabytes of those. Setting
`coordinator.compliance.evidencePerRelease` below zero removes the manifests
entirely for a deployment that will not hold vendor text in its database;
findings are unaffected, because the manifests are what a finding is displayed
against and never what it is derived from.

## 10. API

AIP conventions per [09](09-api.md) §1. Custom methods with a colon.

### The catalogue - what the organization checks

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/compliance/policies` | Every pack and check: ID, title, description, rationale, severity, tier, category, applicability, remediation, reference, engine, pack, and each pack's load status |
| `GET` | `/api/v1/compliance/policies/{check}` | One check in full, including its `appliesTo` and its fixtures' names |
| `GET` | `/api/v1/compliance/policies/export` | The catalogue as CSV/XLSX - the rulebook, on its own, for a vendor who asks "what will you check?" before shipping |

This is the "list the available policies and view the details" requirement, and
it is deliberately **not** under a product: the rulebook is the organization's,
not a product's.

### A release's compliance

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products/{product}/packages/{package}/compliance` | Latest run: verdict, counts, coverage, provenance, and results. `filter` on outcome, severity, check, chart, kind, determinacy; `pageSize`/`pageToken` |
| `POST` | `/api/v1/products/{product}/packages/{package}/compliance:run` | Start a run. `validateOnly=true` returns the **plan** - which artifacts would be fetched and rendered, which would be skipped and why - and stores nothing |
| `POST` | `/api/v1/products/{product}/packages/{package}/compliance:cancel` | Stop the run, wherever it is running |
| `GET` | `/api/v1/products/{product}/packages/{package}/compliance/progress` | Live progress: stage, done/total, notes |
| `GET` | `/api/v1/products/{product}/packages/{package}/compliance/runs` | History, newest first |
| `GET` | `/api/v1/products/{product}/packages/{package}/compliance/export` | `format=csv\|xlsx\|json\|zip` (§11) |
| `GET` | `/api/v1/products/{product}/packages/{package}/compliance/compare` | `against={tag}` - fixed, new, still failing, by fingerprint |

### The manifests a run judged

The evidence behind a finding, so it can be verified rather than trusted. See
[compliance/00](../compliance/00-compliance-model.md) §2 rule 5 for why a report
keeps its inputs at all, and `internal/compliance/evidence.go` for how a check's
locus is resolved to a line.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `…/compliance/rendered` | Index of the documents the latest run kept: chart or file, lines, bytes, whether truncated. No content - a coverage table must not cost a download |
| `GET` | `…/compliance/rendered/content?document=` | One document as `text/yaml`. Omit `document` for the whole release in one file, each part named under a header stating the run, helm and Kubernetes versions and the rulebook digest. `download=1` attaches it |
| `GET` | `…/compliance/rendered/excerpt?seq=&context=` | The lines one result is about, numbered as they are in the document, with the line its locus resolves to |

`seq` is the result's position in the run, and the excerpt endpoint takes that
rather than an address. The caller has the chart, the line and the field on the
row it is displaying, and could send all three - and then the excerpt would be a
claim assembled by whoever asked for it. Reading the address off the stored run
makes the response a statement about what the run found, which is what evidence
has to be.

Two deliberate absences, each of which will be asked for:

- **No endpoint enables, disables or re-severities a check.** That is
  configuration; it lives in Git beside the pack. An API that could switch off a
  blocking check is an approval process with no reviewer - the same argument that
  keeps products read-only over the API ([02](02-configuration.md) §1).
- **No endpoint creates a waiver.** Same reason, and a waiver is the more
  consequential of the two. [compliance/00](../compliance/00-compliance-model.md) §7.

## 11. The report

`internal/export` already writes CSV, XLSX, JSON and ZIP bundles and is used by
the security exporter. Compliance reuses it unchanged.

**Every sheet carries the whole address on every row.** A spreadsheet row is read
on its own, filtered, sorted and pasted into a ticket; a row that says
"`readinessProbe` missing" without saying which container of which workload of
which chart of which release is a row nobody can act on.

| Sheet | One row per | Why it is a separate sheet |
|---|---|---|
| **Summary** | field | Product, release, package digest, verdict, counts by outcome and severity, coverage, and the full provenance block - policy bundle digest, engine, helm, kube version, release name, namespace, determinacy mode, run time, who asked. A vendor cannot reproduce a finding without these |
| **Findings** | failing result | The working sheet. Check ID, title, category, severity, tier, determinacy, product, release, package digest, chart name, chart version, chart artifact digest, source file, rendered line, apiVersion, kind, namespace, name, container, locus, observed, expected, message, remediation, reference, pack, fingerprint, waiver |
| **All checks** | every result | Including passes and skips. This is what tells the vendor *what was verified*, not only what broke - and it is what makes the report evidence rather than a complaint |
| **Resources** | Kubernetes object | Every object found, with its address and its pass/fail/skip tallies. The "every Kubernetes artifact level" view: this Deployment, this ConfigMap, this Secret, and how each did |
| **Charts** | chart | Name, version, artifact digest, OCI ref, render status, resource count, failures, determinacy. The coverage denominator |
| **Problems** | error / unrendered artifact | What could not be checked and why, with the renderer's own message |
| **Policy catalog** | check that ran | The rulebook itself: ID, title, description, rationale, expected outcome, severity, tier, category, reference. So the vendor receives the standard, not just the verdicts |
| **Waivers** | waived result | What was accepted, by whom, why, and when the acceptance expires |

The **ZIP bundle** adds the evidence: the rendered manifests per chart, the
chart's own `values.yaml`, the renderer's stderr for failed charts, and the
findings as CSV. That is what a vendor needs to reproduce a finding without
access to this platform, and it is the artifact that makes the conversation
short.

## 12. Lifecycle integration

### 12.1 When it runs by itself

```yaml
coordinator:
  compliance:
    autoRun: onAnalysis      # off | onAnalysis | onDownload | both
```

`onAnalysis` is the recommended default: expansion has just established the
artifact tree, everything needed is known, and the answer arrives before anyone
decides whether to spend the bandwidth. It is enqueued on the leader, rate-limited,
and it never blocks the analysis that triggered it.

`onDownload` re-runs against a **target** repository after a download completes.
That is not a repeat: it answers "is what landed what the vendor published?", and
a difference is a finding about the pipeline rather than about the chart.

### 12.2 The download gate

Deferred to M11-C, designed now so the shape is not accidental:

```yaml
downloads:
  - name: to-lab
    targets: [jfrog-store]
    gates:
      compliance: off        # off | warn | block
```

`block` refuses to open a download whose release has an unwaived blocking failure
with determinacy `fixed`. It sits beside the verification gate that already exists
in [20](20-download-rules.md), uses the same mechanism, and defaults to `off`:
a gate that surprises an operator during an incident is a gate that gets removed.

### 12.3 The timeline

`ReleaseTimeline` (`web/src/components/layout.tsx`) gains a **Checked** moment
between Published and Downloaded, drawn in time order like the rest. It carries
the verdict's tone - green for pass, amber for conditional, red for fail, grey
for inconclusive - and its `pending` state is a live run, which is what makes
"the checks are running" visible on the release page without a second widget.

Every state change is an audit event ([12](12-observability-and-audit.md)):
`compliance.started`, `compliance.completed` with the verdict, `compliance.cancelled`,
`compliance.failed`. The Activity page picks them up with no work.

### 12.4 Metrics

| Metric | Type | Labels |
|---|---|---|
| `sgw_compliance_runs_total` | counter | product, trigger, verdict |
| `sgw_compliance_run_duration_seconds` | histogram | product |
| `sgw_compliance_results_total` | counter | outcome, severity |
| `sgw_compliance_check_failures_total` | counter | check_id, severity |
| `sgw_compliance_render_failures_total` | counter | reason |
| `sgw_compliance_policy_packs_loaded` | gauge | status (ok/broken) |
| `sgw_compliance_check_duration_seconds` | histogram | check_id |

`sgw_compliance_check_duration_seconds` earns its place: a pack with an
accidental O(n²) comprehension is invisible until a large release takes twenty
minutes, and this is where it shows.

## 13. Configuration

Deployment-scoped, in `config.yaml` beside `security` and for the same reason
([02](02-configuration.md) §8): none of it is a property of a *product*. Where
the policies live and which Kubernetes version to render for belong to this
installation. A product document says one thing about compliance - whether it is
on.

```yaml
coordinator:
  compliance:
    enabled: true

    # WHERE THE RULES COME FROM. Projected volumes, discovered on start and
    # watched for change - the same mechanism as products (02 section 3), so a
    # developer drops a directory in a folder and it is picked up with no
    # cluster and no restart.
    #
    # The built-in `sgw-baseline` pack is compiled into the binary and is always
    # present. It cannot be removed by unmounting a volume, which is what stops
    # a misconfigured mount turning every release green.
    policyPaths:
      - /etc/softwaregateway/policies
    waiverPaths:
      - /etc/softwaregateway/waivers

    # RENDERING. Pinned, not discovered. `.Capabilities.KubeVersion` gates whole
    # blocks of many charts, so taking this from a live cluster would make a
    # release's verdict depend on which cluster this Coordinator can see - and
    # the answer would change under a cluster upgrade nobody connected to it.
    helm:
      path: helm                     # resolved on PATH when relative
      kubeVersion: "1.31.0"
      apiVersions: []                # extra --api-versions entries
      releaseName: sgw-compliance    # fixed: .Release.Name appears in output
      namespace: sgw-compliance      # fixed: so does .Release.Namespace
      renderTimeout: 60s
      maxRenderedBytes: 67108864     # 64 MiB
    kustomize:
      path: kustomize                # optional; absent means kustomize sources error

    # Two renders per chart instead of one, to establish whether a value is
    # fixed by the template or merely defaulted. This is what lets a tier-1
    # finding block. `off` costs nothing and weakens every T1-R verdict to
    # advisory - it never strengthens one. See section 6.
    determinacy: probe               # off | probe

    # BYTES. The Coordinator reads charts and small files, never image layers.
    # These are the bound that keeps section 4's exception an exception.
    maxArtifactBytes: 33554432       # 32 MiB - a chart above this is not a chart
    maxRunBytes: 536870912           # 512 MiB per run
    concurrency: 4                   # charts rendered at once

    # WHEN. onAnalysis answers before the bandwidth is spent, which is the whole
    # argument for checking at ingest rather than in CI.
    autoRun: onAnalysis              # off | onAnalysis | onDownload | both

    maxResultsPerRun: 200000         # truncate loudly rather than fall over
    sweepInterval: 15m

    # HOW MANY AT ONCE. Two numbers because the two stages are bound by
    # different things: downloading a chart layer is almost all round trip
    # against SOMEBODY ELSE'S registry, so the limit is politeness - thirty
    # parallel requests is a rate limiter and a slower answer, not a faster
    # one. Rendering is `helm template`, which is CPU-bound and local, so its
    # limit is this machine's cores.
    #
    # Zero picks a default (6 fetching; 4-8 rendering, from the CPU count).
    # One does that stage in sequence. Neither can change a RESULT: what a run
    # produces is assembled in chart order regardless of which worker finished
    # first, and the byte budget is decided before anything is fetched. See
    # section 9.1.
    fetchConcurrency: 0
    renderConcurrency: 0

    # NOT RENDERING AT ALL. A chart's rendered output is a pure function of its
    # bytes and the pinned render inputs, so the same chart is never rendered
    # twice - and because the cache is keyed by the chart's LAYER DIGEST, a hit
    # also means it is never downloaded. Section 5.4a.
    #
    # Evictable and safe to evict: a missing entry costs one render and can
    # never be wrong. The TTL reclaims charts a vendor has stopped shipping,
    # which no byte budget would ever reach; the budget reclaims the tail when a
    # large estate keeps everything warm. Below zero on the budget disables the
    # cache entirely.
    renderCacheTTL: 720h             # 30 days
    renderCacheBytes: 536870912      # 512 MiB
    renderCacheSweep: 1h

    # THE MANIFESTS THE RUN JUDGED, kept so a finding can be SHOWN rather than
    # only asserted (compliance/00 section 2, rule 5). Two numbers for the
    # reason the fetch budgets above take two: one pathological chart and four
    # hundred ordinary ones are different problems. Over the cap a document is
    # kept TRUNCATED and says so; below zero nothing is kept at all, which is
    # for a deployment that will not hold vendor manifests in its database.
    # Findings are unaffected either way.
    #
    # Kept for the LATEST run of a release only - a completed run reclaims what
    # it supersedes. Nothing displays an older run, so nothing reads one.
    evidencePerDocument: 4194304     # 4 MiB
    evidencePerRelease: 25165824     # 24 MiB

    # Deployment-specific inputs to the shipped checks. Constants in a policy
    # file would need a rebuild to change and would be wrong for the next
    # installation.
    checkConfig:
      approvedRegistries:            # SUP-02
        - registry.internal.example.com
        - quay.internal.example.com
      probeBounds:                   # PRB-05
        timeoutSeconds: [1, 3]
        periodSeconds: [5, 10]
      ownershipAnnotations:          # MTA-07
        - example.com/owner
        - example.com/oncall
        - example.com/runbook
      credentialPatterns: []         # CFG-01, appended to the built-in set
```

Per product, one field, in the product document:

```yaml
spec:
  compliance:
    enabled: true          # default true when the feature is on
    packs: []              # empty = every loaded pack; names a subset otherwise
```

## 14. User interface

Four surfaces. Three are additions to pages that exist.

### 14.0 While a run is going, the run is the whole tab

A check of a real orb is minutes. For all of it, the verdict card, the coverage
table and the findings table read from the LATEST run - which, the moment
somebody presses the button, is the one that has just started and has nothing in
it. So the tab showed "Not checked" over four zeros and an empty findings table
redrawing itself every second. Every one of those is a true statement about a
run that has not finished and a false impression of the release.

While `progress` is present the tab renders **only** the run panel. The previous
run's verdict is not shown beside it either, and that is the harder call: it is
real, and it is about to be replaced. Showing it beside a running check is how a
stale verdict gets read out in a release meeting.

The panel answers "is this working at all", which is the question somebody
actually asks in front of a bar that has not moved - not "how far along is it".
It does that with things that CHANGE and things that have HAPPENED:

```
┌─────────────────────────────────────────────────────────────────────────┐
│ ◌ Checking this release   4s elapsed                    [ Stop check ]  │
├─────────────────────────────────────────────────────────────────────────┤
│ Rendering charts                                              91 of 95  │
│ Running helm template on each chart, twice…    about 0s left on this stage│
│ ████████████████████████████████████████████████████████████░░░░        │
│ [4 at a time] [cfx-lmf-chart] [cfx-nssaaf-chart3] [cfx-tngf-chart] …    │
│                                                                          │
│ Find charts 0s › Download 0s › Render › Evaluate › Record               │
│                                                                          │
│  Charts found 95    Downloaded 95    Rendered 93                        │
│                                                                          │
│ What has happened                                                        │
│  4s  cfx-ucmf-chart3 rendered 3 object(s)                               │
│  3s  cfx-nssaaf-chart3 rendered 4 object(s)                             │
└─────────────────────────────────────────────────────────────────────────┘
```

- **Elapsed** ticks; the **in-flight chart names** rotate. Either one moving
  says the run is alive whatever the bar is doing, and it is the only thing that
  distinguishes a slow registry from a wedged one.
- **The estimate is the current STAGE's, and says so.** A whole-run estimate
  would have to guess the cost of stages that have not started, whose cost
  depends on what the ones running now produce; a confident number that turns
  out four times wrong is worse than none. It appears only after the second item
  of a stage, because one sample of a stage whose first item paid for a
  connection is not a rate.
- **The route shows what each finished stage cost.** "Eight minutes" is
  unreadable; "six of those eight were the download" is a decision.
- **Refusals and failures are counted beside the successes**, and coloured. A
  run that has fetched 92 of 95 charts is about to produce a report with a hole
  in it, and knowing at minute two rather than at minute nine is the difference
  between fixing the cause and reading a verdict nobody can use.
- **The log is bounded and drops ordinary progress before it drops a failure**,
  so the lines that survive a long run are the ones worth scrolling back for.

`Stop check` is a danger button with the sentence saying what stopping promises,
which is the shape the analysis bar on the Details tab uses. It was a text link,
which read as navigation on the one control that can abandon minutes of work
against somebody's registry.

### 14.1 Release page - a Compliance tab

Beside the Security tab, in the same shape, because a reader has already learned
that shape ([19](19-user-interface.md) §3).

```
┌─────────────────────────────────────────────────────────────────────────┐
│ ⚠ CONDITIONAL   3 blocking · 14 warnings · 1 041 passed · 26 skipped     │
│ 97 of 97 charts rendered · 612 resources · 88 checks · helm 3.16.2      │
│ Checked   12 Aug 2026 14:22 · bundle a91f…  [ Re-run ]  [ Export ▾ ]    │
└─────────────────────────────────────────────────────────────────────────┘

 [ Failures ] [ All results ] [ By resource ] [ Charts ] [ Problems ]
 ┌──────────────────────────────────────────────────────────────────────┐
 │ 🔴 mysvc 4.2.1                                            2 blocking │
 │   ├ Deployment/mysvc-api                                             │
 │   │   🔴 PDB-01  No PodDisruptionBudget selects this workload  fixed │
 │   │      chart ships no PDB template · spec.template.metadata.labels │
 │   │      [ why this check exists ]                                   │
 │   │   🟠 PRB-03  liveness is more sensitive than readiness           │
 │   │      container `api` · periodSeconds 5 vs 10   configurable      │
 │   └ ServiceAccount/mysvc                                             │
 │       🔴 RBAC-05  Role grants list on secrets       rules[1].verbs   │
 └──────────────────────────────────────────────────────────────────────┘
```

Five properties, each answering something in the request directly:

- **The verdict is a sentence, not a score.** Counts by outcome with coverage
  beside them, because a number without a denominator is the thing this whole
  model exists to prevent.
- **Grouped chart → resource → check by default.** That is the shape of the
  vendor conversation and the shape of the fix.
- **Every row carries its determinacy.** `fixed` and `configurable` are visually
  distinct, and hovering says what each means. A reader must never have to guess
  whether a finding is about the software or about its defaults.
- **Passes are one click away, never hidden.** The "All results" tab is the
  evidence that the check ran, and the empty-state for a chart with no failures
  says *"38 checks passed, 4 did not apply"*, never nothing.
- **`Problems` is a first-class tab**, not a footnote. A chart that would not
  render is the most important thing on the page, because everything else about
  it is unknown.

### 14.2 Policies page - the catalogue

A new top-level page, because the rulebook is a thing people read on its own -
before a release arrives, when writing a check, and when a vendor asks what the
standard is.

- Every check: ID, title, category, severity, tier, pack, engine.
- Search and filter by any of those.
- Opening one shows the full description, the rationale, what it applies to,
  what it expects, the remediation, the reference, and the fixtures that prove
  it - which is the "how does it work" half of the request.
- Packs are listed with their load status; a broken pack is red, with its error.
- `Export catalogue` writes the whole rulebook to XLSX.

### 14.3 Software listing - a compliance column

A verdict pill beside the existing security column, sortable and filterable, so
"show me every release with a blocking failure" is a filter rather than a
question. Absent means *not checked* and renders as such - never as a pass.

### 14.4 The timeline and Reports

The `Checked` moment (§12.3), and on the Reports page a fleet rollup: verdicts
by product, the checks that fail most often across all releases, and the vendors
whose releases fail most. The second of those is what turns a per-release tool
into an argument for changing a standard - a check that fails on every vendor is
either a real industry gap or a check that is wrong, and both are worth knowing.

## 15. Code layout

```
internal/compliance/
    check.go        Check, CheckMeta, Finding, Result, Outcome, Severity,
                    Tier, Determinacy
    address.go      Address, fingerprinting
    release.go      the evaluation input: resources, charts, images, indexes
    index.go        label-selector evaluation, pod-template index, service
                    index, PDB index, CRD index, image-reference extraction
    pack.go         PolicyPack manifest parse + validate
    catalog.go      loaded packs, lookup, bundle digest
    loader.go       directory discovery, per-pack fail-closed compile
    watch.go        fsnotify reload
    engine.go       the eleven steps of section 7
    applicability.go
    verdict.go      scoring, waivers, expiry
    waiver.go
    runner.go       claim, heartbeat, progress, cancel
    progress.go
    builtin/        one file per category: pdb.go, probes.go, security.go,
                    rbac.go, config.go, resources.go, network.go, storage.go,
                    metadata.go, supply.go, scheduling.go, upgrade.go
    parse.go        rendered manifests -> addressed resources
    progress.go     stages, counts, in-flight items, the event log
    run.go          one run: charts, counts, verdict, provenance
    evidence.go     the manifests a run judged; locus -> line; excerpts
    cel/            the ONLY package importing cel-go
      env.go          declarations; compile-time and run-time environments
      funcs.go        value/text/present, quantity, imageRef, selects, pdbFor
      funcs2.go       covers, replicas, declaresPort, boundToRole, allLabels,
                      allAnnotations
      k8s.go          quantity, image reference and selector semantics
      heuristics.go   the stated false-positive budgets, shared
      shorthand.go    required/forbidden/equals/… compiled to the same CEL
      compile.go      per-check compile, load-time errors, per-run planning
    baseline/       the shipped pack, as embedded YAML, plus the fixture corpus
    rendercache.go  the key, and why reusing a render cannot change an answer
    render/         helm.go, probe.go, source.go, evidence.go (the keep budget)
    source/         artifact acquisition, budget, unpack
internal/store/compliance.go
internal/api/compliance.go, compliancewire.go, complianceevidence.go,
             complianceexport.go, compliancesheets.go
internal/maintenance/compliance.go
pkg/apis/softwaregateway/v1/compliance.go
cmd/transferctl/compliance.go
web/src/components/compliancepanel.tsx
web/src/pages/Policies.tsx
db/migrations/{postgres,sqlite}/00035_compliance.sql
test/fixtures/compliance/
```

Three `depguard` rules, added to `.golangci.yml` beside the existing ones
([15](15-code-layout.md) §3):

| Rule | Denies | Why |
|---|---|---|
| `compliance-imports-no-api` | `internal/compliance/**` → `internal/api` | The domain rule everything else follows |
| `cel-confined-to-evaluator` | everything except `internal/compliance/cel/**` → `cel.dev/cel-go/**` | One package owns the expression language. **Delete `internal/compliance/cel` and the Go baseline still builds and still checks** - the same mechanical test that keeps the vendor plugins optional (`internal/vendors/classify.go`) |
| `no-opa` | anywhere → `github.com/open-policy-agent/opa/**` | 18 linked modules and 59 in the module graph, for a second expression language. §19 decision 5 |
| `builtin-imports-no-render` | `internal/compliance/builtin/**` → `internal/compliance/render` | A check judges parsed resources. One that shelled out to helm for itself would be unreproducible and untestable without the binary |

## 16. Failure matrix

| Failure | Detected by | Behaviour | Verdict effect |
|---|---|---|---|
| `helm` absent | Start-up probe | `T1-C` checks run; `T1-R` report `error` | inconclusive |
| One chart will not render | Non-zero exit | That chart's `T1-R` → `error` with helm's stderr; others unaffected | inconclusive |
| Chart dependencies not vendored | `Chart.yaml` vs `charts/` | SUP-07 fails; chart not rendered | fail (SUP-07 is `block`) |
| Chart exceeds `maxArtifactBytes` | Descriptor size, before fetch | Not fetched; recorded as `too_large` | inconclusive |
| Run exceeds `maxRunBytes` | Running total | Remaining artifacts skipped, loudly | inconclusive |
| Render times out | Wall clock | `SIGKILL`, chart → `error` | inconclusive |
| A pack fails to compile | Load | Pack rejected, listed as broken; its checks → `error` | inconclusive |
| A check panics | `recover` | One `error` result per applicable resource; pack marked unhealthy | inconclusive |
| A check exceeds its timeout | Per-check deadline | `error`, and the pack is reported as slow in metrics | inconclusive |
| Registry unreachable | Fetch | Run fails with the reason; **no partial run is recorded as a verdict** | run `failed`, no verdict |
| Coordinator dies mid-run | Stale `heartbeat_at` | Claim released by the sweeper; the release is checkable again | previous run stands |
| Results exceed `maxResultsPerRun` | Counter | Truncated; `truncated=true` on the run, stated in API, UI and export | inconclusive |
| Rendered text exceeds `evidencePerRelease` | Running total | Later documents kept truncated or not at all; each says which. Findings unaffected - the manifests are what a finding is displayed against, never what it is derived from | none |

Every row that ends in `inconclusive` is deliberate. A run that could not examine
everything is not a run that passed.

## 17. Testing

Per [15](15-code-layout.md) §5, and with one addition that is the quality
mechanism for the whole feature.

### 17.1 The fixture corpus

`test/fixtures/compliance/charts/`:

| Fixture | Exists to prove |
|---|---|
| `good-app` | **Zero findings across the entire baseline.** The false-positive gate. Every new check runs against it, so "my check does not fire on correct charts" is asserted by CI, not by its author |
| `bad-pdb` | `maxUnavailable: 0`, `"0%"`, `minAvailable` at replica count, a 3-replica workload with no PDB, an orphan PDB, a PDB using `matchExpressions` that **must not** fire (the false positive in [compliance/03](../compliance/03-sample-policy-review.md) §3.3) |
| `bad-probes` | No readiness on a service-backed container; identical liveness and readiness; liveness stricter than readiness; a probe pointing at a sidecar's port |
| `bad-security` | privileged, UID 0, `NET_ADMIN` added, `hostPath`, a runtime socket mount, `hostNetwork`, `Unconfined` seccomp |
| `bad-rbac` | Wildcard verbs, `list` on secrets, a ClusterRoleBinding, `pods/exec` |
| `bad-images` | `:latest`, a semver tag, an unapproved registry, an image in a CronJob's `jobTemplate`, an image in an operator CR |
| `bad-resources` | Missing requests on a **sidecar and an init container** - the case [compliance/03](../compliance/03-sample-policy-review.md) §3.7 says is usually forgotten |
| `bad-cronjob` | Every applicable check must fire on a `CronJob`. Directly targets the seven-file false negative in [compliance/03](../compliance/03-sample-policy-review.md) §3.1 |
| `bad-networking` | Allow-all NetworkPolicy, a Service selecting nothing, a `targetPort` naming a port no container declares, `/metrics` on an Ingress |
| `bad-cnf` | A `NetworkAttachmentDefinition` with no IPAM, an SR-IOV resource in `requests` but not `limits`, a pod on a secondary network with a NetworkPolicy that cannot cover it |
| `bad-storage` | A 2-replica Deployment with a PVC, an init container `chown`-ing a mount, a claim with no `storageClassName` |
| `configurable-replicas` | `replicas` from `.Values` - the determinacy probe must return `configurable`, and the PDB finding must be worded for defaults |
| `fixed-replicas` | The same chart with `replicas: 3` hard-coded - determinacy must be `fixed` and the finding must block |
| `gated-pdb` | A PDB behind `{{- if .Values.pdb.enabled }}` defaulting off - the probe must report "ships a PDB, disabled by default" |
| `broken-render` | A template that fails - must produce `error`, never `pass` |
| `missing-dep` | An unvendored `Chart.yaml` dependency - SUP-07 fails and the chart is not rendered |
| `nondeterministic` | A `checksum` annotation built from `now` - CFG-10 fails and the whole chart's determinacy is `unknown` |
| `no-workloads` | Only ConfigMaps - workload checks must `skip` with a reason, never `pass` |
| `subchart` | A finding inside a vendored subchart must be attributed to the subchart |

Each carries an `expected.yaml` asserting the **exact set** of
`(check, outcome, kind, name, container, locus, determinacy)` tuples. Not a
subset. A check that also fires on the fixture's ConfigMap fails its own test.

### 17.2 The gates

| Test | Runs | Needs helm | Gate |
|---|---|---|---|
| Engine against **committed rendered manifests** | Every PR | No | Merge |
| Fixture corpus, exact-set expectations | Every PR | Yes - CI image carries it | Merge |
| `good-app` produces zero findings | Every PR | Yes | Merge |
| **Coverage meta-test**: every registered check has a positive and a negative fixture | Every PR | No | Merge |
| **Determinism**: the same fixture checked twice produces byte-identical results | Every PR | Yes | Merge |
| Renderer against a real chart corpus | Nightly | Yes | Release |
| Store, both dialects | Every PR | No | Merge |

**PR CI must not need Docker for unit tests** ([15](15-code-layout.md) §5), and
this respects that: the engine's own tests run against committed rendered YAML
and need nothing. Helm is a binary in the CI image, not a container.

The coverage meta-test is the rule that stops the catalogue rotting. A check
without fixtures cannot be registered, so it cannot reach a release, so nobody
can add a plausible-sounding assertion that has never been shown to fire.

## 18. Delivery

### M11-A - Engine and catalogue

No UI, no export. The half that has to be right.

- `internal/compliance`: types, address, indexes, pack manifest, loader, watcher, engine, verdict
- `render/helm.go` with the sandbox, the pinned inputs and the determinacy probe
- `source/` with the byte budget, reusing the existing blob-read path
- `cel/`: the environment, the engine functions of [compliance/02](../compliance/02-authoring-checks.md) §4.2, the shorthand compiler
- `policy/`: the manifest, and the **88 tier-1 checks** of [compliance/01](../compliance/01-check-catalog.md) §4 - declarative wherever the check is expressible that way, `builtin/` where it is not
- Migration `00035`, both dialects; store
- The fixture corpus and all five merge gates from §17.2
- `transferctl compliance <product> <tag>` - the CLI is the first client, as it is for everything else

**Acceptance:** every catalogued v1 check has a positive and a negative fixture;
`good-app` produces zero findings; the same release checked twice is
byte-identical; a release with no `helm` on the box reports `inconclusive` and
never `pass`.

### M11-B - API, UI and the catalogue people read

- Routes of §10, wire types, pagination and filtering
- Compliance tab, Policies page, Software listing column, `Checked` timeline moment
- Live progress, cancel, audit events, metrics

**Acceptance:** a release engineer opens a release, sees which charts fail which
checks and why, and can explain any check on screen without leaving the UI.

### M11-C - The report, waivers and automation

- XLSX / CSV / JSON / ZIP export, all eight sheets
- Waiver loading, expiry, the waived column
- `autoRun: onAnalysis`, retention sweep, cross-release comparison by fingerprint
- The migration of the organization's existing `.rego` policies to declarative
  checks per [compliance/03](../compliance/03-sample-policy-review.md) §5

**Acceptance:** a vendor receives one file that names every failure with its full
address, states the rule that produced it, and lists what passed - and can
reproduce any finding from the bundle without access to this platform.

### M11-D - Gates and tier 2 (not scheduled)

The download gate of §12.2, and tier-2 compliance against site values files.
Deliberately after the first three have been used in anger: a gate designed
before anyone has read a hundred reports is a gate designed against a guess.

### Sequencing

M11 needs `expand` (the artifact tree) and the blob-read path, both of which
exist. It needs nothing from M10, and the CLI-first shape of M11-A means it
delivers value before any UI work starts.

## 19. Decisions recorded

Consolidated; each is argued where it is made.

| # | Decision | Alternative rejected | Where |
|---|---|---|---|
| 1 | The Coordinator reads chart and file blobs, never image layers | Render in a worker | §4 |
| 2 | The `helm` binary, not the Helm Go SDK | `helm.sh/helm/v3` in-process | §5.1 |
| 3 | Determinacy by differential render | Static template analysis; assuming defaults | §6 |
| 4 | Passes derived from a declared `appliesTo` | Policies emitting their own passes | [compliance/00](../compliance/00-compliance-model.md) §5 |
| 5 | Checks are YAML with CEL expressions; Go only where the platform must be consulted | Embedding OPA/Rego: +18 linked modules against cel-go's +4 (+59 against +3 in the module graph), an evaluator with no termination guarantee, and a second language | [compliance/02](../compliance/02-authoring-checks.md) §7 |
| 6 | Severity on the check, outcome on the result | One conflated field, as the samples have | [compliance/00](../compliance/00-compliance-model.md) Rule 3 |
| 7 | The engine binds one subject at a time; checks never loop | Checks iterating the release themselves, as the samples do | [compliance/02](../compliance/02-authoring-checks.md) §2 |
| 8 | Waivers in Git, never through the API | A waiver UI | [compliance/00](../compliance/00-compliance-model.md) §7 |
| 9 | `error` is an outcome, and it makes a run inconclusive | Treating an undecidable check as a pass | [compliance/00](../compliance/00-compliance-model.md) Rule 2 |
| 10 | Fixtures are a merge gate, enforced by a meta-test | Fixtures by convention | [compliance/02](../compliance/02-authoring-checks.md) §6 |
| 11 | Cross-resource semantics are engine functions, not per-check code | Every author re-implementing selector matching, as `pdb.rego` did | [compliance/02](../compliance/02-authoring-checks.md) §4.2 |
| 12 | Denormalized result rows | A normalized schema with joins | §9 |

## 20. Open questions

| # | Question | Why it is open | What would settle it |
|---|---|---|---|
| Q1 | Does the determinacy probe hold up on a real 97-chart corpus? | Flipping booleans changes which resources render. The design handles that (a resource in one render only is `configurable`), but the *proportion* of `unknown` results is unknown until it is measured | Run it against a real orb in M11-A and count |
| Q2 | Is 88 checks too many to ship at once? | A first report with 400 warnings on every vendor release is a report nobody reads | Measure against three real releases; if the warning volume is unusable, ship the `block` set first and stage the `warn` set |
| Q3 | Should MTA-06/07 severities be organization-configurable after all? | §13 says one organization, one severity. Provenance and ownership annotations are the checks most likely to be right for one product family and wrong for another | Whether the waiver mechanism handles it. If waivers are being written in bulk for one check, the check's severity is wrong |
| Q4 | Where do the evidence-only items (26 of them) live? | They are real requirements that no check can decide. Tracking them nowhere means they are not tracked | Probably a release checklist beside the automated results, sourced from the same catalogue - but that is a feature, not a footnote, and it needs its own design |
| Q5 | How many of the 88 baseline checks are expressible declaratively? | The target is most of them; the ones that are not define how much `builtin/` has to carry, and how good the shorthand has to be | Counted in M11-A as the baseline pack is written. A baseline that is 80% Go means the extension point is decorative |
