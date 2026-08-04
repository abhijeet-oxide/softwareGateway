# 10 — State Machines

> **Prerequisite:** [03 — Persistence](03-persistence.md) · **Related:** [04](04-queue-and-scheduling.md), [05](05-transfer-engine.md), [08](08-verification.md)

> **Requirement: avoid implicit states.** Every state in this document is named, stored in a column, and constrained by a `CHECK` that enumerates exactly these values. A state that exists only as a combination of nullable timestamps is not a state — it is a bug waiting for someone to add a sixth interpretation.

---

## 1. The guard

All transitions go through one function. There is no `UPDATE … SET state = …` anywhere else in the codebase.

```go
package state

type Transition[S ~string, E ~string] struct {
    From  S
    Event E
    To    S
    Guard func(ctx context.Context, subject any) error   // optional precondition
}

// Apply returns ErrIllegalTransition when (from, event) is not in the table.
// It never falls through to a default, and never silently no-ops.
func (m *Machine[S, E]) Apply(ctx context.Context, from S, event E, subj any) (S, error)
```

Three properties this buys, each of which is a bug class removed:

1. **Illegal transitions error rather than corrupt.** A `cancel` on a `succeeded` transfer returns `ErrIllegalTransition`, surfaced as `409 FAILED_PRECONDITION` with the current state ([09](09-api.md) §5). Without the guard it would be an `UPDATE … WHERE id = $1` that quietly rewrites a completed record.
2. **The table is the documentation.** These tables are generated from the same declarations the code uses, so they cannot drift from the implementation.
3. **The database is a second line of defence.** `CHECK` constraints mean a bug that bypasses the guard still cannot persist a state the machine does not define.

Transitions are applied **inside the transaction that performs the associated side effects** ([09](09-api.md) §7.2), so a state change and its consequences commit together or not at all.

## 2. Package lifecycle

Column: `packages.state`. Reflects a package's status **at its source**, aggregated across targets.

```
                    ┌──────────────┐
  discovery ───────►│  discovered  │◄──── initial
                    └──────┬───────┘
             transfer      │
             requested     ▼
                    ┌──────────────┐
                    │    queued    │
                    └──────┬───────┘
                           │ first job leased
                           ▼
                    ┌──────────────┐   any transfer fails   ┌──────────┐
                    │ transferring ├───────────────────────►│  failed  │
                    └──────┬───────┘                        └────┬─────┘
       all transfers       │                                     │ retry
       succeeded           ▼                                     │
                    ┌──────────────┐                             ▼
                    │ transferred  │                        (queued)
                    └──────┬───────┘
                           │ verification starts
                           ▼
                    ┌──────────────┐
                    │  verifying   │
                    └──┬────────┬──┘
                passed │        │ failed
                       ▼        ▼
              ┌──────────┐  ┌──────────────────────┐
              │ verified │  │ verification_failed  │
              └──────────┘  └──────────────────────┘

  ANY non-terminal state ──► superseded   (vendor re-pushed the tag, 07 section 4)
```

| From | Event | To | Notes |
|---|---|---|---|
| — | `Discovered` | `discovered` | Insert; idempotent via unique constraint |
| `discovered` | `TransferRequested` | `queued` | |
| `queued` | `TransferStarted` | `transferring` | First job of any transfer leased |
| `transferring` | `AllTransfersSucceeded` | `transferred` | |
| `transferring` | `TransferFailed` | `failed` | Any target failed terminally |
| `transferred` | `VerificationStarted` | `verifying` | Skipped when verification disabled → `verified` |
| `verifying` | `VerificationPassed` | `verified` | Terminal (success) |
| `verifying` | `VerificationFailed` | `verification_failed` | Terminal; artifacts retained ([08](08-verification.md) §4) |
| `verifying` | `VerificationError` | `verification_failed` | Distinguished in `verifications.state`, not here |
| `failed` | `RetryRequested` | `queued` | |
| `verification_failed` | `RetryRequested` | `verifying` | Re-verify without re-transferring |
| any except terminal | `Superseded` | `superseded` | Terminal |

**Terminal:** `verified`, `verification_failed`, `superseded`. `failed` is *not* terminal — it is retryable, which is the point of naming retry as an explicit event rather than treating it as a fresh start.

**Aggregation rule.** A package with transfers to `lab` and `production` is `transferred` only when both succeed, and `failed` as soon as either fails terminally. Per-target status stays on the Transfer and is what the API surfaces ([09](09-api.md) §3) — the aggregate is a summary, not the source of truth. A package is never "half transferred" as a state; that condition is read off its transfers.

## 3. Transfer and promotion lifecycle

Column: `transfers.state`. **This is one machine, not two** — a promotion is a transfer whose origin is a target repository ([01](01-domain-model.md) §3.4), so it uses these states unchanged. Duplicating it for promotion would create two tables to keep in sync.

```
   ┌─────────┐  plan   ┌──────────┐  jobs   ┌───────┐ lease ┌─────────┐
   │ pending ├────────►│ planning ├────────►│ ready ├──────►│ running │
   └─────────┘         └────┬─────┘ created └───┬───┘       └────┬────┘
                            │ plan failed       │                │
                            ▼                   │        pause ⇅ resume
                       ┌────────┐               │                │
                       │ failed │◄──────────────┴────────► ┌────────┐
                       └────────┘   job failed terminally  │ paused │
                            ▲                              └────────┘
                            │                                   │
                            │                     all waves drained
                            │                                   ▼
                            │  verify failed            ┌─────────────┐
                            └───────────────────────────┤  verifying  │
                                                        └──────┬──────┘
                                                               │ passed / skipped
                                                               ▼
                                                        ┌────────────┐
                                                        │ succeeded  │
                                                        └────────────┘

   any non-terminal ──► cancelling ──► cancelled   (04 section 8)
```

| From | Event | To | Notes |
|---|---|---|---|
| — | `Created` | `pending` | |
| `pending` | `PlanningStarted` | `planning` | |
| `planning` | `PlanCompleted` | `ready` | Jobs inserted; `max_wave` set |
| `planning` | `PlanFailed` | `failed` | Manifest unreadable, target unreachable |
| `planning` | `PlanEmpty` | `succeeded` | **Everything already present.** A no-op transfer is a success |
| `ready` | `FirstJobLeased` | `running` | |
| `running` | `PauseRequested` | `paused` | In-flight jobs finish ([04](04-queue-and-scheduling.md) §8) |
| `ready` | `PauseRequested` | `paused` | |
| `paused` | `ResumeRequested` | `running` | |
| `running` | `AllWavesDrained` | `verifying` | → `succeeded` when verification is off |
| `running` | `JobFailedTerminally` | `failed` | A job past `max_attempts` |
| `verifying` | `VerificationPassed` | `succeeded` | Terminal |
| `verifying` | `VerificationFailed` | `failed` | Artifacts retained |
| `failed` | `RetryRequested` | `ready` | Failed jobs reset to `pending` |
| non-terminal | `CancelRequested` | `cancelling` | |
| `cancelling` | `InFlightDrained` | `cancelled` | Terminal |

**Terminal:** `succeeded`, `cancelled`. `failed` is retryable.

Two entries deserve attention:

- **`PlanEmpty` → `succeeded`.** When deduplication finds every blob already present, the correct outcome is success with zero jobs — not an error, and not a transfer that sits in `ready` forever waiting for work that will never be created. This is a common case for promotion ([05](05-transfer-engine.md) §6), not an edge case.
- **`cancelling` is a real state, not a flag.** It exists because leased jobs take up to one heartbeat to abort ([09](09-api.md) §7.4). Without it, a cancel would either appear instantaneous while bytes were still moving, or appear stuck. Naming the window makes it observable.

## 4. Job (layer) lifecycle

Column: `jobs.state`. The highest-volume machine — hundreds of thousands of rows.

```
    ┌─────────┐   wave advanced    ┌─────────┐   lease    ┌────────┐
    │ blocked ├───────────────────►│ pending │◄──────────►│ leased │
    └─────────┘   (04 section 3.3) └────┬────┘  requeue   └───┬────┘
       wave ≥ 1                         ▲                     │
       at creation                      │                     ├──► succeeded   (bytes moved)
                                        │                     ├──► skipped     (already present / mounted)
                          retry with    │                     │
                          backoff       └─────────────────────┤
                                                              └──► failed      (attempts exhausted)

    pending | blocked | leased ──► cancelled
```

| From | Event | To | Notes |
|---|---|---|---|
| — | `Created` (wave 0) | `pending` | |
| — | `Created` (wave ≥ 1) | `blocked` | |
| `blocked` | `WaveAdvanced` | `pending` | Bulk update ([04](04-queue-and-scheduling.md) §3.3) |
| `pending` | `Leased` | `leased` | `attempts` incremented **here**, not on failure |
| `leased` | `CompletedTransferred` | `succeeded` | Writes `blob_placements` |
| `leased` | `CompletedExists` | `skipped` | `skip_reason = placement_hit` / `exists_at_target` |
| `leased` | `CompletedMounted` | `skipped` | `skip_reason = mounted` |
| `leased` | `Failed` (retryable, attempts left) | `pending` | `next_visible_at` set by backoff (§6) |
| `leased` | `Failed` (terminal or exhausted) | `failed` | |
| `leased` | `LeaseExpired` | `pending` or `failed` | Reaper; depends on `attempts` |
| `leased` | `BlobUnknownOnManifest` | `pending` | Placement invalidation ([05](05-transfer-engine.md) §9) |
| `pending`/`blocked`/`leased` | `Cancelled` | `cancelled` | |

**Terminal:** `succeeded`, `skipped`, `cancelled`. `failed` is retryable via `transfers:retry`.

> **`skipped` is a first-class success, not an exception.** It is the state that makes deduplication measurable: `sum(size_bytes) where state='skipped'` grouped by `skip_reason` is exactly the bandwidth-saved metric ([12](12-observability-and-audit.md) §2). Folding "already present" into `succeeded` would make the system's most valuable optimization invisible, and an optimization nobody can measure is one nobody will notice regressing.

**Wave-drain counts `succeeded` and `skipped` together** ([04](04-queue-and-scheduling.md) §3.4) — both mean the content is present, which is the only thing the next wave cares about.

## 5. Verification lifecycle

Column: `verifications.state`. One row per verification attempt; re-verification inserts rather than mutates ([08](08-verification.md) §9).

| From | Event | To | Notes |
|---|---|---|---|
| — | `Requested` | `pending` | |
| `pending` | `Started` | `running` | |
| `pending` | `SkippedByPolicy` | `skipped` | Disabled, or no signatures with `warn` |
| `running` | `SignaturesValid` | `passed` | Terminal |
| `running` | `SignaturesInvalid` | `failed` | Terminal. **Not retried** |
| `running` | `VerificationUnavailable` | `error` | Retryable with backoff |
| `error` | `RetryRequested` | `pending` | |

**`failed` and `error` are deliberately distinct**, and this is the most important distinction in this document:

| | `failed` | `error` |
|---|---|---|
| Meaning | Verification ran; the signature did not check out | Verification could not run |
| Cause | Bad or missing signature, identity mismatch | Rekor unreachable, malformed policy, network fault |
| Class | **Security event** | **Availability event** |
| Retried | No | Yes, with backoff |
| Under `enforce` | Blocks | Blocks |

Collapsing them would make a Sigstore outage indistinguishable from a supply-chain attack. They page different people and demand different responses. Retrying a signature that definitively does not verify accomplishes nothing but repeating the alert.

## 6. Retry lifecycle

Retry is not a separate state column — it is `attempts` plus `next_visible_at` on the job, and the `leased → pending` edge in §4. It is documented as a machine because its behaviour must be explicit.

```
attempt N fails
      │
      ▼
classify error (06 section 7)
      │
      ├── non-retryable (401, 403, ErrUnsupported) ──────────────► failed
      │
      ├── attempts >= maxAttemptsFor(class) ─────────────────────► failed
      │
      └── retryable ──► next_visible_at = now() + backoff(N) ────► pending
                        backoff(N) = random(0, min(1s · 2^N, 5m))
```

| Error class | Max attempts | Rationale |
|---|---|---|
| `ErrUnavailable` (5xx) | 8 | Registries recover; this is the common transient case |
| `ErrTimeout` | 8 | Same |
| `ErrRateLimited` (429) | 8 | Honours `Retry-After` over the computed backoff |
| `ErrDigestMismatch` | 2 | Bytes are wrong. Retrying rarely helps and may indicate real corruption worth surfacing |
| `ErrUnauthorized` / `ErrForbidden` | 1 | Credentials will not fix themselves; hammering an auth endpoint gets us throttled |
| `ErrNotFound` (source blob) | 1 | The vendor deleted content mid-transfer. A human needs to know |
| `ErrUnsupported` | 1 | Capability absent; will not change on retry |
| Lease expiry | counts toward the class cap | A worker that reliably dies on one job must not loop forever |

**Full jitter** (`random(0, …)` rather than a fixed exponential) because failures here are strongly correlated — one registry returning 503 fails all 40 in-flight jobs simultaneously. Without jitter they retry in lockstep, re-hammering a struggling registry in synchronized waves and turning a blip into an outage. This is well-established and not re-derived here.

**Retries resume rather than restart.** `bytes_transferred` and `upload_state` persist across attempts ([05](05-transfer-engine.md) §4.6).

## 7. Supporting machines

Lower-stakes, listed for completeness — no implicit states anywhere.

**Transfer request** (`transfer_requests.state`): `pending → expanded → completed | failed`, or `pending → scheduled → expanded` when `scheduleAt` is set. `cancelled` from any non-terminal state. `expanded` means Transfers exist for every target; the request's own terminal state is a rollup of theirs.

**Scheduled request** (`scheduled_requests.state`): `scheduled → due → expanded`, plus `cancelled` and `failed` (expansion failed, or `maxDelay` exceeded — [04](04-queue-and-scheduling.md) §10). `due` exists so a scheduler crash between "found it" and "expanded it" is recoverable rather than ambiguous.

**Notification** (`notifications.state`): `pending → sending → sent | failed`, plus `suppressed` (channel disabled or deduplicated). Same backoff shape as §6, capped at 5 attempts.

## 8. Invariants across machines

Relationships the guards enforce jointly. These are the properties that make the whole set coherent rather than five independent tables.

| # | Invariant | Enforced |
|---|---|---|
| S1 | A transfer cannot be `succeeded` while any job is non-terminal | Wave-drain check ([04](04-queue-and-scheduling.md) §3.4) |
| S2 | A package cannot be `verified` while any transfer is non-terminal | Package aggregation (§2) |
| S3 | A job cannot be `leased` if its transfer is `paused` or `cancelling` | `jobs.paused` + dequeue predicate ([04](04-queue-and-scheduling.md) §4.1) |
| S4 | A manifest job cannot be `leased` before its wave is current | `blocked` state (§4) — invariant I1 |
| S5 | A terminal state is never left, except `failed` via explicit retry | Absence of edges in the tables above |
| S6 | Every transition writes an audit event | Guard emits within the same transaction ([12](12-observability-and-audit.md) §4) |
