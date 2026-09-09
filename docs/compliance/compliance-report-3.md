# Rescan Verification — Delta Report

**Scan:** Same release, rescanned after the previous feedback round
**Previous:** 1,593 findings / 257 Critical  →  **Now:** 1,701 findings / **236 Critical**
**Method:** All 688 rendered objects re-parsed; every changed check family independently recomputed and compared row by row.

---

## Verdict

**Nine of the eleven items we raised are fixed, and every fix we could independently verify matched our recount exactly.** This is the strongest result across the three scans we have reviewed.

Two new defects were introduced by the fixes, and three items remain open.

| Outcome | Count |
|---|---|
| Items fixed and verified | 9 |
| New defects introduced | 2 |
| Items still open | 3 |
| Checks independently recomputed | 6 families, **6 exact matches** |

---

## Part A — Fixed and verified

### A1 / A2 — Namespace join defect: resolved

| Check | Before | Now | Our independent count |
|---|---|---|---|
| `PDB-01` | 3 Critical (all false) | **0** | **0 — match** |
| `PDB-09` | 4 Critical (all false) | **0** | **0 — match** |

We recomputed workload-to-policy matching across all 14 PodDisruptionBudgets and every replicated workload. Zero uncovered workloads, zero orphaned policies. The scanner agrees. **7 false Criticals eliminated.**

### A4 — `CFG-14` credential classification: correct

The check was split, and the split is accurate. We decoded every one of the 50 secrets carrying inline data and classified by content:

| Content | Count | Landed in | Correct |
|---|---|---|---|
| PEM `PRIVATE KEY` block | 10 | `CFG-14` Critical | Yes |
| Real credential value under a credential-named key | 25 | `CFG-14` Critical | Yes |
| Public key material (e.g. `ssh_public_key`) | — | `CFG-15` Informational | Yes |
| Config template with **empty** credential fields | — | `CFG-15` Informational | Yes |

We specifically re-checked the two properties-file secrets that prompted the original complaint. Both carry `dataSource.password` with an **empty** value — a template awaiting injection, not an exposed credential. Both are now Informational. **Correct call.**

The `Observed` text is also much better: `server-key.pem: a private key, tls.key: a private key` names the offending keys directly.

### A6 — `CFG-13` → `CFG-16`: correctly re-tiered

Was 5 Critical with confidence `Confirmed from the chart`. Now 7 findings at **Warning** with confidence **`Needs someone who knows this workload`** — exactly the calibration we asked for on a value-shape inference.

### A9 / B3 — Capability tiering: implemented and verified

| Tier | Our count | Tool |
|---|---|---|
| High-risk (`SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE`, `DAC_*`) | 0 | `SEC-13` = **0** |
| Privilege-relevant (`SETUID`, `SETGID`, `CHOWN`, `NET_RAW`) | 6 | `SEC-14` = **6 Warning** |

**Exact match.** No container now falls through unreported, and the six that only take back `CHOWN`/`SETUID`/`SETGID` are no longer rated Critical.

### B4 — `readOnlyRootFilesystem`: new check, verified

`SEC-05` — 4 findings. We counted 4 of 200 containers without `readOnlyRootFilesystem: true`. **Exact match.**

The paired advisory was also implemented as `SEC-16` (77 Informational), catching the inverse case:

> *"this container's program files are read-only, which is right, and it has nowhere writable at all: it mounts no volumes."*

That is a genuinely useful finding and it was not in our original ask — good addition.

### D1 — `Value is` placeholder: eliminated

| Scan | `Could not be established` |
|---|---|
| Two scans ago | 32% of rows |
| Previous scan | 47% of rows |
| **This scan** | **0%** |

The field is now left blank (552 rows) where it cannot be determined, rather than filled with a placeholder. This is what we asked for — an empty cell is honest.

### D4 — Severity now respects confidence: fully applied

**All 236 Critical findings carry confidence `Confirmed from the chart`. Zero exceptions.**

Findings the scanner cannot confirm from the chart alone are now capped below Critical. `Needs someone who knows this workload` rose from 6 to **102** rows. This single change removes most of the disputes we would otherwise have had to arbitrate.

### Also noted

- **`Tier` is now populated** — 1,624 at tier 1, 77 at tier 2. Previously constant.
- **`In practice` column added** — the effective-value field we requested. See D2 below on coverage.
- `Line` is populated on all 1,701 rows.

---

## Part B — New defects introduced

### N1 — `PDB-02` percentage arithmetic is wrong, and non-deterministic

**Priority: P1.** This is a new false positive created by the percentage fix.

```
PDB-02 | Critical | PodDisruptionBudget ca-service
  chart-m/templates/ca-service-pdb.yaml, line 2
  Observed:    "allows 0 of the covered copies to be moved: at least 50% must stay running"
  In practice: "0 copies may be moved - the percentage rounds down to zero
                against the number of copies this service runs"
```

**Manifest:**
```yaml
kind: PodDisruptionBudget
metadata:
  name: ca-service
spec:
  minAvailable: 50%
  selector:
    matchLabels:
      app: ca-service
      component: ca-service
```

The covered Deployment has **`replicas: 2`**.

**Kubernetes semantics for `minAvailable` as a percentage** — the required count is rounded **up**, and the remainder is what may be evicted:

```
required  = ceil(replicas × pct) = ceil(2 × 0.50) = 1
allowed   = replicas − required  = 2 − 1          = 1
```

One copy may be moved. This is not a deadlock. Correct behaviour across replica counts:

| replicas | `minAvailable: 50%` | required | allowed | Deadlock? |
|---|---|---|---|---|
| 1 | 50% | 1 | 0 | **Yes** |
| 2 | 50% | 1 | 1 | No |
| 3 | 50% | 2 | 1 | No |
| 4 | 50% | 2 | 2 | No |

The finding text — *"the percentage rounds down to zero"* — suggests `floor()` is being applied to `minAvailable`. Rounding down is correct for `maxUnavailable`; for `minAvailable` the rounding is up, and the quantity being computed is the **required** count, not the allowance.

**It is also inconsistent.** A second policy in the same release has the identical configuration and is **not** flagged:

| Policy | `minAvailable` | Replicas | Allowed | Flagged |
|---|---|---|---|---|
| `ca-service` | 50% | 2 | 1 | **Yes — false positive** |
| `cm-reader` | 50% | 2 | 1 | No |

Same threshold, same replica count, same outcome — one flagged, one not. Whatever path produces this finding is not deterministic.

**Fix:**
```python
def disruptions_allowed(pdb, replicas):
    if (v := pdb.get('minAvailable')) is not None:
        required = math.ceil(replicas * pct(v)) if is_pct(v) else v
        return replicas - required
    if (v := pdb.get('maxUnavailable')) is not None:
        return math.floor(replicas * pct(v)) if is_pct(v) else v
    return None

fire = (a := disruptions_allowed(...)) is not None and a <= 0
```

**Acceptance test.** Against this release `PDB-02` must emit **zero** findings — there is no deadlock in it. Both 50% policies cover two-replica workloads. Add a fixture with `minAvailable: 50%` on a single-replica workload, which must fire.

---

### N2 — `RBAC-05` and `RBAC-11` double-count the same rule

**Priority: P2.** The check split we asked for was implemented, but the two halves are not mutually exclusive.

| Check | Findings | Title |
|---|---|---|
| `RBAC-05` | 39 | *"No service can list the namespace's credentials"* |
| `RBAC-11` | 39 | *"A service that reads a credential names which credential"* |

**35 roles appear in both** — two Critical findings for one rule.

```yaml
# example role, flagged by both checks
- apiGroups: [""]
  resources: ["pods", "secrets"]
  verbs: ["get", "list", "delete", "watch", "patch"]
```

Our independent counts:

| Condition | Our count | Tool |
|---|---|---|
| Roles with `list`/`watch` on secrets | 38 | `RBAC-05` = 39 |
| Roles with **only** unscoped `get` | **4** | `RBAC-11` = **39** |

`RBAC-11` is firing on every role that reads secrets without `resourceNames`, including the 35 that already carry `list`/`watch` and are reported by `RBAC-05`.

**Fix.** Make the split exclusive:

| Check | Condition |
|---|---|
| `RBAC-05` | `list` or `watch` on secrets → *"can enumerate every credential in the namespace"* |
| `RBAC-11` | `get` on secrets, **no `list`/`watch`**, no `resourceNames` → *"can fetch any credential in the namespace by name"* |

Expected against this release: `RBAC-05` = 38, `RBAC-11` = 4.

**Effect:** removes 35 Critical findings. Combined with N1, Critical drops from 236 to approximately **200**.

`RBAC-11` also has an empty `In practice` field on all 39 rows.

---

## Part C — Still open

### C1 — `SUP-01` not split (B5)

Still a single check, 200 findings, Warning. No `SUP-01a` / `SUP-01b`.

A future release introducing `image: app:latest` would surface at the same Warning level as 200 existing rows and be invisible. Mutable tag and missing digest are different severities with different owners.

### C2 — No pass records (B6)

All 1,701 rows are `Outcome: Fail`. Still no denominator, no compliance rate, and no way to distinguish a passing check from one that never ran.

This scan makes the cost concrete: **`SEC-13` now correctly reports 0 findings**, because no container in this release takes back a high-risk capability. That is a real, verified good result — and the report cannot express it. It looks identical to a check that did not execute.

### C3 — `In practice` populated on only 1% of rows (D2)

The column was added, and where present the text is genuinely good:

> `SEC-05` — *"false — Kubernetes fills the blank with a writable root filesystem"*
> `CFG-11` — *"the pods stay in CreateContainerConfigError until something else supplies Secret X"*
> `PDB-03` — *"no eviction can ever be approved, because there is no second copy to keep running"*

But it is populated on **32 of 1,701 rows**. The highest-volume checks are all empty:

| Check | Rows | `In practice` |
|---|---|---|
| `SUP-01` | 200 | empty |
| `RES-03` | 130 | empty |
| `CFG-03` | 126 | empty |
| `MTA-09` | 126 | empty |
| `PDB-08` | 113 | empty |
| `RBAC-02` | 67 | empty |

These are exactly the checks where the effective value differs most from the declared one — `RBAC-02` reports `(absent)` for a field that defaults to `true`, `PDB-08` for a field that defaults to 30 seconds. Suggested values:

| Check | Observed | Suggested `In practice` |
|---|---|---|
| `RBAC-02` | `not declared` | `true — a platform token is mounted` |
| `PDB-08` | `not declared` | `30s — the Kubernetes default applies` |
| `SUP-01` | `tag 1.4.0` | `resolves to whatever that tag points to at pull time` |
| `CFG-03` | `not declared` | `mutable — the ConfigMap can be edited in place while in use` |

`Waiver` also remains empty on all rows.

---

## Part D — Summary

| ID | Item | Status |
|---|---|---|
| A1 | `PDB-01` namespace join | **Fixed — verified 0/0** |
| A2 | `PDB-09` namespace join | **Fixed — verified 0/0** |
| A4 | `CFG-14` content classification | **Fixed — verified correct** |
| A6 | `CFG-13` → `CFG-16` re-tiering | **Fixed** |
| A8 | `RBAC-05` title accuracy | **Fixed** — but see N2 |
| A9 / B3 | Capability tiering | **Fixed — verified 0/6** |
| B4 | `readOnlyRootFilesystem` | **Fixed — verified 4/4** |
| D1 | `Value is` placeholder | **Fixed — 47% → 0%** |
| D4 | Severity respects confidence | **Fixed — 236/236 Confirmed** |
| **N1** | **`PDB-02` percentage arithmetic** | **New defect — P1** |
| **N2** | **`RBAC-05`/`RBAC-11` double-count** | **New defect — P2** |
| B5 | `SUP-01` split | Open |
| B6 | Pass records | Open |
| D2 | `In practice` coverage (1%) | Partially done |

**Critical trajectory:** 618 → 257 → **236** → approximately **200** once N1 and N2 are resolved.

**What we would need next.** A rescan with N1 and N2 fixed. We will re-run independent verification and confirm. If `In practice` can be populated on the six high-volume checks in the same round, that would resolve the largest remaining readability gap.
