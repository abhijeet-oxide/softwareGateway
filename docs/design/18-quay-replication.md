# 18 — Quay Replication Strategies

> **Prerequisites:** [01 — Domain Model](01-domain-model.md), [02 — Configuration](02-configuration.md), [05 — Transfer Engine](05-transfer-engine.md), [06 — Registry Abstraction](06-registry-abstraction.md)
> **Status: PROPOSED. Nothing in this document is implemented.** It specifies the design and is scheduled at [M8](17-delivery-plan.md#m8--quay-replication-strategies). Where it changes an existing rule — in particular [00 §3](00-overview.md#3-non-goals)'s "not a pull-through cache or mirroring proxy" — it says so and says why.

---

## 1. What this is for

Today the system has exactly one way to get content into a destination: **our workers pull it from the origin and push it to the target** ([05](05-transfer-engine.md)). That is `copy`, it works, and it stays the default.

But copy is not the only mechanism a Red Hat Quay registry offers, and the deployment that prompted this document makes the difference matter. The shape is three hops:

```
   vendor (NEAR)                JFrog Artifactory              Quay on OCP
   registry.vendor.example  ──►  artifactory/docker-local  ──►  quay.apps.ocp.example
        the vendor's                   STORAGE                 WHERE PODS PULL FROM
        publication                (what we keep)             (what actually runs)
```

The first hop has no alternative — a vendor registry offers nothing but OCI, so somebody has to pull and push, and that somebody is us. **The second hop is different.** Quay can fetch from JFrog by itself, on its own schedule, without our workers touching a byte, in two entirely different ways:

- **Repository mirroring** — Quay is configured with an external repository, a tag pattern, credentials and an interval, and its own `repomirror` worker pulls on that schedule.
- **Proxy cache** — a Quay organization is configured to front an upstream registry, and content is cached lazily when a client pulls through it.

The requirement is that a target declares which of the three it wants, the same way a source declares `vendor: near`:

```yaml
targets:
  - name: ocp-prod
    type: quay
    replication:
      mode: mirror        # copy | mirror | proxy
```

This document establishes what each mechanism actually is, what it can and cannot promise, what has to be configurable, what changes inside our own domain model when we stop being the thing that moves the bytes, and which mode to pick for which job.

**The short answer, so it is not buried:** support all three, default to `copy`, and treat `mirror` and `proxy` as *delegation* — we stop being the mover and become the configurator and the observer. That trade is worth making in specific, nameable situations, and it is a bad trade everywhere else. §4 says which is which.

## 2. The three mechanisms, precisely

### 2.1 Copy — what exists today

Our planner walks the manifest tree, classifies every blob against the destination, and workers stream each one origin→destination with the digest verified inline ([05](05-transfer-engine.md) §3–4). Nothing at the destination needs to be configured beyond a repository and a credential with push rights.

Properties that matter for the comparison, all of them consequences of us being in the data path:

- Per-blob progress, byte totals, speed and ETA, because we are counting the bytes as they pass.
- Deduplication against `blob_placements` and cross-repository mount ([01](01-domain-model.md) §4, [05](05-transfer-engine.md) §4.2).
- Resumption at blob granularity after a worker dies ([11](11-resiliency-and-backpressure.md) §2).
- Exact content selection: we transfer *a package*, meaning a tag pinned to the digest it resolved to at discovery, plus every artifact under it — signatures and SBOMs included.
- An audit record that says we moved these digests, at this time, on this request.

### 2.2 Repository mirroring

**A per-repository configuration on the Quay side.** The destination repository is put into `MIRROR` state and given an external reference; Quay's mirroring worker then pulls matching tags on an interval.

Mechanics worth knowing before designing against it:

| Fact | Consequence for us |
|---|---|
| The repository must be in **`MIRROR` state** (`PUT /api/v1/repository/{ns}/{repo}/changestate`, `{"state": "MIRROR"}`; values are case-sensitive) | Flipping state is destructive to the write path. A repository in `MIRROR` state accepts pushes **only** from the designated robot |
| **Only the designated robot may push** — this supersedes role-based permissions on the repository | A `copy` transfer into a mirrored repository will fail. Mode is therefore a property of the target, and mutually exclusive per target (§5.4) |
| Selection is **tag globs only** — Unix shell wildcards (`*`, `?`, `[seq]`, `[!seq]`), comma-separated | A mirror cannot be told "this exact digest". Our Package identity is `(repo, tag, digest)` ([01](01-domain-model.md) §2.2) and a mirror cannot express it |
| The repository is **converged to the rule**, not merely added to. Content that stops matching is removed | A mirror is authoritative and destructive. That is a feature for hygiene and a hazard for anything hand-placed |
| Mirroring is **one-way** and cannot be combined with manual pushes | A mirrored target cannot also be a promotion destination |
| Sync is scheduled (`sync_start_date` + `sync_interval`, default 24 h) and can be forced (`POST …/mirror/sync-now`) or stopped (`…/mirror/sync-cancel`) | We can make it on-demand, so "delegated" does not have to mean "whenever Quay feels like it" |
| It requires `FEATURE_REPO_MIRROR: true` **and a `repomirror` worker pod actually running** | A preflight check that only validates our config would pass against a Quay that will never sync anything |
| Tags within one repository are **not** mirrored in parallel; parallelism comes from mirroring several repositories at once | Throughput scales with repository count and mirror pod count, not with our worker fleet |
| Quay emits `Repository Mirror Started` / `Success` / `Unsuccessful` notifications | An event source we can subscribe to instead of polling |
| `ROBOTS_DISALLOW: true` breaks mirroring entirely | A registry-wide setting we do not control can silently invalidate the whole mode |

The configuration surface, which is also the API body — `POST`/`PUT /api/v1/repository/{ns}/{repo}/mirror`:

| Quay field | Type | What it is |
|---|---|---|
| `is_enabled` | bool | Sync on or off, without discarding the configuration |
| `external_reference` | string | The source, e.g. `artifactory.example.com/docker-local/vendor-a/platform` |
| `external_registry_username` / `_password` | string | Credentials **stored inside Quay** for reaching that source |
| `sync_start_date` | RFC 3339 | When the first sync is due |
| `sync_interval` | integer seconds | How often afterwards |
| `robot_username` | string | The robot that performs the push; `org+robot` form |
| `root_rule` | object | `{"rule_kind": "tag_glob_csv", "rule_value": ["v3*", "latest"]}` |
| `external_registry_config.verify_tls` | bool | TLS verification against the source |
| `external_registry_config.unsigned_images` | bool | Accept unsigned images |
| `external_registry_config.proxy.http_proxy` / `.https_proxy` / `.no_proxy` | string | Egress proxy **for Quay's pull**, not for ours |
| Skopeo timeout | integer seconds | Per-operation ceiling; 300 s default, 43 200 s maximum |

Registry-wide settings, which belong to whoever operates Quay and not to a product: `REPO_MIRROR_INTERVAL` (how often the worker looks for due repositories, default 30 s), `REPO_MIRROR_TLS_VERIFY`, `REPO_MIRROR_ROLLBACK` (revert the repository after a failed sync, default `false`), `REPO_MIRROR_SERVER_HOSTNAME`.

**Note what is in that table and what is not.** The proxy, the CA posture and the credentials are Quay's, not ours. A mirror moves the entire network problem — egress, trust, throttling, timeouts — from our transport stack ([06](06-registry-abstraction.md) §5) into a Quay cluster we may not operate. Everything doc 02 says about `network.proxy`, `caBundleRef` and `insecureSkipVerify` stops applying to that hop, and the equivalent knobs have different names, different scope and a different owner. That is the single most under-appreciated cost of this mode.

### 2.3 Proxy cache

**An organization-level configuration**, not a repository one. A Quay organization is declared to front an upstream registry (a whole registry, `quay.io`, or a single namespace, `docker.io/library`), and thereafter a pull against `quay.example.com/<proxy-org>/<upstream-path>:<tag>` is served from cache when possible and fetched through on a miss.

| Fact | Consequence for us |
|---|---|
| **Content enters only on a pull.** Nothing is pre-positioned | There is no moment at which we can say the package "is in Quay". Only that it *can be* |
| A hit is validated against the upstream digest before being served | Cache freshness is correct, but it means an upstream round trip on essentially every pull |
| `expiration_s` (default 86 400) sets the staleness window. Pull inside it and the cached copy is served; pull after it and an upstream failure is propagated to the client | **Not an air-gap or outage strategy.** A cached image does not survive an upstream outage past the staleness window |
| Expired tags disappear on schedule but remain stored until garbage collection, which follows the organization's Time Machine setting (14 days by default) | Storage does not shrink when you expect it to |
| With a quota configured, least-recently-used tags are auto-pruned | Storage self-manages, which is the main operational attraction |
| **No pushes** into a proxy organization; **no way to list** upstream tags through it | A proxy target can never be a transfer destination, and discovery cannot run against one |
| Anonymous pulls of not-yet-cached images are refused | Cluster pull secrets are mandatory, not optional |
| Requires `FEATURE_PROXY_CACHE: true` | Same preflight problem as mirroring |

Configuration surface — `POST /api/v1/organization/{org}/proxycache`, with `POST …/validateproxycache` to test before committing, plus `GET` and `DELETE`:

| Quay field | Type | What it is |
|---|---|---|
| `upstream_registry` | string | `quay.io`, or `docker.io/library` to scope to one namespace |
| `upstream_registry_username` / `_password` | string | Optional; for private upstreams or better rate limits |
| `expiration_s` | integer seconds | Staleness window, default 86 400 |
| `insecure` | bool | Do not validate the upstream's TLS |

The API returns the configuration without the password, which matters for drift detection (§8).

## 3. The axis that actually separates them

The three are routinely presented as three ways to copy images. They are not. **They differ in who moves the bytes and who owns the schedule**, and every other difference follows from those two.

| | `copy` | `mirror` | `proxy` |
|---|---|---|---|
| Who moves the bytes | our workers | Quay's `repomirror` worker | Quay, during a client pull |
| Who decides *when* | us — a request, a rule, a schedule | Quay's timer, or our `sync-now` | whoever runs `podman pull` |
| Our role | mover | configurator + observer | configurator |
| Network path required | origin ⇄ **our workers** ⇄ target | origin ⇄ **Quay** | upstream ⇄ **Quay**, at pull time |
| Selection granularity | one package, pinned to a digest | tag glob | whatever is pulled |
| Content present at time *T* | **known** | eventually, within one interval | only if someone asked for it |
| Byte-level progress / ETA | yes | no — a state, not a percentage | no |
| Dedupe + cross-repo mount | ours ([05](05-transfer-engine.md) §4) | Quay's storage layer, invisible to us | Quay's, invisible to us |
| Resumable at blob granularity | yes | Quay's business (skopeo, with a timeout) | n/a |
| Survives *our* outage | no | **yes** | **yes** |
| Survives the *origin's* outage | in-flight only | no | only inside the staleness window |
| Removes content when the rule changes | never | **yes** | on expiry / LRU prune |
| Audit claim we can honestly make | "we moved these digests" | "we configured this, and observed that sync" | "we configured this" |

Three rows carry most of the weight.

**Network path.** Mirroring and proxy cache require *Quay* to reach the origin. In the topology in §1, that means the OCP cluster's egress must reach JFrog — and in the deployments where this tool earns its keep, that link is frequently the one that does not exist, or exists only through a proxy nobody has written down. Copy is the only mode whose reachability requirement is satisfied by the machine we control. **This is the first question to ask, because a negative answer ends the discussion.**

**Content present at time *T*.** Copy is the only mode that can answer "is release 2.14.0 in production right now" with a fact rather than a probability. A mirror answers "it will be, within an interval, if it matched the glob and the sync succeeded". A proxy cache answers "it will be, the first time a pod asks, if the upstream is up". For a maintenance window, a regulated deployment, or a disaster-recovery premise, only the first answer is worth anything.

**Survives our outage.** This is the one genuine capability copy cannot offer at any price. A mirror configured once keeps converging while softwareGateway is down, being upgraded, or decommissioned. If continuity through *our* absence is a requirement, mirroring is the answer and no amount of engineering on the copy path produces it.

## 4. What each mode buys, and when to choose it

### 4.1 Copy

**Buys:** determinism, per-blob progress and ETA, deduplication and mounting, blob-granular resumption, exact digest selection, signature and SBOM fidelity because we walk the tree ourselves, inline digest verification (invariant I9), and an audit record that names what moved. It needs no Quay feature flags, no robot with push rights beyond the ordinary one, no vendor credentials inside Quay, and it works identically against quay.io, Project Quay, ACR, Artifactory and a plain distribution registry.

**Costs:** our bandwidth and our fleet are in the path; content arrives only when we say so; and the destination has no protection against somebody hand-pushing into it.

**Choose it for:** production and regulated destinations, air-gapped or one-way-diode environments, anything with a maintenance window, anything that has to be verified before it is used, and every first hop from a vendor registry. **This is the default and should stay the default.**

### 4.2 Mirror

**Buys:**

- **The bytes stop traversing us.** JFrog→Quay direct is one hop instead of two, over whatever link the OCP cluster has, and our fleet is free for the vendor hop that only we can do.
- **Standing convergence.** A new tag matching `v3.*` appears in the destination without anyone filing a request — continuous replication rather than an event per release.
- **Independence from us.** It keeps working through our outages and upgrades. Nothing else on this list does.
- **A write-protected destination.** `MIRROR` state makes robot-only push a registry-enforced property, not a policy anyone can forget. For a production namespace whose provenance has to be defensible, "nobody can hand-push here, structurally" is a real governance win.
- **Rollback on failure** (`REPO_MIRROR_ROLLBACK`) and Quay's own retry behaviour, at no cost to us.
- **Hygiene by construction.** Content that stops matching the rule is removed, so the destination cannot silently accumulate a decade of tags.

**Costs:** coarse observability (a sync state, not a byte count); tag globs rather than digests; destructive convergence; vendor or JFrog credentials stored inside Quay; per-repository configuration and a robot for each; a network, proxy and TLS posture that moves outside our configuration model (§2.2); throughput that scales with mirror pods rather than with our HPA; and a hard dependency on registry-level settings (`FEATURE_REPO_MIRROR`, `ROBOTS_DISALLOW`) that a Quay administrator can change without telling us.

**Choose it for:** a curated, stable set of repositories that should track an internal upstream continuously; the JFrog→Quay hop specifically, when the OCP cluster has a good path to JFrog; destinations that must keep converging while we are down; and destinations where robot-only push is wanted for its own sake.

### 4.3 Proxy cache

**Buys:**

- **Nothing is pre-positioned.** Storage is spent only on what is actually pulled. Against a vendor catalog of hundreds of repositories where a cluster consumes a dozen images, this is the difference between terabytes and gigabytes.
- **Zero latency to availability.** Anything upstream is pullable the moment it exists — no discovery interval, no transfer, no request.
- **Rate-limit and egress shielding**, which is the feature's original purpose, and one hostname for every cluster pull policy.
- **Self-managing storage** through quota plus LRU auto-pruning.

**Costs:** nothing is guaranteed to be present; the first pull pays full upstream latency; upstream must be reachable at pull time, and beyond the staleness window an upstream outage becomes a pod failure; tags cannot be enumerated, so discovery, `compare` and any inventory question are impossible through it; no pushes, so it can never be a transfer destination; configuration is organization-scoped, so it is coarse; and expired content lingers in storage until Time Machine and garbage collection catch up.

**Choose it for:** development and CI namespaces, base images and the long tail, and any place where the cost of holding everything exceeds the cost of an occasional slow first pull. **Do not choose it** as the path for the software this system exists to replicate: a proxy cache cannot tell you what you have, and "what did we ship in March" ([FUNCTIONAL-OVERVIEW](../FUNCTIONAL-OVERVIEW.md) §5.9) has no answer through one.

### 4.4 They compose, and the useful configurations mix them

A product declares several targets, and the modes are per target. The pattern that falls out of §4.1–4.3:

```yaml
targets:
  - {name: jfrog-store,  type: artifactory, replication: {mode: copy}}    # vendor hop: only we can do it
  - {name: ocp-prod,     type: quay,        replication: {mode: mirror}}  # standing convergence from JFrog
  - {name: ocp-dev-cache, type: quay,       replication: {mode: proxy}}   # the long tail, on demand
```

That is the deployment in §1, written down: we own the hop nobody else can do, Quay owns the hop it can do better, and the cache absorbs everything nobody planned for.

> **Decision — support all three rather than picking one.**
>
> *Alternative considered:* keep `copy` only, on the grounds that it is the mode with real progress, dedupe and audit, and that a second and third mechanism triples the surface for one registry vendor.
>
> *Rejected because* two of the three properties the other modes offer cannot be built on the copy path at any cost: convergence that survives our absence, and content that materialises without being pre-positioned. Declining to support them does not remove them from the estate — operators configure Quay mirroring by hand today, outside any record we keep, and our `compare` output then disagrees with reality for reasons nobody can see. Bringing them into the configuration model is how they become visible.
>
> *Cost accepted:* three modes with genuinely different contracts, which §6 makes explicit rather than papering over with a uniform-looking API.
>
> *What would change our mind:* if `mirror` in practice cannot be observed well enough to report honestly — if a sync failure is not reliably detectable — then it is delegation without accountability and should be dropped rather than shipped with a status field nobody can trust. That is the M8 exit criterion.

## 5. Configuration

### 5.1 The block

`replication` is a new optional block on a target. Absent means `mode: copy`, which is exactly what every existing document means today — no configuration in the estate changes meaning.

```yaml
targets:
  # ── copy: today's behaviour, stated explicitly ────────────────────
  - name: jfrog-store
    registry: artifactory.example.com
    repository: docker-local/vendor-a/platform
    type: artifactory
    credentialsRef: {secretName: jfrog-writer}
    replication:
      mode: copy                       # default; the block may be omitted entirely

  # ── mirror: Quay pulls from JFrog on its own schedule ─────────────
  - name: ocp-prod
    registry: quay.apps.ocp.example.com
    repository: platform-prod/vendor-a-platform
    type: quay
    credentialsRef: {secretName: quay-reader}   # OCI credentials, for our own reads
    environment: production
    replication:
      mode: mirror
      mirror:
        # WHERE QUAY PULLS FROM. Names another target in this product, so the
        # chain jfrog-store -> ocp-prod is declared once and stays consistent
        # when the JFrog path changes. `externalReference` is the escape hatch
        # for a source this product does not otherwise declare.
        from: jfrog-store
        # externalReference: artifactory.example.com/docker-local/vendor-a/platform

        # WHAT TO SYNC. Tag globs -- Quay's only selector. Shell wildcards,
        # NOT the RE2 used everywhere else in this document set (§5.3).
        tags: ['v3.*', 'sha256-*.sig']

        # WHEN. `interval` maps to sync_interval; `startAt` to sync_start_date
        # and defaults to the moment the configuration is first applied.
        interval: 6h
        # startAt: "2026-09-01T02:00:00Z"
        enabled: true                  # is_enabled -- pause syncing, keep config

        # WHO PUSHES. A Quay robot in the destination organization. Required by
        # Quay; there is no default we could invent.
        robot: platform-prod+swgw-mirror

        # CREDENTIALS QUAY WILL USE TO REACH THE SOURCE. Written into Quay,
        # by reference as everywhere else (02 §5.5). Omit for an anonymous
        # source. See §11 -- these leave our custody.
        sourceCredentialsRef: {secretName: jfrog-reader}

        # HOW QUAY REACHES IT. Quay's egress, not ours: these are written into
        # Quay's mirror configuration and have no effect on our own transport.
        network:
          verifyTLS: true              # external_registry_config.verify_tls
          proxy:
            httpsProxy: http://proxy.internal.example.com:3128
            httpProxy:  http://proxy.internal.example.com:3128
            noProxy: ['artifactory.example.com', '.svc.cluster.local']
        acceptUnsignedImages: false    # external_registry_config.unsigned_images
        skopeoTimeout: 300s            # 300s default, 43200s maximum

        # WHAT WE DO ABOUT IT. See §8: we never mutate Quay as a side effect of
        # a config reload.
        manage: apply                  # apply | detect
        syncOnRequest: true            # a `download` becomes a sync-now + wait

  # ── proxy: a Quay organization fronting an upstream ───────────────
  - name: ocp-dev-cache
    registry: quay.apps.ocp.example.com
    repository: dev-cache/vendor-a/platform   # <proxy-org>/<upstream-path>
    type: quay
    replication:
      mode: proxy
      proxy:
        organization: dev-cache        # the Quay org that IS the cache
        upstreamRegistry: artifactory.example.com/docker-local
        upstreamCredentialsRef: {secretName: jfrog-reader}
        expiration: 24h                # expiration_s; default 86400s
        insecure: false                # do not validate upstream TLS
        manage: apply
        # Optional: pull every blob of a package THROUGH the proxy so the cache
        # is populated before anyone needs it. Bytes are discarded; nothing is
        # pushed. See §6.3 -- this is the only way to make a lazy cache
        # deterministic, and it is off by default because it costs a full pull.
        prewarm: false
```

### 5.2 Field reference

| Field | Type | Required | Default | Notes |
|---|---|---|---|---|
| `replication.mode` | enum | no | `copy` | `copy`, `mirror`, `proxy`. Non-`copy` requires `type: quay` |
| `mirror.from` | string | one of `from`/`externalReference` | — | Names another target in the same product |
| `mirror.externalReference` | string | " | — | `host/path`, no tag or digest |
| `mirror.tags` | []string | yes | — | Shell globs. At least one |
| `mirror.interval` | duration | no | `24h` | Quay's default. Sent as seconds |
| `mirror.startAt` | RFC 3339 | no | apply time | |
| `mirror.enabled` | bool | no | `true` | `is_enabled` |
| `mirror.robot` | string | yes | — | `org+name`. Validated against the target's organization |
| `mirror.sourceCredentialsRef` | secretRef | no | — | Omit only for an anonymous source |
| `mirror.network.verifyTLS` | bool | no | `true` | Quay's TLS posture, not ours |
| `mirror.network.proxy.*` | object | no | — | Quay's egress proxy, not ours |
| `mirror.acceptUnsignedImages` | bool | no | `false` | |
| `mirror.skopeoTimeout` | duration | no | `300s` | Clamped to 43 200 s |
| `mirror.manage` | enum | no | `apply` | `apply` writes to Quay on an explicit apply; `detect` never writes and only reports drift (§8) |
| `mirror.syncOnRequest` | bool | no | `true` | Whether `download` triggers `sync-now` |
| `proxy.organization` | string | yes | — | The Quay org that is the cache |
| `proxy.upstreamRegistry` | string | yes | — | Registry or registry+namespace |
| `proxy.upstreamCredentialsRef` | secretRef | no | — | |
| `proxy.expiration` | duration | no | `24h` | `expiration_s` |
| `proxy.insecure` | bool | no | `false` | |
| `proxy.manage` | enum | no | `apply` | as above |
| `proxy.prewarm` | bool | no | `false` | §6.3 |

### 5.3 Tag globs are not our regexes, and the schema must not pretend otherwise

Everywhere else in this system a pattern is **RE2** — `tagFilters`, `autoDownload.rules[].tagPattern` ([02](02-configuration.md) §5.4). `mirror.tags` is **not**: Quay accepts shell-style globs and nothing else, and there is no faithful translation in either direction (`v3.*` means different things in the two dialects — under RE2 it matches `v3` followed by anything, under globbing it matches a literal dot).

So the field is named `tags` rather than `tagPattern`, its documentation says "glob", and validation rejects a value containing RE2-only constructs (`^`, `$`, `\d`, `(?…)`) with an error that names the dialect. Silently accepting `^v\d+\.\d+\.\d+$` as a glob would produce a mirror that matches exactly nothing and reports success forever — the most expensive possible failure, because it looks like a working configuration.

### 5.4 Validation

Beyond the field rules, the combinations that must fail at load rather than in production:

| Rejected | Why |
|---|---|
| `mode: mirror` or `proxy` with `type` other than `quay` | Neither mechanism exists elsewhere; a silently-ignored block is worse than an error |
| `mode: mirror` on a target named by an `autoDownload` rule, unless `syncOnRequest: true` | A rule would create transfers that have nothing to do |
| `mode: proxy` named by an `autoDownload` rule or any transfer target | A proxy organization cannot be pushed to. This one must fail at load, because it would otherwise fail on the day a rule first matches |
| `mode: proxy` with `promotionOnly` or `default: true` | Same reason; a proxy target is not a destination |
| `mode: mirror` as a promotion **destination** | Robot-only push; a promotion would be rejected by Quay |
| `mirror.from` naming a target that does not exist, is disabled, or is itself `mode: proxy` | A cache cannot be enumerated, so it cannot be a mirror source |
| `mirror.tags` empty, or containing RE2 syntax | §5.3 |
| `mirror.robot` absent or not in `org+name` form | Quay requires it and we cannot invent one |
| `proxy.organization` disagreeing with the first path segment of `repository` | The repository path *is* the proxy layout; two spellings that disagree cannot both be right |

`transferctl config validate` reports every one of these before merge, which is where a mode mistake is cheap.

### 5.5 Two credentials, and they are not the same credential

Quay's **management API** is not its **registry API**, and this trips people up:

| Purpose | Endpoint | Credential |
|---|---|---|
| Pull and push artifacts | `/v2/…` | Robot account, `org+robot` as the username ([06](06-registry-abstraction.md) §6.4) |
| Read and write mirror / proxy configuration | `/api/v1/…` | **OAuth 2 bearer token** with `repo:admin` (mirror) or `org:admin` (proxy cache) scope |

So a Quay target in a non-`copy` mode needs a second reference:

```yaml
    type: quay
    credentialsRef: {secretName: quay-robot}          # /v2 -- artifacts
    quay:
      apiTokenRef: {secretName: quay-api, key: token} # /api/v1 -- configuration
      # apiEndpoint: https://quay.apps.ocp.example.com   # defaults to the registry host
```

Declared under a `quay` block rather than inside `replication`, because it is a property of *speaking to this registry's control API*, in the same way `credentialsRef` is a property of speaking to its data API — and because a future Quay-specific capability (quota, notifications) will want the same token without belonging to replication at all.

**A `copy`-mode Quay target needs none of this**, which keeps the common case exactly as it is today.

## 6. What a transfer means in each mode

This is where delegation stops being a configuration detail. Our domain has one vocabulary — TransferRequest, Transfer, Job, progress, dedupe, verification ([01](01-domain-model.md) §3) — and two of the three modes do not move bytes through it. **The contract has to be stated per mode rather than implied**, because the alternative is an API that returns the same-shaped object with fields that quietly mean something else.

| Operation | `copy` | `mirror` | `proxy` |
|---|---|---|---|
| `download` / `POST /transfers` | plans jobs, moves bytes | applies config if needed, `sync-now`, polls to a terminal state, then walks the destination to record what arrived | **rejected** with a problem detail explaining that content arrives on pull, and naming `warm` |
| `promote` **to** it | yes | **rejected** — robot-only push | **rejected** |
| `promote` **from** it | yes | yes — it is an ordinary readable repository | no — cannot enumerate |
| Discovery against it | n/a (targets are not polled) | n/a | n/a |
| `compare` | full, per digest | full — it is readable over `/v2` | **partial**: only tags already cached are visible, and the output must say so |
| `verify` | yes | **yes, and it matters more** (§9) | only what is cached |
| Dry run | job plan, byte totals, dedupe | the *config diff* plus the tags the glob currently selects — never a byte estimate | the resolved cache path and whether the upstream is reachable |
| Progress | bytes, speed, ETA | a **state**: `configuring` → `syncing` → `succeeded`/`failed` | none |
| Blob placements | recorded | recorded **from observation**, after a sync, with `verified_at` set | never |
| Retry / pause / cancel | queue operations | `sync-now` / `sync-cancel`; pause sets `is_enabled: false` | n/a |

### 6.1 Progress that does not lie

A mirror sync gives us a state and, at best, a completion time. **It does not give us bytes**, and the temptation to synthesise one — from the package's known size, from elapsed time, from anything — has to be refused. Every byte column for a mirror transfer reports `—`, and `transferctl transfers describe` says `delegated to Quay mirror` where it would otherwise print a speed.

This follows the rule the transfer output already lives by: measure the byte columns against the same thing, and never present a number whose provenance you cannot state. A progress bar advancing on a timer is worse than no progress bar, because someone will make a decision from it.

### 6.2 A mirror transfer's outcome is an observation, not an assertion

When Quay reports a sync succeeded, we do not know what it did. So the terminal step of a mirror transfer is **a walk of the destination**: resolve the package's tag, compare the digest to the one we discovered, record the artifacts and blob placements we find, and reconcile.

That produces three outcomes worth distinguishing, and they must not collapse into one:

- **`succeeded`** — the tag at the destination resolves to the digest we expected.
- **`diverged`** — the sync succeeded but the destination holds a *different* digest for that tag. This is normal for a mirror (the upstream tag moved, and the mirror faithfully followed it) and is precisely the case our supersession model exists for ([10](10-state-machines.md) §2). It is not a failure; it is a fact, and it must be recorded and notifiable.
- **`failed`** — the sync failed, or the tag is absent because the glob never matched it. The second is the common misconfiguration and the error must say which of the two it was.

### 6.3 `warm` — the only way to make a lazy cache deterministic

A proxy cache has no notion of "put this there". But there is nothing stopping *us* from pulling a package through the proxy endpoint and discarding the bytes: the pull populates the cache exactly as a pod's would, and afterwards the content is present for as long as the staleness window and the LRU allow.

So `transferctl warm <package> --target ocp-dev-cache` (and `replication.proxy.prewarm: true` to do it automatically after discovery) walks the manifest tree through the proxy path, `GET`s every blob, and counts the bytes it discarded. This is a genuine transfer in the byte sense — it costs full bandwidth into the OCP cluster and out again to nowhere — so it is opt-in, its output says plainly that it moved *N* bytes and stored none of them locally, and it never claims the content is durably present. It buys one thing: the first real pull is fast, and a scheduled job before a release window is a defensible reason to want that.

### 6.4 How a mode looks to somebody who is not reading this document

The three modes are a real distinction in the engine and mostly **not** a decision the person requesting a release should be making. The interface therefore surfaces them as *what happens*, not as *what to choose* ([19](19-user-interface.md) §3.1):

- A Download Rule declares the chain once — **Vendor → JFrog → Quay mirror** — which is where `mode` is actually decided, by whoever writes the product configuration. [20](20-download-rules.md) specifies that rule; note that it does *not* re-declare the chain, it **derives** it from `mirror.from` above ([20](20-download-rules.md) §4), so this block stays the only place the edge is written.
- A download then shows the chain as steps: *Downloading to JFrog* (measured bytes, speed, ETA, because we move them) → *Configuring Mirror to Quay* (configured-at, first sync completed, content matches — because Quay moves them) → *Verification* → *Completed*.
- The word "mirror" appears; the words "replication mode", "delegated" and "strategy" do not.

Two steps of one operation with two different kinds of truth, shown differently on purpose. §6.1 is the rule; this is what it looks like on a screen.

## 7. Where the seam goes

The engine in [05](05-transfer-engine.md) is written against `Repository` ([06](06-registry-abstraction.md) §2) and must not learn about modes. The new seam is one level above it, at the point where a Transfer is planned:

```go
// internal/transfer/strategy
//
// A Strategy owns what "make this package present at this target" means.
// Copy is the existing planner and engine; the others delegate and observe.
type Strategy interface {
    // Plan renders what this strategy WOULD do. It is also the dry run --
    // one code path, per 01 §3.4, so the plan cannot drift from the act.
    Plan(ctx context.Context, req Request) (Plan, error)

    // Apply performs the strategy's own side of the work: for copy, creating
    // job rows; for mirror, reconciling the Quay configuration and requesting
    // a sync; for proxy, ensuring the organization is configured.
    Apply(ctx context.Context, p Plan) error

    // Observe reports current truth from the registry's own state. For copy
    // this is the job rollup; for mirror, the sync status plus a destination
    // walk; for proxy, whether the tag is cached.
    Observe(ctx context.Context, t Transfer) (Observation, error)
}
```

| Package | Holds |
|---|---|
| `internal/transfer/strategy/copy` | the existing planner and queue path, moved behind the interface and otherwise unchanged |
| `internal/transfer/strategy/mirror` | config reconciliation, `sync-now`, poll, destination walk |
| `internal/transfer/strategy/proxy` | proxy-cache reconciliation, cache probe, `warm` |
| `internal/registry/quay` | **the Quay management API client** — `/api/v1` mirror, proxycache, changestate, robots. Deliberately separate from the `/v2` data-path implementation, which stays generic ([06](06-registry-abstraction.md) §6.4). Two protocols, two clients, one host |

Persistence ([03](03-persistence.md)) gains two tables and one column:

- `target_replication` — the desired mode and its resolved settings, plus `applied_config_hash` and `applied_at`: what we last wrote to Quay, so drift is computed against what we sent rather than against what we can read back (secrets are never returned — §8).
- `mirror_syncs` — observed sync runs: `started_at`, `finished_at`, `status`, `observed_digest`, `message`. This is the evidence behind §6.2 and the thing `transferctl` renders.
- `transfers.strategy` — which strategy ran, so a year-old transfer record still says how it was performed.

API ([09](09-api.md)) gains, on the target resource:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products/{p}/targets/{t}/replication` | Desired config, applied config, drift, mode |
| `POST` | `/api/v1/products/{p}/targets/{t}/replication:apply` | Write it to Quay. `validateOnly=true` renders the diff |
| `POST` | `/api/v1/products/{p}/targets/{t}/replication:sync` | `sync-now` |
| `POST` | `/api/v1/products/{p}/targets/{t}/replication:cancelSync` | `sync-cancel` |
| `GET` | `/api/v1/products/{p}/targets/{t}/syncs` | Observed sync history |

CLI ([13](13-cli.md)) gains a `targets` group beside `products`, following the existing rule that things you *look at* are nouns and things the tool *does* are verbs:

```
├── targets
│   ├── list [product]           Targets and their replication mode
│   ├── describe <product> <target>
│   ├── apply <product> <target>    Write the replication config to Quay (--dry-run)
│   ├── sync  <product> <target>    Trigger a mirror sync now, --watch to follow
│   └── drift [product]             What differs between Git and Quay
│
├── warm <tag> --target <t>      Populate a proxy cache by pulling through it
```

Metrics ([12](12-observability-and-audit.md) §2), following the existing naming:

```
softwaregateway_mirror_sync_total{product,target,result}
softwaregateway_mirror_sync_duration_seconds{product,target}
softwaregateway_mirror_config_drift{product,target}          gauge, 0 or 1
softwaregateway_proxy_cache_probe_total{product,target,result}
```

Audit ([12](12-observability-and-audit.md) §4.1) gains a `Replication` category: `ReplicationConfigApplied`, `ReplicationConfigDrifted`, `MirrorSyncRequested`, `MirrorSyncSucceeded`, `MirrorSyncFailed`, `MirrorContentDiverged`, `ProxyCacheConfigured`, `CacheWarmed`. Each records what we *did* or *observed* — never "transferred", which we did not.

## 8. Reconciliation: apply is explicit, drift detection is not

> **Decision — configuration reload never writes to Quay. An explicit `apply` does.**
>
> *Alternative considered:* reconcile continuously, the way a Kubernetes controller would. Git is the source of truth ([02](02-configuration.md) §2), so a product whose YAML says `mode: mirror` should make Quay say so, automatically and forever.
>
> *Rejected because* the write is destructive in a way a config reload must never be. Applying a mirror configuration flips the destination repository into `MIRROR` state, which makes it **read-only to everyone but a robot** and converges its content to a tag glob — deleting whatever does not match. A `kubectl apply` of a ConfigMap with a typo in `tags` would empty a production repository within one interval, and nothing in the edit would have looked dangerous. Our own reload path is designed on the same principle already: removing a product does not cancel in-flight transfers, because "deletion of running work is never a side effect of a config edit" ([02](02-configuration.md) §6).
>
> *Chosen:* configuration declares intent; `transferctl targets apply` enacts it, with a diff shown first and `--dry-run` rendering only the diff. **Drift is detected continuously and reported loudly** — on every reload, in `products check`, as a metric, and as an audit event — so the gap between Git and Quay is never silent, merely never closed behind your back.
>
> *For teams that want the controller behaviour anyway:* `replication.mirror.manage: apply` is the default and means "apply when asked"; a future `manage: auto` may close the loop automatically once M8 has produced evidence about how often drift is legitimate. It is deliberately not in the initial schema — the safe default should be the one that exists first.
>
> *What we lose:* a GitOps flow that ends at `git push`. Accepted: one extra deliberate command on a rare change, against a failure mode that empties registries.

**Drift is computed against what we sent, not against what Quay returns**, because Quay does not return the credentials it stores. `target_replication.applied_config_hash` covers the non-secret fields; secret material is compared by the hash of the secret's *value at apply time*, so a rotated credential shows as drift and a redacted response does not. Comparing against the API response directly would report permanent, unfixable drift on every password field — the classic reconciler bug that trains operators to ignore the drift signal entirely.

## 9. Verification, signatures, and the tag-glob trap

**Delegating the transfer makes destination verification more important, not less.** In `copy` mode we hold every byte and check its digest inline (invariant I9); in `mirror` mode we hold nothing and are trusting Quay and skopeo. The natural conclusion is the right one: for non-`copy` modes, `verification.atDestination` should default to **on**, and a product that turns it off is making a decision it should have to write down.

And there is a specific, expensive trap.

**Cosign signatures are tags, and a tag glob will silently exclude them.** A cosign signature for `sha256:abcd…` is published as the tag `sha256-abcd….sig` ([08](08-verification.md) §3). A mirror rule of `v3.*` — the obvious rule, the one anybody would write — matches every release and **not one signature**. The sync succeeds. The tags arrive. Verification at the destination then fails for every package, with an error that says the signature is missing, and the cause is three configuration screens away in a field that looks correct.

Three defences, because this will otherwise be discovered in production:

1. **Validation warns** when `verification.enabled` is true, `transferSignatures` is on, and no entry in `mirror.tags` could match `sha256-*`. Reported by `transferctl config validate`, so it is caught before merge.
2. **The documented example carries `'sha256-*.sig'`** in its tag list (§5.1), so the copyable configuration is the correct one.
3. **`targets describe` shows the selected tag set** as Quay currently resolves it, next to the packages we know about, so "the signatures are not in the mirror" is visible rather than inferred.

The same reasoning applies to OCI 1.1 referrers, SBOMs and attestations — anything discovered through the referrers API is reachable by digest, but anything published as a companion *tag* is subject to the glob. A mirror is only as complete as its rule, and the rule speaks a language that cannot express "and everything attached to these".

## 10. Failure modes

| Failure | Symptom | What we do |
|---|---|---|
| `FEATURE_REPO_MIRROR` off, or no `repomirror` pod running | Config applies (or 404s), nothing ever syncs | Preflight probe at apply time and in `products check`; a mirror whose `sync_status` has never advanced is reported as **stalled**, not pending |
| `ROBOTS_DISALLOW: true` set registry-wide | Mirroring breaks for every target at once | Detected by the same probe; reported as a registry-level fault against every mirror target, not as N unrelated failures |
| Glob matches nothing | Sync "succeeds", destination stays empty | §6.2 `failed` with the two causes distinguished; §9 defence 1 catches the common case pre-merge |
| Upstream tag moved | Destination digest ≠ discovered digest | `diverged` (§6.2) plus supersession, notifiable |
| Quay cannot reach the source | Sync fails on Quay's side, with Quay's error text | Surfaced verbatim in `mirror_syncs.message` — our own connectivity being fine is exactly the confusion to pre-empt |
| Somebody edits the mirror in the Quay UI | Git and Quay disagree | Drift gauge, audit event, `targets drift`; never auto-corrected (§8) |
| Proxy upstream down beyond the staleness window | Pods fail to pull; we look healthy | `products check` probes the upstream through the proxy path and reports the effective staleness window |
| Proxy quota LRU evicts what a release needs | Slow first pull, or failure if upstream is down | Reported, not prevented. `warm` before a window is the mitigation (§6.3) |
| Mode changed from `mirror` to `copy` in Git | Pushes fail while the repository is still in `MIRROR` state | `apply` performs the `changestate` back to `NORMAL` and says so in the diff; a plain reload does not, and `products check` reports the mismatch |

## 11. Security

**Credentials leave our custody.** `mirror.sourceCredentialsRef` and `proxy.upstreamCredentialsRef` are written into Quay's database, where Quay's protections apply and ours stop. This is not a reason to refuse the feature — it is what the feature *is* — but it means:

- Those credentials should be **distinct, least-privilege, read-only** accounts, and the field documentation says so.
- We record `ReplicationConfigApplied` with the secret's *name*, never its value, and the redacting wrapper ([02](02-configuration.md) §5.5) applies on our side of the boundary as it does everywhere else.
- Rotation is a `targets apply`, not a reload, and drift detection notices when a rotated secret has not been applied (§8).

**The management token is more powerful than the registry credential.** `org:admin` on a Quay organization can delete repositories. The token should be scoped per product where Quay's model allows it, and `apiTokenRef` is deliberately a separate secret from the robot so the blast radii do not merge.

**`MIRROR` state is a privilege change on the destination.** Applying it takes push rights away from every human and every other robot. That is a benefit (§4.2) and a hazard, and it is the main reason apply is explicit (§8).

## 12. What this does not do

Stated so it is found rather than discovered.

- **We do not become a registry, a cache or a proxy.** [00 §3](00-overview.md#3-non-goals) stands as written: no artifact bytes are ever served by us, and no transfer is ever a side effect of somebody's pull. What changes is that we can now *configure and observe* a registry that does those things — a distinction worth keeping precise, since the non-goal is about our data path and this is about our control plane.
- **No mirroring for ACR or Artifactory.** Both have comparable native features; neither is in scope here, and the `Strategy` seam is what would make adding one cheap. `mode` is validated against `type` for exactly this reason.
- **No Quay geo-replication.** It is a storage-layer property of a single Quay deployment, configured by whoever runs Quay, and there is nothing for us to declare per product.
- **No mirror-of-a-mirror chains.** `mirror.from` names a target we manage; a chain of three delegated hops has no observable failure semantics worth shipping.
- **No writing to Quay from the Coordinator's reload path.** §8.

## 13. Delivery and open questions

Scheduled at [M8](17-delivery-plan.md#m8--quay-replication-strategies), after the four registry implementations exist at M4 and the audit and notification machinery exists at M6.

| # | Question | Decide by |
|---|---|---|
| Q7 | **Is a mirror sync observable enough to report honestly?** Specifically: does `GET …/mirror` distinguish "never ran" from "running" from "failed" reliably enough to back §6.2, or must we depend on Quay's notifications? | M8 entry — this is the exit criterion in the §4.4 decision |
| Q8 | Should `manage: auto` exist, and if so under what guard? Depends on how often observed drift turns out to be legitimate | M8 exit, from evidence |
| Q9 | Does `warm` belong in the worker plane (it moves real bytes and should obey the concurrency budget) or is it a Coordinator-side operation? | M8 design |
| Q10 | Can a mirror's tag glob be *generated* from an `autoDownload` rule's RE2 pattern for the subset of patterns that translate faithfully, or does the dialect gap make that a trap? | M8, and the default answer is no (§5.3) |

**Acceptance for M8:**

- A `mode: mirror` target is applied from configuration, syncs on request, and its transfer reaches `succeeded` with the destination digest matching the discovered one — and reaches `diverged`, not `succeeded`, when the upstream tag has moved.
- A tag glob that excludes signatures is caught by `config validate` before it is applied (§9).
- A mirror configured by hand in the Quay UI shows as drift within one reload, and `targets apply` closes it; no reload ever closes it by itself.
- A `mode: proxy` target refuses `download` with a problem detail naming `warm`, and `warm` populates the cache and reports the bytes it discarded.
- Byte columns for a mirror transfer render `—`. There is no synthesised percentage anywhere in the output (§6.1).

---

## References

Red Hat Quay product documentation, read while writing this document. Version-specific behaviour was taken from 3.15/3.16 unless noted.

- [Repository mirroring — Manage Red Hat Quay 3.15](https://docs.redhat.com/en/documentation/red_hat_quay/3.15/html/manage_red_hat_quay/repo-mirroring-in-red-hat-quay) — mechanism, tag-glob syntax, worker deployment, mirroring vs geo-replication, limitations, event notifications
- [Repository mirroring — Manage Red Hat Quay 3.16](https://docs.redhat.com/en/documentation/red_hat_quay/3.16/html/manage_red_hat_quay/repo-mirroring-in-red-hat-quay) — `FEATURE_REPO_MIRROR`, `REPO_MIRROR_INTERVAL`, `REPO_MIRROR_TLS_VERIFY`, `REPO_MIRROR_ROLLBACK`, `REPO_MIRROR_SERVER_HOSTNAME`, repository states
- [mirror — Red Hat Quay API reference 3.14](https://docs.redhat.com/en/documentation/red_hat_quay/3.14/html/red_hat_quay_api_reference/mirror) — `/mirror`, `/mirror/sync-now`, `/mirror/sync-cancel`, request bodies, `repo:admin` scope
- [Red Hat Quay as a proxy cache for upstream registries — Use Red Hat Quay 3.16](https://docs.redhat.com/en/documentation/red_hat_quay/3.16/html/use_red_hat_quay/quay-as-cache-proxy) — cache hit/miss behaviour, `expiration_s`, staleness, quota and LRU auto-pruning, limitations
- [Red Hat Quay API guide 3.15](https://docs.redhat.com/en/documentation/red_hat_quay/3.15/html-single/red_hat_quay_api_guide/index) — `/organization/{org}/proxycache`, `validateproxycache`, `changestate`, `FEATURE_PROXY_CACHE`
- [Manage Project Quay](https://docs.projectquay.io/manage_quay.html) — `ROBOTS_DISALLOW` and its effect on mirroring
