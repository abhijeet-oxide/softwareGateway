# 03 - Review of the Existing Rego Policies

> **What the sixteen policies in [sample-policies/](sample-policies/) get right,
> what they get wrong, and what each becomes.** Written because they were handed
> over with "I'm not sure how accurate or how good they were", and because a
> plan that adopts them without reading them would inherit the defects silently.
>
> **Prerequisites:** [00 - The Validation Model](00-validation-model.md), [01 - Check Catalog](01-check-catalog.md), [02 - Authoring Checks](02-authoring-checks.md)

---

## 1. The verdict in one paragraph

The policies are a good body of *domain knowledge* and a poor *reporting
contract*. The individual assertions are largely the right assertions - they
name real defects, they carry rationale and remediation, and someone who has
operated Kubernetes wrote them. What they cannot do is produce the report this
feature needs: no check has an ID, none can report a pass, none can say a check
did not apply, none addresses a finding to a chart or a file, and several
silently check nothing at all for whole categories of workload. Roughly
**70% of the logic is worth carrying forward and 100% of the contract has to
change.**

Concretely, of the sixteen files: **11** become baseline checks with their logic
largely intact, **4** have to be rewritten because their central rule is wrong or
contradicts the organization's own catalog, and **1** is dropped from tier 1
because what it checks is a deployment decision rather than a property of the
artifact. Section 5 has the file-by-file disposition.

## 2. Structural defects - true of all sixteen

These are not bugs in any one file. They are properties of the contract the
files were written against, and each one is a reason
[02](02-authoring-checks.md) defines a different one.

### 2.1 No check has an identity

Every violation carries a `_category` string and nothing else. There is no ID.

The consequences compound:

| Without an ID | Consequence |
|---|---|
| A finding cannot be waived | Every exception is a code edit to the policy |
| A finding cannot be tracked across releases | "Did the vendor fix it in 4.3.0?" is unanswerable |
| A finding cannot be cited in a ticket | The vendor gets a message string, which changes when someone improves the wording |
| Two runs cannot be diffed | Nothing is stable enough to join on |

And the categories themselves are ad hoc per file: `pdb.rego` says
`"Pod Disruption Budget (PDB)"`, `probes.rego` says `"Reliability/HA"`,
`resource_limits.rego` says `"Configuration"` - which is not the category
[custom-validation.md](custom-validation.md) files resource requests under.
There is no controlled vocabulary, so grouping the report by category produces a
different taxonomy than the organization's own document.

**Becomes:** the ID scheme in [02](02-authoring-checks.md) §5, with the source
catalog's own thirteen prefixes, and category as a closed set.

### 2.2 `violations` only - the central flaw

A policy that emits only violations cannot express three of the four outcomes
in [00](00-validation-model.md) Rule 2. There is no way for it to say *this
workload was checked and is fine*, *there was nothing here to check*, or *this
check could not be decided*. All three render as silence.

For a release-gating tool this is disqualifying: silence is exactly what a
completely broken run also produces.

**Becomes:** the `appliesTo` denominator plus derived passes
([00](00-validation-model.md) §5). This is the reason that mechanism exists, and
it is what lets these sixteen files be adopted almost unchanged - **the
violation logic stays; the manifest supplies what was missing.**

### 2.3 Severity is a property of the finding, not of the check

`"severity": "fail" | "warn" | "info"` is written inside each violation object.
So severity is only knowable by running the policy, one rule can emit two
severities, and the catalogue cannot list "what does this check do and how
serious is it" without evaluation. It also conflates *outcome* with *severity* -
`"fail"` is doing both jobs.

**Becomes:** two orthogonal fields, severity declared in the manifest
([00](00-validation-model.md) Rule 3).

### 2.4 The address stops at kind/namespace/name

```rego
"resource": {"kind": …, "namespace": …, "name": …}
```

Missing: which chart, which chart version, which artifact digest, which source
file, which container, which field. The container name and the observed value
exist only as prose inside `msg`, so they cannot be columns in a spreadsheet,
cannot be filtered on, and cannot be grouped by chart - which is the primary
grouping the release page needs.

**Becomes:** the address is handed to the policy in `__address` and echoed back
([02](02-authoring-checks.md) §4.1). A policy author cannot omit it and cannot
get it wrong.

### 2.5 The input shape is a live cluster's, not an artifact's

The documented contract is `input.manifests`, `input.namespace`, and
`input.metadata.helm_releases` / `helm_repositories` - the shape of a tool that
reads a running cluster and its Helm release history. Two mismatches matter:

- **`input.namespace`** presumes one target namespace. A release here is dozens
  of charts that will land in several namespaces, and the namespace is not known
  at validation time at all.
- **`input.manifests`** is flat. Nothing says which chart a manifest came from,
  which is precisely the information the report is built around.

**Becomes:** the release-shaped input in [02](02-authoring-checks.md) §4.1 -
`charts`, `resources` with `__address`, `images`, and no ambient namespace.

### 2.6 No tests, and no fixtures

Sixteen policy files, zero test data. Nothing establishes that a policy fires on
a bad chart, and - far more important - nothing establishes that it *does not*
fire on a good one. Section 4 shows what that costs.

**Becomes:** the two-fixture rule, exact-set expectations, the shared
`good-app` chart, and the meta-test that fails CI when a registered check has no
coverage ([02](02-authoring-checks.md) §6).

## 3. Defects in specific policies

Found by reading; each is reproducible against the file named.

### 3.1 CronJob is declared and never actually checked - 7 files

Ten files list `CronJob` in their workload kinds. Only `image_registry.rego`
walks `spec.jobTemplate.spec.template.spec`. The other nine reach the pod spec
through `spec.template.spec`, which **does not exist on a CronJob**. For two of
them - `labels.rego` and `default_namespace.rego` - that is harmless, because
they only read `metadata`. For the other seven it means the check never runs:

```rego
# resource_limits.rego
_pod_containers(manifest) := array.concat(
    object.get(manifest, ["spec", "template", "spec", "containers"], []),
    object.get(manifest, ["spec", "template", "spec", "initContainers"], []),
)
```

For a `CronJob` both `object.get` calls return their default `[]`, the concat is
empty, `some container in []` never matches, and **no violation is ever
produced**. In `high_uid.rego` the same shape fails one step earlier -
`_pod_spec` is undefined for a CronJob, so every rule body that depends on it
fails silently.

Affected: `resource_limits`, `high_uid`, `security_context`, `seccomp`,
`automount_token`, `image_pull_policy`, `configmap_hygiene`. Every CronJob in
every chart passes all seven, including the ones that would otherwise block.

**This is the exact failure mode [00](00-validation-model.md) Rule 2 exists to
prevent, and it is here in the existing policies today.** A `violations`-only
engine reports "no findings" identically whether the CronJob is compliant or
whether the traversal never reached it. With an `appliesTo` denominator, the
same bug is visible immediately: the applicable set contains the CronJob, the
policy reported nothing about it, and it would be counted as a pass - which is
why the baseline's container traversal is one shared, tested helper rather than
a stanza copied into every file.

### 3.2 `probes.rego` - four rules, two of them noise, and one contradiction

- **`_workload_kinds` is `{"Deployment", "StatefulSet"}`.** A `DaemonSet`
  without a readiness probe - a CNI agent, a node exporter, exactly the
  workloads where readiness matters most in a CNF estate - is never checked.
- **The fourth rule fires on every container that has a liveness probe with
  `initialDelaySeconds <= 30` and no startup probe**, at `info`, with the
  comment "This is purely informational for awareness". That is one finding per
  container across the whole release for a condition that is not a defect. The
  third and fourth rules together cover the complete space of "has liveness, no
  startup", so every such container produces a finding no matter what.
- **A missing `livenessProbe` is a `warn`.** [custom-validation.md](custom-validation.md)
  does not require a liveness probe at all; PRB-03 requires that *if* one
  exists, it is less sensitive than readiness. The policy asks for the opposite
  of the organization's own standard, and following it makes clusters less
  stable, not more: a liveness probe added to satisfy a linter, sharing the
  readiness endpoint, restarts pods during a dependency's slow afternoon.

**Becomes:** PRB-01 (narrowed to traffic-receiving containers, `DaemonSet`
included), PRB-03 (the sensitivity comparison, as the catalog actually states
it), PRB-02 (the `initialDelaySeconds > 30` signal only). The `info`-on-everything
rule is dropped.

### 3.3 `pdb.rego` - the matching rule is wrong in three ways

The most valuable category in the catalog and the file with the most defects.

```rego
_has_pdb_for(wl_name, wl_manifest) if {
    wl_labels := object.get(wl_manifest, ["spec", "template", "metadata", "labels"], {})
    some pdb in input.manifests
    pdb.kind == "PodDisruptionBudget"
    pdb_sel := object.get(pdb, ["spec", "selector", "matchLabels"], {})
    count(pdb_sel) > 0
    _labels_subset(pdb_sel, wl_labels)
}
```

It gets the important part right - it compares against the **pod template's**
labels, not the controller's, which is the thing most implementations get wrong.
Then:

- **`matchExpressions` is ignored.** A PDB written with
  `matchExpressions: [{key: app, operator: In, values: [mysvc]}]` has an empty
  `matchLabels`, fails `count(pdb_sel) > 0`, and the workload it protects is
  reported as unprotected. A false positive on a correct chart, which is the
  most expensive kind.
- **Namespaces are not compared.** A PDB in namespace `a` is accepted as
  protecting a workload in namespace `b`. A false negative, and one that gets
  more likely the more charts a release has.
- **`DaemonSet` is absent** from `_ha_kinds`, so no DaemonSet is ever considered.

Three further problems:

- **`_pdb_targets`, `_ha_workloads` and `_single_replica_workloads` are defined
  and never referenced** - each appears exactly once in the file. `_pdb_targets`
  is the more troubling one: it encodes the idea that a PDB targets a workload
  when *any selector value equals the workload's name*, which is not how label
  selectors work. It reads like an earlier, wrong implementation left in place.
- **The deadlock check covers one of four spellings.** Only
  `maxUnavailable == 0`. `"0%"`, `minAvailable == replicas` and
  `minAvailable: "100%"` all pass ([01](01-check-catalog.md) §3.2).
- **`replicas` defaults to `1` via `object.get`.** In a chart where replicas
  come from values - which is nearly all of them - every workload looks
  single-replica, so the HA-without-PDB rule almost never fires and the
  "single replica with a PDB" rule fires on workloads that are not single
  replica. This is precisely the problem determinacy solves
  ([00](00-validation-model.md) Rule 4), and without it the policy's central
  rule is close to inert on real charts.
- **Severity contradicts the catalog.** HA workload without a PDB is `warn`
  here and `BLOCK` in [custom-validation.md](custom-validation.md).

**Becomes:** PDB-01, PDB-02, PDB-03, PDB-09, specified in
[01](01-check-catalog.md) §3.1-3.2. The pod-template-labels insight is kept; the
selector evaluation is replaced with full label-selector semantics, and the
structural form ("this chart ships no PDB template at all") is what carries the
blocking severity.

### 3.4 `network_policy.rego` - checks existence, not policy

Two problems, one mechanical and one conceptual.

```rego
all_workloads := [m | some m in input.manifests; m.kind in _workload_kinds]
manifest == all_workloads[0]
```

That is a "fire only once" hack that depends on the order of `input.manifests`.
It works, and it means the finding is attached to whichever workload happens to
be first - so the finding's `resource` field points at an arbitrary object that
has nothing to do with the problem. Under [00](00-validation-model.md) Rule 1 a
release-scoped finding is addressed to the release, not smuggled onto a resource.

The conceptual problem is larger: it asserts only that **some** NetworkPolicy
exists. A chart shipping a single `allow-all` policy passes. The catalog asks
for a *default-deny* (NET-01) and for allow rules that are *explicit* (NET-02),
and neither is checked.

**Becomes:** NET-01 (default-deny existence, release-scoped), NET-02 (no
allow-from-all), NET-03 (egress enumerated), NET-04 (stable selectors).

### 3.5 `default_namespace.rego` - misses the case it exists for

```rego
_effective_namespace(manifest) := ns if { ns := manifest.metadata.namespace } else := ""
…
ns == "default"
```

A rendered Helm chart normally carries **no** `metadata.namespace` at all - the
namespace comes from `helm install -n` or from the GitOps layer. `_effective_namespace`
returns `""` for those, `""` is not `"default"`, and the check passes. So it
fires only on manifests that literally say `namespace: default`, which is the
rare case, and misses every chart that will land in `default` because nobody
specified anything.

More fundamentally, the check does not belong at tier 1 at all: **which
namespace a release lands in is a deployment decision, not a property of the
artifact.** A vendor chart that omits `metadata.namespace` is doing the right
thing.

**Becomes:** dropped as a tier-1 check. The genuinely artifact-level part -
"resources hard-code a `metadata.namespace`, so this chart cannot be installed
into a namespace of the operator's choosing" - is the inverse assertion and is
worth `warn`; it is folded into the CFG group as a portability finding.

### 3.6 `labels.rego` - right list, wrong subjects

The six labels are the right six. But it checks only `metadata.labels` on the
controller. MTA-01 requires them on **controllers, pod templates and Services** -
and the pod template is the one that matters, because it is the pod's labels that
Services, PDBs, NetworkPolicies and spread constraints select on.

It also has no relationship to MTA-03, which forbids `app.kubernetes.io/version`
appearing in a *selector*. Requiring a label everywhere without forbidding it in
selectors is how a chart ends up with an immutable `Deployment.spec.selector`
containing a version, which cannot be upgraded - only deleted and recreated.

**Becomes:** MTA-01 (three subjects) and MTA-03 (the selector prohibition), which
are only correct as a pair.

### 3.7 `resource_limits.rego` - correct, and the model for the rest

Included because it is the one to copy. It walks `containers` **and**
`initContainers`, which most implementations forget and which is the case that
actually distorts scheduling. Its only defects are the shared ones: the CronJob
traversal (§3.1), no ID, no passes.

**Becomes:** RES-01 and RES-02, essentially unchanged apart from the contract.

## 4. What "no fixtures" cost

Of the specific defects above, these would have been caught by a single fixture
each - which is why [02](02-authoring-checks.md) §6 makes fixtures the gate
rather than a recommendation:

| Defect | Fixture that catches it |
|---|---|
| CronJob traversal (7 files) | Any chart with a `CronJob` that violates the rule and is expected to fail |
| `matchExpressions` PDB | A compliant chart whose PDB uses `matchExpressions` - run against `good-app`, this is a false positive and CI goes red |
| Cross-namespace PDB match | A two-namespace chart |
| `default_namespace` missing the common case | A chart with no `metadata.namespace` anywhere - which is *most* charts |
| `probes.rego` info-on-everything | `good-app`, which must produce zero findings and would produce one per container |
| `network_policy` allow-all passes | A chart whose only NetworkPolicy is `podSelector: {} , ingress: [{}]` |

Five of the six are caught by the shared good chart alone. That is the argument
for rule 4 in [02](02-authoring-checks.md) §6 in one table.

## 5. Disposition

| File | Disposition | Becomes |
|---|---|---|
| `resource_limits.rego` | **Adopt** - logic sound | RES-01, RES-02, RES-03 |
| `rbac.rego` | **Adopt** - wildcard and escalation logic sound | RBAC-03, RBAC-05, RBAC-06, RBAC-07 |
| `security_context.rego` | **Adopt** - the largest and most complete file | SEC-02, SEC-03, SEC-04, SEC-07, SEC-08 |
| `seccomp.rego` | **Adopt** | SEC-06 |
| `high_uid.rego` | **Adopt**, minus the UID ≥ 10000 rule - that is an OpenShift SCC convention, not a general standard, and it is the arbitrary-UID property (SEC-09) that actually matters | SEC-01 |
| `automount_token.rego` | **Adopt with a refinement** - the exemption becomes derived ("unless the SA is bound to a Role") rather than assumed | RBAC-01, RBAC-02 |
| `image_registry.rego` | **Adopt** - and it is the only file that handles `jobTemplate`; its traversal becomes the shared one | SUP-01, SUP-02 |
| `image_pull_policy.rego` | **Adopt**, folded in beside SUP-01: the pull policy finding is only meaningful next to the tag finding | SUP-01 (paired finding) |
| `configmap_hygiene.rego` | **Adopt**, with the detection rule tightened per [01](01-check-catalog.md) §3.4 | CFG-01, CFG-03, CFG-06, CFG-07 |
| `storage.rego` | **Adopt** - accessModes and size assertions sound | STO-01, STO-02, STO-04 |
| `reliability.rego` | **Adopt** the RollingUpdate half | PDB-05, PDB-06 |
| `labels.rego` | **Rewrite** - right list, wrong subjects (§3.6) | MTA-01, MTA-03 |
| `probes.rego` | **Rewrite** - contradicts the catalog, misses DaemonSets, emits noise (§3.2) | PRB-01, PRB-02, PRB-03 |
| `pdb.rego` | **Rewrite** - selector matching wrong in three ways, deadlock check incomplete (§3.3) | PDB-01, PDB-02, PDB-03, PDB-09 |
| `network_policy.rego` | **Rewrite** - checks existence, not policy (§3.4) | NET-01, NET-02, NET-03 |
| `default_namespace.rego` | **Drop** at tier 1; keep the inverse assertion (§3.5) | a portability `warn` under CFG |

## 6. Why the baseline is Go rather than these files

The disposition above adopts the *logic* of eleven files and none of the *code*.
That is a deliberate choice and it deserves stating, because "we already have
Rego, use it" is the cheaper answer.

Three of the four things the baseline has to do are things a Rego policy cannot
see:

| Needed by the baseline | Available to a Rego policy |
|---|---|
| Determinacy - was this value fixed by the template or defaulted from values | No. It is established by comparing two renders, outside any single evaluation |
| The artifact tree - which chart artifact, which digest, which OCI ref | Only as data the engine hands in, and only because the engine constructed it |
| Another feature's stored result - the security scan, the signature verification (SUP-03, SUP-04, SUP-05) | No |
| Label-selector semantics, `IntOrString`, quantity parsing, OCI reference parsing | Expressible, but re-implemented per policy and wrong in a different way each time - §3.3 is that happening |

The last row is the practical one. `matchExpressions`, `"0%"` versus `0`, and
`registry.example.com:5000/app:tag` are all places where a shared, tested Go
helper is right once and a per-policy Rego implementation is wrong repeatedly.
The baseline is where the organization's standards are enforced hardest, so it
gets the implementation that can be unit-tested against the Kubernetes semantics
it is modelling.

**Rego remains the extension path, and it is a first-class one** -
[02](02-authoring-checks.md) exists for it, packs load without rebuilding this
platform, and the input hands policies the same addresses and the same helpers'
output that the baseline uses. What changed is that the sixteen files are read
as a *specification of what to check*, which is what they are good at, rather
than as an implementation to inherit.
