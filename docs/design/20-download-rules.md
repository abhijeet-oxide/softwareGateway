# 20 - Downloads and Auto-Download

> **Prerequisites:** [07 - Discovery](07-discovery.md), [05 - Transfer Engine](05-transfer-engine.md), [18 - Quay Replication](18-quay-replication.md)
> **Status: IMPLEMENTED at [M9](17-delivery-plan.md#m9--downloads-and-auto-download).** Rules written in the older inline shape keep working unchanged.
>
> | Section | State |
> |---|---|
> | §2 A download produces a TransferRequest; no new aggregate | **Held.** Nothing new was added to the domain |
> | §3 `spec.download` - what happens, with no pattern in it | **Built.** `internal/product/download.go`, `validate_download.go` |
> | §3.4 `spec.autoDownload` - when it happens by itself | **Built.** Same files; evaluation in `internal/discovery/rules.go` |
> | §3.6 Naming the tail names the chain; a shared hop is one transfer | **Built.** `internal/download/chain.go`; the key covers the derived chain |
> | §4 Order derived from `mirror.from`, never declared | **Built** |
> | §5 Verification as a gate, needing no gate mechanism | **Built.** A step waits for `succeeded`, which the transfer machine only reaches after verification |
> | §6 `step_index`, `depends_on_transfer_id`, `waiting`, `skipped` | **Built.** Migration `00018` |
> | §7 A run's rollup and per-step rendering | **Built** for the delegated asymmetry ([18](18-quay-replication.md) §6.1); the combined run view is the UI's job at [M10](17-delivery-plan.md#m10--web-ui) |
> | §8 Two ways in, one path through; download revisions in the idempotency key | **Built.** A person and a rule share `Resolve` and `Open` |
> | §9 No runtime override. `enabled` in Git is the only switch | **Held.** There is no suspension table, route or command |
> | §10 `downloads` and `autoDownloadRules` routes, the `download` verb, the `Download` audit category | **Built.** The metrics are not |

---

## 1. What this is for

`promote` is the shape to copy. It is not a new subsystem: a promotion is a transfer whose origin is a target rather than a source ([01](01-domain-model.md) §3.4), and the state machine is the transfer machine unchanged ([10](10-state-machines.md) §3). One word names an intent an operator already has, and underneath it is the engine that already exists.

**Download had the verb and not the noun.** `transferctl download <tag>` means "make this software present at the targets that matter, right now". What did not exist is the *declared* form: a named, reviewable statement of where software has to go and what it has to pass on the way. That statement was spread across four places - the `autoDownload` rule that picked the tag, the target's `verification` block, the promotion block, and whatever the operator remembered to type - and nothing rendered it as one thing.

The estate this is written for is three hops ([18](18-quay-replication.md) §1):

```
   vendor NEAR registry  ──copy──►  JFrog Artifactory  ──mirror──►  Quay on OCP  ──►  pods
        discovery                    storage of record                what runs
```

Getting a release from the left to the right is **one operation with several steps and two gates**, and it used to be two or three commands with a human holding the ordering in their head. That human was the part of the system with no audit trail, no retry and no dry run.

### 1.1 Two things, and keeping them apart is the whole design

| | `spec.download` | `spec.autoDownload` |
|---|---|---|
| Answers | **What** happens | **When** it happens by itself |
| Holds | targets, gates, priority | a tag pattern, which sources to watch, which download to fire |
| Pattern | **none** | the only place one belongs |
| Runs | when a person asks, or when a rule fires | never - it triggers a download, it does not perform one |

An auto-download rule does not download anything. It **triggers a download** - the same operation a person performs by hand, with the same targets, the same order and the same gates.

That equivalence is the property worth having, and it has a corollary that is easy to get backwards: **a manual download consults no pattern.** By the time one runs, the software has already been chosen - somebody typed `transferctl download vendor-a v3.2.1`, or a rule matched and named it. A pattern asked at that point could only disagree with the person asking, and the only thing it could do about the disagreement is refuse to download software an operator explicitly named. Nothing is gained and an incident path is lost.

| Before | Now |
|---|---|
| A rule carried the pattern *and* the targets *and* the gates; a download by hand had to go through a rule | The rule carries the pattern. The download carries the work. Either can be read on its own |
| `autoDownload` rule fires → one TransferRequest → N independent Transfers, all from the source, all at once | one TransferRequest whose Transfers are **ordered by the chain the targets already declare** |
| Verification is a product/target property; whether it gates anything is implicit | `verify.before` / `verify.after` with a policy, and `enforce` means the next step does not start |
| Quay mirroring is configured by a separate `targets apply` ([18](18-quay-replication.md) §8) | the mirror step is a step of the download, because to the person asking for the release it always was |

## 2. Decision: a download adds no new top-level entity

> **Decision - a download produces a TransferRequest. There is no `Download` object, no run table, no second scheduler.**
>
> *Alternative considered:* a first-class `Download` aggregate with its own lifecycle, its own persistence and its own state machine, owning Transfers the way a Transfer owns Jobs. It is the obvious shape, and it is what "download orchestration" sounds like it needs.
>
> *Rejected because* every property it would need already exists one level down. A TransferRequest already fans out to one Transfer per target, already has a rollup terminal state, already carries an idempotency key, already appears in the API, the CLI and the audit trail ([10](10-state-machines.md) §7). A parallel aggregate would duplicate all of it and then have to be kept consistent with it - and the first divergence would be a download that says `completed` over a transfer that says `failed`.
>
> *Chosen:* the download is **configuration**, the run is a **TransferRequest**, the steps are **Transfers**, and the gates are the **verification the transfer machine already performs**. What is genuinely new is small enough to list in five lines (§10). The user's own description of this work - *"it is still an internal wrapper of the existing transfer, with more features"* - is the design, not a simplification of it.
>
> *What we lose:* a place to hang download-specific fields later. Accepted, because the alternative buys that place by paying for a second lifecycle now.

Everything below is written against that decision. Where a section looks suspiciously short, it is because the machinery is already in [04](04-queue-and-scheduling.md), [08](08-verification.md) and [10](10-state-machines.md) and this document only has to say which of it applies.

## 3. The configuration

### 3.1 `spec.download` - what happens

```yaml
download:
  - name: internal
    # A SET, not a sequence, and it names the destinations you care about -
    # not every hop. `ocp-prod` mirrors from `lab`, so naming it plans both
    # steps (§3.6). The ORDER is derived from the targets' own configuration
    # (§4); declaring it here would be the same chain written twice, and the
    # day the two disagreed one would be silently wrong.
    targets: [lab, ocp-prod]

    verify:
      before: true                # source-side, before any bytes move
      after: true                 # destination-side, after they land
      policy: enforce             # enforce | warn - see §5

    priority: 100                 # 0–1000, defaults to 50, as everywhere else
    default: true                 # only meaningful once there are two
```

Note what is **not** in that block: no `tagPattern`, no repository filter, no `enabled`. A download is not a thing that fires; it is a thing that is performed. What software goes through it is decided by whoever asks - a person, or a rule.

### 3.2 Decision: `download` is a list, and one entry is the default

> **Decision - `spec.download` is a list. A product declaring one download needs no name and no `default`; a product declaring several must mark exactly one `default: true`.**
>
> *Alternative considered:* a single `download:` object, since the shape the estate actually needs is one download and one promote. It is simpler to read and it is what every product will use.
>
> *Rejected because* the day a product needs a second destination set - a release channel that reaches storage but never the cluster - a single object forces a schema change, and a schema change to a block every product already declares is the expensive kind. The list costs a product with one download exactly nothing: it writes one entry, does not name it, does not mark it default, and never thinks about the list again.
>
> *Chosen:* a list where **one entry is the default by being the only one**. `transferctl download` with no `--download` runs the default; a rule naming no download triggers the default. With several entries, `name` becomes required and exactly one must carry `default: true` - validation rejects zero and rejects two, because "which one does a bare `download` run" must have an answer that does not depend on file order.
>
> *What we lose:* nothing at declaration time. What we gain is that adding the second download later is a commit to one product document rather than a migration.

### 3.3 Download field reference

| Field | Type | Required | Default | Notes |
|---|---|---|---|---|
| `download[].name` | string | only with ≥2 | - | Unique within the product. Appears in the API, the metric label and the audit record |
| `download[].targets` | []string | no | default target | Destinations, not hops: the set is closed over `mirror.from` (§3.6) and then ordered (§4). Naming every hop explicitly is a no-op |
| `download[].verify.before` | bool | no | inherits | Source-side ([08](08-verification.md) §4) |
| `download[].verify.after` | bool | no | inherits | Destination-side, per destination |
| `download[].verify.policy` | enum | no | inherits | `enforce` \| `warn`. Overrides the product's for this download's runs only |
| `download[].priority` | int | no | `50` | 0–1000 ([04](04-queue-and-scheduling.md) §6) |
| `download[].default` | bool | only with ≥2 | - | Which one a bare `download` runs, and which one a rule naming none triggers |

"Inherits" means the product's `verification` block, then the target's override, exactly as today ([02](02-configuration.md) §5). A download does not introduce a third trust configuration; it selects **whether the existing one gates this chain**. This is also why the three verification fields are tri-state on the wire - `true`, `false`, `inherit` - and why the CLI renders `inherit` rather than `false`: a download that says nothing about destination verification is not one that turned it off.

### 3.4 `spec.autoDownload` - when it happens by itself

```yaml
autoDownload:
  # The master switch over AUTOMATIC firing only. `false` stops every rule
  # from being triggered by discovery and leaves the manual path untouched -
  # which is exactly what you want during an incident, and exactly what a
  # switch that also disabled the recovery path would take away.
  enabled: true

  rules:
    # Evaluated in configured order, FIRST MATCH WINS - unchanged from today
    # (02 §5.4). Two rules matching one tag with different downloads has no
    # sensible reading, and "most specific" would need a specificity order
    # over regexes that does not exist.
    - name: ga-releases
      tagPattern: '^v\d+\.\d+\.\d+$'    # RE2, as everywhere else (02 §5.4)
      # `download` omitted: triggers the default, which for a product with one
      # download is that one.

    - name: release-candidates
      tagPattern: '^v\d+\.\d+\.\d+-rc\.\d+$'
      # Optional. Absent means every source in the product. Naming sources is
      # how one product carries a vendor's GA channel and its early-access
      # channel without two rules that differ only by a tag pattern nobody
      # can read.
      sources: [primary]
      # Naming a download is how a second one is reached.
      download: storage-only

    - name: near-orbs
      tagPattern: '^orb_\d+\.\d+\.\d+$'
      # Configuration, in Git, and the only way to turn a rule off (§9).
      enabled: false
```

| Field | Type | Required | Default | Notes |
|---|---|---|---|---|
| `autoDownload.enabled` | bool | no | `false` | Automatic firing only |
| `rules[].name` | string | yes | - | Unique within the product |
| `rules[].tagPattern` | RE2 | yes | - | The whole of what a rule decides. RE2 is stated explicitly because a user-supplied pattern evaluated inside a polling loop would, under a backtracking engine, be a denial-of-service vector |
| `rules[].sources` | []string | no | all | Names sources in this product |
| `rules[].download` | string | no | the default | Which download to trigger |
| `rules[].enabled` | bool | no | `true` | §9 |

**A rule with no `tagPattern` is rejected**, and the error says why in the terms the reader is likely to be thinking in: *if you meant to download by hand, that needs no rule at all*. The most probable way to write that document is to have reached for a rule when the thing wanted was a download.

### 3.5 The older shape still loads, and still means what it meant

Before downloads were their own block, a rule carried `targets`, `priority` and `verifyBeforeTransfer` itself. Every product document in the estate is written that way, and a rename is not a reason to break them.

- A rule carrying those fields **describes its own download inline** and behaves exactly as it did. `Product.DownloadFor` resolves it, and the CLI marks its chain as *(its own targets)* rather than hiding the difference - that inline download is precisely why one rule's chain can differ from every other rule's.
- A rule that names a download **and** carries inline targets is rejected. Two statements of where software goes, resolved by precedence, is a configuration nobody can read out loud.
- `verifyBeforeTransfer: true` is accepted as `verify: {before: true}`.

The compatibility contract, stated so it can be tested: **every product document valid before this change is valid after it and produces the same transfers.**

### 3.6 Naming the end of a chain names the chain

> **Decision - a download names the destinations it cares about. Hops those destinations depend on are pulled in automatically.**
>
> `targets: [ocp-prod]`, where `ocp-prod` has `mirror.from: lab`, plans **both** steps. It is identical to writing `targets: [lab, ocp-prod]`.
>
> *Alternative considered:* require every hop to be named, and warn when one is missing. It has the virtue that a download writes to nothing it did not mention.
>
> *Rejected because* it makes every download restate the chain that [18](18-quay-replication.md) §5.1 exists to declare once, and it asks the person writing it to know that Quay pulls rather than gets pushed to - which is exactly the knowledge this document is trying to keep out of the download. The person adding a target configures *how content gets into it*. The person writing a download says *what goes where*. Requiring the second to encode the first collapses the split.
>
> *And the objection does not survive contact:* `mirror.from` may only name a target in the same product ([18](18-quay-replication.md) §5.2), so the hop was already declared, by the target the download did name. Nothing arrives from outside the document.
>
> *Chosen:* the transitive closure over `mirror.from`, with the **full chain rendered wherever the download is shown** - `downloads list`, `download --dry-run`, the rule listing, and the UI. Implicit to type, never implicit to read. Where the derived chain is longer than what was named, the listing says so and attributes the extra hop to the target that pulled it in.
>
> *What we lose:* the ability to say "sync Quay from whatever is already in JFrog, and do not touch JFrog". That is `transferctl targets sync` ([18](18-quay-replication.md) §7), which is the right command for it and already exists.

Two consequences that need stating, because they are where this gets expensive if ignored:

**A shared hop is transferred once, not once per download.** Two downloads whose chains both pass through `lab` must not produce two transfers of the same package to the same target. The step's identity is therefore `(package.digest, target)` - a step whose predecessor work is already `succeeded` for that pair is satisfied by it, and a step already in flight is **joined**, not duplicated. This is also why the idempotency key (§8.2) covers the download and never the rule: two rules that trigger the same download for the same package are asking for one piece of work, and keying them apart would move the bytes twice.

**The precondition check stays.** A step still asks, before requesting a sync, whether the package is actually present at `mirror.from`. With the closure in place this should be unreachable - its predecessor just succeeded - which is precisely why it is worth keeping: if it ever fires, something upstream lied, and the message names the empty target rather than leaving a sync to fail obscurely inside Quay.

### 3.7 Validation

Beyond the field rules, the combinations that must fail at load rather than at 3 a.m.:

| Rejected | Why |
|---|---|
| Several downloads with none, or more than one, marked `default: true` | "Which one does a bare `download` run" must not depend on file order (§3.2) |
| Several downloads where one has no `name` | It cannot be named by a rule, by `--download`, or in an error message |
| Two downloads with the same name | |
| `targets` naming a `promotionOnly` target | Production is reachable by promotion, not by download ([02](02-configuration.md) §5.2) |
| `targets` naming a disabled target | It would fail the first time software matched |
| `targets` naming a `replication.mode: proxy` target | A cache cannot be pushed to ([18](18-quay-replication.md) §5.4). The error names `warm` |
| `targets` whose derived chain contains a cycle | `mirror.from` edges must form a forest (§4) |
| `targets` whose closure reaches a disabled or `promotionOnly` target | The chain is planned in full (§3.6), so a hop it cannot use is as fatal as a destination it cannot use - and the error names the hop and the target that pulled it in |
| A rule with no `tagPattern` | §3.4 - and the error offers the download the writer probably wanted |
| A rule naming a download that does not exist | |
| A rule naming a download **and** carrying inline targets | §3.5 |
| A rule naming a source that does not exist or is disabled | |
| `verify.after: true` on a chain whose mirror glob excludes `sha256-*.sig` | **The important one.** Destination verification of a mirrored tag is impossible if the signatures were never mirrored ([18](18-quay-replication.md) §9). The configuration looks correct, the sync succeeds, and verification fails for every package forever |

The signature-glob check is the reason this document and [18](18-quay-replication.md) have to be validated together rather than separately: neither block is wrong on its own.

## 4. The chain is derived, not declared

> **Decision - a download declares a set of destinations. The order between them comes from the targets' own `replication` configuration.**
>
> *Alternative considered:* an ordered `steps:` list, each naming a target and where it takes content from. It is explicit, it reads well, and it is what a workflow engine would offer.
>
> *Rejected because* the edge is already declared. [18](18-quay-replication.md) §5.1 puts `mirror.from: lab` on the Quay target precisely so "the chain lab → ocp-prod is declared once and stays consistent when the lab path changes". A `steps:` list would be that same edge written a second time, in a second file section, by a second person - and when the two disagreed, one of them would be silently authoritative.
>
> *Chosen:* the planner topologically sorts the download's targets over the `mirror.from` edges. Because `mirror.from` names exactly one target, the graph is a **forest**, not a general DAG: every step has at most one predecessor, cycles are the only structural error possible, and the sort is six lines.
>
> *What we lose:* the ability to express an ordering that is not a content dependency - "put it in DR only after production succeeded", where DR does not pull from production. Accepted, and deliberately: that is a workflow, and §12 says why we are not building one.

Worked against the topology this exists for:

```
targets:
  - name: jfrog-store            replication.mode: copy
  - name: ocp-prod               replication.mode: mirror, mirror.from: jfrog-store
  - name: dr-store               replication.mode: copy

download internal → targets: [ocp-prod, dr-store]
                    (jfrog-store is pulled in by ocp-prod's mirror.from - §3.6;
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

- **Fan-out and chaining are the same feature.** Independent destinations have no edge and therefore share a step index; dependent ones do not. Nothing in the download distinguishes them, and nothing needs to.
- **Fan-out costs vendor egress twice.** `jfrog-store` and `dr-store` each pull the full package from the vendor. That is sometimes right and often not - `dr-store` could mirror from `jfrog-store` instead. `config validate` says so as an advisory when two copy destinations share a source and one is a Quay target; it does not refuse, because the redundancy is occasionally the point.

## 5. The gates are the verification that already exists

This is the part that looked like new machinery and is not.

A Transfer already ends with `running → verifying → succeeded | failed` ([10](10-state-machines.md) §3). Destination verification is already what happens in `verifying`. And a step waits for its predecessor to reach `succeeded`.

Therefore, with no gate mechanism at all:

| `verify.policy` | Verification fails at `jfrog-store` | Consequence for `ocp-prod` |
|---|---|---|
| `enforce` | step 0 → `failed` | step 0 never reached `succeeded`, so step 1 is **skipped** (§6) |
| `warn` | recorded, notified, step 0 → `succeeded` ([08](08-verification.md) §4) | step 1 runs |

**"Do not configure the Quay mirror if what landed in JFrog did not verify" is the whole security value of the chain**, and it costs one word in the configuration. Under `enforce`, the cluster's registry is never pointed at content whose signature did not check - not by policy, not by convention, but because the step that would have pointed it there did not run.

Two things about the far end of the chain that are easy to get backwards:

- **Verifying a mirrored target verifies what OCP will actually run.** For a `copy` destination we verify bytes we pushed; for a `mirror` destination we verify bytes *Quay* pulled, through Quay's own network path, under Quay's own tag glob. It is the stronger check, and it is the one that catches the glob that silently excluded the signatures.
- **It is only possible if the signatures travelled.** `verification.transferSignatures` must be true, and the mirror's `tags` glob must include `sha256-*.sig` ([18](18-quay-replication.md) §9). §3.7 rejects the combination that cannot work.

## 6. Ordering is waves, one level up

The system already orders work with a single integer: jobs carry a wave, wave *n+1* is `blocked` until wave *n* drains ([04](04-queue-and-scheduling.md) §3.2). A download's steps need exactly the same thing at the transfer level, so they get exactly the same thing rather than something new.

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
| - | `Created` | `waiting` | Only when `depends_on_transfer_id` is set; otherwise `pending`, as today |
| `waiting` | `PredecessorSucceeded` | `pending` | Includes the predecessor's verification gate (§5) |
| `waiting` | `PredecessorSettledUnsuccessfully` | `skipped` | Terminal. `diverged` counts as unsuccessful: a chain built on it would propagate the wrong digest onward |
| `waiting` | `CancelRequested` | `cancelled` | |

**`skipped` is a distinct terminal state and not a flavour of `failed`.** The Quay mirror step of a run whose JFrog step failed did not fail - it never started, nothing was attempted against Quay, and no operator should go looking at Quay for the cause. Collapsing the two would make every chained failure report two problems where there is one, and the second report would point at the wrong system. It also keeps `mirror_sync_total{result="failure"}` ([18](18-quay-replication.md) §7) honest: a sync that never ran is not a sync that failed.

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
| any `failed` | `failed` | Retryable - see §8.2 |
| any `skipped`, none `failed` | impossible | A step is only skipped because an earlier one did not succeed |
| any `cancelled` | `cancelled` | |

### 7.1 Progress across steps is not one number

A run whose first step moves 45 GB and whose second step is a Quay sync has two kinds of truth in it, and there is no arithmetic that combines them. [18](18-quay-replication.md) §6.1 forbids synthesising bytes for a delegated step; the same rule at the run level forbids synthesising a percentage across steps whose units differ.

So a run renders as **steps, each with its own kind of progress**:

```
$ transferctl transfers describe req_8f2c…

  internal · SBC v3.2.1 · near/orbs/sbc-8000  →  2 destinations
  triggered by  ga-releases      started  14:02:11Z        elapsed  22m
  ─────────────────────────────────────────────────────────────────────
  1  ✔  jfrog-store    copy      12.4 GB / 12.4 GB   428 MB/s   done 14:24
        verified at destination  ✔  cosign, 6 artifacts
  2  ●  ocp-prod       mirror    configured 14:24 · syncing since 14:24
        delegated to Quay mirror - no byte progress available
  ─────────────────────────────────────────────────────────────────────
```

There is no total, no combined bar and no ETA for the run. The first step has all three because we measured them; the second has none because Quay did not tell us. A reader who wants "how far along is this" gets the step list, which is the honest answer to a question that has no single number in it.

This is also the shape [19](19-user-interface.md) renders as a stepper, and the reason the stepper has per-step content rather than one progress bar across the top.

## 8. Running one

### 8.1 Two ways in, one path through

| Trigger | Origin | Created by |
|---|---|---|
| `discovery` | a rule matched | Discovery evaluates rules against each new package ([07](07-discovery.md) §5) and opens the download the rule names |
| `manual` | a person | `transferctl download <product> <package>…`, and the `POST …/downloads:run` it calls |

Both create the same TransferRequest, with the same derived chain and the same gates, through the same `Resolve` and `Open`. There is no path that skips a step because it was asked for by a person, and no path a rule can take that a person cannot - which is the property that makes the audit trail worth reading, and the property that makes the manual path a usable incident tool.

The request records **which** of the two it was, and a rule-triggered run also records the rule. That is a fact about how the run started; it is not a fact about the work, which is why the rule is absent from the idempotency key below.

`--dry-run` resolves the chain, checks everything and creates nothing. It is the only way to see the derived step order before committing to it.

### 8.2 Re-running, and what idempotency keys on

The derived idempotency key ([04](04-queue-and-scheduling.md) §7) is what makes rule evaluation safe in a loop:

```
key = hash(package, sourceRepo, derived chain, download.revision, priority)
```

The **derived** chain, after the closure in §3.6 - so a download naming only the tail and one naming every hop key identically, because they are the same work. A **step** is identified by `(package.digest, target)` regardless of which download asked for it, which is what stops two downloads sharing a JFrog hop from transferring it twice (§3.6).

`download.revision` is a hash of the download's own resolved fields - targets, priority, the three verification settings. It is there so that **editing a download creates a new run rather than being swallowed by the old one's key**: the failure otherwise is a person tightening `verify.policy` from `warn` to `enforce`, re-running, getting `already exists`, and concluding the stricter policy was applied when nothing ran at all. It is built from an explicit ordered shape rather than from the struct, so adding a field to the schema does not silently invalidate every stored revision in the estate on the next deploy.

**The rule is not in the key.** Two rules that trigger the same download for the same package are asking for one piece of work.

A retry is not a re-run. `transfers retry` resumes the *existing* request: failed steps reset, `skipped` steps return to `waiting`, and steps that already succeeded are not repeated. That matters beyond efficiency - deduplication would make a repeat nearly free ([05](05-transfer-engine.md) §4.1), but the audit trail should show one transfer to JFrog because there was one.

## 9. There is no runtime override, and that is the design

> **Decision - `rules[].enabled` in Git is the only way to turn a rule off. There is no suspend, no resume, and no API or UI that changes whether a rule fires.**
>
> *Alternative considered, and built before being removed:* an audited, database-backed *suspension* - an operational override with an actor, a required reason and an optional expiry, stopping a rule in one API call without editing configuration. The argument for it is real: at 02:00, with a vendor pushing broken images, "open a pull request and wait for Flux" is not a fast path.
>
> *Rejected because* it is a second source of truth for whether a rule is on. Configuration is GitOps ([02](02-configuration.md) §2): the product document is the answer to "what is this system doing", and a suspension makes that answer conditional on a row in a database that no reviewer of the repository can see. [18](18-quay-replication.md) §8 rejected exactly this shape for Quay configuration, for exactly this reason, and accepting it here would have made the rule the one place where the repository lies.
>
> *And the incident argument does not need it.* The fast path already exists and is better: **stop the download, not the rule.** `transfers pause`, `transfers stop` and `autoDownload.enabled: false` all act on work rather than on configuration, and none of them leaves Git saying something untrue. A vendor pushing broken images is stopped by pausing the queue in one call - and the queue is where the damage is.
>
> *What we lose:* the ability to disable one rule of several without a commit. Accepted. That is a rarer case than it sounds, and a commit is the correct artifact for a change that outlives the incident.

Consequences, stated so nothing quietly reintroduces the shape:

- The API exposes downloads and rules **read-only**. There is no `:suspend`, no `:resume`, no `:enable`.
- The CLI has no `rules enable|disable|suspend|resume`, and the renderer tests assert that those words do not appear in its output.
- A UI toggle for "is this rule on" is not a control. [19](19-user-interface.md) shows rule state and links to the file; changing it is a commit.

## 10. Where the code goes

Genuinely new, in full:

| Package / object | Holds |
|---|---|
| `internal/download` | Download resolution, chain derivation, the topological sort, the idempotency key. Depends on `internal/product` and nothing below the API |
| `internal/discovery` | Loses the destination decision to `internal/download` and calls it instead. `rules.go` first-match-wins stays, and resolves the download the winning rule names |
| `transfer_requests` + | `rule_name`, `rule_revision`, `trigger` |
| `transfers` + | `step_index`, `depends_on_transfer_id` |

API ([09](09-api.md) §2). Configuration is read-only; the one verb operates:

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/products/{p}/downloads` | Downloads, their resolved chains and their gates |
| `POST` | `/api/v1/products/{p}/downloads:run` | Download named software. `tags` is required; `validateOnly=true` renders the plan and moves nothing |
| `GET` | `/api/v1/products/{p}/autoDownloadRules` | Rules, what they match, and which download each triggers |
| `GET` | `/api/v1/products/{p}/autoDownloadRules/{rule}/matches` | What this rule would pick up from what has been discovered. Reads and creates nothing |

`downloads:run` with an empty `tags` is an error and not a "download everything": *name at least one package to download; a download takes software, not a pattern.*

There is deliberately **no** `/downloads/{d}/runs`. The runs are transfer requests, and `GET /transfers?filter=…` already returns them ([09](09-api.md) §3). A second listing of the same rows, paginated separately and filtered differently, is how two answers to one question get into a product.

CLI ([13](13-cli.md) §2) - a verb for the thing you do, noun groups for the things you look at:

```
download <product> <package>…           Run the default download, or --download <name>
         [--dry-run]                    Resolve the chain and create nothing

downloads
└── list <product>                       Downloads, their derived chains and their gates

rules
├── list    <product>                    Rules, what they match, and what they trigger
└── matches <product> <rule>             What this rule would pick up
```

Metrics ([12](12-observability-and-audit.md) §2), following the existing naming:

```
softwaregateway_download_rule_matches_total{product,rule}
softwaregateway_download_run_total{product,download,trigger,result}
softwaregateway_download_step_skipped_total{product,download,target}
```

Audit ([12](12-observability-and-audit.md) §4.1) gains a `Download` category: `DownloadRunRequested`, `DownloadRunCompleted`, `DownloadStepSkipped`. A *match* is not audited - it happens for every package on every scan, and burying real events under thousands of routine ones is how an audit trail stops being read. The match is a metric.

## 11. Failure modes

| Failure | Behaviour |
|---|---|
| Step 0 fails | Dependent steps → `skipped`; independent steps continue. The run is `failed` and retryable from where it stopped |
| Destination verification fails, `enforce` | Step `failed`, dependents `skipped`, `VerificationFailed` notification. **Quay is never configured** |
| Destination verification fails, `warn` | Recorded and notified; the chain proceeds ([08](08-verification.md) §4) |
| `mirror.from` target is empty at run time | Step fails its precondition before touching Quay, naming the empty target (§3.6) |
| Mirror sync reports success, digest differs | Step `diverged`, run `completed`, `MirrorContentDiverged` ([18](18-quay-replication.md) §6.2) |
| A tag named by hand was never discovered | The run fails naming that tag, rather than downloading the others silently. A person who names four releases and gets three has been told something untrue |
| Download edited while a run is in flight | The run completes under the revision it started with. `rule_revision` on the request is what makes that statement checkable a year later |
| Download or rule deleted while a run is in flight | The run completes. Deletion of running work is never a side effect of a config edit ([02](02-configuration.md) §6) |
| Coordinator restarts between steps | Nothing to recover. A `waiting` transfer whose predecessor is `succeeded` is advanced by the same sweep that settles transfers ([10](10-state-machines.md) §3) |
| Two Coordinators evaluate one package | One request, by the derived key (§8.2) |

## 12. What this is not

> **Decision - this is not a workflow engine, and the boundary is the `mirror.from` edge.**
>
> "Download rules" is the exact feature that becomes a general orchestrator if nobody says no: conditionals, parallel branches, manual approval steps, retries with custom policy, a step that runs a script. Each is a reasonable request and each arrives after the previous one shipped.
>
> The line is drawn at a place that can be checked rather than argued: **the only ordering primitive is a content dependency that the target configuration already declares.** If two steps have no `mirror.from` edge between them, they are concurrent, and there is no syntax to say otherwise. A download has no conditionals, no branches, no user-defined steps and no scripting.
>
> *If a real case needs more*, the answer is a second download and something outside this system triggering it - not a DSL inside a ConfigMap.

Also out of scope, permanently:

- **Not a promotion replacement.** Promotion remains its own verb with its own guard rails, and `promotionOnly` targets remain unreachable by download ([02](02-configuration.md) §5.2). Downloading brings content in; promoting moves it forward. The estate's shape is one download, source to internal targets, and one promote, target to production - and a download that could push to production would erase that distinction on the day someone mistyped a target name.
- **Not a scheduler.** Rules fire on discovery. Recurring schedules - "download every Tuesday whether anything is new or not" - are Q13, and the default answer is a Kubernetes CronJob calling the CLI.
- **Not a policy engine.** `verify.policy` selects between two behaviours the verification subsystem already has. It does not grow expressions.
- **Not a per-release routing table.** There is one chain into the internal estate, not a different path per release class. A second download exists for the case that genuinely has a different destination set, not as a way to fan releases across bespoke routes.

## 13. Security

- **A manual download is an authenticated action with an actor**, and the `actor` field already exists and already writes `"anonymous"` ([12](12-observability-and-audit.md) §4.2). Until [09](09-api.md) §10 lands, "who downloaded this" has the same answer as every other question about who did anything - which is [17](17-delivery-plan.md) Q6, and one more reason it is a gate.
- **Configuration cannot be changed through the API.** §9 is a security property as much as an operational one: the set of things this system will fetch and where it will put them is reviewable in Git and nowhere else.
- **A download cannot widen trust.** `verify.policy` may tighten `warn` to `enforce`; the reverse is permitted but audited, and `products check` reports every download whose policy is weaker than its product's. Verification keys and identities come from the product and the target, never from a download - a trust configuration assembled from three places is one nobody can audit ([02](02-configuration.md) §5).
- **Download and rule names appear in metric labels**, so they are bounded by the same character rules as product names.

## 14. Delivery

**M9**, after M8. Acceptance criteria are in [17](17-delivery-plan.md#m9--downloads-and-auto-download).

It waits for M8 rather than shipping the copy-only half earlier, and that is a choice with a reason: the ordering model, the `skipped` state and the per-step rendering all have to be built twice if they are built first for a chain that has only copy steps in it. The interesting chain is the one with a mirror at the end, and it is the one the estate actually needs.

It waits for M5 because a gate over a verification that does not execute is a gate in name only.

### Open questions

| # | Question | Decide by |
|---|---|---|
| Q13 | Do recurring schedules belong here at all, or does a Kubernetes CronJob calling `transferctl download` cover every real case? The default answer is the CronJob | M10, from whether anyone asks twice |
| Q14 | Is the run revision the hash of the download, or of the whole product document? The download is more precise; the document is what the config hash already covers, and two hashing schemes for one concept is a cost | M9 exit |
| Q15 | Does anyone declare a second download? If nobody does within two quarters, §3.2's list is flexibility that cost nothing and bought nothing, which is worth knowing before the next such decision | M10 |

## References

- [07 - Discovery](07-discovery.md) §5 - rule evaluation and where it now delegates
- [04 - Queue and Scheduling](04-queue-and-scheduling.md) §3.2, §7 - waves and derived idempotency keys
- [08 - Verification](08-verification.md) §4 - stages, policy, and what `enforce` already does
- [10 - State Machines](10-state-machines.md) §3, §7, §8 - the transfer machine the new states extend
- [18 - Quay Replication](18-quay-replication.md) §5, §6, §8, §9 - `mirror.from`, delegated progress, apply, the signature-tag trap
- [19 - User Interface](19-user-interface.md) §3.1, §4 - the pages these objects appear on
