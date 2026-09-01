# 02 - Authoring Checks

> **The extension contract.** How a team adds a check, what a check must
> declare, how IDs stay unique, and what a new check has to prove before it is
> allowed to fail anybody's release.
>
> **Prerequisites:** [00 - The Validation Model](00-validation-model.md), [01 - Check Catalog](01-check-catalog.md)

---

## 1. The shape of the extension point

A **policy pack** is a directory. It contains one manifest describing the checks
it owns, and the code implementing them.

```
/etc/softwaregateway/policies/
├── sgw-baseline/                 the built-in pack, shipped with the binary
│   └── (compiled in; listed here so it appears in the same catalogue)
├── acme-platform/                a pack this organization wrote
│   ├── pack.yaml                 the manifest - identity, checks, metadata
│   ├── pdb_quorum.rego
│   ├── secondary_networks.rego
│   └── testdata/
│       ├── quorum-ok/            fixtures proving each check fires correctly
│       └── quorum-deadlock/
└── acme-waivers.yaml             accepted exceptions, with expiry
```

The Coordinator discovers this directory on start and on change, exactly the way
it discovers product configuration ([design/02](../design/02-configuration.md) §3):
a mounted volume watched with `fsnotify`, no Kubernetes API, no restart needed,
and the same code path against a plain directory in local development.

**A pack that fails to load does not stop the others.** It is recorded with the
reason, surfaced in the API and shown in the UI's policy list as broken, and the
checks it owns report `error` rather than silently disappearing - fail-closed per
pack, the same rule product configuration uses per product. A missing check that
looks like a passing check is the failure mode this whole design exists to
prevent.

## 2. The manifest

```yaml
apiVersion: softwaregateway.io/v1alpha1
kind: PolicyPack
metadata:
  name: acme-platform
  # The ID prefix this pack OWNS. Registering two packs that claim the same
  # prefix is a load error for the second one, named in the message. This is
  # what makes an ID globally unique without a central registry.
  prefix: ACME
  version: 1.4.0
  description: Acme platform-engineering standards, beyond the shipped baseline.
  maintainer: platform-sre@acme.example
  reference: https://wiki.acme.example/platform/standards

spec:
  checks:
    - id: ACME-01
      title: Quorum workloads keep a majority available during a drain
      # WHAT IT ASSERTS, in the words the report will use. Not a restatement of
      # the title - this is what a vendor engineer reads to understand what was
      # required of them.
      description: >
        A workload annotated acme.example/quorum-size must be covered by a
        PodDisruptionBudget whose minAvailable is at least floor(size/2)+1, so
        that a node drain cannot take the cluster below quorum.
      # WHY. This is what stops a check being cargo-culted forward after the
      # reason for it has gone, and it is printed in the vendor report.
      rationale: >
        Etcd-style quorum members lose the cluster, not just a replica, when a
        drain evicts one too many. Observed in lab during a rolling node update
        in 2026-02: three of five members evicted concurrently because the PDB
        allowed maxUnavailable 2.
      severity: block            # block | warn | info
      tier: 1
      category: Disruption & Availability
      reference: https://wiki.acme.example/platform/standards#quorum
      remediation: >
        Set spec.minAvailable on the PodDisruptionBudget to floor(size/2)+1 and
        remove maxUnavailable.

      # THE DENOMINATOR. The engine computes the applicable set from this before
      # any policy code runs, and derives a `pass` for every applicable resource
      # the policy did not report on. See 00 section 5.
      appliesTo:
        kinds: [StatefulSet]
        annotations:
          acme.example/quorum-size: "*"     # present, any value
        containers: none                    # all | main | init | none

      engine: rego
      rego:
        package: acme.platform.quorum
        rule: violations                    # violations | results
```

Two things about that manifest earn their place:

- **Metadata lives in YAML, not in the policy code.** The UI lists 88 checks and
  explains each one without evaluating anything; the exporter writes the rulebook
  into the vendor's spreadsheet; a reviewer diffs a severity change in a
  four-line patch. In the existing sample policies this metadata is string
  literals scattered through the rules ([03](03-sample-policy-review.md) §2.1),
  where nothing can read it except the engine, and where the same category is
  spelled two ways in two files.
- **`appliesTo` is mandatory.** It is the only way the engine can emit passes and
  skips, and it is the difference between "40 workloads, all compliant" and a
  blank screen.

## 3. The three engines

All three produce the same `Result`. Which one to reach for:

| Engine | Use when | Cost |
|---|---|---|
| `builtin` | The check is in the shipped baseline, or needs render awareness, determinacy, or cross-artifact knowledge the platform holds | Go code, a release of this binary |
| `rego` | The organization already writes Rego, or the check needs set logic across resources | A `.rego` file in the pack |
| `yaml` *(deferred - §7)* | The check is "field X of kind Y must satisfy Z" | Nothing but the manifest |

The recommendation is deliberate and modest: **write it in Rego first.** The
organization already has sixteen policies and the skill to maintain them, and a
Rego pack needs no rebuild of this platform. Reach for a built-in only when the
check needs something Rego cannot see - determinacy, the artifact tree, or
another feature's stored result.

## 4. The Rego contract

### 4.1 Input

Every evaluation receives one release, already rendered:

```json
{
  "release": {
    "product": "vendor-a-platform",
    "tag": "orb_23.8.1076",
    "packageDigest": "sha256:9f2c…"
  },
  "charts": [
    { "name": "mysvc", "version": "4.2.1", "appVersion": "4.2.1",
      "artifactDigest": "sha256:41ab…", "artifactRef": "charts/mysvc",
      "renderStatus": "ok", "values": { … the chart's default values … } }
  ],
  "resources": [
    {
      "apiVersion": "apps/v1",
      "kind": "Deployment",
      "metadata": { … },
      "spec": { … },
      "__address": {
        "chart": "mysvc", "chartVersion": "4.2.1",
        "artifactDigest": "sha256:41ab…",
        "sourceFile": "templates/deployment.yaml",
        "renderedLine": 1
      }
    }
  ],
  "images": [ { "ref": "reg.example.com/mysvc@sha256:…", "foundAt": "…", "__address": { … } } ],
  "config": { "approvedRegistries": [ … ], "probeBounds": { … } }
}
```

**`__address` is handed in, and the policy echoes it back.** This is the
mechanism that makes [00](00-validation-model.md) Rule 1 structural rather than a
convention an author has to remember. No policy ever constructs a chart name, a
source file or an artifact digest - it cannot get them wrong, and it cannot
forget them.

`input.resources` is the whole release, not one chart. Cross-chart checks
(PDB-01, NET-07, CFG-11, UPG-07) need it, and a policy that wants one chart
filters on `__address.chart`.

### 4.2 Output - the `violations` form

The form every existing policy in this repository already uses, and the one to
write by default:

```rego
package acme.platform.quorum
import rego.v1

violations contains v if {
    some r in input.resources
    r.kind == "StatefulSet"
    size := to_number(r.metadata.annotations["acme.example/quorum-size"])
    pdb := _pdb_for(r)
    required := floor(size / 2) + 1
    object.get(pdb, ["spec", "minAvailable"], 0) < required

    v := {
        "subject":  r.__address,                 # echoed, never constructed
        "resource": {"apiVersion": r.apiVersion, "kind": r.kind,
                     "namespace": object.get(r.metadata, "namespace", ""),
                     "name": r.metadata.name},
        "locus":    "spec.minAvailable",
        "observed": sprintf("%v", [object.get(pdb, ["spec", "minAvailable"], "unset")]),
        "expected": sprintf(">= %d", [required]),
        "message":  sprintf("%s/%s declares quorum size %d but its PDB allows the cluster to fall below %d available",
                            [r.kind, r.metadata.name, size, required]),
    }
}
```

The engine supplies everything else: check ID, severity, tier, category,
remediation and reference come from the manifest; determinacy comes from the
probe render; **passes come from `appliesTo` minus these violations**; and a
`skip` is emitted when `appliesTo` matched nothing.

A violation for a resource outside the check's own `appliesTo` set is a **load-time
warning and a run-time error**, not a silently accepted result. A check that
judges things it did not declare cannot produce a correct denominator, and a
wrong denominator is a wrong pass.

### 4.3 Output - the `results` form

For the rare check that must report a `pass` or a `skip` its `appliesTo` cannot
express - a per-container assertion where the container list is data, say:

```rego
results contains r if {
    some c in _containers
    r := {"outcome": "pass", "subject": c.__address, "resource": {…}, "locus": "…"}
}
```

`outcome` is required and must be one of `pass`, `fail`, `skip`, `error`. Choosing
this form turns **off** the derived-pass machinery for that check: the policy now
owns its whole denominator, and the manifest must say `emitsPasses: true` so a
reader of the catalogue knows which is which.

### 4.4 What a policy may not do

| Not allowed | Why |
|---|---|
| Network access (`http.send`) | A check that reaches the internet is not reproducible and is a data-exfiltration path for a policy nobody read carefully. Disabled in the Rego capabilities. |
| Reading the filesystem | Same. Everything a check needs is in `input`. |
| Time (`time.now_ns`) | A check whose answer changes at midnight is not reproducible. Waiver expiry is evaluated by the engine, once, and the evaluation time is recorded on the run. |
| Unbounded recursion or comprehension over the cross product of all resources | A 200-chart release makes an accidental O(n²) into a run that never finishes. The evaluation is bounded by a per-check timeout and the pack is reported as slow. |

These are enforced by the OPA capabilities set the evaluator is built with, not
by review.

## 5. Check IDs

```
<PREFIX>-<NN>            SEC-01, PDB-02, ACME-14
```

| Rule | Enforced |
|---|---|
| A prefix belongs to exactly one pack | Load rejects the second pack claiming it, naming the first |
| An ID is unique within its pack | Load rejects the pack |
| An ID is **permanent** | By convention and review. It appears in waivers, in exported spreadsheets, and in vendor tickets that outlive the release |
| A check whose meaning changes gets a **new ID** | Convention. Tightening `PRB-05`'s bounds is an edit; changing it to assert something else is `PRB-12` |
| A retired check keeps its ID reserved | The manifest marks it `deprecated: true` with a `supersededBy`; it stops running and the catalogue still explains what it used to mean |

The baseline pack owns the thirteen prefixes from
[custom-validation.md](custom-validation.md) - `SCH PDB PRB SEC RBAC CFG RES NET
STO OBS MTA SUP UPG` - so the IDs in the organization's existing document are the
IDs in the tool, and nobody has to translate.

## 6. What a new check must prove

A check that fires on something correct is worse than no check: it costs a
vendor's engineering time, and the second false positive is when people stop
reading the report. So the bar is mechanical.

**Every check ships with at least two fixtures and their expected results.**

```
testdata/
├── quorum-ok/                    a chart that must produce a PASS
│   ├── chart/…
│   └── expected.yaml
└── quorum-deadlock/              a chart that must produce exactly one FAIL
    ├── chart/…
    └── expected.yaml
```

```yaml
# expected.yaml - an exact set, not a minimum
results:
  - check: ACME-01
    outcome: fail
    kind: StatefulSet
    name: etcd
    locus: spec.minAvailable
    determinacy: fixed
```

Four rules turn that into a real gate:

1. **Exact set equality.** Not "contains". A check that also fires on the
   `ConfigMap` in the fixture fails its own test.
2. **Every fixture is run against the *whole* baseline pack**, not just its own
   check. A new check that makes an unrelated one fire is caught here, which is
   the only place that interaction is ever visible.
3. **A meta-test enumerates registered checks and fails if any has no
   positive-and-negative fixture.** A check cannot reach production untested,
   and the coverage table in CI is the list.
4. **The good fixture is shared.** `test/fixtures/validation/charts/good-app` is
   a chart that must produce zero failures across the entire baseline. Every new
   check runs against it, so "my check has no false positives" is asserted by CI
   rather than by its author.

Rules 3 and 4 are what separate this from a policy directory that accumulates.

## 7. What is deliberately not here yet

> **Decision - no YAML check DSL in the first release.**
>
> *The idea:* most checks are "field X of kind Y must satisfy Z", and a
> declarative form - selector, JSON path, assertion, metadata - would let an SRE
> add a check with no Rego and no Go.
>
> *Why it is deferred rather than rejected:* it is a language, and languages
> acquire conditionals, then joins, then a way to reach another resource, and
> then they are Rego with worse error messages. Building one before the
> organization has felt where Rego actually hurts would be designing against a
> guess.
>
> *What keeps the door open:* `engine:` is already a field in the manifest and
> the engines sit behind one interface. Adding `engine: yaml` is a package, not
> a migration - no existing pack changes, and no stored result means anything
> different.
>
> *What would decide it:* if, after the baseline is in use, most new packs are
> Rego files that are structurally identical apart from a field path, the DSL is
> worth building. If they are not, it was never the barrier.

Also absent, and for a stated reason:

- **No check-level enable/disable through the API.** Whether a check runs is
  configuration; it lives in Git beside the pack. An API that could switch off a
  blocking check is an approval process with no reviewer - the same argument
  that keeps product configuration read-only over the API
  ([design/02](../design/02-configuration.md) §1).
- **No per-product severity overrides.** One organization, one severity per
  check. A vendor whose chart legitimately needs an exception gets a waiver -
  scoped, justified, approved and expiring ([00](00-validation-model.md) §7) -
  rather than a quietly softened rule that then applies to everybody.
