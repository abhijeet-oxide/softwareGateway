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
      # THE MECHANISM, in the words an engineer uses for it, and the vocabulary
      # this check is findable by. The title above deliberately contains none of
      # these words - that is what makes it readable by somebody who is not a
      # Kubernetes engineer - so without them the check is invisible to the
      # person who has to fix it. See section 2.2.1.
      subcategory: PodDisruptionBudget
      keywords: [PodDisruptionBudget, PDB, minAvailable, quorum, eviction, "node drain"]
      reference: https://wiki.acme.example/platform/standards#quorum

      # THE TRIAGE BLOCK. A severity says how much this organization cares. It
      # does not say what anybody should do, and a report of severities alone
      # is a list somebody forwards rather than a list somebody works through.
      # See section 2.1.
      confidence: confirmed      # confirmed | probable | needs-review
      whenItBites: node-maintenance
      fixOwner: chart-template   # who changes something
      fixEffort: low
      remediation: >
        Set spec.minAvailable on the PodDisruptionBudget to floor(size/2)+1 and
        remove maxUnavailable.
      # THE FIX, not a description of it. Prose about a fix and the lines that
      # ARE the fix are not the same artifact, and only one gets applied.
      fixExample: |
        spec:
          minAvailable: 3        # floor(5/2) + 1

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

- **Metadata lives in YAML, not in the check logic.** The UI lists ninety-nine checks and
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

## 2.1 The triage block, and why it is not optional

Five fields, all validated at load, all printed in the vendor report and shown
in the finding drawer.

| Field | Values | What it answers |
|---|---|---|
| `confidence` | `confirmed`, `probable`, `needs-review` | Does the tool KNOW this, or is it inferring? |
| `whenItBites` | `install`, `upgrade`, `node-maintenance`, `under-load`, `on-failure`, `continuously` | When does the consequence actually arrive? |
| `fixOwner` | `chart-template`, `chart-values`, `application`, `build-pipeline`, `platform-team`, `needs-decision` | Who changes something? |
| `fixEffort` | `low`, `medium`, `high` | How much work is it? |
| `fixExample` | YAML | What does the corrected configuration look like? |

**`confidence` carries a rule with teeth.** A check declaring `needs-review`
may not be `severity: block`, and the loader refuses one that is - see
`Check.Validate`. The reason is the single largest category of unproductive
argument about a compliance report: the tool asserts as a defect something a
vendor chose deliberately and correctly for that workload, and one of those is
enough to cost the whole report its credibility. A check that says in its own
metadata that somebody has to look at the workload cannot also decide the
verdict on its own.

**`fixOwner` has no "unknown".** It replaces a column that was headed `Owner`,
held something else entirely, and read `Could not be established` on roughly a
third of the rows of a real report. Where ownership genuinely depends on the
site, the value is `needs-decision` and the `rationale` names who decides -
which is a sentence somebody can act on.

## 2.2 The language standard

Every check is written for two readers who need different things from the same
row, and §2.2.1 is how the second one gets what they need without the first one
losing it.


Every string in a check is read by somebody who is not a Kubernetes engineer: a
release manager deciding whether to ship, a programme lead reading a summary, a
supplier quality engineer forwarding a spreadsheet. A finding they cannot act on
without a conversation has not been delivered.

Eight rules, applied to `title`, `description`, `rationale`, `message` and
`observed`:

1. **The title states the desired state, not the defect.** "A service with more
   than one copy survives planned maintenance", not "Replicated workloads are
   covered by a PodDisruptionBudget". Somebody scanning the catalogue should be
   reading a description of a well-built chart, not a list of accusations.
2. **Name the thing, not the field.** "The chart does not say how many copies
   must stay running", not "`spec.maxUnavailable` is absent". The field belongs
   in `locus`, `remediation` and `fixExample`, where somebody is about to edit
   it.
3. **State the consequence in operational terms.** "The service goes offline
   during patching", not "evictions are permitted".
4. **Gloss Kubernetes vocabulary on first use, or avoid it.** "a
   PodDisruptionBudget - the rule that tells the platform how many copies must
   stay running". No unexpanded acronyms at all.
5. **Say when it bites.** The `whenItBites` field carries it; the `rationale`
   should say it in words too.
6. **Say who acts.** Same: the field, and the sentence.
7. **One idea per sentence.**
8. **Never state a defect without a fix.** `remediation` is mandatory, and
   `fixExample` is expected wherever the fix is structural.

The test for all of this is not a linter. It is that somebody who is not a
Kubernetes specialist reads the finding and can explain the consequence back. If
they cannot, the description is wrong regardless of how accurate it is.

### 2.2.1 The two vocabularies

The plain-language rules above have a cost, and it is worth naming rather than
discovering: **the plainer the prose gets, the fewer technical words survive
anywhere in the report.** The PodDisruptionBudget check now reads "a service
with more than one copy survives planned maintenance" and contains the word
`PodDisruptionBudget` nowhere at all. That is right for the release manager and
useless for the engineer, who opens the report and types `toleration`, or
`maxUnavailable`, or `RWX`, or `seccomp`, and gets nothing back from a report
that is full of findings about exactly that.

So the technical vocabulary is carried deliberately, on two fields:

| Field | What it holds |
|---|---|
| `subcategory` | The MECHANISM, in an engineer's words: `PodDisruptionBudget`, `Taints & tolerations`, `Seccomp`, `Shared storage`. One short noun phrase, from a closed list. |
| `keywords` | The vocabulary the check is findable by: field paths (`spec.strategy.type`), API kinds (`NetworkPolicy`), acronyms (`PDB`, `RWX`, `SCC`, `HPA`), annotation names (`helm.sh/hook`), and the words for the symptom (`node drain`, `OOMKilled`, `CrashLoopBackOff`). At least three. |

Both are stored on every result and indexed by the report's search, alongside
the resource name, the chart, the file, the field path and the message. One
search box, two vocabularies, and neither reader has to learn the other's.

**`subcategory` is a closed vocabulary**, declared in
`baseline/contract_test.go` and asserted there. Free text drifts within a month -
"Helm hooks" and "Helm hook", "Probe timing" and "Probe timings" - and a filter
offering both spellings is worse than no filter, because each hides half the
findings and neither says so. Adding a value is an edit somebody makes on
purpose.

A subcategory may span categories, and that is the point rather than a defect:
Helm hooks are metadata to the labels section of the standard and lifecycle to
the upgrade section, and an engineer looking at a hook problem wants both.

**The terms are asserted, not hoped for.** `TestTechnicalTermsFindTheirChecks`
maps about fifty terms somebody would plausibly search for to the checks that
must come back. A rewritten description cannot silently take any of them away,
which is the failure this whole section exists to prevent - and which is exactly
what the plain-language rewrite did before these fields existed.

## 2.3 The severity rubric

Applied mechanically. The source catalogue's own severity is the starting point;
this decides where it lands after review.

| Severity | Test | Response |
|---|---|---|
| `block` | The standard prohibits it, AND the condition is a security exposure, data loss, or an outage or blocked upgrade under a foreseeable event | Do not accept the release |
| `warn` | The standard recommends it, AND its absence measurably degrades resilience, operability or security | Fix in the next release |
| `info` | An observation, a documentation gap, or a condition where the platform default is adequate | Record; no action required |

Two constraints on top:

- A check whose `confidence` is `needs-review` may not be `block` (§2.1).
- Where two checks would fire on one configuration choice, one of them is
  primary and the other narrows its applicability. Two checks independently
  penalising a single decision doubles the count and halves the credibility.

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
| `present(doc, path)` | Whether a path exists. Absent-safe: a missing field is a value, not a fault |
| `value(doc, path)` / `text(doc, path)` | The value at a path, raw or as the text the manifest wrote |
| `podField(workload, path)` | A value from the workload's pod spec, wherever that kind keeps it - two levels deeper on a CronJob |
| `securityValue(container, owner, field)` | A securityContext field resolved the way the kubelet resolves it: container, then pod, then nothing |
| `securitySource(container, owner, field)` | Which of those three it came from, so a finding names the line to edit |
| `runsAsRoot(workload)` | Whether anything in a workload runs as root. Undeclared is not root |
| `pdbFor(workload)` | The PodDisruptionBudget selecting it, or a null-safe empty object |
| `covers(obj)` | The reverse: the workloads an object's own selector matches |
| `servicesFor(workload)` | Services whose selector matches its pod labels |
| `selectedBy(service)` | Workloads a Service routes to. Selector keys the platform supplies at pod creation are skipped, because no chart can contain them |
| `selects(selector, obj)` | Full label-selector semantics: `matchLabels` **and** `matchExpressions`, namespace-scoped |
| `selectorKeys(obj)` | Every label key an object selects on, across the several shapes a selector takes |
| `declaresPort(container, port)` | Whether a probe port resolves. A number always does; a name has to be declared |
| `probeHandler(probe)` | A probe reduced to what it actually calls, timings dropped and defaults filled in |
| `pvcMountPaths(workload)` | Where a workload mounts persistent claims |
| `mountersOf(claim)` | The workloads that mount a claim, so a storage finding can name the software |
| `configRefs(workload)` | Every ConfigMap and Secret a pod asks for, from all six places a pod spec can name one |
| `crdFor(cr)` | The CustomResourceDefinition the release ships for a CR's `apiVersion`/`kind` |
| `builtinApiGroup(apiVersion)` | Whether the cluster serves that group itself, so nothing is asked to ship a definition of `Deployment` |
| `boundToRole(namespace, sa)` | Whether the release binds a service account to any Role |
| `ruleGrants(rule, resource, verbs)` | Whether an RBAC rule grants a verb, honouring wildcards in both directions |
| `quantity(v)` | A Kubernetes quantity as a number - `"1Gi"`, `"250m"`, `"0.5"` |
| `imageRef(s)` | `{registry, repository, tag, digest, hasDigest}` from an image reference |
| `resourcesIn(kinds)` | Release-wide lookup, for the few checks that need a set |
| `replicas(workload)` | The copy count, with the Kubernetes default of 1 for an absent field |
| `allLabels(obj)` / `allAnnotations(obj)` | The object's metadata merged with its pod template's |
| `credentialClass(key, value)` | What class of credential a value looks like, or `""`. See §4.2.1 |
| `looksLikeCredential(key, value)` | The same, as a boolean |
| `decodeBase64(v)` | A Secret value decoded, or the value unchanged when it is not base64 |
| `unstableLabelKey(k)` | Whether a label's value changes between releases |
| `runtimeLabelKey(k)` | Whether Kubernetes adds that label itself at pod creation |
| `extendedResource(name)` | Whether a resource name is one where request must equal limit |
| `operationalPath(path)` | Whether a URL path exposes a metrics, debug or admin endpoint |
| `semverCompare(a, b)` | -1, 0 or 1. Semver order, not string order |

#### 4.2.1 Credential detection, and its false-positive budget

`credentialClass` is the one heuristic in the set, so its rule is written down
and tested in `cel/heuristics_test.go`. It looks at the **value** first:

- **Shape signals are conclusive whatever the field is called.** A PEM private
  key header, a JSON web token, a cloud access key id, a URI with a password
  written into it, a ready-made authorization header, a vendor token prefix, or
  a placeholder somebody was obviously meant to replace.
- **The field name only corroborates.** Before it is trusted at all, fields
  naming a parameter *about* a credential are excluded - a retry count, an
  interval, a cache lifetime, a minimum length, a file path, a class name - and
  then values that cannot be the credential their key is named for are excluded
  too: numbers, durations, paths, hostnames, URLs, class identifiers, UUIDs,
  digests and single-word settings.
- **Raw entropy is never a signal on its own.** It flags every checksum, UUID
  and base64-encoded certificate in a chart. It is used only to sharpen the
  wording of a finding that already matched on its key name.
- **Secret values are decoded before analysis**, because base64 is an encoding
  and not protection, and a detector reading the encoded form finds nothing.
- **No finding prints the value.** It names the object, the key and the class.
  A compliance report is itself something that gets forwarded, and a report
  quoting the password has copied the exposure rather than described it.

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

### 5.1 Checks that report rather than reject

Some checks exist to say what is there: which containers cap their processing
power, which claims several workloads write to, what a release runs outside the
ordinary install. Written the obvious way, such a check has to FAIL on every
subject in order to say anything - and that is how a pack ends up with three
checks producing a third of the report's rows and close to none of its defects,
which is what the audit found.

`assert.observeOnPass: true` records the author-supplied `observed` value on a
pass as well as on a failure. The check then passes on everything correct and
still carries what it saw, so the inventory lives in the full record and the
action report stays about defects.

```yaml
    - id: UPG-08
      title: Every task Helm runs is named and understood
      severity: info
      assert:
        observeOnPass: true
        expr: |
          text(self, "metadata.annotations[helm.sh/hook]").split(",").all(h,
            h.trim() in ["pre-install", "post-install", …])
        observed: |
          "Helm runs this " + text(self, "kind") + " at: " +
          sorted(text(self, "metadata.annotations[helm.sh/hook]").split(",")
                 .map(h, h.trim())).join(", ")
```

One rule comes with it: **the expression has to read correctly in both cases.**
"runs at pre-upgrade, pre-install" does; "runs at nothing Helm recognises" does
not, and a check whose observed value is written for the failure only should
leave `observeOnPass` off. It applies to the author-supplied `observed` alone,
never to the shorthand's per-term one - a term's observed value describes the
term that failed, and on a pass there is no failing term.

### 5.2 Determinism, and the one way to lose it

A finding that lists the offending keys renders them from a map comprehension,
and **map iteration order is randomised**. The check is correct, the finding is
correct, and the words come out in a different order on every run - so the same
release checked twice produces different text, and a release-over-release
comparison reports the finding as fixed and reintroduced with nothing having
changed.

Wrap it: `sorted(map.filter(…).map(…)).join(", ")`. Lists that come from an
engine function are already ordered; lists that come from a map are not. The
determinism test runs every fixture twice and compares the TEXT, which is what
makes this catchable rather than a thing somebody notices in a diff six months
later.

### 6.1 The output contract

Three more assertions, in `baseline/contract_test.go`, each written for a
failure that reached a real report before anybody noticed:

- **Every shipped pack must load.** A pack that stops compiling takes its checks
  with it, and nothing else fails: the good fixture is still clean, because a
  check that does not exist cannot fire, and the report simply has a category
  missing from it. Absence and compliance are indistinguishable on the screen a
  release manager reads.
- **A finding may not name a field and leave the value blank**, and neither its
  observed value nor its message may contain an unsubstituted template fragment.
  Two checks shipped emitting `" on "` where the value should have been, both
  blocking, so they were among the first rows a reviewer opened.
- **Every check carries the triage block, a rationale and a reference**, and no
  blocking check declares `confidence: needs-review`. The rubric in §2.3 is
  enforced rather than trusted.
- **Every check carries a subcategory from the closed vocabulary and at least
  three keywords**, and about fifty technical terms are asserted to find the
  checks they belong to. See §2.2.1.

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
