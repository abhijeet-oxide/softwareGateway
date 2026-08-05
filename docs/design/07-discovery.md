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

Idempotent and safe: it is the same scan the loop runs. Concurrent triggers are collapsed — a scan already running for that repository returns the in-progress operation rather than starting a second one.
