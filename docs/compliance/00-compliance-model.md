# 00 - The Compliance Model

> **Ground truth.** What a check is, what it is allowed to say, and how every
> word of a finding gets attached to a specific Kubernetes object inside a
> specific chart inside a specific release.
>
> **Consumed by:** [01 - Check Catalog](01-check-catalog.md), [02 - Authoring Checks](02-authoring-checks.md), [design/23 - Compliance](../design/23-compliance.md)

---

## 1. The problem this exists to solve

A vendor ships a release. It contains Helm charts, sometimes kpt packages or
kustomize overlays, and container images. Somewhere in that tree is a
`Deployment` with no `readinessProbe`, a `PodDisruptionBudget` with
`maxUnavailable: 0`, a `ClusterRole` with `verbs: ["*"]`, and an image tagged
`:latest`. Each of those is a specific, well-understood way for a cluster to
lose availability or containment, and each is cheap to find automatically and
expensive to find during an upgrade at 02:00.

The organization already knows this. [source-standards.md](source-standards.md)
is the accumulated list - 109 assertions across 13 categories, written from
production and lab experience. What it lacks is a machine that runs it and a
report a vendor can act on.

**The output that matters is not a score.** It is a sentence a release engineer
can paste into a vendor ticket:

> `PDB-02` **FAIL** (block) - `PodDisruptionBudget/mysvc-pdb` in chart
> `mysvc 4.2.1` (`charts/mysvc`, rendered from `templates/pdb.yaml` line 9)
> sets `spec.maxUnavailable: 0`, which permits zero voluntary evictions and
> deadlocks every node drain that touches this workload. Expected
> `maxUnavailable >= 1` or `minAvailable < replicas`. Release
> `orb_23.8.1076` of `vendor-a-platform`, package digest `sha256:9f2c…`.

Everything in this document exists to make every finding look like that, and to
make the ones that cannot look like that say so out loud instead of guessing.

## 2. The five rules

Everything downstream is derived from these. They are stated first because each
one was chosen against a cheaper alternative that produces a tool nobody trusts.

### Rule 1 - A result is about ONE resource, or it is not a result

A finding whose subject is "the chart" or "the release" cannot be fixed by
anybody. The vendor's engineer needs to open one file and change one field.

So the unit of storage, of display and of export is the **(check, resource)
pair**, never the check alone. A check that finds four bad containers produces
four results, not one result with four names in the message.

The cost is row count and the answer is arithmetic: a large release is ~200
workloads and ~50 applicable checks, so ~10,000 rows. That is small. The
alternative - one row per check with a list inside it - is unfilterable,
unsortable, and unpasteable into a ticket.

### Rule 2 - A pass is a result, and so is a skip

The single most consequential decision in this document, and the one the
existing sample policies get wrong ([03](03-sample-policy-review.md) §2).

A policy engine that emits only violations cannot distinguish:

| Situation | Violations-only output | What it actually means |
|---|---|---|
| The chart has 40 workloads, all with readiness probes | nothing | compliant |
| The chart has 40 workloads and the probe check crashed | nothing | **unknown** |
| The chart has no workloads at all | nothing | **not applicable** |
| The chart failed to render | nothing | **nothing was checked** |

Three of those four render identically as a green screen, and two of them are a
release shipping unchecked. So every result carries an **outcome** from a
closed set:

| Outcome | Means | Rendered as |
|---|---|---|
| `pass` | The check applied to this resource and the assertion held | green |
| `fail` | The check applied and the assertion did not hold | red / amber by severity |
| `skip` | The check did not apply, and `reason` says why | grey, always counted |
| `error` | The check could not be decided - render failed, helm absent, malformed YAML - and `reason` says what happened | orange, **never** folded into pass |

`error` is not a failure of the software under test and must not be scored as
one. It is a failure of the *compliance*, and a run with errors in it is
reported as **inconclusive** rather than as a pass with a footnote.

### Rule 3 - Severity belongs to the check; outcome belongs to the result

`BLOCK`/`WARN`/`INFO` is a property of the rule the organization wrote. It does
not change per resource, per run, or per vendor. Mixing it into the outcome (as
`severity: "fail" | "warn" | "info"`, which is what the sample policies do)
means a policy can silently downgrade itself, and it makes "how many things
failed" a question with no single answer.

Two orthogonal fields:

```
outcome   pass | fail | skip | error        established by this run
severity  block | warn | info               declared by the check, fixed
```

A red screen is `outcome=fail AND severity=block`. An amber one is
`outcome=fail AND severity=warn`. Nothing else changes.

### Rule 4 - Say what you actually know: fixed, configurable, or unknown

This is what the tier question is really about, and it is the difference
between a tool that helps and a tool that cries wolf.

`helm template` with a chart's own defaults renders `replicas: 1`. Two very
different charts produce that line:

```yaml
# chart A - templates/deployment.yaml
replicas: 1                          # hard-coded; no deployment can change it

# chart B - templates/deployment.yaml
replicas: {{ .Values.replicaCount }} # values.yaml says 1; any site can raise it
```

For chart A, "this workload cannot be made highly available" is a **fact about
the software**. For chart B it is a fact about the *defaults*, and telling the
vendor their chart is not HA would be wrong and would cost the report its
credibility.

So every result carries a **determinacy**:

| Determinacy | Means | Weight in the verdict |
|---|---|---|
| `fixed` | The observed value is the same however the chart is configured. The finding is a property of the shipped software. | Full. A `fixed` block failure fails the release. |
| `configurable` | The observed value came from a default a values file can override. The finding is about what the chart does out of the box. | Advisory at tier 1. Reported, not blocking, and worded "at chart defaults". |
| `unknown` | Determinacy could not be established (the probe render failed, the chart could not be re-rendered). | Reported as `configurable` would be, and the run says the probe did not complete. |
| `n/a` | The check does not read a value - it asserts the *existence* of a resource, file or template. | Full. |

How determinacy is established is a mechanism, described in
[design/23](../design/23-compliance.md) §6: render twice, the second time with
every scalar in `values.yaml` replaced by a sentinel, and compare. A field that
does not move is fixed. It is two `helm template` invocations per chart and it
replaces guesswork with measurement.

**The `n/a` row is where most of the real value is.** "This chart ships no
PodDisruptionBudget template at all" is not a fact about defaults - no values
file can conjure one - so it is `fixed` and it blocks. That single distinction
is what lets tier 1 make hard statements about a chart without ever seeing a
site's values file.

### Rule 5 - Reproducible, or it is an opinion

The same release, checked twice, must produce byte-identical results, and two
people looking at the same report must be looking at the same rules.

A run therefore records everything that could change its answer, and every
report carries it:

| Recorded on the run | Why it can change the answer |
|---|---|
| `policyBundleDigest` | sha256 over every loaded pack file. A pack edited between runs is a different rulebook. |
| `engineVersion` | The built-in checks are code. |
| `helmVersion` | Template function behaviour and default capabilities differ across minor versions. |
| `kubeVersion` | `.Capabilities.KubeVersion` gates whole blocks of many charts. **Pinned in config**, never taken from a live cluster. |
| `apiVersions` | Same, for `.Capabilities.APIVersions.Has`. |
| `releaseName`, `namespace` | `.Release.Name` and `.Release.Namespace` appear in rendered names and labels. Fixed in config. |
| `determinacyMode` | Whether the probe render ran. |

Two runs whose recorded environment differs are **not comparable**, and the
comparison view says so rather than presenting a diff that is really a helm
upgrade.

#### The manifests, not only the inputs

Recording the inputs makes a finding **re-derivable**. It does not make it
**checkable**, and those are different guarantees with different audiences.

A vendor engineer reading "Deployment cfx-crds container main:
`securityContext.runAsNonRoot` - runAsUser 0" has one question, and it is not
"could I reproduce this pipeline". It is *show me*. Answering it from the table
above means pulling the chart out of the registry, installing that helm and
rendering it again with those pinned versions. Nobody does that. So a disputed
finding gets settled by whether the vendor trusts the tool, which is not a
technical conversation and does not converge.

So a run also keeps **the rendered manifests it judged** - the stream `helm
template` produced for each chart, plus any manifest the release ships as-is -
and the report shows the lines a finding is about, numbered as they are in the
document.

Three properties make them evidence rather than illustration:

- **They are the bytes that were judged**, kept from the run. Not a re-render
  performed when somebody clicks: a chart rendered again could differ from what
  was judged - a template that reads the clock, a helm upgraded since - and
  evidence that can differ from what it is evidence for is not evidence.
- **The line numbers are the document's own.** A number quoted out of an excerpt
  into a mail points at the same line of the downloadable file. An excerpt
  numbered from 1 would be a screenshot.
- **A line is pointed at only when there is one.** Half the findings in any run
  are about something ABSENT, and an absent field has no line. The report says
  so, and shows the deepest part of the path that does exist - the container a
  memory limit is missing from - marked as exactly that. A highlight on a
  plausible line would be a claim about the document that is false.

Kept for the **latest run of a release only**. This is the one part of a run
whose size the vendor sets, it is bounded per document and per release by
`coordinator.compliance.evidencePerDocument` / `evidencePerRelease`, and a
document cut at that budget says so rather than serving lines that stop without
warning. Nothing displays an older run, so nothing reads an older run's
manifests. A deployment that will not hold vendor manifests in its database sets
the budget below zero; findings are unaffected, because the manifests are what a
finding is DISPLAYED against and never what it is derived from.

## 3. What a result contains

The address is the feature. Everything else is text.

```
── identity ────────────────────────────────────────────────────────
checkId          PDB-02
pack             sgw-baseline            which rulebook this came from
title            No rule makes maintenance impossible
severity         block
tier             1
category         Disruption & Availability
subcategory      PodDisruptionBudget     the mechanism, in an engineer's words
keywords         PodDisruptionBudget PDB maxUnavailable minAvailable eviction

── where, from the outside in ───────────────────────────────────────
product          vendor-a-platform
release          orb_23.8.1076           the tag a person types
packageDigest    sha256:9f2c…            what was actually checked
artifactDigest   sha256:41ab…            the chart artifact in the tree
artifactRef      charts/mysvc            the OCI ref-name annotation
chartName        mysvc
chartVersion     4.2.1
appVersion       4.2.1
sourceFile       templates/pdb.yaml      from helm's own `# Source:` marker
renderedLine     9                       line in the rendered document

── which object ─────────────────────────────────────────────────────
apiVersion       policy/v1
kind             PodDisruptionBudget
namespace        (as rendered)
name             mysvc-pdb
container        (empty; set when the subject is one container)
locus            spec.maxUnavailable     JSON path to the field judged

── what was found ───────────────────────────────────────────────────
outcome          fail
determinacy      fixed
observed         at most 0 copies may be unavailable
expected         a rule that lets at least one copy be moved
message          …one sentence, the thing a person reads first
remediation      …what to change
reference        source-standards.md - PDB-02
reason           (empty; set for skip and error)

── what to do about it ──────────────────────────────────────────────
confidence       confirmed          what the tool knows vs. what it infers
whenItBites      node-maintenance   when the consequence arrives
fixOwner         chart-template     who changes something
fixEffort        low                how much work
fixExample       …the corrected YAML, not a description of it

── tracking ─────────────────────────────────────────────────────────
fingerprint      sha256(checkId | chartName | kind | namespace | name | container | locus)
waiver           (empty, or the waiver that suppressed it)
```

Three properties of that shape are deliberate:

- **`renderedLine`, not template line.** Helm emits `# Source: chart/templates/pdb.yaml`
  above each document, so the file is exact. The *line within the template* is
  not recoverable from helm's output, and inventing one would send a vendor to
  the wrong line. The report gives the source file and the rendered line, and
  attaches the rendered document to the evidence bundle so both ends can look
  at the same text.
- **`locus` is a JSON path, not prose.** It is what makes a finding
  machine-diffable and what lets the UI highlight the exact field.
- **`fingerprint` excludes the chart version and the release tag** on purpose.
  It is what makes "the vendor fixed this in 4.3.0" and "this has been failing
  for six releases" answerable, and it is what a waiver is keyed on.

`subcategory` and `keywords` are the technical index over a report written in
plain language, and they exist because the plain language took something away.
The title above says "no rule makes maintenance impossible" and contains the
word "PodDisruptionBudget" nowhere - which is what makes it readable by somebody
who is not a Kubernetes engineer, and what leaves the engineer who has to fix it
with nothing to search for. Both fields are matched by the report's search
alongside the resource, the chart, the file and the field path, so one box
serves both readers. See [02](02-authoring-checks.md) §2.2.1.

The triage block is the newest part of the shape and the one that was missing
longest. A severity says how much this organization cares about the rule; it
does not say who changes something, how much work it is, when the consequence
arrives, or how firmly the tool knows what it is asserting - and a report
without those is one that gets forwarded rather than acted on. It is copied from
the check onto the result, like the title and the remediation, so a spreadsheet
sent to a vendor still says what it said the day it was exported. See
[02](02-authoring-checks.md) §2.1, and [04](04-audit-response.md) for the audit
that established it was needed.

## 4. Tiers

The vocabulary the organization already uses, given a precise boundary.

| Tier | Inputs | Question it answers |
|---|---|---|
| **Tier 1** | Only what the vendor shipped: chart archives, their default `values.yaml`, plain manifests, kpt packages, kustomize bases, the artifact tree, and the OCI metadata | *Does this software, as published, comply with our standards?* |
| **Tier 2** | Tier 1 plus a site's own values files, cluster facts (available storage classes, node labels, zones), and evidence documents (restore tests, drain rehearsals) | *Will this deployment of this software, here, comply?* |
| **Evidence** | Documents and attestations a human reads | *Has the vendor demonstrated what they claim?* |

**Tier 1 is not "the easy subset".** It is the subset that is *decidable from
the artifact alone*, and it is where the leverage is: it runs the moment a
release is discovered, before 40 GB moves, and its findings are the ones a
vendor can actually be held to, because they are about the software rather than
about how one customer configured it.

Two rules keep the boundary honest:

1. **A tier-2 check never reports a tier-1 pass.** If a check needs a values
   file, it emits `skip` with `reason: "needs site values (tier 2)"`. It does
   not quietly assume defaults and call it compliant. Silent optimism is how a
   compliance tool becomes decoration.
2. **A tier-1 check may report on tier-2 ground when the finding is structural.**
   "No PDB template exists in this chart" is decidable now and is decidable
   *forever*, whatever values arrive later. That is the shape most tier-1
   checks should take, and [01](01-check-catalog.md) marks each check
   accordingly: `T1-C` for a structural assertion about the chart, `T1-R` for
   an assertion about the manifests rendered from its defaults.

## 5. Applicability, and how passes are generated

Every check declares what it applies to:

```yaml
appliesTo:
  kinds: [Deployment, StatefulSet, DaemonSet]
  # optional narrowing
  excludeKinds: [Job, CronJob]
  containers: main            # all | main | init | none
  when:                       # optional guard, evaluated per resource
    minReplicas: 2
```

The engine computes the **applicable set** for each check on each chart before
any policy code runs. Then:

```
applicable set  =  resources the check declares it judges
violated set    =  what the check reported
passes          =  applicable − violated          ← derived, not authored
skip            =  emitted once when applicable is empty, with the reason
```

This is the mechanism that makes Rule 2 cheap. A policy author writes only the
violation logic - which is the natural way to write a rule and the way every
existing `.rego` file in this repository is already written - and gets correct
per-resource passes for free. No author has to remember to emit them, and no
author can get the denominator wrong.

It also means an existing violations-only policy becomes a full-fidelity check
by adding a manifest that declares its `appliesTo`. That is the whole migration
path for the sixteen policies in [sample-policies/](sample-policies/), and it is
why the contract in [02](02-authoring-checks.md) is shaped the way it is.

## 6. The verdict

Per release, from [source-standards.md](source-standards.md)'s own scoring
model, with `error` and determinacy folded in:

| Verdict | Condition |
|---|---|
| **Inconclusive** | Any `error`, or coverage incomplete - a chart that would not render, an artifact that could not be fetched. **Checked first**, because a release nobody could examine is not a release that passed. |
| **Fail** | No errors, and at least one unwaived `fail` at `block` severity with determinacy `fixed` or `n/a`. |
| **Conditional** | No errors, no blocking `fixed` failures, and at least one of: an unwaived `warn`; a `block` failure whose determinacy is `configurable` (the defaults are wrong, a site can fix it). |
| **Pass** | No errors, complete coverage, no unwaived failures at `block` or `warn`. |

**Coverage is reported beside the verdict, always** - charts rendered / charts
failed / files parsed / files unreadable / resources examined / checks applied.
A number without its denominator is the failure mode this whole model exists to
prevent, and it is the same discipline the security feature already applies to
scanned-vs-unscanned images ([design/21](../design/21-security-posture.md)).

## 7. Waivers

A `BLOCK` failure that the organization has accepted needs a record, not a
comment in a spreadsheet. Waivers live in Git beside the policies, never in the
API, for the same reason product configuration does ([design/02](../design/02-configuration.md) §1):
an approval that can be granted through a UI is an approval with no review.

```yaml
apiVersion: softwaregateway.io/v1alpha1
kind: Waivers
spec:
  - check: SEC-03
    scope:
      product: vendor-a-platform
      chart: mysvc
      resource: DaemonSet/node-agent
    reason: >
      The node agent requires CAP_NET_ADMIN to program the dataplane. Reviewed
      against the CNF security exception process, ticket SEC-4471.
    compensatingControl: >
      Runs only on labelled dataplane nodes, dedicated ServiceAccount, no API
      access, seccomp RuntimeDefault enforced.
    approver: platform-security@example.com
    expires: 2027-03-31
```

Rules that make a waiver a control rather than a hole:

- **Expiry is mandatory**, and an expired waiver stops applying the day it
  expires. The result reappears as a failure and the report says the waiver
  lapsed - it does not silently keep suppressing.
- **A waived result is still recorded, still exported, and still counted** in
  its own column. It moves out of the verdict, never out of the report; the
  vendor conversation and the audit both need to see it.
- Scope is by check plus any of product / chart / resource / fingerprint.
  A waiver with no scope beyond the check ID is rejected at load: blanket
  waivers are how a policy set dies quietly.

### 7.1 A waiver is not a declared exception, and the two must not be confused

A waiver is **ours**: our organization accepting a failure the vendor shipped,
recorded on our side, expiring on a date we set. A declared exception is
**theirs**: the chart itself saying, in the manifest, that it needs something
the standard forbids by default and why.

Both exist because some checks describe a rule with real exceptions, and the two
answer different questions. SCH-08 is the worked example: a pod must not
tolerate a node-pressure taint, *unless* the workload carries

```yaml
metadata:
  annotations:
    compliance.softwaregateway.io/toleration-rationale: >-
      Collects kernel logs off nodes already under disk pressure, which is the
      condition the logs are needed for. Approved by platform-sre 2026-04-11.
```

on itself or on its pod template. The annotation makes the check PASS - it is
not a suppression, and nothing is moved out of the verdict.

The reason to build it this way rather than as a waiver is that the deciding
fact is in the chart. A DaemonSet that has to run on a failing node and one that
tolerates pressure taints by copy-paste are byte-identical without the
declaration, so a check that simply forbade the toleration would be waived
release after release, and a check that exempted DaemonSets would pass the
copy-paste one forever. Asking the author to write the sentence is what turns
"we tolerate everything" into a claim somebody can disagree with in review.

Where a check offers a declared exception, the annotation key, where it may be
written, and what an empty value does are stated in the check's own catalog row.
An exception the vendor will not declare is what a waiver is for.

## 8. What this model deliberately does not do

Stated so nobody has to infer it from an absence.

| Not done | Why | Where it goes instead |
|---|---|---|
| Admission control | This runs at *ingest*, on artifacts, before anything is deployed. Blocking at admission is Gatekeeper/Kyverno's job and duplicating it would put two rulebooks in one estate. | The org's admission stack; this feature exports the same intent as evidence. |
| Running against a live cluster | It judges *what a vendor shipped*, not what an operator later did. Reading a cluster would make results depend on a cluster's current state and stop them being reproducible. | Tier 2, and only from declared cluster facts in config. |
| Scoring, grading, percentages as the headline | "87% compliant" is unactionable and rounds a blocking failure into a good number. | Counts by severity and outcome, with coverage. |
| Fixing charts | A tool that rewrites a vendor's chart owns the result of that chart. | A remediation string per finding, and the evidence bundle. |
| Vulnerability scanning | Already built, already good, already has a comparison model. | [design/21 - Security Posture](../design/21-security-posture.md). The two appear side by side on the release page and are never merged. |

## 9. Why this is not a re-implementation of an existing tool

Reasonable question, given kube-score, Polaris, Datree, Kyverno CLI and
`conftest` all exist. What none of them do is the part that carries the value
here:

| What is needed | Why an off-the-shelf CLI does not supply it |
|---|---|
| Findings addressed to *product → release → package digest → chart artifact → chart version → source file → resource → field* | Those tools take a directory of YAML. Everything above `sourceFile` is knowledge this platform already holds and they never see. |
| Rendering from an OCI artifact tree already indexed by digest | They start from a filesystem. Getting the chart out of the vendor registry, by digest, under the credential that discovered it, is this platform's job. |
| Determinacy (`fixed` vs `configurable`) | None of them distinguish a hard-coded value from a default. Without it, tier 1 is guesswork. |
| Passes, skips and coverage as first-class output | Most emit violations only; the ones that emit passes do not emit applicability. |
| A vendor-shareable report carrying the rules themselves | They report verdicts, not the rulebook that produced them. |
| Running inside the ingest lifecycle, before the bytes move | They are CI tools. This is a gate on the pipeline that already exists here. |

The check *logic* for a dozen of the built-ins genuinely is the same logic
kube-score has - that is a sign the logic is right, not a reason to shell out to
it. What is being built is the addressing, the determinacy, the applicability
model, the lifecycle integration and the report. The rules are the cheap part.
