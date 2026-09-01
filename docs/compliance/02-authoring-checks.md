# 02 - Authoring Checks

> **The extension contract.** How a team adds a check, what a check must
> declare, how IDs stay unique, and what a new check has to prove before it is
> allowed to fail anybody's release.
>
> **Prerequisites:** [00 - The Compliance Model](00-compliance-model.md), [01 - Check Catalog](01-check-catalog.md)

---

## 1. The shape of the extension point

A **policy pack** is a directory. It contains one or more manifests describing
the checks it owns. In the common case that is the whole pack - a check is
data, not code.

```
/etc/softwaregateway/policies/
├── sgw-baseline/                 the built-in pack, shipped with the binary
│   └── (compiled in; listed here so it appears in the same catalogue)
├── acme-platform/                a pack this organization wrote
│   ├── pack.yaml                 the manifest - identity, checks, assertions
│   ├── networking.yaml           more checks, same pack, split for readability
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

Because every expression is compiled when the pack loads, a typo in a check is a
**load error at start-up or on the next `fsnotify` event**, named with its file,
its check ID and its column - not a surprise on release 47.

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
      # any expression runs, and derives a `pass` for every applicable resource
      # the check did not report on. See 00 section 5.
      appliesTo:
        kinds: [StatefulSet]
        annotations:
          acme.example/quorum-size: "*"     # present, any value
        containers: none                    # all | main | init | none

      # THE ASSERTION. True means compliant.
      assert:
        expr: |
          pdbFor(self).spec.minAvailable >=
            (int(self.metadata.annotations["acme.example/quorum-size"]) / 2) + 1
        observed: 'string(pdbFor(self).spec.minAvailable)'
        locus: spec.minAvailable
        message: |
          "quorum size " + self.metadata.annotations["acme.example/quorum-size"] +
          " needs minAvailable >= " + string((int(self.metadata.annotations["acme.example/quorum-size"]) / 2) + 1)
```

Three things about that manifest earn their place:

- **Metadata lives in YAML, not in the check logic.** The UI lists 88 checks and
  explains each one without evaluating anything; the exporter writes the rulebook
  into the vendor's spreadsheet; a reviewer diffs a severity change in a
  four-line patch. In the existing sample policies this metadata is string
  literals scattered through the rules ([03](03-sample-policy-review.md) §2.1),
  where nothing can read it except the engine, and where the same category is
  spelled two ways in two files.
- **`appliesTo` is mandatory.** It is the only way the engine can emit passes and
  skips, and it is the difference between "40 workloads, all compliant" and a
  blank screen.
- **The author never writes a loop.** `appliesTo` selects the subjects; the
  engine iterates and binds each one to `self`. This is what makes the CronJob
  false negative in [03](03-sample-policy-review.md) §3.1 unrepeatable: reaching
  a `CronJob`'s pod spec is the engine's job, and it is written once.

## 3. The three layers

All three produce the same `Result`. Which one to reach for:

| Layer | Use when | Cost |
|---|---|---|
| `assert:` shorthand | "field X of kind Y must satisfy Z" - the majority of checks | A few lines of YAML |
| `assert.expr:` (CEL) | The condition needs arithmetic, a quantifier, a comparison across fields, or a helper function | One expression |
| `builtin` | The check needs render awareness, determinacy, the artifact tree, or another feature's stored result | Go code, a release of this binary |

The recommendation is: **write the YAML form first**, drop to CEL for the
condition, and reach for a built-in only when the check needs something the
input cannot see. A pack of YAML needs no rebuild of this platform and no
restart of the Coordinator.

### 3.1 The declarative shorthand

Common assertions do not need an expression at all. The shorthand compiles to
the same CEL, so there is exactly one evaluator:

```yaml
    - id: ACME-04
      title: Dataplane pods pin hugepages in requests and limits
      severity: block
      appliesTo:
        kinds: [Deployment, StatefulSet, DaemonSet, Job, CronJob]
        containers: all           # `self` is a container; `owner` is its workload
      assert:
        equalPaths: [resources.requests.hugepages-1Gi, resources.limits.hugepages-1Gi]
```

| Form | Asserts |
|---|---|
| `required: [path, …]` | Each path exists and is non-empty |
| `forbidden: [path, …]` | No path exists |
| `equals: {path: value}` | Exact match |
| `oneOf: {path: [v, …]}` | Value is in the set |
| `matches: {path: regexp}` | RE2 match, anchored unless the pattern says otherwise |
| `equalPaths: [a, b]` | Two paths hold the same value - the "requests == limits" family |
| `numeric: {path: {min, max}}` | Parsed as a Kubernetes quantity, then bounded |
| `expr: <CEL>` | Anything else |

A check may combine forms; all of them must hold. `observed`, `locus` and
`message` are derived from the shorthand when not given - `required` reports the
first missing path as the locus, `numeric` reports the offending value.

## 4. The CEL contract

[CEL](https://cel.dev) - the expression language Kubernetes itself uses for CRD
`x-kubernetes-validations` and ValidatingAdmissionPolicy - is the escape hatch.
An engineer who learns it here uses it again in admission control.

### 4.1 What is bound

| Name | Bound to |
|---|---|
| `self` | The subject: a resource, or a container when `appliesTo.containers` selects one |
| `owner` | The workload owning `self`, when `self` is a container |
| `address` | The subject's address - chart, chart version, artifact digest, source file, line |
| `chart` | The chart `self` came from: `name`, `version`, `appVersion`, `values` |
| `release` | `product`, `tag`, `packageDigest` |
| `config` | Site configuration: `approvedRegistries`, `probeBounds`, … |

`self` is one subject, never the whole release. A check cannot accidentally
judge a resource outside its own `appliesTo`, because it is never handed one.

### 4.2 Engine functions

Cross-resource work - the part Rego is genuinely better at, and the part the
existing `pdb.rego` got wrong three ways ([03](03-sample-policy-review.md) §3.3) -
is not re-implemented per check. The engine exposes it as CEL functions whose
implementations are Go, unit-tested once, and correct for every caller:

| Function | Returns |
|---|---|
| `pdbFor(workload)` | The PodDisruptionBudget selecting it, or a null-safe empty object |
| `servicesFor(workload)` | Services whose selector matches its pod labels |
| `endpointsOf(service)` | Workloads the Service selects - empty is NET-07 |
| `selects(selector, obj)` | Full label-selector semantics: `matchLabels` **and** `matchExpressions`, namespace-scoped |
| `crdFor(cr)` | The CustomResourceDefinition the release ships for a CR's `apiVersion`/`kind` |
| `quantity(v)` | A Kubernetes quantity as a number - `"1Gi"`, `"250m"`, `"0.5"` |
| `imageRef(s)` | `{registry, repository, tag, digest, hasDigest}` from an image reference |
| `resourcesIn(kinds)` | Release-wide lookup, for the few checks that need a set |
| `semver(a).lt(semver(b))` | Version comparison |

Adding a function is a platform change, deliberately: it is the point where a
new Kubernetes semantic gets one tested implementation instead of five
approximate ones.

### 4.3 What a check may not do

| Not allowed | How it is prevented |
|---|---|
| Network access | CEL has no I/O. There is nothing to disable. |
| Reading the filesystem | Same. Everything a check needs is bound. |
| Time | No `now()` is registered. A check whose answer changes at midnight is not reproducible; waiver expiry is evaluated by the engine, once, against the run's recorded time. |
| Unbounded evaluation | **Structural.** CEL is not Turing-complete: it has no recursion and no unbounded loop, and the compiler rejects a program whose cost estimate exceeds the configured budget. Termination is a property of the language, not a timeout the engine hopes fires in time. |

This row is the reason for the language choice. Under Rego these were four
capability flags and a deadline, each of which can be mis-set; under CEL the
first three do not exist and the fourth is refused at compile time
([design/23](../design/23-compliance.md) §4).

### 4.4 Type checking, honestly

Expressions are type-checked at load against a declared environment. For the
well-known Kubernetes kinds the engine registers typed schemas, so
`self.spec.replicaz` fails to load with the misspelling named. For a custom
resource with no schema in the release, `self` is a dynamic map: syntax, arity
and function signatures are still checked, but a wrong field path resolves to
absent at run time and the check reports `error` for that subject rather than a
silent pass. Absence is never a pass ([00](00-compliance-model.md) Rule 2).

### 4.5 Multi-subject checks

For the rare check whose subjects its `appliesTo` cannot express, a check may
declare `emitsResults: true` and provide a `subjects:` expression returning a
list. Choosing this turns **off** the derived-pass machinery for that check: it
now owns its whole denominator, and the catalogue marks it so a reader knows
which is which.

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
[source-standards.md](source-standards.md) - `SCH PDB PRB SEC RBAC CFG RES NET
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
4. **The good fixture is shared.** `test/fixtures/compliance/charts/good-app` is
   a chart that must produce zero failures across the entire baseline. Every new
   check runs against it, so "my check has no false positives" is asserted by CI
   rather than by its author.

Rules 3 and 4 are what separate this from a policy directory that accumulates.

## 7. What is deliberately not here

> **Decision - no Rego engine.**
>
> *The alternative:* ship the organization's sixteen existing `.rego` files as
> the baseline and embed OPA as the evaluator.
>
> *Why not:* three reasons, in order of weight.
>
> **Dependency cost, measured** against this repository at the commit that
> introduced compliance, by two counts that answer different questions:
>
> | | linked into the binary | in the module graph |
> |---|---|---|
> | `cel.dev/cel-go` | **+4** modules | **+3** |
> | `github.com/open-policy-agent/opa/rego` | **+18** modules | **+59** |
>
> The first column is `go list -deps` - what is actually compiled. The second is
> `go list -m all` - what a supply-chain audit of this repository has to
> enumerate, and where OPA brings a WebAssembly runtime (`wazero`), an embedded
> key-value store (`badger` + `ristretto`), `secp256k1`, `blake256` and two
> separate Levenshtein implementations. cel-go's four are itself, its expression
> protos, the ANTLR runtime and `golang.org/x/exp`.
>
> A tool whose purpose is telling people what is inside their software does not
> quietly add a wasm runtime to itself - the same argument `internal/export`
> already makes for writing XLSX by hand.
>
> **Robustness.** CEL is non-Turing-complete, so bounded evaluation is a
> guarantee rather than a timeout (§4.3). Errors surface when the pack loads,
> not on the release where the branch is first taken.
>
> **Direction.** CEL is what Kubernetes chose for CRD validation rules and
> ValidatingAdmissionPolicy, and what Kyverno moved its policy model to. Rego is
> a second language that buys nothing here.
>
> *What is lost:* Rego's set logic across resources. That is exactly what the
> existing `pdb.rego` and `network_policy.rego` got wrong
> ([03](03-sample-policy-review.md) §3.3, §3.4), so it moves into tested Go and
> is exposed as CEL functions (§4.2) - reachable from a YAML pack, without a
> rebuild.
>
> *What keeps the door open:* `engine:` is a field on every check and the
> evaluators sit behind one interface. If the organization later needs Rego for
> a case CEL cannot express, adding `engine: rego` is a package behind a
> depguard rule, not a migration - no existing pack changes, and no stored
> result means anything different.

Also absent, and for a stated reason:

- **No check-level enable/disable through the API.** Whether a check runs is
  configuration; it lives in Git beside the pack. An API that could switch off a
  blocking check is an approval process with no reviewer - the same argument
  that keeps product configuration read-only over the API
  ([design/02](../design/02-configuration.md) §1).
- **No per-product severity overrides.** One organization, one severity per
  check. A vendor whose chart legitimately needs an exception gets a waiver -
  scoped, justified, approved and expiring ([00](00-compliance-model.md) §7) -
  rather than a quietly softened rule that then applies to everybody.
