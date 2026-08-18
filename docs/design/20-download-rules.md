# 20 — Download Rules

> **Prerequisites:** [07 — Discovery](07-discovery.md), [05 — Transfer Engine](05-transfer-engine.md), [18 — Quay Replication](18-quay-replication.md)
> **Status: IMPLEMENTED at [M9](17-delivery-plan.md#m9--download-rules)**, except the window and the scheduled trigger. `autoDownload.rules` remains its older spelling and keeps working unchanged.
>
> | Section | State |
> |---|---|
> | §2 A rule produces a TransferRequest; no new aggregate | **Held.** Nothing new was added to the domain |
> | §3 The `download` block, its fields, the `autoDownload` alias, validation | **Built.** `internal/product/download.go`, `validate_download.go` |
> | §3.5 Naming the tail names the chain; a shared hop is one transfer | **Built.** `internal/download/chain.go`; the key covers the derived chain |
> | §4 Order derived from `mirror.from`, never declared in the rule | **Built** |
> | §5 Verification as a gate, needing no gate mechanism | **Built.** A step waits for `succeeded`, which the transfer machine only reaches after verification |
> | §6 `step_index`, `depends_on_transfer_id`, `waiting`, `skipped` | **Built.** Migration `00018` |
> | §7 A run's rollup and per-step rendering | **Built** for the delegated asymmetry ([18](18-quay-replication.md) §6.1); the combined run view is the UI's job at [M10](17-delivery-plan.md#m10--web-ui) |
> | §8.1 `trigger: [discovery, manual]`, one path through | **Built.** Discovery and a person share `Resolve` and `Open` |
> | §8.2 The window | **Schema and semantics built; the scheduler binding is not.** A rule may declare one and it validates, but a run is not yet deferred to the next opening — Q12 |
> | §8.3 Rule revisions in the idempotency key | **Built** |
> | §9 Suspension as an audited override that never edits Git | **Built.** `download_rule_suspensions` |
> | §10 `downloadRules` routes, `rules` commands, the `Download` audit category | **Built.** The metrics are not |

---

## 1. What this is for

`promote` is the shape to copy. It is not a new subsystem: a promotion is a transfer whose origin is a target rather than a source ([01](01-domain-model.md) §3.4), and the state machine is the transfer machine unchanged ([10](10-state-machines.md) §3). One word names an intent an operator already has, and underneath it is the engine that already exists.

**Download has the verb and not the noun.** `transferctl download <tag>` exists, and it means "make one package present at one or more targets, right now, with these flags". What does not exist is the *declared* form: a named, reviewable, reusable statement of what a release must go through before anyone can deploy it. Today that statement is spread across four places — the `autoDownload` rule that picks the tag, the target's `verification` block, the promotion block, and whatever the operator remembers to type — and nothing renders it as one thing.

The estate this is written for is three hops ([18](18-quay-replication.md) §1):

```
   vendor NEAR registry  ──copy──►  JFrog Artifactory  ──mirror──►  Quay on OCP  ──►  pods
        discovery                    storage of record                what runs
```

Getting a release from the left to the right is **one operation with several steps and two gates**, and today it is two or three commands with a human holding the ordering in their head. That human is the part of the system with no audit trail, no retry and no dry run.

So: a **download rule** is that operation, declared once.

| Today | With rules |
|---|---|
| `autoDownload` rule fires → one TransferRequest → N independent Transfers, all from the source, all at once | one TransferRequest whose Transfers are **ordered by the chain the targets already declare** |
| Verification is a product/target property; whether it gates anything is implicit | `verify.before` / `verify.after` with a policy, and `enforce` means the next step does not start |
| Rules fire on discovery or not at all | `trigger: [discovery, manual]`, runnable from the CLI or the UI, with a dry run |
| A rule is turned off by editing Git and waiting for Flux | still true for *intent* — plus a suspension you can apply in ten seconds during an incident (§9) |
| Quay mirroring is configured by a separate `targets apply` ([18](18-quay-replication.md) §8) | the mirror step is a step of the download, because to the person asking for the release it always was |

## 2. Decision: a download rule adds no new top-level entity

> **Decision — a rule produces a TransferRequest. There is no `Download` object, no run table, no second scheduler.**
>
> *Alternative considered:* a first-class `Download` aggregate with its own lifecycle, its own persistence and its own state machine, owning Transfers the way a Transfer owns Jobs. It is the obvious shape, and it is what "download orchestration" sounds like it needs.
>
> *Rejected because* every property it would need already exists one level down. A TransferRequest already fans out to one Transfer per target, already has a rollup terminal state, already carries an idempotency key, already appears in the API, the CLI and the audit trail ([10](10-state-machines.md) §7). A parallel aggregate would duplicate all of it and then have to be kept consistent with it — and the first divergence would be a download that says `completed` over a transfer that says `failed`.
>
> *Chosen:* the rule is **configuration**, the run is a **TransferRequest**, the steps are **Transfers**, and the gates are the **verification the transfer machine already performs**. What is genuinely new is small enough to list in five lines (§10). The user's own description of this work — *"it is still an internal wrapper of the existing transfer, with more features"* — is the design, not a simplification of it.
>
> *What we lose:* a place to hang download-specific fields later. Accepted, because the alternative buys that place by paying for a second lifecycle now.

Everything below is written against that decision. Where a section looks suspiciously short, it is because the machinery is already in [04](04-queue-and-scheduling.md), [08](08-verification.md) and [10](10-state-machines.md) and this document only has to say which of it applies.

## 3. The rule

### 3.1 The block

```yaml
download:
  # Master switch for AUTOMATIC firing. `false` stops every rule from being
  # triggered by discovery and leaves all of them runnable by hand — which is
  # exactly what you want during an incident, and exactly what
  # `autoDownload.enabled: false` means today.
  enabled: true

  rules:
    # Evaluated in configured order, FIRST MATCH WINS — unchanged from today
    # (02 §5.4). Two rules matching one tag with different priorities and
    # different destinations has no sensible reading, and "most specific"
    # would need a specificity order over regexes that does not exist.
    - name: ga-releases
      enabled: true

      # ── WHAT IT MATCHES ────────────────────────────────────────────
      tagPattern: '^v\d+\.\d+\.\d+$'    # RE2, as everywhere else (02 §5.4)
      # Optional. Absent means every source in the product. Naming sources is
      # how one product carries a vendor's GA channel and its early-access
      # channel without two rules that differ only by a tag pattern nobody
      # can read.
      sources: [near, components]

      # ── WHERE IT PUTS IT ───────────────────────────────────────────
      # A SET, not a sequence, and it names the destinations you care about
      # — not every hop. `ocp-prod` mirrors from `jfrog-store`, so naming it
      # plans both steps (§3.5). The ORDER is derived from the targets' own
      # configuration (§4); declaring it here would be the same chain written
      # twice, and the day the two disagreed one would be silently wrong.
      targets: [ocp-prod]

      # ── WHEN IT RUNS ───────────────────────────────────────────────
      trigger: [discovery, manual]      # default [discovery]
      # Optional (§8.2). Outside the window the request is created with
      # `scheduleAt` set to the next opening, using the scheduling that
      # already exists (04 §10) rather than anything new.
      window: {start: "02:00", end: "06:00", timeZone: "Europe/London"}

      # ── HOW CAREFULLY ──────────────────────────────────────────────
      priority: 100                     # 0–1000, defaults to 50, as today
      verify:
        before: true                    # source-side, before any bytes move
        after: true                     # destination-side, after they land
        policy: enforce                 # enforce | warn — see §5

    - name: release-candidates
      tagPattern: '^v\d+\.\d+\.\d+-rc\.\d+$'
      targets: [jfrog-store]            # RCs reach storage, never the cluster
      priority: 10
      verify: {before: true, after: true, policy: warn}

    # Nothing is downloaded automatically; someone asks. The rule still exists
    # so that when they do, it goes through the same chain and the same gates
    # as everything else.
    - name: hotfix
      tagPattern: '^v\d+\.\d+\.\d+\+hotfix\.\d+$'
      targets: [ocp-prod]
      trigger: [manual]
      priority: 100
      verify: {before: true, after: true, policy: enforce}
```

### 3.2 Field reference

| Field | Type | Required | Default | Notes |
|---|---|---|---|---|
| `download.enabled` | bool | no | `true` | Automatic firing only. Manual runs are unaffected — a master switch that also disabled the recovery path would be a switch nobody dares use |
| `rules[].name` | string | yes | — | Unique within the product. Appears in the API path, the metric label and every audit record, so it is immutable in practice |
| `rules[].enabled` | bool | no | `true` | The *configured* intent. See §9 for the operational override, which is a different thing on purpose |
| `rules[].tagPattern` | RE2 | yes | — | Unchanged |
| `rules[].sources` | []string | no | all | Names sources in this product |
| `rules[].targets` | []string | no | default target | Destinations, not hops: the set is closed over `mirror.from` (§3.5) and then ordered (§4). A document that named every hop already keeps working, because naming them is a no-op |
| `rules[].trigger` | []enum | no | `[discovery]` | `discovery`, `manual`. `[]` is rejected — a rule that can never run is a typo, not a configuration |
| `rules[].window` | object | no | — | `start`, `end` (`HH:MM`), `timeZone` (IANA). §8.2 |
| `rules[].priority` | int | no | `50` | 0–1000 ([04](04-queue-and-scheduling.md) §6). Unchanged |
| `rules[].verify.before` | bool | no | inherits | Source-side ([08](08-verification.md) §4). `verifyBeforeTransfer` is the older spelling and still works |
| `rules[].verify.after` | bool | no | inherits | Destination-side, per destination |
| `rules[].verify.policy` | enum | no | inherits | `enforce` \| `warn`. Overrides the product's for this rule's runs only |

"Inherits" means the product's `verification` block, then the target's override, exactly as today ([02](02-configuration.md) §5). A rule does not introduce a third trust configuration; it selects **whether the existing one gates this chain**.

### 3.3 `autoDownload` still loads, and still means what it means

The block is renamed because the old name became false the moment a rule could be triggered by hand. That is not a reason to break every document in the estate.

- `autoDownload:` is accepted as a **deprecated alias** for `download:`. A document using it loads, validates and behaves identically to today.
- Declaring **both** is a validation error, not a merge. Two blocks that mean the same thing, resolved by precedence, is a configuration nobody can read out loud.
- `verifyBeforeTransfer: true` on a rule is accepted as `verify: {before: true}`.
- `transferctl config validate` prints one deprecation line per document, naming the file and the replacement. It does not fail; a rename is not a reason for red CI.

The compatibility contract, stated so it can be tested: **every product document valid before M9 is valid after M9 and produces the same transfers.** The reference product ([`dev/products/reference.yaml`](../../dev/products/reference.yaml)) gains the new spelling at M9 and not before — the loader uses `yaml.UnmarshalStrict`, so a field documented here and absent from the schema would break `task validate` on the fixture that exists to catch exactly that.

### 3.4 Validation

Beyond the field rules, the combinations that must fail at load rather than at 3 a.m.:

| Rejected | Why |
|---|---|
| `targets` naming a `promotionOnly` target | Already rejected today; unchanged. Production is reachable by promotion, not by rule |
| `targets` naming a disabled target | Already rejected today; unchanged — it would fail the first time a package matched |
| `targets` naming a `replication.mode: proxy` target | A cache cannot be pushed to ([18](18-quay-replication.md) §5.4). The error names `warm` |
| `targets` whose derived chain contains a cycle | `mirror.from` edges must form a forest (§4) |
| `targets` whose closure reaches a disabled or `promotionOnly` target | The chain is planned in full (§3.5), so a hop it cannot use is as fatal as a destination it cannot use — and the error names the hop and the target that pulled it in |
| `trigger: []`, or `trigger: [manual]` on a rule that nothing else can reach | A rule that can never run |
| `verify.after: true` on a chain whose mirror glob excludes `sha256-*.sig` | **The important one.** Destination verification of a mirrored tag is impossible if the signatures were never mirrored ([18](18-quay-replication.md) §9). The configuration looks correct, the sync succeeds, and verification fails for every package forever |
| `window` whose `start` equals `end` | A zero-length window never opens |
| `sources` naming a source that does not exist or is disabled | |
| Both `download` and `autoDownload` | §3.3 |

The signature-glob check is the reason this document and [18](18-quay-replication.md) have to be validated together rather than separately: neither block is wrong on its own.

### 3.5 Naming the end of a chain names the chain

> **Decision — a rule names the destinations it cares about. Hops those destinations depend on are pulled in automatically.**
>
> `targets: [ocp-prod]`, where `ocp-prod` has `mirror.from: jfrog-store`, plans **both** steps. It is identical to writing `targets: [jfrog-store, ocp-prod]`.
>
> *Alternative considered:* require every hop to be named, and warn when one is missing. It has the virtue that a rule writes to nothing it did not mention.
>
> *Rejected because* it makes every rule restate the chain that [18](18-quay-replication.md) §5.1 exists to declare once, and it asks the person writing the rule to know that Quay pulls rather than gets pushed to — which is exactly the knowledge this document is trying to keep out of the rule. The person adding a target configures *how content gets into it*. The person writing a rule says *what goes where*. Requiring the second to encode the first collapses the split.
>
> *And the objection does not survive contact:* `mirror.from` may only name a target in the same product ([18](18-quay-replication.md) §5.2), so the hop was already declared, by the target the rule did name. Nothing arrives from outside the document.
>
> *Chosen:* the transitive closure over `mirror.from`, with the **full chain rendered wherever the rule is shown** — `rules describe`, `rules run --dry-run`, and the rule row in the UI. Implicit to type, never implicit to read.
>
> *What we lose:* the ability to say "sync Quay from whatever is already in JFrog, and do not touch JFrog". That is `transferctl targets sync` ([18](18-quay-replication.md) §7), which is the right command for it and already exists.

Two consequences that need stating, because they are where this gets expensive if ignored:

**A shared hop is transferred once, not once per rule.** Two rules whose chains both pass through `jfrog-store` must not produce two transfers of the same package to the same target. The step's identity is therefore `(package.digest, target)` and not the rule — a step whose predecessor work is already `succeeded` for that pair is satisfied by it, and a step already in flight is **joined**, not duplicated. This is the one place where the derived idempotency key (§8.3) intentionally drops `rule.name`: the request is per rule, the step is per destination, and a byte moved twice because two rules wanted it is a byte moved twice.

**The precondition check stays.** A step still asks, before requesting a sync, whether the package is actually present at `mirror.from`. With the closure in place this should be unreachable — its predecessor just succeeded — which is precisely why it is worth keeping: if it ever fires, something upstream lied, and the message names the empty target rather than leaving a sync to fail obscurely inside Quay.

## 4. The chain is derived, not declared

> **Decision — a rule declares a set of destinations. The order between them comes from the targets' own `replication` configuration.**
>
> *Alternative considered:* an ordered `steps:` list in the rule, each naming a target and where it takes content from. It is explicit, it reads well, and it is what a workflow engine would offer.
>
> *Rejected because* the edge is already declared. [18](18-quay-replication.md) §5.1 puts `mirror.from: jfrog-store` on the Quay target precisely so "the chain jfrog-store → ocp-prod is declared once and stays consistent when the JFrog path changes". A `steps:` list would be that same edge written a second time, in a second file section, by a second person — and when the two disagreed, one of them would be silently authoritative.
>
> *Chosen:* the planner topologically sorts the rule's targets over the `mirror.from` edges. Because `mirror.from` names exactly one target, the graph is a **forest**, not a general DAG: every step has at most one predecessor, cycles are the only structural error possible, and the sort is six lines.
>
> *What we lose:* the ability to express an ordering that is not a content dependency — "put it in DR only after production succeeded", where DR does not pull from production. Accepted, and deliberately: that is a workflow, and §12 says why we are not building one.

Worked against the topology this exists for:

```
targets:
  - name: jfrog-store            replication.mode: copy
  - name: ocp-prod               replication.mode: mirror, mirror.from: jfrog-store
  - name: dr-store               replication.mode: copy

rule ga-releases → targets: [ocp-prod, dr-store]
                   (jfrog-store is pulled in by ocp-prod's mirror.from — §3.5;
                    naming it explicitly changes nothing)

derived:

  step 0   near ──copy──► jfrog-store      ┐ no predecessor: run together
  step 0   near ──copy──► dr-store         ┘
             │
             │ jfrog-store succeeded (which includes its verification gate)
             ▼
  step 1   jfrog-store ──mirror──► ocp-prod
```

Two properties worth naming:

- **Fan-out and chaining are the same feature.** Independent destinations have no edge and therefore share a step index; dependent ones do not. Nothing in the rule distinguishes them, and nothing needs to.
- **Fan-out costs vendor egress twice.** `jfrog-store` and `dr-store` each pull the full package from the vendor. That is sometimes right and often not — `dr-store` could mirror from `jfrog-store` instead. `config validate` says so as an advisory when two copy destinations share a source and one is a Quay target; it does not refuse, because the redundancy is occasionally the point.

## 5. The gates are the verification that already exists

This is the part that looked like new machinery and is not.

A Transfer already ends with `running → verifying → succeeded | failed` ([10](10-state-machines.md) §3). Destination verification is already what happens in `verifying`. And a step waits for its predecessor to reach `succeeded`.

Therefore, with no gate mechanism at all:

| `verify.policy` | Verification fails at `jfrog-store` | Consequence for `ocp-prod` |
|---|---|---|
| `enforce` | step 0 → `failed` | step 0 never reached `succeeded`, so step 1 is **skipped** (§6) |
| `warn` | recorded, notified, step 0 → `succeeded` ([08](08-verification.md) §4) | step 1 runs |

**"Do not configure the Quay mirror if what landed in JFrog did not verify" is the whole security value of the chain**, and it costs one word in the configuration. Under `enforce`, the cluster's registry is never pointed at content whose signature did not check — not by policy, not by convention, but because the step that would have pointed it there did not run.

Two things about the far end of the chain that are easy to get backwards:

- **Verifying a mirrored target verifies what OCP will actually run.** For a `copy` destination we verify bytes we pushed; for a `mirror` destination we verify bytes *Quay* pulled, through Quay's own network path, under Quay's own tag glob. It is the stronger check, and it is the one that catches the glob that silently excluded the signatures.
- **It is only possible if the signatures travelled.** `verification.transferSignatures` must be true, and the mirror's `tags` glob must include `sha256-*.sig` ([18](18-quay-replication.md) §9). §3.4 rejects the combination that cannot work.

## 6. Ordering is waves, one level up

The system already orders work with a single integer: jobs carry a wave, wave *n+1* is `blocked` until wave *n* drains ([04](04-queue-and-scheduling.md) §3.2). A rule's steps need exactly the same thing at the transfer level, so they get exactly the same thing rather than something new.

`transfers` gains two columns:

| Column | Meaning |
|---|---|
| `step_index` | The derived wave within the request. Steps sharing an index run concurrently |
| `depends_on_transfer_id` | The predecessor, or `NULL`. At most one, because `mirror.from` names one target (§4) |

And the transfer machine ([10](10-state-machines.md) §3) gains two states:

```
   ┌─────────┐  predecessor succeeded   ┌─────────┐
   │ waiting ├─────────────────────────►│ pending │──► planning ──► … ──► succeeded
   └────┬────┘                          └─────────┘
        │ predecessor failed, was cancelled, or was itself skipped
        ▼
   ┌─────────┐
   │ skipped │   terminal
   └─────────┘
```

| From | Event | To | Notes |
|---|---|---|---|
| — | `Created` | `waiting` | Only when `depends_on_transfer_id` is set; otherwise `pending`, as today |
| `waiting` | `PredecessorSucceeded` | `pending` | Includes the predecessor's verification gate (§5) |
| `waiting` | `PredecessorSettledUnsuccessfully` | `skipped` | Terminal |
| `waiting` | `CancelRequested` | `cancelled` | |

**`skipped` is a distinct terminal state and not a flavour of `failed`.** The Quay mirror step of a run whose JFrog step failed did not fail — it never started, nothing was attempted against Quay, and no operator should go looking at Quay for the cause. Collapsing the two would make every chained failure report two problems where there is one, and the second report would point at the wrong system. It also keeps `mirror_sync_total{result="failure"}` ([18](18-quay-replication.md) §7) honest: a sync that never ran is not a sync that failed.

Invariant, joining [10](10-state-machines.md) §8:

| # | Invariant | Enforced |
|---|---|---|
| S7 | A transfer cannot leave `waiting` while its predecessor is non-terminal | The transition table above; `depends_on_transfer_id` is `NULL` or references a transfer in the same request |

## 7. What a run reports

A request's terminal state is the rollup of its transfers ([10](10-state-machines.md) §7), and that stays true. What is new is that the steps are no longer interchangeable, so the rollup has to be readable:

| All steps | Request | Notes |
|---|---|---|
| `succeeded` | `completed` | |
| `succeeded`, at least one `diverged` | `completed` | Divergence is a fact, not a failure ([18](18-quay-replication.md) §6.2). Recorded on the step, surfaced in output, notifiable |
| any `failed` | `failed` | Retryable — see §8.3 |
| any `skipped`, none `failed` | impossible | A step is only skipped because an earlier one did not succeed |
| any `cancelled` | `cancelled` | |

### 7.1 Progress across steps is not one number

A run whose first step moves 45 GB and whose second step is a Quay sync has two kinds of truth in it, and there is no arithmetic that combines them. [18](18-quay-replication.md) §6.1 forbids synthesising bytes for a delegated step; the same rule at the run level forbids synthesising a percentage across steps whose units differ.

So a run renders as **steps, each with its own kind of progress**:

```
$ transferctl transfers describe req_8f2c…

  ga-releases · SBC v3.2.1 · near/orbs/sbc-8000  →  2 destinations
  triggered by  discovery        started  14:02:11Z        elapsed  22m
  ─────────────────────────────────────────────────────────────────────
  1  ✔  jfrog-store    copy      12.4 GB / 12.4 GB   428 MB/s   done 14:24
        verified at destination  ✔  cosign, 6 artifacts
  2  ●  ocp-prod       mirror    configured 14:24 · syncing since 14:24
        delegated to Quay mirror — no byte progress available
  ─────────────────────────────────────────────────────────────────────
```

There is no total, no combined bar and no ETA for the run. The first step has all three because we measured them; the second has none because Quay did not tell us. A reader who wants "how far along is this" gets the step list, which is the honest answer to a question that has no single number in it.

This is also the shape [19](19-user-interface.md) renders as a stepper, and the reason the stepper has per-step content rather than one progress bar across the top.

## 8. Triggering

### 8.1 Three ways in, one path through

| Trigger | Origin | Created by |
|---|---|---|
| `discovery` | `auto_download` (unchanged) | Discovery evaluates rules against each new package ([07](07-discovery.md) §5) |
| `manual`, CLI | `manual` | `transferctl rules run <product> <rule> [tag…]` |
| `manual`, UI | `manual` | `POST …/downloadRules/{rule}:run` — the same endpoint the CLI calls |

All three create the same TransferRequest with the same derived chain and the same gates. There is no path that skips a step because it was asked for by a person, which is the property that makes the audit trail worth reading.

`transferctl download <tag>` keeps working exactly as it does now — an ad-hoc replication with flags, no rule, no chain beyond what `mirror.from` implies. It gains `--rule <name>` to run one package through a named rule's chain and gates instead.

### 8.2 The window

The one field here that nobody asked for, included because it costs nothing structurally and because a rule that fires on discovery at 14:00 and saturates a vendor link for two hours is a real way to be paged.

Outside the window, the request is created with `scheduleAt` set to the next opening and enters `scheduled` — the mechanism that already exists ([04](04-queue-and-scheduling.md) §10), not a second scheduler. Inside it, nothing changes.

Two details that would otherwise be discovered in October: the window is evaluated in its named IANA zone, and a window that a DST jump skips entirely opens at the transition instead of being missed for a year. A manual run **ignores the window** — someone typing the command at 15:00 has already decided.

If §14's Q12 concludes nobody uses it, this field is the one thing in this document that can be deleted without touching anything else.

### 8.3 Re-running, and what idempotency keys on

The derived idempotency key ([04](04-queue-and-scheduling.md) §7) is what makes rule evaluation safe in a loop, and it must now cover more:

```
key = hash(product, package.digest, sorted(targets), rule.name, rule.revision)
```

`sorted(targets)` is the **derived** set, after the closure in §3.5 — so a rule naming `[ocp-prod]` and one naming `[jfrog-store, ocp-prod]` key identically, because they are the same work. And the key above identifies the *request*; a **step** is identified by `(package.digest, target)` regardless of which rule asked for it, which is what stops two rules sharing a JFrog hop from transferring it twice (§3.5).

`rule.revision` is the hash of the rule's own resolved fields. It is there so that **editing a rule creates a new run rather than being swallowed by the old one's key** — the failure otherwise is a person tightening `verify.policy` from `warn` to `enforce`, re-running, getting `already exists`, and concluding the stricter policy was applied when nothing ran at all.

A retry is not a re-run. `transfers retry` resumes the *existing* request: failed steps reset, `skipped` steps return to `waiting`, and steps that already succeeded are not repeated. That matters beyond efficiency — deduplication would make a repeat nearly free ([05](05-transfer-engine.md) §4.1), but the audit trail should show one transfer to JFrog because there was one.

## 9. Suspending a rule is an operation. Disabling it is a commit.

> **Decision — `rules[].enabled` stays in Git. A separate, audited, database-backed *suspension* stops a rule immediately without editing configuration.**
>
> *Alternative considered:* let the API flip `enabled`. It is the first thing anyone asks for, and [19](19-user-interface.md) §4 has already committed to the UI showing a toggle.
>
> *Rejected because* configuration is GitOps ([02](02-configuration.md) §2) and a UI or API that wrote it would create a second source of truth that Flux reverts — a change that works for five minutes and then undoes itself, which is the worst failure mode this system has. [18](18-quay-replication.md) §8 rejected the same shape for the same reason.
>
> *Also rejected:* leaving it to Git alone. At 02:00, with a vendor pushing broken images and a rule downloading each one, "open a PR and wait for Flux" is not an answer. The safe path has to be the fast path or people will find another one.
>
> *Chosen:* a suspension is an **operational override**, recorded in `download_rule_suspensions` with an actor, a required reason and an optional `until`. While suspended the rule matches nothing and can be run by nobody. It is reported everywhere — `rules list`, `products check`, a `download_rule_suspended` gauge, an audit event on both edges — and reported as an **override of what Git says**, in the same voice as replication drift ([18](18-quay-replication.md) §8). It never edits configuration, so Flux has nothing to revert and nothing to fight.
>
> *What we lose:* one more piece of state that is not in Git. Mitigated by making it loud rather than by making it impossible: an indefinite suspension is a standing complaint in `products check`, not a quiet fact in a table.

The UI toggle from [19](19-user-interface.md) §4 therefore writes a suspension, and reads back as *"Suspended by alice@example.com — configuration says enabled"*. Both facts are true and the interface shows both.

## 10. Where the code goes

Genuinely new, in full:

| Package / object | Holds |
|---|---|
| `internal/download` | Rule evaluation, chain derivation, the topological sort, the window. Depends on `internal/product` and nothing below the API |
| `internal/discovery` | Loses rule evaluation to `internal/download` and calls it instead. `rules.go` first-match-wins moves unchanged |
| `download_rule_suspensions` | `product`, `rule`, `reason`, `actor`, `suspended_at`, `until`, `released_at`, `released_by` |
| `transfer_requests` + | `rule_name`, `rule_revision`, `trigger` |
| `transfers` + | `step_index`, `depends_on_transfer_id` |

API ([09](09-api.md) §2). Rules are configuration, so the collection is read-only; the verbs operate:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products/{p}/downloadRules` | Rules, resolved chains, suspension state, last run |
| `GET` | `/api/v1/products/{p}/downloadRules/{rule}` | One, with the derived step order and the gates it applies |
| `POST` | `/api/v1/products/{p}/downloadRules/{rule}:run` | Trigger. `validateOnly=true` renders the plan and moves nothing |
| `POST` | `/api/v1/products/{p}/downloadRules/{rule}:suspend` | Requires a reason |
| `POST` | `/api/v1/products/{p}/downloadRules/{rule}:resume` | |

There is deliberately **no** `/downloadRules/{rule}/runs`. The runs are transfer requests, and `GET /transfers?filter=rule="ga-releases"` already returns them ([09](09-api.md) §3). A second listing of the same rows, paginated separately and filtered differently, is how two answers to one question get into a product.

CLI ([13](13-cli.md) §2) — a noun group for the thing you look at, verbs for the things the tool does:

```
├── rules
│   ├── list [product]                   Rules, their chains, and what is suspended
│   ├── describe <product> <rule>        The derived step order and the gates
│   ├── run     <product> <rule> [tag…]  Trigger now (--dry-run shows the plan)
│   ├── suspend <product> <rule> --reason <text> [--until <duration>]
│   └── resume  <product> <rule>
│
└── download <tag> [--rule <name>]       Ad-hoc, or through a named rule's chain
```

Metrics ([12](12-observability-and-audit.md) §2), following the existing naming:

```
softwaregateway_download_rule_matches_total{product,rule}
softwaregateway_download_run_total{product,rule,trigger,result}
softwaregateway_download_step_skipped_total{product,rule,target}
softwaregateway_download_rule_suspended{product,rule}            gauge, 0 or 1
```

Audit ([12](12-observability-and-audit.md) §4.1) gains a `Download` category: `DownloadRunRequested`, `DownloadRunCompleted`, `DownloadStepSkipped`, `DownloadRuleSuspended`, `DownloadRuleResumed`. A *match* is not audited — it happens for every package on every scan, and burying five real events under thousands of routine ones is how an audit trail stops being read. The match is a metric.

## 11. Failure modes

| Failure | Behaviour |
|---|---|
| Step 0 fails | Dependent steps → `skipped`; independent steps continue. The run is `failed` and retryable from where it stopped |
| Destination verification fails, `enforce` | Step `failed`, dependents `skipped`, `VerificationFailed` notification. **Quay is never configured** |
| Destination verification fails, `warn` | Recorded and notified; the chain proceeds ([08](08-verification.md) §4) |
| `mirror.from` target is empty at run time | Step fails its precondition before touching Quay, naming the empty target (§3.5) |
| Mirror sync reports success, digest differs | Step `diverged`, run `completed`, `MirrorContentDiverged` ([18](18-quay-replication.md) §6.2) |
| Rule edited while a run is in flight | The run completes under the revision it started with. `rule_revision` on the request is what makes that statement checkable a year later |
| Rule deleted while a run is in flight | The run completes. Deletion of running work is never a side effect of a config edit ([02](02-configuration.md) §6) |
| Rule suspended while a run is in flight | The run completes; no new runs start. Cancelling is a separate, explicit act |
| Coordinator restarts between steps | Nothing to recover. A `waiting` transfer whose predecessor is `succeeded` is advanced by the same sweep that settles transfers ([10](10-state-machines.md) §3) |
| Two Coordinators evaluate one package | One request, by the derived key (§8.3) |

## 12. What this is not

> **Decision — this is not a workflow engine, and the boundary is the `mirror.from` edge.**
>
> "Download rules" is the exact feature that becomes a general orchestrator if nobody says no: conditionals, parallel branches, manual approval steps, retries with custom policy, a step that runs a script. Each is a reasonable request and each arrives after the previous one shipped.
>
> The line is drawn at a place that can be checked rather than argued: **the only ordering primitive is a content dependency that the target configuration already declares.** If two steps have no `mirror.from` edge between them, they are concurrent, and there is no syntax to say otherwise. A rule has no conditionals, no branches, no user-defined steps and no scripting.
>
> *If a real case needs more*, the answer is a second rule and something outside this system triggering it — not a DSL inside a ConfigMap.

Also out of scope, permanently:

- **Not a promotion replacement.** Promotion remains its own verb with its own guard rails, and `promotionOnly` targets remain unreachable by rule ([02](02-configuration.md) §5.2). Downloading brings content in; promoting moves it forward. A rule that could push to production would erase that distinction on the day someone mistyped a target name.
- **Not a scheduler.** The window reuses `scheduleAt` (§8.2). Recurring schedules — "download every Tuesday whether anything is new or not" — are Q13.
- **Not a policy engine.** `verify.policy` selects between two behaviours the verification subsystem already has. It does not grow expressions.

## 13. Security

- **A manual run is an authenticated action with an actor**, and the `actor` field already exists and already writes `"anonymous"` ([12](12-observability-and-audit.md) §4.2). Until [09](09-api.md) §10 lands, "who ran this rule" has the same answer as every other question about who did anything — which is [17](17-delivery-plan.md) Q6, and one more reason it is a gate.
- **A suspension is a privileged operation** in the role model that arrives with authentication ([19](19-user-interface.md) G2): it stops content reaching the cluster, and that is exactly as consequential as starting it.
- **A rule cannot widen trust.** `verify.policy` may tighten `warn` to `enforce`; the reverse is permitted but audited, and `products check` reports every rule whose policy is weaker than its product's. Verification keys and identities come from the product and the target, never from a rule — a trust configuration assembled from three places is one nobody can audit ([02](02-configuration.md) §5).
- **Rule names appear in metric labels**, so they are bounded by the same character rules as product names.

## 14. Delivery

**M9**, after M8. Acceptance criteria are in [17](17-delivery-plan.md#m9--download-rules).

It waits for M8 rather than shipping the copy-only half earlier, and that is a choice with a reason: the ordering model, the `skipped` state and the per-step rendering all have to be built twice if they are built first for a chain that has only copy steps in it. The interesting chain is the one with a mirror at the end, and it is the one the estate actually needs.

It waits for M5 because a gate over a verification that does not execute is a gate in name only.

### Open questions

| # | Question | Decide by |
|---|---|---|
| Q11 | Should a suspension **expire by default** — say, 24 hours — rather than persisting until someone remembers? An indefinite override that survives the incident is how a rule stays off for a quarter | M9 design, from how §9's `products check` complaint reads in practice |
| Q12 | Does anyone use `window`? It is the one field here beyond the stated requirement, and it is deletable in isolation (§8.2) | M9 exit |
| Q13 | Do recurring schedules belong here at all, or does a Kubernetes CronJob calling `rules run` cover every real case? The default answer is the CronJob | M10, from whether anyone asks twice |
| Q14 | Is `rule_revision` the hash of the rule, or of the whole product document? The rule is more precise; the document is what the config hash already covers, and two hashing schemes for one concept is a cost | M9 design |

## References

- [07 — Discovery](07-discovery.md) §5 — the rule evaluation this replaces
- [04 — Queue and Scheduling](04-queue-and-scheduling.md) §3.2, §7, §10 — waves, derived idempotency keys, scheduled requests
- [08 — Verification](08-verification.md) §4 — stages, policy, and what `enforce` already does
- [10 — State Machines](10-state-machines.md) §3, §7, §8 — the transfer machine the new states extend
- [18 — Quay Replication](18-quay-replication.md) §5, §6, §8, §9 — `mirror.from`, delegated progress, apply, the signature-tag trap
- [19 — User Interface](19-user-interface.md) §3.1, §4 — the Download Rules page and the vocabulary this document's objects appear under
