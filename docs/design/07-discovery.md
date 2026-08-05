# 07 — Discovery

> **Prerequisites:** [02 — Configuration](02-configuration.md), [03 — Persistence](03-persistence.md), [06 — Registry Abstraction](06-registry-abstraction.md)

Discovery answers one question, repeatedly: *has this vendor published something we have not seen?*

---

## 1. Where it runs

On the **leader** Coordinator only ([04](04-queue-and-scheduling.md) §9). One goroutine per source repository with `discovery.enabled: true`, each on its own configured interval (default 15 m).

Per-repository rather than one global loop, because a slow or unreachable vendor must not delay every other vendor. A single loop iterating all sources would make one dead registry a fleet-wide discovery stall — the exact failure that turns a vendor's bad afternoon into ours.

## 2. The scan

```
for each enabled source repository, every `interval`:

  1. ListTags with Link-header pagination        (06)
  2. Apply tagFilters (include, then exclude)    (02 section 4)
  3. For each surviving tag:
        ResolveTag -> manifest digest            (HEAD; body not fetched)
        INSERT INTO packages ... ON CONFLICT DO NOTHING
  4. For each row actually inserted:
        - fetch and store the manifest tree      (03 section 5)
        - write an audit event
        - enqueue notifications                  (section 5)
        - evaluate auto-download rules           (section 4)
```

**Step 3 is the whole idempotency story.** The unique constraint on `(source_repo_id, tag, manifest_digest)` ([03](03-persistence.md) §5) means a repeated scan, an overlapping scan, or a scan that crashed halfway through and restarted produces no duplicates. There is no "have I seen this?" lookup to race against — `ON CONFLICT DO NOTHING` and the `RETURNING` clause tell us precisely which rows are new.

`ResolveTag` uses `HEAD` and reads the `Docker-Content-Digest` header, so the common case — a scan where nothing changed — costs one small request per tag and transfers no manifest bodies. Manifest trees are fetched only for genuinely new packages (step 4).

## 2.1 Which repositories a source covers

A source is **one registry**. Which repositories on it get scanned is decided by one question, with no separate switch:

```
repositories named   ─▶ scan exactly those
none named           ─▶ every repository on the registry,
                        narrowed by discovery.repositoryFilters
```

> **Decision — naming no repositories IS the request to enumerate.**
>
> *Alternative:* a `repositoryDiscovery.enabled` flag, which an earlier revision had.
>
> *Rejected because* it made two fields say the same thing, and let them disagree. `repositories` listed with `enabled: false`, or nothing listed with `enabled: false`, are both configurations that look deliberate and scan nothing. A single source of truth cannot express a contradiction.
>
> *And the enumerating case is the one that matters.* A product whose components each ship as a **new repository** cannot list them in advance. Requiring a list means a new component is silently not replicated until somebody edits the ConfigMap — a failure with no symptom, which is the worst kind.

Both filters live under `discovery`, because discovery is what they govern: a scan finds **repositories**, then **tags**, and each step gets one.

```yaml
discovery:
  enabled: true
  interval: 15m
  repositoryFilters: {include: [...], exclude: [...]}
  tagFilters:        {include: [...], exclude: [...]}
  maxRepositories: 200
```

The repository set is re-resolved on **every scan**, for exactly the reason the tag set is (§3): a repository published since the last pass should be found without a restart or a configuration reload.

> **Decision — catalog enumeration is supported, unfiltered enumeration is a warning rather than an error.**
>
> *An earlier revision of this document rejected `/v2/_catalog` outright.* That was too strong, and this records the correction rather than quietly rewriting it.
>
> *The original argument, which still holds for a vendor registry:* enumeration is slow on large registries, inconsistently paginated, and frequently forbidden for the credentials a vendor issues — the credential is usually scoped to pulling a named repository, not to listing the registry.
>
> *Why it is nonetheless supported:* none of that holds for an **internal registry you control**, which is exactly where a product spans repositories nobody can enumerate in advance.
>
> *Why unfiltered enumeration is not rejected:* on a registry dedicated to one product it is correct, and on a shared one it is a mistake — and only the operator knows which. A blanket rule would be wrong half the time. So [`transferctl products check`](13-cli.md) reports **the number that decides it**: how many repositories this source would actually adopt. A fact beats a rule when the rule cannot be right for everyone.
>
> *What is kept from the original concerns:* adoption is capped (`maxRepositories`, default 200), because a catalog suddenly returning thousands is far more likely to be a misconfiguration than a real change. And a registry that refuses enumeration produces an error naming the fix — list the repositories — rather than a generic 403.

**Two populations of repository rows.** `repositories.managed_by` distinguishes them, because their lifecycles differ:

| `managed_by` | Created by | Deactivated by |
|---|---|---|
| `config` | reconciliation, from YAML | reconciliation, when the declaration is removed |
| `discovery` | a scan, from the registry | a scan, when it leaves the registry |

Without the distinction, every configuration reload would deactivate every discovered repository and the next scan would revive it — a flap that would churn the audit trail for no reason.

## 3. Full scan, not incremental

Every scan lists every tag. There is no cursor, no "tags since" watermark, no cached tag set.

> **Decision — stateless full scans over incremental discovery.**
>
> *Alternative:* remember the last-seen tag set or a pagination cursor and scan only the delta.
>
> *Rejected because* the OCI tag list has **no ordering guarantee and no change feed**. There is no "tags newer than X" — a cursor is a position in an arbitrary, registry-defined order that can change between calls. Any incremental scheme would need reconciliation against reality to avoid permanently missing a tag, and that reconciliation is a full scan.
>
> *And it is cheap.* A repository with 500 tags costs 500 `HEAD` requests every 15 minutes. That is under one request per second per repository, well inside any vendor's rate limit, and the requests are small.
>
> *The property that matters:* a full scan is **self-healing**. Discovery that was down for a day, or that crashed mid-scan, or that ran against a registry serving a stale replica, simply catches up on the next pass. There is no divergent state to detect and no repair path to write, because there is no state.
>
> *What would change our mind:* a source repository with tens of thousands of tags. The mitigation is `tagFilters` (§2 step 2), applied before any `ResolveTag`, which bounds the cost by what we actually care about.

## 4. Re-pushed tags

> **First, what supersession is *not*.** Different tags never supersede each other. `v2.13.0`, `v2.14.0` and `v2.14.1` are independent software packages that coexist indefinitely, each separately transferable, verifiable and deployable. Discovering a newer tag does nothing whatsoever to an older one — a repository holding fifty versions holds fifty active packages.
>
> Supersession applies to exactly one situation: **the same tag re-pushed with different content.**

A vendor can re-push `v2.14.0` with different content. The tag is the same; the manifest digest is not.

Because identity is `(source_repo, tag, manifest_digest)` ([01](01-domain-model.md) §2.2), this inserts a **new** package row. The previous row — the one carrying *the same tag* and the *old* digest — is marked `superseded` with `superseded_by` pointing at the new one. Note the `AND tag = $3` clause below: the statement cannot touch a package with a different tag.

```sql
UPDATE packages SET state = 'superseded', superseded_by = $1, updated_at = now()
 WHERE source_repo_id = $2 AND tag = $3 AND id <> $1
   AND state NOT IN ('superseded');
```

The old package's history — what we replicated, when, to where, and whether it verified — is preserved. Overwriting in place would be simpler and would destroy the ability to answer "which bytes did we actually ship in March", which is exactly the question an audit trail exists for.

A re-push is a notable event: it emits a `PackageSuperseded` audit event and is surfaced by `transferctl packages list`, because a vendor silently changing a released tag is something an operator should know about.

## 5. Auto-download rules

Evaluated against each newly discovered package, in configured order, **first match wins** ([02](02-configuration.md) §5.4).

```
for each new package:
    for each rule in product.autoDownload.rules:      # in order
        if rule.tagPattern matches package.tag:
            create TransferRequest{
                targets:  rule.targets or product default target,
                priority: rule.priority,
                origin:   'auto_download',
                ruleName: rule.name,
                idempotencyKey: derive(...),          # 04 section 7
            }
            break                                     # first match wins
```

**Idempotency is what makes this safe to run in a loop.** The derived key ([04](04-queue-and-scheduling.md) §7) means that if discovery re-runs, or the Coordinator restarts between the package insert and the request creation, or leadership flaps and two Coordinators both evaluate the rules, exactly one request exists. This matters more here than anywhere else in the system: an auto-download rule is the one path that creates 45 GB of work with no human in the loop.

Patterns are RE2 (Go `regexp`) — linear time, no backtracking. Stated explicitly in [02](02-configuration.md) §5.4 because a user-supplied pattern evaluated inside a polling loop would, under a backtracking engine, be a denial-of-service vector.

`verifyBeforeTransfer` on a rule sets source-side verification for the resulting request ([08](08-verification.md) §4), so a product can be configured to auto-download only what already verifies.

## 6. Notifications

A new package emits `PackageDiscovered` into the outbox ([03](03-persistence.md) §7), routed by the product's subscriptions ([02](02-configuration.md) §4).

Written **in the same transaction** as the package insert. This is why the outbox exists: it is impossible to insert the package and fail to enqueue the notification, or to notify about a package that was rolled back. Delivery is a separate, retried concern; *deciding to notify* is atomic with the fact that caused it.

## 7. Failure handling

| Failure | Behaviour |
|---|---|
| Registry unreachable | Log, increment `discovery_errors_total{repository}`, back off (exponential to a 4× interval cap), retry. **Never** disable the source — a vendor outage must not require human re-enablement afterwards |
| Auth failure | Same, plus a `DiscoveryFailed` notification: this needs a human, and silently retrying a bad credential forever helps nobody |
| Partial page failure | Keep the packages already inserted; the next full scan completes the rest (§3) |
| Malformed manifest | Record the package as `failed` with the reason; continue the scan. One bad artifact must not stop discovery of the rest |
| Coordinator restart mid-scan | Nothing to recover. The next scan is a full scan |

`softwaregateway_discovery_last_success_timestamp_seconds{repository}` is the metric that matters operationally: it catches the dangerous failure mode, which is not "discovery is erroring loudly" but "discovery quietly stopped finding anything". Alert on staleness, not on error rate.

## 8. Manual discovery

`POST /api/v1/products/{product}/packages:discover` ([09](09-api.md) §3) triggers an immediate scan, bypassing the interval. Used after a vendor announces a release and when validating configuration.

Idempotent and safe: it is the same scan the loop runs. Concurrent triggers are collapsed — a request arriving while a scan is running JOINS that scan and returns its result, rather than starting a second one against the same source.

### Collapsing means joining, not reporting

The first implementation collapsed by handing the trigger to the worker through a one-deep buffered channel and, when it found the slot occupied, returning the worker's *last* result. That was wrong in a way that looked like success.

While a scan is running the slot stays occupied for its whole duration, so any further trigger took the fallback: it returned the previous scan's numbers — or the **zero value**, when no scan had completed yet — with no error, in microseconds, having done nothing. Measured at 19µs against a worker with an occupied slot. The caller could not tell, because nothing in the response said so, and `packages discover` printed

```
Scanned vendor-a-platform in 0ms
  Repositories scanned      0
  ...
Nothing new. A scan that finds nothing is the normal steady state, not a failure.
```

for a scan that never ran. The visible symptom was inconsistency: the same command sometimes took seconds and did real work, and sometimes returned instantly with zeros, depending only on whether a scan happened to be in flight — which, with the interval scan firing immediately at startup and a slow registry taking up to two minutes to fail, was often.

The fix is a shared `scanCall`: the first caller registers it, everyone else waits on the same one, and all of them get that scan's real result. Execution stays on the polling goroutine so a scan is always on the same goroutine as the backoff counter and the interval timer, and a caller who gives up does not cancel a scan other callers are waiting on.

The response carries `collapsed: true` when a request joined a scan rather than starting one. The numbers are real either way, but "a scan ran for you" and "you were shown a scan already under way" are different facts, and an operator watching a count they expect to change deserves to know which they are looking at.

### Zero repositories is not "nothing new"

A scan that resolves **no repositories** is reported distinctly from one that scanned repositories and found no new tags. The two produced identical output and are not the same event: the first means nothing was looked at.

It has two causes, and the CLI names them. Either `discovery.repositoryFilters` rejected every candidate, or the source names no repositories and the registry's catalog returned none. A third cause — an enumerating source with no catalog client — used to fall through the same path silently, producing a sub-millisecond successful scan with no network call at all; it is now an error.

---

## 9. Knowing what a scan is doing

A scan is synchronous by default, and against a slow registry that meant `packages discover` showed a blank terminal for two and a half minutes and then reported a timeout. Everything the operator needed — that we had reached the registry, which repository we were on, that the counters were moving — existed in the process and was never exposed. A slow scan and a hung one looked identical.

`GET /api/v1/products/{product}/discovery` reports, per source: whether a scan is running, its phase (`ENUMERATING_REPOSITORIES`, `LISTING_TAGS`, `RESOLVING_TAGS`), how long it has been going, repositories done of total, the current repository, tags resolved of admitted, and the outcome of the last completed scan.

It is a read of in-memory counters behind their own mutex — deliberately not the scanner's, which is held across client construction, so a status request never waits on a TLS handshake. Safe to poll every second, which is what `transferctl packages discover` does on a second connection while the scan request blocks. The live line goes to **stderr**; stdout carries the result, so `-o json` stays pipeable.

`RepositoriesTotal` is zero until enumeration finishes, and that is information rather than a gap: it means the scan is still waiting on `/v2/_catalog`.

### `wait: false`

`packages:discover` accepts `wait: false`, which registers the scan, returns immediately, and reports how many sources started versus how many were already scanning. The distinction matters — "I started four scans" and "one started, three were already going" are different answers, and only one of them is true.

Holding an HTTP request open for the several minutes a slow registry can take makes every intermediary's idle timeout part of the control plane. `wait: false` is the way out; the progress endpoint is how you then follow it.

---

## 10. Vendor registries and why there are no vendor plugins

The generic client speaks four endpoints, and that is the whole of what discovery needs:

| | |
|---|---|
| `GET /v2/_catalog` | enumerate repositories |
| `GET /v2/<name>/tags/list` | list tags, with RFC 8288 `Link` pagination |
| `HEAD /v2/<name>/manifests/<ref>` | resolve a tag to a digest |
| `GET /v2/<name>/manifests/<ref>` | fetch the manifest, verbatim |

Every registry we have been pointed at — including vendor-hosted distribution registries behind corporate proxies — serves exactly these. A source that fails against them is failing on TLS, credentials, proxying or timeouts, not on protocol.

That is why `registry_type` exists but has one implementation. The vendor types in [06](06-registry-abstraction.md) §6 are reserved for genuine deviations, and a new backend should be added only when a measured request differs — not because a registry has a vendor's name on it. A second implementation of the same four calls is a second place for the pagination bug to live.

---

## 11. Concurrency

A scan used to be strictly sequential: one repository at a time, one tag at a time. That is fine for a handful of repositories on a fast registry and useless against a real vendor catalogue.

Measured in the field: 48 repositories, 28 admitted tags in the first one, and after **122 seconds the scan was still on the first tag of the first repository**. Three things were compounding, and only one of them was the network.

**Sequential work.** 48 repositories × ~28 tags × 2 round trips minimum is ~2,700 sequential requests across a WAN. Even at a healthy 200 ms each that is nine minutes; at a second each it is three quarters of an hour.

**Retry amplification.** A manifest `GET` that blows the 30 s `ResponseHeaderTimeout` was retried up to 8 times. One unresponsive endpoint therefore cost up to **four minutes for a single request**, and discovery makes two per tag. The attempt count alone is the wrong bound: it assumes attempts fail fast, which is exactly what a timeout does not do.

**The underlying stall**, which is a network or proxy problem and the operator's to fix — but the first two turned "slow" into "apparently hung".

### What changed

Three bounded axes, because they cost different things:

| | Default | Bounded by |
|---|---|---|
| `discovery.concurrency.repositories` | 4 | repositories are independent; they share the source's pool and limiter |
| `discovery.concurrency.tags` | 8 | tags within a repository share one client, so they are already bounded by its connection pool and rate limiter |
| `discovery.concurrency.artifacts` | 8 | siblings of one index — the narrowest axis, and the one that was invisible |

**The artifact axis was the last serial bottleneck, and the one that looked like a hang.** A tag's manifest tree was walked strictly one manifest at a time. A product bundle whose index references sixty artifacts therefore cost sixty *sequential* round trips inside a single tag — two and a half minutes at 2.5 s each, during which the tag counter did not move at all while every one of those requests succeeded. From the outside, "hundreds of requests returning 200 and no progress" is indistinguishable from a hang.

The walk is still breadth-first and still level by level, so parents are recorded before their children and `Artifact.Parent` — an *index* into the artifact slice — stays correct. Only the fetches within one level run in parallel; the bookkeeping that appends to the tree runs on one goroutine in level order. A test asserts the parallel walk produces a tree byte-identical to the sequential one, because a reordering there would silently reparent artifacts rather than fail.

Progress now also reports `artifacts`, the count of manifests fetched. It is the counter that keeps moving when nothing else does.

Both are clamped to 64: a typo of `1000` must not become a thousand connections to a vendor.

Nothing about a scan requires ordering. Repositories are independent; supersession is per `(repository, tag)` and two tags never touch the same row. Results are aggregated in configuration order *after* the work, so the result and the log order are identical run to run even though the execution was not.

**Writes stay serial.** Everything expensive — the `HEAD`, the existence check, the manifest-tree fetch — already happened outside the transaction. What remains is a short local write, and serialising it per source costs nothing measurable while removing a class of problem: SQLite serialises writers anyway and returns `SQLITE_BUSY` rather than queueing, and on Postgres concurrent inserts for one repository would contend on the unique index for no gain.

**And a retry time budget.** `RetryMaxElapsed`, 90 seconds by default — three `ResponseHeaderTimeout`s. Long enough for a genuine transient failure to be retried a couple of times, short enough that a systematically unresponsive endpoint costs seconds per request rather than minutes. It does not shorten the schedule for the case retries exist for, where attempts fail fast and the budget is never reached.

### Why not unbounded

The temptation is to fan out over everything at once. A vendor registry is someone else's infrastructure, the cost of being impolite falls on them, and a 429 storm makes the scan slower, not faster — the rate limiter sits outermost precisely so retries cannot bypass it ([06](06-registry-abstraction.md) §5). The defaults are chosen to make a scan minutes rather than hours without looking like an attack. Raise them for a registry you own.

---

## 12. What a scan actually fetches

Per tag, in order:

| Step | Requests |
|---|---|
| `HEAD manifests/<tag>` → digest | 1 |
| already known? | 0 — a DB lookup, and the scan **stops here** |
| `GET manifests/<digest>` | 1 |
| its children | **0** |

So an unchanged tag costs **one** request and a newly discovered one costs **two**, whatever the package contains.

### Why the children are not fetched

Discovery used to walk the whole tree. It does not any more, and the argument is that **nothing is lost**: the root digest immutably determines the entire tree. Given a digest we already hold, the tree can be walked exactly, at any time, by whatever needs it. The traversal was a *cache* — and it was paid for on every newly discovered tag.

The cost was not marginal. A bundle whose index references sixty artifacts cost sixty extra round trips inside a single tag, and a first scan of a real vendor catalogue — 48 repositories, dozens of tags each, bundles throughout — ran into five figures of requests. Discovery's cost was O(total artifacts) when the question it answers is O(tags).

An index also already carries what a listing needs. Each child descriptor states its digest, media type, size and platform, so "this package contains three images, a chart and two files" is answerable from the bytes just fetched. Those children are recorded as artifact rows **without raw bytes**, and that distinction is deliberate: a row with bytes was fetched and verified against its digest, a row without has the vendor's word for it. The API reports it as `fetched`, and `packages describe` prints `(listed, not fetched)`.

### The one thing that is genuinely deferred

A bundle's **transfer size**. An index states the size of each child *manifest*, not of the layers underneath it, so without fetching the children the layer bytes are unknown.

That is reported as unknown — `totalBytes` and `blobCount` are NULL in the database, absent from the JSON, and rendered as `not measured` and `?` — rather than summed from what we happen to hold. A total that omitted the layers would understate a bundle by nearly all of it, and **a wrong size is worse than a missing one, because nobody questions a number.**

A package whose root is a plain image manifest is still fully measured: its config and layer descriptors are inside the one manifest we fetched.

M3's transfer walks the tree, because it must fetch those blobs anyway — and it does so against the digest recorded here, which is what makes deferring safe rather than merely cheaper.

---

## 13. Two stages: discover, then inspect

Discovery answers one question — **what is new** — and stops there. Everything else about a package is recoverable from the digest it recorded, so it is recovered when someone actually wants it.

| | Discovery | `packages:inspect` |
|---|---|---|
| Cost per tag | 1 request unchanged, 2 new | 1 per artifact in the tree |
| Runs | on an interval, unattended | on demand, for one package |
| Answers | is there something new? | what is in it, and how big? |

`POST /api/v1/products/{product}/packages/{package}:inspect` walks the tree: it fetches the artifacts discovery only listed, records their blobs, and measures the transfer size. `transferctl packages inspect <product> <tag>` is the same thing.

An AIP-136 custom method rather than a GET, because it has side effects — it writes artifacts, blobs and a measured size. Idempotent all the same: **the tree under a digest cannot change**, so a second call fetches nothing and says `alreadyExpanded`.

### One walker, two callers

`InspectPackage` is a function, not logic inside an HTTP handler, because **M3's transfer calls it too**. A transfer has to walk the tree anyway — it cannot copy blobs it has not enumerated — so it performs this expansion before moving bytes. That makes `inspect` optional rather than a required first step: it is for deciding whether you *want* the transfer, not for enabling it.

The alternative was two code paths computing what a package contains, which would eventually disagree about something like whether a repeated child counts once.

### It runs through the source's own client

Inspect is routed through the discovery loop rather than given the API its own registry client, so it uses the same per-source stack: one connection pool, one rate limiter, one cached token, the configured proxy and CA. A second client would be invisible to the ceilings the operator set — which is precisely the bug that made scans slow ([06](06-registry-abstraction.md) §5).

The consequence is that inspect runs on the **leader**, and a follower answers `FAILED_PRECONDITION` saying why rather than a 500.

### Writing over what discovery listed

Discovery wrote the children as rows with no bytes. Inspect fills the same rows in — `ON CONFLICT ... DO UPDATE`, with `raw = COALESCE(EXCLUDED.raw, package_artifacts.raw)` so a re-run cannot blank a manifest already held — and adds anything deeper for the first time.

All in **one transaction**, because a half-expanded package is the worst outcome available: it would carry a size that omits most of its bytes, with nothing marking it partial. Either the whole tree is known or none of it is.

---

## 14. What the tag's manifest is for

Discovery fetches exactly one manifest per newly discovered tag. It is worth being specific about what that buys, because it is more than a digest.

From that single response:

- **the digest** — identity, and what makes supersession work when a vendor re-pushes a tag;
- **the media type** — index or single artifact, which decides everything else;
- **the child list** — each child's digest, size, media type and platform, so a listing can say what a package contains without fetching any of it;
- **the annotations** — on the manifest itself and on each child descriptor.

The annotations were being parsed and thrown away. A real vendor index carries a great deal in them:

```json
"annotations": {
  "org.opencontainers.image.created": "2024-06-12T17:56:19Z",
  "org.opencontainers.image.vendor":  "Nokia",
  "com.nokia.ncd.orb.rb.name":        "CFX-5000-k8s",
  "com.nokia.ncd.orb.rb.version":     "23.8.1076"
}
```

and per child:

```json
"org.opencontainers.image.ref.name": "cfx-5000-product/crdb-redisio:9.0.3",
"com.nokia.ncd.orb.type":            "helmchart"
```

### One key promoted, the rest kept whole

`org.opencontainers.image.created` becomes a column, `packages.published_at`. That key is defined by the **OCI image spec**, under the reserved `org.opencontainers.` namespace — it is not any one vendor's, which is exactly what makes it safe to build on. Every registry and build tool that sets it agrees on its meaning: the date and time the artifact was built, RFC 3339.

It earns a column because it is the one you want to sort and filter by. Everything else — including a vendor's own `com.nokia.ncd.*` keys — is stored verbatim as JSON on the artifact, so it reaches an operator without this project knowing those keys exist. The alternative is a column per vendor, which does not end.

Three properties, all of which the spec forces:

**It is optional.** A registry that sets no annotations is fully conformant, so `published_at` is nullable and nothing may require it.

**It is free text.** Whoever published the artifact wrote it. A value that is not RFC 3339 is dropped rather than stored for something downstream to trip over.

**It is a claim, not an observation.** The vendor says this is when it was built; we cannot verify that. So it is kept strictly separate from `discovered_at`, which is a fact about us. Folding them into one "date" would lose the ability to say which — and *"published in March, we only noticed in July"* is precisely the sort of thing worth being able to see.

Read from the **root manifest only**. An index's children each carry their own created time, and a package's date is the release's, not its earliest component's — the children can be rebuilt independently.
