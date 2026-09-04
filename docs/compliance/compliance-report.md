# Helm Chart Compliance Scanner
## Policy Library, Findings Schema and Tool Improvement Specification

**Document type:** Engineering specification and defect report
**Audience:** Scanner development team; secondary audience: chart authors and reviewers
**Classification:** Generic. Every resource name, image, registry, namespace, credential and value in this document is synthetic. No customer, product or release data is included.

---

## Table of contents

| Part | Contents |
|---|---|
| **1** | Purpose, scope and how to read this document |
| **2** | Findings schema v2 — field-by-field redesign |
| **3** | Policy authoring standard — naming, language, severity, confidence |
| **4** | Rewritten policy library — all existing policies restated in plain language |
| **5** | Defects in current policies — false positives, severity errors, description errors |
| **6** | New policies — 47 proposed rules with grounding and samples |
| **7** | Critical gaps — the five highest-value additions |
| **8** | Implementation roadmap |
| **A** | Appendix A — Complete policy catalogue and severity map |
| **B** | Appendix B — Golden-corpus test manifest |
| **C** | Appendix C — Glossary for non-specialist readers |

---

# Part 1 — Purpose, scope and how to read this document

## 1.1 Purpose

This document specifies improvements to a Helm chart compliance scanner across four dimensions:

1. **Findings schema** — what fields each result carries, so a finding is self-explanatory without the reader opening the chart.
2. **Policy language** — how each rule is named and described, so that a partially technical reader (a release manager, a programme lead, a supplier quality engineer) can understand what is wrong and why it matters.
3. **Policy correctness** — defects in existing rules, including false positives and severity miscalibration.
4. **Policy coverage** — rules that should exist and do not, including several that address serious conditions currently invisible to the scanner.

## 1.2 The core problem this document addresses

A compliance report is only useful if the person receiving it can act on it without a conversation. In the current output, many findings fail that test. A representative example:

```
PDB-01 | Critical | no PodDisruptionBudget in this release selects the pods
of Deployment app-alpha, so a node drain can evict all 2 replicas at once
```

This assumes the reader knows what a PodDisruptionBudget is, what "selects the pods" means, what a node drain is, and why evicting two replicas simultaneously matters. A chart author may know all four. A release manager deciding whether to ship will not.

The same finding, rewritten to the standard proposed in this document:

```
Policy:   Service survives planned maintenance
Severity: High
What we found:
  "app-alpha" runs 2 copies, but nothing tells the platform to keep at least
  one copy running during maintenance.
Why it matters:
  When a server is taken offline for patching, the platform is free to shut
  down both copies at the same time. The service goes down completely, with
  no alert and no rollback, during routine maintenance.
How to fix:
  Add a PodDisruptionBudget that allows at most 1 copy to be unavailable.
  Example below. Effort: low — one small file, no application change.
```

Same detection, same evidence, same severity. The difference is entirely in presentation, and it is the difference between a report that gets acted on and one that gets forwarded and forgotten.

## 1.3 Scope of change

| Area | Current state | Proposed |
|---|---|---|
| Policies | ~40 | ~87 (40 revised, 47 added) |
| Schema fields | 31, several empty or ambiguous | 24, all populated and load-bearing |
| Severity levels | 3, applied inconsistently | 4, applied by a mechanical rubric |
| Policy names | Terse identifiers (`PDB-01`) | Identifier plus a plain-language name |
| Descriptions | Condition-only | Condition, consequence, fix, effort |
| Pass records | None | Emitted, enabling compliance rates and trends |

## 1.4 How to read a policy entry in Part 4 and Part 6

Every policy is specified in a consistent block:

| Element | Purpose |
|---|---|
| **ID** | Stable identifier. Never reused, never renumbered. |
| **Name** | A short sentence describing the desired state, not the defect. |
| **Plain statement** | One sentence a non-specialist can read. No Kubernetes vocabulary where avoidable. |
| **Why it matters** | The real-world consequence. What breaks, when, and who notices. |
| **Severity** | Per the rubric in §3.4. |
| **Detects** | The precise technical condition. This is for the scanner developer. |
| **Does not detect** | Explicit exclusions. This is where false positives are prevented. |
| **Fail sample / Pass sample** | Synthetic YAML. Directly usable as a regression test. |
| **Grounding** | The clause of the governing standard that authorises the rule. |
| **Effort to fix** | Low / Medium / High, so a reader can plan. |

---

# Part 2 — Findings schema v2

## 2.1 Problems with the current schema

The current output carries 31 columns. Analysis of a representative run found:

| Problem | Detail |
|---|---|
| **Empty on every row** | `Reference` (0% populated), `Waiver` (0% populated) |
| **Constant on every row** | `Tier` (always `1`), `Outcome` (always `Fail`) |
| **Non-actionable values** | `Owner` reads `Could not be established` on ~35% of rows |
| **Empty on some checks** | `Observed` blank or containing unsubstituted template fragments on ~47 findings |
| **Condition without consequence** | `Finding` states what was seen but usually not what will happen |
| **No fix guidance beyond a sentence** | `Remediation` is prose; no example, no effort estimate |
| **Redundant** | `Chart digest`, `Chart reference`, `Release digest` duplicate information available from `Chart` + `Chart version` |

Net effect: four columns carry no information, one is misleading, and the two most important columns for action — what will break, and exactly how to fix it — do not exist.

## 2.2 Proposed schema

### 2.2.1 Identity block

| Field | Type | Required | Description |
|---|---|---|---|
| `policy_id` | string | Yes | Stable identifier, e.g. `AVAIL-001`. Never reused. |
| `policy_name` | string | Yes | Plain-language name, e.g. `Service survives planned maintenance`. |
| `policy_version` | string | Yes | Version of the rule logic, e.g. `2.1`. Lets a vendor tell a rule change from a chart change. |
| `domain` | enum | Yes | `Availability`, `Security`, `Identity`, `Configuration`, `Networking`, `Storage`, `Observability`, `Supply chain`, `Metadata`, `Lifecycle` |
| `severity` | enum | Yes | `Critical`, `High`, `Medium`, `Advisory` — see §3.4 |
| `outcome` | enum | Yes | `Fail`, `Pass`, `Not applicable`, `Waived`, `Inconclusive` |
| `confidence` | enum | Yes | `Confirmed`, `Probable`, `Requires review` — see §3.5 |

### 2.2.2 Location block

| Field | Type | Required | Description |
|---|---|---|---|
| `chart` | string | Yes | Sub-chart name |
| `chart_version` | string | Yes | Sub-chart version |
| `source_file` | string | Yes | Template path within the chart |
| `source_line` | integer | Where known | Line in the rendered manifest |
| `resource_kind` | string | Yes | e.g. `Deployment` |
| `resource_name` | string | Yes | e.g. `app-alpha` |
| `container` | string | Where applicable | Container name |
| `container_role` | enum | Where applicable | `app`, `init`, `sidecar` — a finding on a short-lived init container is not the same as on a long-running service container |
| `field_path` | string | Yes | Full path, e.g. `spec.template.spec.containers[0].securityContext.runAsNonRoot` |

### 2.2.3 Evidence block — the most important change

| Field | Type | Required | Description |
|---|---|---|---|
| `observed_value` | string | Yes | The actual value found. **Never empty.** |
| `observed_source` | enum | Yes | `explicit`, `inherited from pod`, `platform default`, `not declared` |
| `effective_value` | string | Yes | The value that will apply at runtime after defaults and inheritance are resolved |
| `expected_value` | string | Yes | What the policy requires |
| `evidence_snippet` | string | Yes | 3–8 lines of the rendered YAML with the offending line marked |

`observed_source` resolves a recurring class of confusion. A field that is absent, a field inherited from the pod, and a field explicitly set to a non-compliant value are three different situations that today all render as `(absent)` or as a bare value.

```yaml
# Illustration of why observed_source matters
# Three containers, three very different situations, one current output.

# A) Field genuinely not declared anywhere. Platform default applies.
observed_value:  "not declared"
observed_source: "not declared"
effective_value: "false (Kubernetes default)"

# B) Field not on the container, but set at pod level and inherited.
observed_value:  "false"
observed_source: "inherited from pod"
effective_value: "false"

# C) Field explicitly set on the container.
observed_value:  "false"
observed_source: "explicit"
effective_value: "false"
```

### 2.2.4 Explanation block

| Field | Type | Required | Description |
|---|---|---|---|
| `what_we_found` | string | Yes | One or two sentences. Plain language. Names the resource. |
| `why_it_matters` | string | Yes | The consequence. When it bites, and what the operator sees. |
| `blast_radius` | enum | Yes | `Single container`, `Single workload`, `Namespace`, `Cluster` |
| `manifests_when` | enum | Yes | `At install`, `At upgrade`, `During node maintenance`, `Under load`, `On failure`, `Continuously` |

`manifests_when` is valuable for triage. A finding that only bites during node maintenance is urgent before a platform upgrade window and can wait otherwise. Today nothing in the report conveys this.

### 2.2.5 Remediation block

| Field | Type | Required | Description |
|---|---|---|---|
| `fix_summary` | string | Yes | One sentence, imperative. |
| `fix_example` | string | Yes | A YAML snippet showing the corrected configuration. |
| `fix_owner` | enum | Yes | `Chart values`, `Chart template`, `Application code`, `Build pipeline`, `Platform team`, `Requires decision` |
| `fix_effort` | enum | Yes | `Low`, `Medium`, `High` |
| `breaking_change` | boolean | Yes | Whether the fix requires an application change or a restart behaviour change |

`fix_owner` replaces the current `Owner` field. The value `Could not be established` is removed from the vocabulary — where ownership is genuinely ambiguous, `Requires decision` is used and `why_it_matters` names who decides.

### 2.2.6 Governance block

| Field | Type | Required | Description |
|---|---|---|---|
| `standard_reference` | string | Yes | Clause of the governing standard, e.g. `§4.2.1` |
| `standard_quote` | string | Yes | The sentence being enforced, verbatim, ≤ 200 characters |
| `waiver_id` | string | Where waived | Reference to an approved waiver |
| `waiver_expires` | date | Where waived | Waivers expire; they do not accumulate silently |
| `fingerprint` | string | Yes | Stable hash for suppression and release-over-release diffing |

`standard_quote` is a deliberate addition. Including the enforced sentence in the finding makes disagreements resolvable by reading, and makes rule drift from the standard immediately visible — the failure mode described in §5.3.

## 2.3 Before and after — the same finding in both schemas

**Current output:**

```csv
PDB-01,Replicated workloads are covered by a PodDisruptionBudget,Critical,Fail,
Could not be established,app-alpha,1.4.0,app-alpha/templates/deployment.yaml,42,
apps/v1,Deployment,app-namespace,app-alpha,,,,,,"no PodDisruptionBudget in this
release selects the pods of Deployment app-alpha, so a node drain can evict all
2 replicas at once","Add a PodDisruptionBudget.",,Disruption & Availability,
baseline-pdb,1,sha256:...,...,...,...,,PDB-01app-alpha...
```

**Proposed output** (rendered as a report card rather than a raw row):

```
────────────────────────────────────────────────────────────────────────
AVAIL-001   Service survives planned maintenance          Severity: HIGH
────────────────────────────────────────────────────────────────────────
Where:      Deployment "app-alpha"   (chart: app-alpha 1.4.0)
            app-alpha/templates/deployment.yaml, line 42

What we found
  "app-alpha" runs 2 copies of itself, but the chart does not tell the
  platform how many copies must stay running during maintenance.

Why it matters
  When a server is taken offline for patching or upgrade, the platform
  asks each service how many copies it can spare. With no answer, it
  assumes all of them. Both copies of "app-alpha" can be shut down at
  the same moment, taking the service fully offline during what was
  meant to be a routine, non-disruptive maintenance window.

  Blast radius:   Single workload
  Manifests when: During node maintenance

Evidence
  No PodDisruptionBudget in this release matches labels:
      app.kubernetes.io/name=app-alpha
      app.kubernetes.io/instance=release-one

  spec:
    replicas: 2                    <-- 2 copies
    selector:
      matchLabels:
        app.kubernetes.io/name: app-alpha

How to fix                          Owner: Chart template   Effort: LOW
  Add a PodDisruptionBudget allowing at most one copy to be unavailable.

    apiVersion: policy/v1
    kind: PodDisruptionBudget
    metadata:
      name: app-alpha
    spec:
      maxUnavailable: 1
      selector:
        matchLabels:
          app.kubernetes.io/name: app-alpha
          app.kubernetes.io/instance: release-one

  No application change required. Not a breaking change.

Standard
  §4.2.1 — "Create PDBs for Deployments/StatefulSets that have
            replicas >= 2 and serve production traffic."
────────────────────────────────────────────────────────────────────────
```

The CSV remains available as the machine-readable artefact. The report card is the human-facing rendering of the same record.

---

# Part 3 — Policy authoring standard

## 3.1 Policy naming

Policy names state the **desired state**, not the defect. This matters more than it appears: a reader scanning a list of policy names should be reading a description of a well-built chart, not a list of accusations.

| Current name | Problem | Proposed name |
|---|---|---|
| "Replicated workloads are covered by a PodDisruptionBudget" | Jargon in every noun | **Service survives planned maintenance** |
| "No PodDisruptionBudget covers a single-replica workload" | Double negative, jargon | **Maintenance is never blocked indefinitely** |
| "A workload behind a Service does not use the Recreate strategy" | Negative, assumes "Recreate" is understood | **Updates roll out without downtime** |
| "Pods declare a non-zero termination grace period" | Jargon, and the rule is wrong (§5.4) | **Shutdown time is stated explicitly** |
| "Containers do not run as root" | Negative | **Containers run as an unprivileged user** |
| "No role grants a wildcard" | Jargon | **Permissions are specific, not blanket** |
| "No role can enumerate secrets" | "Enumerate" is unusual | **Credentials cannot be listed or harvested** |
| "Every image is pinned by digest" | Jargon in every noun | **Deployed images are exactly reproducible** |
| "Replicated workloads spread across zones and nodes" | Reasonable but dense | **Copies are spread across failure domains** |
| "Every custom resource has its definition in the same release" | Circular | **All required extensions are installed first** |

## 3.2 Plain-language rules for descriptions

Applied to every `what_we_found` and `why_it_matters` field:

1. **Name the thing, not the field.** "The chart does not say how many copies must stay running" rather than "`spec.maxUnavailable` is absent."
2. **State the consequence in operational terms.** "The service goes offline during patching" rather than "evictions are permitted."
3. **No unexplained Kubernetes vocabulary.** On first use in a finding, gloss the term: "a PodDisruptionBudget (a rule telling the platform how many copies must stay running)".
4. **No acronyms without expansion.** `PDB`, `SCC`, `RBAC`, `CRD`, `RWX`, `PVC`, `SA`, `HPA`, `NAD` must be expanded on first use in every finding.
5. **Say when it bites.** "during node maintenance", "on every upgrade", "under peak load". Timing drives prioritisation.
6. **Say who acts.** Chart author, application team, pipeline, or platform team.
7. **One idea per sentence.** Maximum 25 words per sentence in the explanation block.
8. **Never state a defect without a fix.** Every failing finding carries a `fix_example`.

## 3.3 Worked example — improving the hardest policies

The availability and disruption policies are the ones flagged as hardest to interpret. Here is each, rewritten.

### AVAIL-001 (was PDB-01)

| | |
|---|---|
| **Before** | "no PodDisruptionBudget in this release selects the pods of Deployment app-alpha, so a node drain can evict all 2 replicas at once" |
| **After — what we found** | "app-alpha" runs 2 copies, but the chart does not tell the platform how many copies must stay running during maintenance. |
| **After — why it matters** | When a server is taken offline for patching, the platform asks each service how many copies it can spare. With no answer, it assumes all of them. Both copies can stop at once, taking the service fully offline during routine maintenance. |

### AVAIL-002 (was PDB-03)

| | |
|---|---|
| **Before** | "this budget covers a workload that cannot spare a pod: Deployment app-beta. Evictions of it will be refused indefinitely" |
| **After — what we found** | "app-beta" runs a single copy, and the chart insists that copy must never stop. |
| **After — why it matters** | Maintenance on the server hosting this service will hang. The platform will wait indefinitely for permission to move the service, and that permission can never be granted. In practice an engineer must intervene manually, and a cluster upgrade can stall for hours. Protecting a single copy does not make it more available — it only blocks maintenance. |
| **Fix** | Either run 2 or more copies and allow one to be unavailable, or remove the rule and accept a brief restart during maintenance. |

### AVAIL-003 (was PDB-06)

| | |
|---|---|
| **Before** | "Deployment app-gamma: spec.strategy.type; expected RollingUpdate for a workload receiving traffic" (with the observed value blank) |
| **After — what we found** | "app-gamma" receives live traffic, but is set to replace itself by stopping every copy before starting any new one. |
| **After — why it matters** | Every update to this service causes a complete outage, lasting from when the last old copy stops until the first new copy is ready and healthy. This is not a risk — it happens on every single deployment, by design. |
| **Fix** | Change the update strategy to a rolling update so new copies start before old ones stop. |

### AVAIL-004 (was PDB-08) — see §5.4; the current rule is also factually wrong

| | |
|---|---|
| **Before** | "terminationGracePeriodSeconds — (absent); expected set, above zero" |
| **After — what we found** | "app-delta" does not state how long it needs to finish work in progress before being shut down. The platform will allow 30 seconds by default. |
| **After — why it matters** | If this service handles requests that can take longer than 30 seconds, those requests are cut off mid-flight during every restart, upgrade and maintenance event. Stating the value explicitly also documents the intent for whoever operates the service later. |
| **Severity** | Advisory. The 30-second default is usually adequate; this is a documentation gap, not a defect. |

### SCHED-001 (was SCH-01)

| | |
|---|---|
| **Before** | "runs 2 replicas that the scheduler may place on one node: it declares no topologySpreadConstraints" |
| **After — what we found** | "app-epsilon" runs 2 copies, but nothing prevents both from being placed on the same physical server. |
| **After — why it matters** | Running two copies is only useful if they fail independently. If both land on one server, that server failing takes the whole service down — the second copy provides no protection at all, while consuming twice the resources. |

## 3.4 Severity rubric

Applied mechanically. No per-policy judgement.

| Severity | Test — all conditions must hold | Typical response |
|---|---|---|
| **Critical** | The standard uses prohibitive language ("Don't", "never", "must not") **and** the condition causes a security exposure, data exposure, or guaranteed outage | Block release |
| **High** | The standard uses prohibitive or strong language **and** the condition causes an outage or blocked maintenance **under a specific, foreseeable event** (node drain, upgrade, failure) | Fix before production |
| **Medium** | The standard recommends ("Do", "prefer", "should") **and** absence measurably degrades resilience, operability or security posture | Fix in the next release |
| **Advisory** | Observation, documentation gap, or a condition where the platform default is acceptable | Review; no action required |

Two additional constraints:

- **No policy may be Critical if the finding cannot be verified from the rendered manifest alone.** Anything requiring runtime knowledge is capped at Medium.
- **Where two policies detect overlapping conditions, one is primary and the other is suppressed.** Two policies must never independently penalise a single configuration choice.

Effect of applying this rubric to the existing pack: Critical findings drop by approximately 79%, from ~620 to ~130 in a representative run.

## 3.5 Confidence levels

New field. Distinguishes what the scanner **knows** from what it **infers**.

| Level | Meaning | Example |
|---|---|---|
| **Confirmed** | Directly readable from the manifest; no assumption | `runAsUser: 0` is present |
| **Probable** | Readable, but depends on a platform assumption the scanner cannot verify | A service has no NetworkPolicy — but policy may be applied centrally |
| **Requires review** | The condition may be intentional and correct for this workload class | A data-plane workload requests host network access |

**Rule: no `Requires review` finding may be rated above Medium.** This single constraint prevents the largest category of unproductive dispute, where the scanner asserts as a defect something the vendor deliberately chose for a valid reason.

## 3.6 Policy applicability — the "not applicable" outcome

Many current false positives arise from applying a policy to a workload class it was never meant for. Every policy declares its applicability:

| Dimension | Values |
|---|---|
| Workload kind | `Deployment`, `StatefulSet`, `DaemonSet`, `Job`, `CronJob`, `Pod` |
| Traffic role | `Receives traffic` (backed by a Service), `Internal only`, `Batch` |
| Lifetime | `Long-running`, `Short-lived` (Job, init container, Helm hook) |
| Replica count | `Single`, `Replicated (≥2)` |
| Container role | `app`, `init`, `sidecar` |

A policy that does not apply emits `outcome: Not applicable` rather than being silently skipped. This makes coverage auditable and prevents the failure described in §5.9, where a policy that never runs is indistinguishable from a policy that always passes.

Example: readiness probes are required on long-running containers that receive traffic. Applying that policy to a short-lived database-migration Job produces a false positive today; under the applicability model it produces `Not applicable`.

---

# Part 4 — Rewritten policy library

All existing policies, restated. Grouped by domain and renumbered with meaningful prefixes. The `Was` column maps to the current identifier so results can be reconciled across versions.

## 4.1 Availability and disruption

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `AVAIL-001` | Service survives planned maintenance | PDB-01 | A service with more than one copy tells the platform how many copies must stay running while servers are patched. | High |
| `AVAIL-002` | Maintenance is never blocked indefinitely | PDB-03 | No service demands protection the platform can never grant, which would stall maintenance forever. | High |
| `AVAIL-003` | Updates roll out without downtime | PDB-06 | Services that receive traffic start new copies before stopping old ones, rather than going fully offline during each update. | High |
| `AVAIL-004` | Shutdown time is stated explicitly | PDB-08 | The chart states how long a service needs to finish work in progress before shutdown, rather than relying on the platform's 30-second default. | Advisory |
| `AVAIL-005` | Stalled rollouts are detected | PDB-07 | The chart states how long an update may take before it is declared failed, so a stuck deployment surfaces instead of hanging silently. | Medium |

## 4.2 Scheduling and placement

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `SCHED-001` | Copies are spread across failure domains | SCH-01 | Multiple copies of a service are placed on different servers and in different data-centre zones, so one failure cannot take them all out. | Medium |

## 4.3 Health checking

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `HEALTH-001` | Traffic only reaches ready services | PRB-01 | Every service receiving traffic tells the platform when it is ready, so requests are not sent to a copy that is still starting up. | High |
| `HEALTH-002` | Slow-starting services are given time to start | PRB-02 | Services that take a while to initialise declare a startup grace period, so the platform does not kill them mid-startup and restart them in a loop. | Medium |
| `HEALTH-003` | Restart checks are less twitchy than traffic checks | PRB-03 | The check that restarts a service is more tolerant than the check that routes traffic to it, so brief slowness removes traffic rather than triggering restarts. | Medium |
| `HEALTH-004` | Restart and traffic checks test different things | PRB-04 | The check deciding "restart this" is distinct from the check deciding "send traffic here", so a temporary dependency problem does not cause a restart loop. | Medium |
| `HEALTH-005` | Health checks respond promptly | PRB-05 | Health checks are given a sensible time limit and frequency for their type, so a slow check does not delay failure detection. | Medium |
| `HEALTH-006` | Health checks point at a reachable port | PRB-06 | A health check that refers to a port by name uses a name the container actually defines, otherwise the check can never succeed. | High |

## 4.4 Identity and access

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `IAM-001` | Each service has its own identity | RBAC-01 | Every service runs under an identity created for it, not a shared account used by everything else in the namespace. | High |
| `IAM-002` | Services without platform access carry no platform key | RBAC-02 | A service that never talks to the platform's control system is not handed a key to it. | Medium |
| `IAM-003` | Permissions are specific, not blanket | RBAC-03 | No permission rule grants "everything on everything"; each rule names the exact actions and resources needed. | Critical |
| `IAM-004` | Permissions stay inside the namespace | RBAC-04 | A service cannot reach or affect anything outside its own namespace. | Critical |
| `IAM-005` | Credentials cannot be listed or harvested | RBAC-05 | No service can ask the platform for a list of all stored credentials; each may fetch only the specific ones it needs, by name. | Critical |
| `IAM-006` | Services cannot grant themselves more access | RBAC-06 | No service can create or alter permission rules, which would let it silently escalate its own privileges. | Critical |
| `IAM-007` | Services cannot impersonate others or open shells | RBAC-07 | No service can act as another identity, or open a command shell inside a running container. | Critical |

## 4.5 Container security

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `SEC-001` | Containers run as an unprivileged user | SEC-01 (split) | No container runs as the all-powerful root user. | Critical |
| `SEC-002` | Containers declare their unprivileged identity | SEC-01 (split) | Every container states that it must not run as root, rather than relying on the platform to impose it. | Medium |
| `SEC-003` | Containers use the standard system-call filter | SEC-06 | Every container enables the platform's standard restriction on which low-level operating system calls it may make. | Medium |
| `SEC-004` | Containers stay inside their own sandbox | SEC-07 | No container shares the host server's network, process list or memory, which would let it observe or affect other workloads. | Critical |

## 4.6 Configuration and secrets

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `CFG-001` | No credentials in plain configuration | CFG-01 | Passwords, tokens and keys are never written into ordinary, unprotected configuration files. | Critical |
| `CFG-002` | Configuration cannot drift after deployment | CFG-03 | Configuration is marked unchangeable, so it cannot be edited in place, leaving different copies of a service running different settings. | Medium |
| `CFG-003` | Certificates are delivered as files, not variables | CFG-06 | Certificates and keys are mounted as files rather than passed as environment variables, where they can leak into logs and crash reports. | Medium |

## 4.7 Networking

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `NET-001` | External entry points are encrypted | NET-05 | Anything reachable from outside the cluster requires an encrypted connection. | Critical |
| `NET-002` | Every internal address points somewhere | NET-07 | Every internal service address is wired to a real running workload, so nothing routes into a void. | Medium |

## 4.8 Storage

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `STOR-001` | Storage requests name the storage type | STO-01 | Every storage request names the class of storage it needs, rather than accepting whatever the cluster happens to default to. | Medium |
| `STOR-002` | Shared storage is used safely | STO-02 (redesigned, §5.3) | Where several copies write to the same shared folder, the chart follows the agreed rules for file ownership and permissions. | High |
| `STOR-003` | Multiple copies do not fight over one disk | STO-05 | A service running several copies does not point them all at a single exclusive disk, which only one can ever attach. | Critical |
| `STOR-004` | File ownership is set for shared folders | STO-08 | Services using persistent storage state which group owns the files, so they can read what they wrote after a restart. | Medium |
| `STOR-005` | Stateful services do not store state in temporary space | STO-10 | A service designed to remember things stores them on real disk, not in memory-backed scratch space that is wiped on every restart. | High |

## 4.9 Observability

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `OBS-001` | Live services publish health metrics | OBS-01 | Every service handling traffic publishes basic performance numbers, so operators can see it working or failing. | Medium |

## 4.10 Supply chain

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `SUP-001` | Deployed images cannot silently change | SUP-01 (split, §5.2) | No service points at a moving image label like "latest", which would let the running software change without any record. | Critical |
| `SUP-002` | Deployed images are exactly reproducible | SUP-01 (split) | Each image is identified by its exact content fingerprint, so the same version always deploys identical software. | Medium |

## 4.11 Metadata

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `META-001` | Workloads are identifiable | MTA-01 | Every workload carries standard identifying labels, so it can be found, grouped and reported on. | Medium |
| `META-002` | Addressing uses stable identifiers | MTA-03 | Internal addressing uses labels that do not change between releases, so routing does not break on every deployment. | Critical |
| `META-003` | Custom labels are namespaced | MTA-05 | Organisation-specific labels carry a domain prefix, so they cannot collide with platform labels. | Advisory |
| `META-004` | Setup tasks clean up after themselves | MTA-08 | One-off installation tasks state when they should be removed and how many times they may retry, so they do not accumulate or hang. | Medium |

## 4.12 Lifecycle

| ID | Name | Was | Plain statement | Sev |
|---|---|---|---|---|
| `LIFE-001` | All required extensions are installed first | UPG-07 | Where a chart uses a custom resource type, either it ships the definition or the definition is a known platform prerequisite. | Medium |

---

# Part 5 — Defects in current policies

Twelve defects, grouped by class. Each carries a synthetic reproduction and an acceptance test.

## 5.1 False positives

### 5.1.1 `NET-002` (was NET-07) — runtime labels treated as unmatchable

**Rate:** 100% of findings. **Severity:** Critical. **Volume:** ~24.

The scanner reports internal addresses as pointing nowhere, while its own output prints a valid target. Root cause: selectors are matched only against labels present in the rendered chart, and labels the platform adds at runtime are treated as non-existent.

```yaml
# FALSE POSITIVE — must not fail
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: app-alpha
spec:
  serviceName: app-alpha
  replicas: 3
  template:
    metadata:
      labels:
        app.kubernetes.io/name: app-alpha
        # statefulset.kubernetes.io/pod-name is added by the platform
        # at pod creation. It is never present in the chart.
---
apiVersion: v1
kind: Service
metadata:
  name: app-alpha-0
spec:
  clusterIP: None
  selector:
    app.kubernetes.io/name: app-alpha
    statefulset.kubernetes.io/pod-name: app-alpha-0
```

**Fix.** Maintain a runtime-supplied label allowlist, minimum contents:

```
statefulset.kubernetes.io/pod-name    controller-revision-hash
pod-template-hash                     pod-template-generation
apps.kubernetes.io/pod-index          batch.kubernetes.io/job-name
job-name
```

Reframe the policy — rendering a chart cannot prove an address has a live target, because that is a runtime property:

| Condition | Outcome |
|---|---|
| Selector absent entirely on a routable Service | Fail, High |
| All selector keys match a workload or are allowlisted | Pass |
| A selector key neither matches nor is allowlisted | Fail, **Medium**, confidence `Requires review` — may be an operator-managed label |
| `ExternalName` Service, or headless with selector deliberately omitted | Not applicable |

### 5.1.2 `META-002` (was MTA-03) — pod ordinals treated as unstable

**Rate:** 100%. **Severity:** Critical. **Volume:** ~20 — the same objects as 5.1.1, penalised twice.

The standard's prohibition (§10.2.1, §3.2.5) names commit SHAs, build IDs and timestamps. A pod ordinal is the opposite: stable for the life of that pod identity, and the documented mechanism for addressing individual members of a clustered service.

```yaml
# TRUE POSITIVE — value changes every build
selector:
  example.com/git-sha: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"

# FALSE POSITIVE — ordinal is stable across every rollout
selector:
  statefulset.kubernetes.io/pod-name: app-alpha-0
```

**Fix.** Classify selector keys:

| Class | Examples | Outcome |
|---|---|---|
| Stable identity | `app.kubernetes.io/name`, `/instance`, `/component`, `/part-of` | Pass |
| Stable runtime identity | `statefulset.kubernetes.io/pod-name`, `apps.kubernetes.io/pod-index` | Pass |
| Release-varying | keys matching `git-sha`, `commit`, `build-id`, `revision`; values matching 40-hex or RFC3339; `app.kubernetes.io/version` | Fail, Critical |

**Also:** per §3.4, two policies must not penalise one condition. Make `NET-002` primary and suppress `META-002` where both fire on the same object.

### 5.1.3 `CFG-001` (was CFG-01) — credential detection matches key names, not values

**Rate:** 100%. **Severity:** Critical. **Volume:** ~4. **This is the highest-consequence policy in the pack, and every finding it produced was wrong.**

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-alpha-config
data:
  # --- CURRENTLY FLAGGED, ALL FALSE ---
  SECRET_FETCH_RETRYCOUNT: "5"                 # a retry counter
  SECRET_REFRESH_INTERVAL_SEC: "300"           # a polling interval
  TOKEN_CACHE_TTL_SECONDS: "900"               # a cache lifetime
  PASSWORD_MIN_LENGTH: "12"                    # a policy parameter
  KEYSTORE_PATH: /etc/tls/keystore.jks         # a file path
  CREDENTIAL_PROVIDER_CLASS: com.example.Vault # a class name

  # --- CURRENTLY MISSED, ALL REAL ---
  DB_PASSWORD: "S3cr3t-P@ssw0rd-Value"
  API_TOKEN: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.abc123"
  CLOUD_ACCESS_KEY: "AKIAIOSFODNN7EXAMPLE"
  CONNECTION_STRING: "postgresql://svcuser:hunter2@db.example.com:5432/appdb"
```

**Fix — four stages.**

*Stage 1, exclude by key suffix:*
```
_COUNT _RETRYCOUNT _INTERVAL _INTERVAL_SEC _TIMEOUT _TTL _TTL_SECONDS
_SECONDS _MS _ENABLED _PATH _FILE _DIR _URL _URI _HOST _PORT _CLASS
_PROVIDER _MIN_LENGTH _MAX_LENGTH _POLICY _ALGORITHM _MODE _FORMAT
```

*Stage 2, exclude by value shape:* pure integer, boolean, absolute path, bare hostname, class or package identifier, empty string.

*Stage 3, flag by value shape:*

| Pattern | Regex sketch | Confidence |
|---|---|---|
| PEM private key | `-----BEGIN [A-Z ]*PRIVATE KEY-----` | Confirmed |
| JSON web token | `eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}` | Confirmed |
| Cloud access key | `\b(AKIA\|ASIA)[0-9A-Z]{16}\b` | Confirmed |
| URI with inline credentials | `[a-z][a-z0-9+.-]*://[^/\s:@]+:[^/\s:@]+@` | Confirmed |
| Authorisation header literal | `\b(Bearer\|Basic)\s+[A-Za-z0-9+/=_-]{16,}` | Confirmed |
| High-entropy opaque string | length ≥ 16, Shannon entropy ≥ 3.5 bits/char, not a path, URL or UUID | Probable |
| Known-weak literal | `admin`, `administrator`, `changeme`, `password`, `passw0rd`, `root`, `test`, `secret`, `default`, `123456` | Confirmed |

*Stage 4, corroborate:* for `Probable` matches only, require a credential token in the key name after Stage 1 exclusion.

**Acceptance test:** exactly 4 Critical findings on the sample above; zero on the first six keys.

### 5.1.4 `HEALTH-004` (was PRB-04) — handler comparison matches type, not content

**Rate:** ~32%. **Severity:** Critical. **Volume:** ~59, ~19 incorrect.

```yaml
# CASE 1 — TRUE POSITIVE, identical content
- name: c1
  livenessProbe:  { exec: { command: ["/opt/app/bin/health-check.sh"] } }
  readinessProbe: { exec: { command: ["/opt/app/bin/health-check.sh"] } }

# CASE 2 — FALSE POSITIVE, same type, different content
- name: c2
  livenessProbe:
    exec: { command: ["/bin/sh","-c","curl -sf http://127.0.0.1:8080/healthz/live"] }
  readinessProbe:
    exec: { command: ["/bin/sh","-c","curl -sf http://127.0.0.1:8080/healthz/ready"] }

# CASE 3 — FALSE POSITIVE, only one probe exists
- name: c3
  livenessProbe: { httpGet: { path: /healthz/live, port: 8080 } }

# CASE 4 — TRUE POSITIVE, identical including scheme and port
- name: c4
  livenessProbe:  { httpGet: { path: /healthz, port: 8443, scheme: HTTPS } }
  readinessProbe: { httpGet: { path: /healthz, port: 8443, scheme: HTTPS } }
```

**Fix.** Extract only the handler key (`httpGet`, `tcpSocket`, `exec`, `grpc`), discard timing fields, normalise (sort keys, apply defaults `scheme: HTTP` and `host: ""`, join `exec.command`), then compare deeply. If either probe is absent, return `Not applicable`.

**Also:** downgrade to Medium. §9.2.3 offers guidance on endpoint semantics; it does not prohibit a shared handler.

### 5.1.5 `HEALTH-006` (was PRB-06) — numeric probe ports incorrectly flagged

**Volume:** ~1. In Kubernetes, `containerPort` entries are informational — they open and reserve nothing. A numeric probe port resolves regardless. The policy is valid only for **named** ports, where an unresolvable name makes the check permanently fail.

```yaml
# FALSE POSITIVE — numeric port works with no ports block
- name: c1
  readinessProbe: { httpGet: { path: /healthz/ready, port: 8443, scheme: HTTPS } }

# TRUE POSITIVE — named port the container never declares
- name: c2
  ports: [{ name: http, containerPort: 8080 }]
  readinessProbe: { httpGet: { path: /healthz/ready, port: metrics } }
```

**Fix.** Skip integer ports. Fail High only on an unresolvable named port.

### 5.1.6 `LIFE-001` (was UPG-07) — platform-supplied extensions flagged

**Rate:** ~6%. **Volume:** ~50, ~3 incorrect. Two classes are wrongly flagged: built-in cluster APIs that are not custom resources at all and can never have a definition; and operator-supplied types installed by the platform team, which §19.2.1 explicitly assigns away from application charts.

**Fix.** Maintain a configurable platform-supplied API group allowlist. For allowlisted groups emit Advisory — "requires platform prerequisite `<group>`; confirm it is installed on the target cluster" — rather than a failure.

**Also:** the API group renders with a trailing period (`vendor.example.com/v1.`), making the field unusable for automated diffing.

## 5.2 Severity defects

### 5.2.1 `SUP-002` (was SUP-01) — image pinning over-severe and partly outside the chart's control

**Detection accuracy:** 100%. **Volume:** ~200 — the largest single Critical block, ~32% of all Criticals.

Three reasons the severity is wrong:

1. **The standard's prohibition is on moving labels, not on the absence of a fingerprint.** §11.2.4 places "Pin images by immutable digest" under *Do*, and "Use `:latest` tags in production" under *Don't*. Only the second is prohibitive.
2. **The prohibitive half was already satisfied.** Zero images in the reference run used `:latest` or an untagged reference. The scanner could not report this, because it emits no pass records — so a well-managed chart appeared to fail comprehensively.
3. **Fingerprint pinning is often a pipeline responsibility.** Where images are relocated into a target registry at deploy time, a fingerprint recorded at chart-authoring time does not survive the rewrite.

```yaml
# Author writes:
image: registry.example.com/app-alpha:1.4.0-rocky9
# Relocation rewrites at deploy time:
image: relay-registry:5000/mirror/app-alpha:1.4.0-rocky9
# An author-time fingerprint would be invalid after the rewrite:
image: registry.example.com/app-alpha@sha256:0f1e2d3c4b5a...
```

**Fix — split:**

| ID | Condition | Severity | Owner |
|---|---|---|---|
| `SUP-001` | Uses `:latest`, or no tag and no fingerprint | **Critical** | Chart template |
| `SUP-002` | Tagged but not fingerprint-pinned | **Medium** | Build pipeline |

### 5.2.2 `SEC-001` / `SEC-002` (was SEC-01) — title contradicts the data; inheritance unresolved

**Volume:** ~51, of which roughly 2% describe the stated condition.

Two problems. First, nearly all findings carry `Observed: runAsNonRoot not set`, but on a platform enforcing a restricted security policy, an unset user results in the platform assigning an arbitrary **non-root** identity. Those containers do not run as root. Second, pod-level inheritance is not resolved: findings reading "runAsNonRoot not set" appeared on pods where it was explicitly set to `false` at pod level — a **more** serious condition, reported with wording implying it was merely unspecified.

```yaml
# CASE 1 — explicit root on the container. Critical.
containers: [{ name: c1, securityContext: { runAsUser: 0 } }]

# CASE 2 — explicitly disabled at pod level, inherited.
#          Currently reported as "not set". Must be Critical, with
#          observed_source = "inherited from pod".
securityContext: { runAsNonRoot: false }
containers: [{ name: c2, securityContext: { allowPrivilegeEscalation: false } }]

# CASE 3 — nothing asserted. Platform assigns a non-root identity.
#          Currently Critical. Must be Medium.
containers:
  - name: c3
    securityContext:
      allowPrivilegeEscalation: false
      capabilities: { drop: ["ALL"] }

# CASE 4 — compliant. Must pass.
securityContext: { runAsNonRoot: true, runAsUser: 10001 }
```

**Fix.** Resolve the effective security context first — container value wins, else inherit from pod, else unset — and record provenance in `observed_source`. Then split:

| ID | Effective condition | Severity |
|---|---|---|
| `SEC-001` | `runAsUser == 0` or `runAsNonRoot == false` | **Critical** |
| `SEC-002` | Neither asserted at either level | **Medium** |

### 5.2.3 `META-001` (was MTA-01) — cosmetic condition rated Critical

**Volume:** ~130, ~21% of all Criticals. Missing identifying labels degrade queryability and reporting. They cause no outage, no exposure and no blocked upgrade.

| Missing label | Severity |
|---|---|
| `app.kubernetes.io/name` | Medium — appears in selectors and spread rules |
| `app.kubernetes.io/instance` | Medium — same |
| `app.kubernetes.io/component` | Advisory |
| `app.kubernetes.io/part-of` | Advisory |
| `app.kubernetes.io/managed-by` | Advisory |
| `app.kubernetes.io/version` | Advisory |

**Also:** emit one finding per resource listing all missing labels, not one per label per resource.

```
# CURRENT — two rows, one remediation action
META-001,Critical,Deployment,app-alpha,labels[app.kubernetes.io/component],(absent)
META-001,Critical,Deployment,app-alpha,labels[app.kubernetes.io/part-of],(absent)

# PROPOSED — one row
META-001,Advisory,Deployment,app-alpha,metadata.labels,
  "missing: app.kubernetes.io/component, app.kubernetes.io/part-of"
```

### 5.2.4 `SCHED-001` (was SCH-01) — recommendation rated as prohibition

**Volume:** ~11. The finding text is genuinely good. But §3.2.1 advises starting with **soft** spreading, and §3.2.6, §13.2.4 and §14.2.3 all warn against hard constraints without proven capacity. Absence is a resilience gap, not a prohibited configuration. **Downgrade to Medium.**

Note the asymmetry: the standard warns about over-constraining more forcefully than under-constraining, and the scanner detects only the latter. See `SCHED-002` in Part 6.

## 5.3 Policy contradicting the standard

### `STOR-002` (was STO-02) — shared storage rule is inverted

**Volume:** ~6. Current behaviour:

```
Expected:    an access mode other than ReadWriteMany
Remediation: ...or use ReadWriteOnce with a StatefulSet
```

This contradicts §16.2, "RWX Storage via NFS" — the most recent substantive addition to the standard. That section does not discourage shared-write storage. It legitimises it and defines a contract: non-root execution, documented ownership per mount, access via supplementary groups rather than startup ownership changes, server-side protections retained, directory permissions pre-provisioned.

A rule telling teams to abandon shared storage directs them away from the standard's own guidance. This is the clearest instance of drift from the source document and the defect most likely to erode trust in the tool's fidelity.

**Fix — replace with a contract check.**

```yaml
# COMPLIANT — must pass
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: shared-data-claim }
spec:
  accessModes: ["ReadWriteMany"]
  storageClassName: shared-file-standard
  resources: { requests: { storage: 50Gi } }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: app-alpha }
spec:
  replicas: 3
  template:
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 10001
        runAsGroup: 10001
        supplementalGroups: [20001]        # group access, not ownership change
        fsGroupChangePolicy: OnRootMismatch
      containers:
        - name: app
          volumeMounts: [{ name: shared-data, mountPath: /var/shared }]
      volumes:
        - name: shared-data
          persistentVolumeClaim: { claimName: shared-data-claim }
```

```yaml
# NON-COMPLIANT — must fail
spec:
  template:
    spec:
      securityContext: { runAsUser: 0 }                    # DEFECT
      initContainers:
        - name: fix-perms
          command: ["sh","-c","chown -R 1001:1001 /var/shared"]   # DEFECT
          volumeMounts: [{ name: shared-data, mountPath: /var/shared }]
```

| Sub-policy | Condition | Severity |
|---|---|---|
| `STOR-002a` | A pod mounting a shared-write claim runs as root | Critical |
| `STOR-002b` | A container or init container runs `chown`, `chmod` or `chgrp` against a shared-write mount path | Critical |
| `STOR-002c` | A pod mounting a shared-write claim declares neither a file-system group nor supplementary groups | Medium |
| `STOR-002d` | A shared-write claim is mounted writable by more than one distinct workload | Advisory — flag for concurrent-writer review |

Remove the "use a different access mode" remediation entirely.

## 5.4 Description defects

### 5.4.1 `AVAIL-004` (was PDB-08) — "absent" described as "zero"

**Volume:** ~113, ~8% of the entire report. Every finding was `(absent)`; **not one** pod declared a shutdown time of zero. The wording "expected set, above zero" implies pods are killed instantly. They are not — the platform applies a 30-second default.

| Condition | Severity | Proposed `observed_value` |
|---|---|---|
| Explicitly `0` | Medium | `0 — pods are killed immediately, work in progress is lost` |
| Absent, no traffic | Advisory | `not declared; 30s platform default applies` |
| Absent, receives traffic | Advisory | `not declared; 30s platform default applies. Declare explicitly if requests can take longer.` |

### 5.4.2 `HEALTH-005` (was PRB-05) — web-request time limits applied to script checks

**Volume:** ~63; ~89% of the flagged checks execute shell scripts.

```
Expected: timeoutSeconds in [1,3] and periodSeconds in [5,10]
```

§9.2.4 scopes that band explicitly to "in-cluster HTTP endpoints". A script-based check pays process-fork, shell-startup and interpreter cost before the check begins. Holding it to a web request's latency budget misreads the standard.

**Fix — bounds by check type:**

| Check type | Time limit | Frequency |
|---|---|---|
| Web request, TCP connection, gRPC | 1–3 s | 5–10 s |
| Script execution | 1–10 s | 5–30 s |

Add a type-independent structural rule, which catches real defects the current bound misses:

| Rule | Severity |
|---|---|
| Time limit exceeds check frequency (checks overlap; failure detection time becomes unbounded) | Medium |

### 5.4.3 `IAM-002` (was RBAC-02) — finding reads as compliant

**Volume:** ~67, ~79% showing `(absent)`. Absent and `true` produce the identical runtime outcome — a platform key is mounted — but a reviewer scanning the column will read them as different severities.

| Manifest state | Proposed `observed_value` |
|---|---|
| Omitted | `not declared; defaults to true — a platform key is mounted` |
| `true` | `true — a platform key is mounted` |
| `false` | Pass |

Apply this provenance principle wherever a platform default determines the outcome: shutdown time, update strategy, rollout deadline, revision history, restart policy.

### 5.4.4 Empty `observed_value` on two policy families

**Volume:** ~47 findings, all Critical-rated — among the first a reviewer will open.

```csv
# Probe comparison — handler never printed; template substitution failed
HEALTH-004,Critical,Deployment,app-alpha,c1,livenessProbe," on ",
  "livenessProbe —  on ; expected a liveness handler distinct from readiness"

# Update strategy — observed value omitted entirely
AVAIL-003,Critical,Deployment,app-gamma,,spec.strategy.type,,
  "spec.strategy.type; expected RollingUpdate for a workload receiving traffic"
```

**Fix, plus a standing guard.** Assert as a build-time invariant: for every emitted row where `field_path` is non-empty, `observed_value` must be non-empty and contain no unsubstituted template fragments. This single assertion would have caught both defects before release.

---

# Part 6 — New policies

47 proposed rules, each grounded in a clause of the governing standard. Grouped by domain. Priority indicates implementation order.

## 6.1 Configuration and secrets — 8 new policies

**This group contains the most serious gap in the current pack.** The scanner examines ordinary configuration for credential-shaped key names but performs **no analysis of protected secret objects shipped inside the chart**. In the reference run this meant a substantial population of secrets carrying inline material — private keys, database passwords, a default administrative credential — was emitted with no finding at all.

This is the exact inverse of the `CFG-001` defect: the policy written to find credentials produced only false positives, while the real exposure went entirely undetected.

### `CFG-004` — Credentials are not shipped inside the chart

| | |
|---|---|
| **Plain statement** | The chart does not carry passwords, keys or certificates inside it. Credentials are fetched from a secure store at install time. |
| **Why it matters** | Anything inside a chart is stored in version control, copied to every mirror, included in every archive, and visible to anyone who can read the repository. A credential shipped in a chart must be assumed compromised from the moment it is committed, and rotating it means rebuilding and redistributing the chart. |
| **Severity** | **Critical** |
| **Detects** | A `Secret` object with inline `data` or `stringData` |
| **Does not detect** | External secret references; empty secret shells created for a later process to populate |
| **Grounding** | §7.2.6 "Don't store plaintext secrets in Git"; §19.2.5 "Secrets in Git are a common compromise vector"; §11.2.3 |
| **Effort** | Medium — requires a secret store integration |
| **Priority** | **1** |

```yaml
# FAIL
apiVersion: v1
kind: Secret
metadata: { name: app-alpha-db }
type: Opaque
data:
  password: UzNjcjN0LVAhc3N3MHJkLTIwMjY=

# PASS — reference, not material
apiVersion: external-secrets.example.com/v1
kind: ExternalSecret
metadata: { name: app-alpha-db }
spec:
  secretStoreRef: { name: platform-vault, kind: ClusterSecretStore }
  target: { name: app-alpha-db }
  data:
    - secretKey: password
      remoteRef: { key: app-alpha/db, property: password }
```

### `CFG-005` — Private keys are never embedded

| | |
|---|---|
| **Plain statement** | No private cryptographic key is written into the chart. |
| **Why it matters** | A private key is the single credential that cannot be contained after exposure. Anyone holding it can impersonate the service indefinitely. Unlike a password, it cannot be invalidated by a policy change — every certificate issued from it must be revoked and reissued. |
| **Severity** | **Critical** |
| **Detects** | Any decoded secret or configuration value containing `-----BEGIN ... PRIVATE KEY-----` |
| **Grounding** | §7.2.6; §11.2.3 "Don't store secrets in ConfigMaps, images, or Git repositories" |
| **Priority** | **1** |

**Implementation note.** Base64-decode all values before analysis. In the reference run, credentials were plainly visible after a single decode; a scanner inspecting only the encoded form will miss them.

**Reporting note.** Never print the decoded value. Report the object name, the key name, and the class of material. The report is itself a distributable artefact.

```
CFG-005 | Critical | Secret "app-alpha-tls", key "tls.key"
  contains an inline private key (RSA, PEM-encoded).
  Do not include the key value in any report or ticket.
```

### `CFG-006` — No default or well-known credentials

| | |
|---|---|
| **Plain statement** | No account ships with a guessable password such as "admin" or "changeme". |
| **Why it matters** | Default credentials are the first thing an attacker tries and require no skill to exploit. They are frequently never changed after installation because nothing forces the change. |
| **Severity** | **Critical** |
| **Detects** | Any decoded value in `{admin, administrator, changeme, password, passw0rd, root, test, guest, default, 123456, secret}` |
| **Grounding** | §11.2.3; the standard's own forward plan names "default credentials" as a compliance area |
| **Priority** | **1** |

### `CFG-007` — Certificates come from an issuer, not the chart

| | |
|---|---|
| **Plain statement** | Where a service needs a certificate, the chart requests one rather than carrying a pre-made certificate and its key. |
| **Why it matters** | A certificate shipped in a chart has a fixed expiry, so every renewal requires a chart rebuild. It is also identical across every installation, so one compromise affects every deployment everywhere. |
| **Severity** | **Critical** |
| **Detects** | A `kubernetes.io/tls` secret shipping `tls.key` inline |
| **Grounding** | §7.2.6; §11.2.5 |
| **Priority** | 2 |

### `CFG-008` — Credentials are not passed on command lines

| | |
|---|---|
| **Plain statement** | Passwords and tokens are not written into container start-up commands or arguments. |
| **Why it matters** | Command-line arguments are visible to every process on the same machine and are captured in crash dumps and process listings. A credential passed this way leaks to anyone who can run a process listing. |
| **Severity** | **High** |
| **Detects** | `command` or `args` entries matching credential value patterns, or referencing a secret via inline substitution |
| **Grounding** | §7.2.4 "Don't put secrets in command-line args (frequently exposed in process listings)" |
| **Priority** | 2 |

```yaml
# FAIL
command: ["/opt/app/bin/start", "--db-password=S3cr3t-P@ssw0rd"]

# PASS
command: ["/opt/app/bin/start"]
env:
  - name: DB_PASSWORD
    valueFrom: { secretKeyRef: { name: app-alpha-db, key: password } }
```

### `CFG-009` — Only the needed part of a credential is mounted

| | |
|---|---|
| **Plain statement** | Where a service needs one value from a credential store, it is given that one value, not the whole store. |
| **Why it matters** | Mounting an entire credential set to obtain one value exposes every other value in it to the same process. If that process is compromised, the blast radius is everything it was over-granted. |
| **Severity** | **Medium** |
| **Detects** | `envFrom.secretRef` where the secret has more than one key; whole-secret volume mounts where only a subset of keys is referenced elsewhere |
| **Does not detect** | Whole-secret mounts where the secret has exactly one key |
| **Grounding** | §7.2.4 "Don't mount an entire secret if you only need one key" |
| **Priority** | 3 |

### `CFG-010` — Configuration changes trigger a restart

| | |
|---|---|
| **Plain statement** | When configuration changes, the chart ensures running copies actually pick it up. |
| **Why it matters** | Changing configuration does not restart a service. Without a restart trigger, the change appears applied but nothing has changed — until an unrelated event restarts one copy, at which point some copies run the new configuration and others the old. This produces intermittent, extremely hard to diagnose behaviour. |
| **Severity** | **Medium** |
| **Detects** | A workload references a configuration or secret object but carries no `checksum/*` annotation on the pod template |
| **Does not detect** | Workloads whose configuration is mounted and documented as hot-reloadable |
| **Grounding** | §7.2.3; §8.2.2 "Don't assume updating a ConfigMap/Secret will automatically restart pods"; §22.2.4 |
| **Priority** | 2 |

```yaml
# PASS
spec:
  template:
    metadata:
      annotations:
        checksum/config: "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f0"
        checksum/secret: "sha256:9e8d7c6b5a4938271605f4e3d2c1b0a9"
```

### `CFG-011` — Restart triggers are stable

| | |
|---|---|
| **Plain statement** | The value used to detect configuration changes is derived from the configuration itself, not from the time of installation. |
| **Why it matters** | If the restart trigger includes a timestamp or random value, every single deployment restarts every copy of the service, whether anything changed or not. This causes unnecessary outages and makes automated deployment tooling report constant drift. |
| **Severity** | **Medium** |
| **Detects** | `checksum/*` annotation values matching a timestamp, random string, or install-time identifier |
| **Grounding** | §22.2.4 "Don't restart pods on every Helm upgrade due to unstable checksum inputs (e.g. timestamps)" |
| **Priority** | 3 |

## 6.2 Container security — 7 new policies

### `SEC-005` — No container runs with full host privileges

| | |
|---|---|
| **Plain statement** | No container is granted unrestricted control of the server it runs on. |
| **Why it matters** | A fully privileged container can read and modify anything on the host, including other services' data and the platform's own components. A single vulnerability in that container becomes a compromise of the entire server and, in practice, the cluster. |
| **Severity** | **Critical** |
| **Detects** | `securityContext.privileged: true` on any container or init container |
| **Grounding** | §11.2.1 "Don't use privileged: true or host namespaces unless there is a reviewed exception"; §6.1.2 |
| **Priority** | **1** |

### `SEC-006` — Containers cannot gain privileges while running

| | |
|---|---|
| **Plain statement** | A container cannot acquire more permissions after it has started than it was given at launch. |
| **Why it matters** | Without this control, a program inside the container can use standard operating system mechanisms to elevate itself to full administrative rights, defeating every other restriction placed on it. |
| **Severity** | **High** |
| **Detects** | `allowPrivilegeEscalation` not `false` (resolved through pod-level inheritance) |
| **Grounding** | §11.2.1 "Set allowPrivilegeEscalation: false"; §6.1.2; §12.2.6 |
| **Priority** | **1** |

### `SEC-007` — Containers hold only the permissions they need

| | |
|---|---|
| **Plain statement** | Containers give up all special operating system permissions, and take back only the specific ones they can justify. |
| **Why it matters** | Special permissions such as the ability to change file ownership or act as another user are frequently added to work around a startup problem, then never removed. Each one widens what an attacker can do after gaining a foothold. |
| **Severity** | See sub-policies |
| **Grounding** | §11.2.1 "Drop all capabilities and add only what is proven necessary"; §6.1.2 |
| **Priority** | 2 |

| Sub-policy | Condition | Severity |
|---|---|---|
| `SEC-007a` | Adds a high-risk permission: full administration, network administration, process tracing, kernel module loading, or file-permission bypass | **Critical** |
| `SEC-007b` | Adds a privilege-relevant permission: act as another user, act as another group, change file ownership, raw network access | **Medium** |
| `SEC-007c` | Does not give up all permissions before adding any back | **Medium** |

`SEC-007b` should cross-reference `STOR-002b`. Ownership-change permissions are frequently added specifically to modify file ownership at startup, which §16.2 prohibits on shared storage. The two findings together tell a complete story that neither tells alone.

### `SEC-008` — Containers cannot modify their own software

| | |
|---|---|
| **Plain statement** | A container's own program files are read-only; it writes only to folders explicitly provided for the purpose. |
| **Why it matters** | If an attacker can write to a container's program files, they can replace the program itself and persist across restarts. Read-only program files mean a restart always returns a known-good state. |
| **Severity** | **Medium** |
| **Detects** | `readOnlyRootFilesystem` not `true`, resolved through pod-level inheritance |
| **Grounding** | §11.2.1 "Use readOnlyRootFilesystem: true for stateless workloads"; §6.1.2 |
| **Priority** | 2 |

```yaml
# PASS — read-only, with a writable scratch area provided
securityContext:
  readOnlyRootFilesystem: true
volumeMounts: [{ name: tmp, mountPath: /tmp }]
volumes: [{ name: tmp, emptyDir: {} }]
```

Pair with an Advisory sub-policy: where program files are read-only but no writable scratch area is provided, the container is likely to fail at runtime if it writes temporary files.

### `SEC-009` — No container mounts a host folder

| | |
|---|---|
| **Plain statement** | No container reaches directly into the server's own file system. |
| **Why it matters** | A host folder mount lets a container read or modify files belonging to the server and to every other service on it. Mounting the container runtime's own control socket is equivalent to granting full cluster administration. |
| **Severity** | **Critical** for runtime sockets and sensitive system paths; **High** otherwise |
| **Detects** | Any `hostPath` volume. Critical where the path matches a container runtime socket, `/etc`, `/var/lib/kubelet`, `/root`, `/proc`, `/sys` |
| **Grounding** | §11.2.1 "Don't mount the Docker socket or hostPath volumes to application pods"; §12.2.6; §6.1.2 |
| **Priority** | **1** |

### `SEC-010` — No container claims a fixed port on the server

| | |
|---|---|
| **Plain statement** | Containers do not reserve a specific port number on the physical server. |
| **Why it matters** | A fixed server port means only one copy of the service can run per server, silently capping how far it can scale. It also bypasses the platform's network controls, exposing the service on the server's own address. |
| **Severity** | **High** |
| **Detects** | Any `containerPort` entry with a `hostPort` |
| **Grounding** | §6.1.2 "allowHostPorts: false" in the recommended baseline; §11.2.1 |
| **Priority** | 2 |

### `SEC-011` — The chart does not grant its own elevated permissions

| | |
|---|---|
| **Plain statement** | The chart does not include a rule that raises its own security limits, and does not grant elevated rights to broad groups. |
| **Why it matters** | A chart that installs its own permission-raising rule bypasses the platform's security review entirely. Granting elevated rights to a broad group extends them to every service in the namespace, including services installed later by unrelated teams. |
| **Severity** | **Critical** |
| **Detects** | A workload security policy object shipped in the chart; or a permission binding whose subject is a broad system group rather than a specific identity |
| **Grounding** | §6.1.2 "Don't grant SCC use to broad groups like system:authenticated or system:serviceaccounts:<namespace>"; §5.2.6 |
| **Priority** | 2 |

## 6.3 Identity and access — 4 new policies

### `IAM-008` — Credentials cannot be modified by the services that read them

| | |
|---|---|
| **Plain statement** | A service may read the credentials it needs, but cannot change, create or delete them. |
| **Why it matters** | The current rules prevent a service from listing all credentials, but not from overwriting one. A compromised service that can rewrite a credential can lock out the legitimate owner or substitute one it controls. |
| **Severity** | **Critical** |
| **Detects** | `create`, `update`, `patch`, `delete` or `deletecollection` on secrets, in any role in the release |
| **Does not detect** | Roles belonging to a component whose documented purpose is credential management, where flagged as `Requires review` |
| **Grounding** | §5.2.5 "Don't grant update/patch on Secrets unless required (and reviewed)" |
| **Priority** | **1** |

**Note.** This is a genuine gap, not a refinement. The existing rule covers only listing and enumeration. Write access to credentials is at least as serious and is currently invisible.

### `IAM-009` — Credential access is scoped to named items

| | |
|---|---|
| **Plain statement** | Where a service may read credentials, the rule names exactly which ones. |
| **Why it matters** | A rule granting read access to credentials in general lets the service fetch any credential in the namespace, including those belonging to other services, as long as it can guess or discover the name. |
| **Severity** | **High** |
| **Detects** | A `get` permission on secrets with no `resourceNames` restriction |
| **Grounding** | §5.2.5 "Only grant get to specific Secrets the workload must read"; §7.2.5 |
| **Priority** | 2 |

```yaml
# FAIL — any secret in the namespace
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]

# PASS — exactly two, by name
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["app-alpha-db", "app-alpha-tls"]
    verbs: ["get"]
```

### `IAM-010` — Permissions are not granted to everyone

| | |
|---|---|
| **Plain statement** | Permissions are given to a specific service identity, never to "all authenticated users" or "everything in this namespace". |
| **Why it matters** | A permission granted to a broad group applies to every service in scope, including ones installed later by other teams. The grant becomes invisible over time and nobody can safely remove it. |
| **Severity** | **Critical** |
| **Detects** | A permission binding whose subject is a broad system group |
| **Grounding** | §5.2.1; §6.1.2 |
| **Priority** | 2 |

### `IAM-011` — Setup tasks do not run with deployment-level rights

| | |
|---|---|
| **Plain statement** | One-off installation tasks use their own limited identity, not the same identity the running service uses. |
| **Why it matters** | Installation tasks often need broad rights to create resources. If the running service shares that identity, it keeps those rights forever, long after they are needed. |
| **Severity** | **Medium** |
| **Detects** | A hook or setup Job sharing a service identity with a long-running workload, where that identity holds create or update rights |
| **Grounding** | §5.2.4 "Don't reuse the runtime SA for deployments (it leads to over-permissioned runtime pods)" |
| **Priority** | 3 |

## 6.4 Availability and disruption — 5 new policies

### `AVAIL-006` — Maintenance rules leave room to move

| | |
|---|---|
| **Plain statement** | A service never demands that every copy stays running, which would make maintenance impossible. |
| **Why it matters** | If a service requires all copies to remain available, the platform can never get permission to move any of them. Maintenance on any hosting server hangs indefinitely, and a cluster-wide upgrade stalls until someone intervenes manually. The standard names this as a frequent cause of stalled upgrades. |
| **Severity** | **High** |
| **Detects** | A disruption rule where the minimum-available count equals or exceeds the replica count, or where the maximum-unavailable count is zero |
| **Grounding** | §4.2.2 "Don't set minAvailable equal to replicas... commonly deadlocks upgrades"; §4.2.5; §21.2.2 |
| **Priority** | **1** |

```yaml
# FAIL — deadlock. 3 copies, all 3 must stay up.
spec:
  replicas: 3
---
spec:
  minAvailable: 3
  selector: { matchLabels: { app.kubernetes.io/name: app-alpha } }

# FAIL — deadlock. Zero may ever be unavailable.
spec:
  maxUnavailable: 0

# PASS
spec:
  maxUnavailable: 1
```

**Note.** The existing `AVAIL-002` catches only the single-copy case. The general deadlock — the one the standard warns about most explicitly — is currently undetected.

### `AVAIL-007` — Every maintenance rule protects something

| | |
|---|---|
| **Plain statement** | Each maintenance rule matches a real service in the release. |
| **Why it matters** | A rule whose labels match nothing protects nothing. It appears in the chart, passes review, and provides zero protection — the most dangerous kind of failure, because it looks correct. |
| **Severity** | **Medium** |
| **Detects** | A disruption rule whose selector matches no workload in the release |
| **Grounding** | §4.2.1; §4.2.7 |
| **Priority** | 2 |

### `AVAIL-008` — Updates always have room to manoeuvre

| | |
|---|---|
| **Plain statement** | During an update, the chart allows either a temporary extra copy or a temporary missing copy — at least one of the two. |
| **Why it matters** | If neither is permitted, the update has no way to proceed: it cannot start a new copy without room, and cannot stop an old one without breaching the availability rule. The deployment hangs with no error, and the standard calls this out specifically. |
| **Severity** | **High** |
| **Detects** | `maxSurge: 0` and `maxUnavailable: 0` on the same rolling update |
| **Grounding** | §21.2.2 "ensure your rollout strategy provides at least one safety valve"; §14.2.4 |
| **Priority** | 2 |

### `AVAIL-009` — Services handling live traffic run more than one copy

| | |
|---|---|
| **Plain statement** | Anything receiving user traffic runs at least two copies. |
| **Why it matters** | A single copy means every restart, every update and every server maintenance event is a full outage. No amount of maintenance-rule configuration can protect a service that only exists once. |
| **Severity** | **Medium**, confidence `Requires review` — single-copy may be a deliberate choice for non-critical services |
| **Detects** | A workload with `replicas: 1` that is the target of a Service and declares a readiness check |
| **Grounding** | §14.2.3 "node failure: >= 2 replicas on distinct nodes"; §4.2.1 |
| **Priority** | 3 |

### `AVAIL-010` — Shutdown does not rely on long fixed delays

| | |
|---|---|
| **Plain statement** | Services shut down when their work is finished, rather than waiting out a fixed sleep. |
| **Why it matters** | A long fixed delay before shutdown makes every update, restart and maintenance operation slower by that amount, multiplied by every copy. On a large release this turns a five-minute upgrade into an hour. |
| **Severity** | **Advisory** |
| **Detects** | A pre-stop action consisting of a sleep longer than 30 seconds |
| **Grounding** | §14.2.4 "Use preStop hooks only when needed and tested (avoid long sleeps without justification)" |
| **Priority** | 3 |

## 6.5 Scheduling — 2 new policies

### `SCHED-002` — Placement rules do not paint the service into a corner

| | |
|---|---|
| **Plain statement** | The chart does not stack multiple strict placement requirements that together make a service impossible to move. |
| **Why it matters** | Each strict rule individually seems prudent. Together they can make it impossible for the platform to find anywhere to place a copy during maintenance or after a failure. The service then cannot recover, and the more rules were added to protect it, the more thoroughly it is stuck. The standard warns about this more forcefully than about the absence of placement rules. |
| **Severity** | **Medium** |
| **Detects** | Two or more of: required node affinity, required anti-affinity, spread constraint set to refuse scheduling |
| **Grounding** | §3.2.6 "Don't combine multiple hard constraints... unless you've done failure-mode testing"; §13.2.4; §14.2.3 |
| **Priority** | 2 |

```yaml
# FAIL — three strict rules stacked
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution: { ... }
  podAntiAffinity:
    requiredDuringSchedulingIgnoredDuringExecution: [ ... ]
topologySpreadConstraints:
  - whenUnsatisfiable: DoNotSchedule
    topologyKey: topology.kubernetes.io/zone

# PASS — one strict rule, one flexible
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution: { ... }
topologySpreadConstraints:
  - whenUnsatisfiable: ScheduleAnyway
    topologyKey: topology.kubernetes.io/zone
```

### `SCHED-003` — Placement rules use stable identifiers

| | |
|---|---|
| **Plain statement** | Rules that spread copies apart refer to labels that do not change between releases. |
| **Why it matters** | If the spreading rule refers to a label containing a build number, the rule silently stops working after the first update — copies of the new version do not recognise each other and can all land on one server. |
| **Severity** | **High** |
| **Detects** | A spread constraint or anti-affinity selector keyed on a release-varying label |
| **Grounding** | §3.2.5 "Don't use release-specific labels (commit hashes, build IDs) as the selector basis; it breaks spreading" |
| **Priority** | 2 |

## 6.6 Resources — 6 new policies

The scanner currently evaluates CPU **caps** but not resource **reservations**. A cap without a reservation is the wrong half of the pair: reservations drive placement and eviction priority, and their absence is what the standard prohibits.

### `RES-001` — Services reserve the processing power they need

| | |
|---|---|
| **Plain statement** | Every container states how much processing capacity it needs, so the platform can place it sensibly. |
| **Why it matters** | Without a reservation, the platform cannot tell whether a server has room, so it places the service anywhere and hopes. Under pressure these services are the first to be terminated, without warning, regardless of importance. |
| **Severity** | **Medium** |
| **Detects** | No `resources.requests.cpu` |
| **Grounding** | §13.2.2 "Don't run without requests (unpredictable placement and eviction risk)"; §12.2.5 |
| **Priority** | 2 |

### `RES-002` — Services reserve the memory they need

| | |
|---|---|
| **Plain statement** | Every container states how much memory it needs. |
| **Why it matters** | As `RES-001`. Memory is the more urgent of the two, because a server running out of memory terminates services abruptly with no graceful shutdown. |
| **Severity** | **Medium** |
| **Grounding** | §13.2.2; §12.2.5 |
| **Priority** | 2 |

### `RES-004` — Services cap their memory use

| | |
|---|---|
| **Plain statement** | Every container states a maximum memory it may use. |
| **Why it matters** | One service with a memory leak and no cap will consume the entire server and take down every other service running on it. The cap converts a single-service failure into a single-service failure, rather than a server-wide one. |
| **Severity** | **Medium** |
| **Grounding** | §13.2.2 "Set memory limits to protect node stability"; §11.2.1 |
| **Priority** | 2 |

### `RES-005` — Memory caps are proportionate to reservations

| | |
|---|---|
| **Plain statement** | A service's memory cap is not far above what it reserves. |
| **Why it matters** | A service reserving a little and permitted a great deal will be placed on a server that cannot actually satisfy it. Under load it grows into space that is not there, and either it or a neighbouring service is terminated. |
| **Severity** | **Advisory** |
| **Detects** | Memory cap more than four times the memory reservation |
| **Grounding** | §13.2.2 "Set memory limits... size to avoid OOM during peak" |
| **Priority** | 3 |

### `RES-006` — Caps are never below reservations

| | |
|---|---|
| **Plain statement** | No container is permitted less than it reserves. |
| **Why it matters** | This configuration is invalid. The platform rejects the service outright and it never starts. |
| **Severity** | **Critical** |
| **Detects** | Any cap lower than its corresponding reservation |
| **Grounding** | §13.2.2 |
| **Priority** | 2 |

### `RES-007` — Sidecars declare their own footprint

| | |
|---|---|
| **Plain statement** | Helper containers running alongside the main service state their own resource needs. |
| **Why it matters** | Helper containers are easy to overlook, and their consumption is charged to the same pod. An unaccounted helper causes the whole pod to be misplaced or terminated, taking the main service with it. |
| **Severity** | **Medium** |
| **Detects** | A sidecar or init container with no resource declaration where the main container has one |
| **Grounding** | §13.2.2 "Use separate requests/limits per container (including sidecars)" |
| **Priority** | 3 |

## 6.7 Networking — 6 new policies

### `NET-003` — Internal traffic is restricted by default

| | |
|---|---|
| **Plain statement** | The release includes a rule blocking unexpected internal network connections, then explicitly permits the ones it needs. |
| **Why it matters** | By default, anything in the cluster can connect to anything else. Without a blocking rule, a compromise of any single service gives an attacker a clear path to every other service, including databases that were never meant to be reachable. |
| **Severity** | **Medium**, confidence `Probable` — policy may be applied centrally by the platform |
| **Detects** | The release ships workloads but no default-deny ingress rule |
| **Grounding** | §15.2.3 "Implement namespace-level default deny ingress"; §11.2.5 |
| **Priority** | 2 |

```yaml
# The baseline this policy looks for
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: default-deny-ingress }
spec:
  podSelector: {}
  policyTypes: ["Ingress"]
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: allow-app-alpha }
spec:
  podSelector: { matchLabels: { app.kubernetes.io/name: app-alpha } }
  policyTypes: ["Ingress"]
  ingress:
    - from: [{ podSelector: {} }]
      ports: [{ protocol: TCP, port: 8080 }]
```

Where network rules are owned centrally, emit this with `fix_owner: Platform team` rather than suppressing it. Silence is worse than a correctly attributed finding.

### `NET-004` — Network rules do not admit everything

| | |
|---|---|
| **Plain statement** | No network rule permits connections from every namespace in the cluster. |
| **Why it matters** | A rule that admits all namespaces provides the appearance of network control with none of the substance. It is frequently added to resolve a connectivity problem during testing and never narrowed. |
| **Severity** | **Medium** |
| **Detects** | A network rule with an unconstrained namespace selector |
| **Grounding** | §15.2.3 "Don't use allow all from all namespaces unless it is a deliberate platform/shared-service policy" |
| **Priority** | 3 |

### `NET-005` — Network rules use stable identifiers

| | |
|---|---|
| **Plain statement** | Network rules refer to labels that do not change between releases. |
| **Why it matters** | If a network rule refers to a build number, the rule stops matching after the first update. Traffic that should be permitted is silently blocked, or traffic that should be blocked is silently permitted. |
| **Severity** | **High** |
| **Detects** | A network rule selector keyed on a release-varying label |
| **Grounding** | §11.2.5 "Ensure label strategy is stable (avoid release-hash labels in policies)"; §15.2.3 |
| **Priority** | 3 |

### `NET-006` — Administrative interfaces are not exposed externally

| | |
|---|---|
| **Plain statement** | Management, debug and metrics interfaces are not reachable from outside the cluster. |
| **Why it matters** | Administrative interfaces frequently have weaker authentication than the main service, because they were designed for internal use. Exposing one externally hands an attacker the easiest available route in. |
| **Severity** | **Critical** |
| **Detects** | An external entry point routing to a path or port named `admin`, `debug`, `metrics`, `actuator`, `console`, `management`, or to a port carrying such a name |
| **Grounding** | §11.2.5 "Don't expose internal services publicly via Route without explicit review"; §12.2.7; §17.3.4 |
| **Priority** | **1** |

### `NET-007` — External exposure is deliberate

| | |
|---|---|
| **Plain statement** | Where a service is exposed on the cluster's own network address, that choice is visible for review. |
| **Why it matters** | Exposing a service directly on server addresses bypasses the platform's ingress controls, including encryption termination, access logging and rate limiting. It is often chosen for convenience during testing and never reverted. |
| **Severity** | **Medium**, confidence `Requires review` |
| **Detects** | A Service of type node-port or load-balancer |
| **Grounding** | §15.2.2 "Minimise publicly exposed Routes; separate external and internal endpoints" |
| **Priority** | 3 |

### `NET-008` — Additional network attachments are declared consistently

| | |
|---|---|
| **Plain statement** | Where a service requests an extra network connection, the definition of that network is included or documented as a prerequisite. |
| **Why it matters** | A service requesting a network attachment that does not exist on a given server will not start there. During maintenance, copies move to other servers and simply fail to come back, with an error that points at the network layer rather than the chart. |
| **Severity** | **Medium** |
| **Detects** | A pod requesting a secondary network attachment with no corresponding definition in the release and none declared as a prerequisite |
| **Grounding** | §15.2.5 "Version-control NetworkAttachmentDefinitions and apply via GitOps"; §14.2.7 |
| **Priority** | 3 |

## 6.8 Storage — 5 new policies

### `STOR-006` — Storage is provisioned where the service will run

| | |
|---|---|
| **Plain statement** | Storage is created only once the platform knows which server and zone the service will run in. |
| **Why it matters** | If storage is created before placement is decided, it can be created in one data-centre zone while the service is placed in another. The service then cannot attach it and never starts, with an error that appears to be a storage fault. |
| **Severity** | **Medium**, confidence `Probable` — depends on the cluster's storage configuration |
| **Detects** | A storage request naming a class known to be zone-bound, where the class does not delay provisioning until placement |
| **Grounding** | §16.2.2 "Prefer WaitForFirstConsumer for zonal backends to avoid wrong-AZ provisioning"; §19.2.7 |
| **Priority** | 3 |

### `STOR-007` — Production data is not deleted with the release

| | |
|---|---|
| **Plain statement** | Storage holding important data is configured to survive the removal of the release. |
| **Why it matters** | With the default setting, uninstalling a release deletes its data permanently. This is correct for temporary caches and catastrophic for anything else. The setting is invisible until the day someone uninstalls. |
| **Severity** | **High**, confidence `Requires review` |
| **Detects** | A storage request for a stateful workload with a delete-on-removal reclaim setting and no retention annotation |
| **Grounding** | §16.2.3 "Set reclaimPolicy appropriate to data criticality (often Retain for production datasets)"; §22.2.2 |
| **Priority** | 3 |

### `STOR-008` — Local server storage is used only for disposable data

| | |
|---|---|
| **Plain statement** | Data that matters is not stored on the server's own disk. |
| **Why it matters** | Server-local storage cannot move. When that server is taken offline for maintenance, the service either cannot move with its data or moves and finds the data gone. Neither outcome is acceptable for data that matters. |
| **Severity** | **High** |
| **Detects** | A stateful workload using a host path or local storage class for a mount that is not a cache or temporary area |
| **Grounding** | §16.2.6 "Don't expect seamless multi-AZ failover for node-local volumes"; §16.2.1 |
| **Priority** | 3 |

### `STOR-009` — Stateful services keep a stable identity

| | |
|---|---|
| **Plain statement** | Services that remember things are deployed in a form that gives each copy a stable name and its own storage. |
| **Why it matters** | A stateless deployment gives copies random names and no individual storage. A database deployed this way loses its identity on every restart, and clustered databases cannot form a cluster at all. |
| **Severity** | **High** |
| **Detects** | A `Deployment` with more than one copy mounting persistent storage |
| **Grounding** | §16.2.4 "Don't run stateful workloads in Deployments with shared PVCs unless the app is explicitly designed for it" |
| **Priority** | 2 |

### `STOR-010` — Backup expectations are declared

| | |
|---|---|
| **Plain statement** | Where a service stores important data, the chart states whether that data is backed up. |
| **Why it matters** | Storage durability is not a backup. Replication protects against hardware failure; it does not protect against accidental deletion, corruption, or a bad software release. Without a stated expectation, everyone assumes someone else arranged it. |
| **Severity** | **Advisory** |
| **Detects** | A persistent storage request for a stateful workload with no backup-policy annotation |
| **Grounding** | §16.2.5 "Backups, snapshots, and restore testing are mandatory for critical data"; §16.2.5 "Don't assume replication equals recoverability" |
| **Priority** | 3 |

## 6.9 Observability — 4 new policies

### `OBS-002` — Logs go to the standard output stream

| | |
|---|---|
| **Plain statement** | Services write their logs to the standard output stream, where the platform collects them automatically. |
| **Why it matters** | Logs written to a file inside the container vanish when the container restarts — exactly when they are most needed. They are also invisible to central log search, so an incident investigation has to reach into individual containers one at a time. |
| **Severity** | **Medium** |
| **Detects** | A container mounting a writable volume at a path indicating log storage, or configuration directing log output to a file path |
| **Grounding** | §17.2.3 "Don't write application logs to node filesystem paths"; §12.3.6 |
| **Priority** | 3 |

### `OBS-003` — Metrics are reachable by the monitoring system

| | |
|---|---|
| **Plain statement** | Where a service publishes performance numbers, something in the release tells the monitoring system where to find them. |
| **Why it matters** | A service can publish perfect metrics that nobody ever collects. The dashboard stays empty, the alerts never fire, and the gap is usually discovered during an incident. |
| **Severity** | **Medium** |
| **Detects** | A container exposing a metrics-named port with no corresponding monitoring configuration or scrape annotation in the release |
| **Grounding** | §17.2.2; §17.3.1 |
| **Priority** | 3 |

### `OBS-004` — Metrics are not exposed publicly

| | |
|---|---|
| **Plain statement** | Performance metrics are reachable inside the cluster but not from the internet. |
| **Why it matters** | Metrics reveal internal structure, traffic volumes, error patterns and version information. Published externally they provide an attacker with a detailed map and a live view of whether their activity is being noticed. |
| **Severity** | **High** |
| **Detects** | An external entry point routing to a metrics port or path |
| **Grounding** | §17.3.4 "Prefer internal-only access to /metrics... avoid public Routes" |
| **Priority** | 2 |

### `OBS-005` — Workloads carry ownership and escalation information

| | |
|---|---|
| **Plain statement** | Every workload records which team owns it and where the operating instructions are. |
| **Why it matters** | At three in the morning, the first question is who to call. Without ownership recorded on the workload itself, that question is answered by searching chat history. |
| **Severity** | **Advisory** |
| **Detects** | A workload with no owner-team, on-call or runbook annotation |
| **Grounding** | §22.2.1 "Use annotations to record... support ownership (team, on-call, runbook URL)"; §22.4 "This best practice makes them required for production" |
| **Priority** | 3 |

## 6.10 Lifecycle and supply chain — 4 new policies

### `LIFE-002` — Setup tasks are ordered explicitly

| | |
|---|---|
| **Plain statement** | Where one part of the release must be installed before another, the chart says so. |
| **Why it matters** | Without explicit ordering, installation succeeds on a fast cluster and fails on a slow one, or succeeds on first install and fails on reinstall. These are the hardest defects to reproduce because they depend on timing. |
| **Severity** | **Medium** |
| **Detects** | A release shipping both a custom resource type and instances of it, with no ordering weight on the definition |
| **Grounding** | §19.3.3 "Use sync waves... to ensure CRDs exist before CRs are applied" |
| **Priority** | 3 |

### `LIFE-003` — Retained resources are documented

| | |
|---|---|
| **Plain statement** | Where the chart marks something to survive uninstall, it states why and who is responsible for eventually removing it. |
| **Why it matters** | Resources marked to persist accumulate silently across every install and uninstall cycle. Nobody knows what they are for, so nobody removes them, and they eventually consume quota or block a reinstall. |
| **Severity** | **Advisory** |
| **Detects** | A resource marked for retention on uninstall with no accompanying ownership or retirement annotation |
| **Grounding** | §22.2.2 "Don't keep resources on uninstall without an ownership and retirement plan" |
| **Priority** | 3 |

### `SUP-003` — Images come from approved registries

| | |
|---|---|
| **Plain statement** | Every image is pulled from a registry on the approved list. |
| **Why it matters** | An image from an unapproved source has not been scanned, has no provenance record and may not be available at all when the service needs to restart during an incident. |
| **Severity** | **High**, configurable per organisation |
| **Detects** | An image reference whose registry is not on the configured allowlist |
| **Grounding** | §11.2.4 "Don't pull from untrusted public registries without governance"; §18.2.2 |
| **Priority** | 2 |

### `SUP-004` — Releases carry build provenance

| | |
|---|---|
| **Plain statement** | Every workload records which source revision and build produced it. |
| **Why it matters** | When something breaks, the first question is what changed. Without provenance recorded on the workload, answering that means correlating deployment timestamps against build logs by hand. |
| **Severity** | **Advisory** |
| **Detects** | A workload with no source-revision or build-identifier annotation |
| **Grounding** | §18.2.2 "Attach provenance metadata: commit SHA, build ID, SBOM reference"; §22.2.1 |
| **Priority** | 3 |

## 6.11 Health checking — 3 new policies

### `HEALTH-007` — Readiness does not depend on distant systems

| | |
|---|---|
| **Plain statement** | A service reports itself ready based on its own state, not on whether every system it talks to is reachable. |
| **Why it matters** | If readiness depends on a remote database, a brief network problem marks every copy unavailable simultaneously. Traffic stops entirely, and the service cannot recover until the dependency does — turning a partial degradation into a total outage. |
| **Severity** | **Medium**, confidence `Requires review` |
| **Detects** | A readiness check whose command references an external hostname, or a check path suggesting a dependency check (`/ready/deps`, `/health/full`, `/healthz/all`) |
| **Grounding** | §9.2.6 "Avoid dependency roulette in readiness checks"; §14.2.2 |
| **Priority** | 3 |

### `HEALTH-008` — Startup allowance covers realistic start-up time

| | |
|---|---|
| **Plain statement** | Where a startup grace period is declared, it is long enough to be useful. |
| **Why it matters** | A startup allowance shorter than the service's real start-up time is worse than none at all: it gives the appearance of protection while still killing the service mid-start, producing a restart loop that looks like a crash. |
| **Severity** | **Medium** |
| **Detects** | A startup check whose total allowance (attempts × interval) is under 30 seconds where a restart check is also configured |
| **Grounding** | §9.2.4 "set failureThreshold * periodSeconds to cover worst-case startup time + buffer (e.g. 120s)" |
| **Priority** | 3 |

### `HEALTH-009` — Restart checks are not configured on short-lived tasks

| | |
|---|---|
| **Plain statement** | One-off tasks do not carry restart checks intended for long-running services. |
| **Why it matters** | A restart check on a task designed to finish and exit can kill it just before completion, causing the installation to fail in a way that appears random. |
| **Severity** | **Medium** |
| **Detects** | A restart or readiness check on an init container or a task with a completion-based restart policy |
| **Grounding** | §9.2.1 "Configure a readiness probe for every long-running service"; §4.2.1 |
| **Priority** | 3 |

---

# Part 7 — Critical gaps

Of the 47 proposed policies, five address conditions that are both serious and entirely invisible to the scanner today. These are the highest-value additions.

## 7.1 Credentials shipped inside the chart — `CFG-004`, `CFG-005`, `CFG-006`

**Why this is first.** The scanner has a policy specifically intended to catch credentials in configuration. In the reference run it produced four findings, all false, while a substantial population of secret objects carrying inline material — private keys, database passwords, a default administrative credential — passed through with no finding at all.

The failure is doubly instructive. The false positives arose from matching key **names**; the false negatives arose from never examining secret **objects**. One value-shape detector, applied to both object types, resolves both.

**Detection cost:** low. The logic is a single decoder plus the pattern table in §5.1.3.
**Value:** the difference between a report that catches credential exposure and one that does not.

## 7.2 Full host privileges and host folder access — `SEC-005`, `SEC-009`

**Why this matters.** The scanner detects host **network** sharing. It does not detect fully privileged containers or host folder mounts, which are equal or greater exposures. A container mounting the container runtime's control socket has effective cluster administration, regardless of every other restriction applied to it.

The standard groups all four in one sentence — §11.2.1, "Don't use `privileged: true` or host namespaces unless there is a reviewed exception. Don't mount the Docker socket or hostPath volumes to application pods." The scanner implements one quarter of it.

**Detection cost:** trivial. Both are single-field checks.

## 7.3 Write access to credentials — `IAM-008`

**Why this matters.** The identity policies are the strongest part of the current pack — verified accurate with no false positives. But they cover credential **reading** and **listing** only. A role granting `update` or `patch` on credentials is currently invisible.

A service that can overwrite a credential can lock out its legitimate owner, or substitute a credential it controls and intercept everything that authenticates with it. The standard addresses this directly: §5.2.5, "Don't grant update/patch on Secrets unless required (and reviewed)."

**Detection cost:** trivial — an extension of the existing role-parsing logic to a wider verb set.

## 7.4 Maintenance deadlocks — `AVAIL-006`, `AVAIL-008`

**Why this matters.** The scanner detects a maintenance rule on a single-copy service. It does not detect the general deadlock — a rule requiring all copies to stay running — which is the case the standard warns about most explicitly, in four separate sections (§4.2.2, §4.2.5, §13.2.5, §21.2.2).

The consequence is concrete and expensive: a cluster upgrade stalls, an engineer investigates, and the eventual fix is a one-line change that should have been caught before the chart shipped.

**Detection cost:** trivial — compare two integers.
**Note:** these are precisely the findings that most need the plain-language treatment in Part 3, because "the disruption budget deadlocks the drain" is opaque to almost everyone who is not a Kubernetes specialist.

## 7.5 Resource reservations — `RES-001`, `RES-002`, `RES-004`, `RES-006`

**Why this matters.** The scanner examines CPU **caps** and advises removing them — see §5.4 and the `RES-003` defect — while not examining **reservations** at all. This is the wrong half of the pair. The standard's prohibitive language is on the absence of reservations ("Don't run without requests"), not on the presence of caps, where it merely counsels caution.

There is a structural point here that generalises. In the reference run every container did declare reservations, so this policy would have found nothing. But **the policy does not exist**, which means a future release could regress to unreserved workloads with no signal at all. A policy that cannot pass is also a policy that cannot detect a regression — see `TF-023` in §5 and the pass-record recommendation in Part 2.

---

# Part 8 — Implementation roadmap

## 8.1 Release 1 — correctness

Objective: no finding in the report is provably wrong.

| # | Action | Section | Effect |
|---|---|---|---|
| 1 | Suppress the runtime-label and ordinal false positives | 5.1.1, 5.1.2 | −44 false Criticals |
| 2 | Rewrite the credential detector to be value-driven | 5.1.3 | −4 false Criticals; enables 7.1 |
| 3 | Fix probe handler comparison | 5.1.4 | −19 false Criticals |
| 4 | Skip numeric probe ports; allowlist platform API groups | 5.1.5, 5.1.6 | −4 false findings |
| 5 | Split root-user detection; resolve pod-level inheritance | 5.2.2 | Corrects a factually wrong title on ~50 Criticals |
| 6 | Split image pinning into moving-label and fingerprint policies | 5.2.1 | Reclassifies ~200 findings |
| 7 | Withdraw the shared-storage rule; replace with the contract check | 5.3 | Removes contradiction with the standard |
| 8 | Narrow or withdraw the CPU-cap advisory | 5.4 | −188 non-defect rows |
| 9 | Enforce the non-empty evidence invariant | 5.4.4 | Makes ~47 Criticals actionable |

**Expected effect on a representative run:** ~1,450 findings and ~620 Criticals become ~1,020 findings and ~130 Criticals, with no known false positives and every Critical traceable to prohibitive language in the standard.

## 8.2 Release 2 — schema and language

| # | Action | Section |
|---|---|---|
| 10 | Implement findings schema v2 | Part 2 |
| 11 | Populate `standard_reference` and `standard_quote` on every policy | 2.2.6 |
| 12 | Rewrite all policy names and descriptions to the plain-language standard | Part 3, Part 4 |
| 13 | Implement `observed_source` and effective-value resolution | 2.2.3 |
| 14 | Emit pass records and per-policy compliance rates | 2.1 |
| 15 | Implement the applicability model and `Not applicable` outcome | 3.6 |
| 16 | Implement the confidence model | 3.5 |
| 17 | Implement waivers with expiry | 2.2.6 |
| 18 | Split output into an action report and a full inventory | 8.4 |

## 8.3 Release 3 — coverage

Implement the new policies in priority order:

| Priority | Policies | Theme |
|---|---|---|
| **1** | `CFG-004` `CFG-005` `CFG-006` `SEC-005` `SEC-006` `SEC-009` `IAM-008` `AVAIL-006` `NET-006` | Serious and currently invisible |
| **2** | `CFG-007` `CFG-008` `CFG-010` `SEC-007` `SEC-008` `SEC-010` `SEC-011` `IAM-009` `IAM-010` `AVAIL-007` `AVAIL-008` `SCHED-002` `SCHED-003` `RES-001` `RES-002` `RES-004` `RES-006` `NET-003` `OBS-004` `STOR-009` `SUP-003` | Substantive coverage gaps |
| **3** | Remaining 17 policies | Completeness and documentation quality |

## 8.4 Report structure

Produce two artefacts from one scan.

**Action report** — the vendor-facing document. Critical and High findings only, ordered by severity then by blast radius. Capped at 10 rows per policy, with "and N more" and the remainder in an appendix. Rendered as report cards per §2.3.

**Full inventory** — the machine-readable audit artefact. Every record including passes, waivers, not-applicable outcomes and advisories.

The cap matters. In the reference run, three policies alone — the CPU-cap advisory, the shutdown-time check and the labelling check — accounted for roughly 30% of all output while representing, after the corrections in Part 5, close to zero actionable defects.

## 8.5 Standing engineering practices

These would have prevented most defects in this document.

| Practice | Rationale |
|---|---|
| **Golden-corpus regression suite** | One compliant and one non-compliant sample per policy, of the kind used throughout this document. Assert exact finding counts. Appendix B is the seed. |
| **Output contract assertions** | For every emitted record: `observed_value` non-empty where `field_path` is non-empty; no unsubstituted template fragments; `standard_reference` non-empty; `fix_owner` not `Requires decision` without an accompanying explanation. |
| **Clause traceability matrix** | Every policy maps to a clause; every prohibitive clause maps to at least one policy. Regenerate on each revision of the standard. The `RES-003` and `STOR-002` defects are policies with no supporting clause; the whole of Part 6 is clauses with no policy. |
| **Mechanical severity assignment** | Encode the §3.4 rubric. Do not permit per-policy judgement. |
| **Shared default-resolution layer** | One component resolving platform defaults and pod-to-container inheritance, used by every policy. The root-user, shutdown-time and token-mount description defects are three symptoms of its absence. |
| **False-positive budget** | Release gate: no policy family may exceed a 2% false-positive rate against the golden corpus, and **no Critical policy may exceed 0%**. |
| **Plain-language review** | Every new or changed policy description is read by someone who is not a Kubernetes specialist before release. If they cannot explain the consequence back, the description is rewritten. |

---

# Appendix A — Complete policy catalogue

## A.1 Existing policies — severity changes

| ID | Was | Name | Current | Proposed | Basis |
|---|---|---|---|---|---|
| `IAM-003` | RBAC-03 | Permissions are specific, not blanket | Critical | **Critical** | §5.2.3 prohibitive |
| `IAM-004` | RBAC-04 | Permissions stay inside the namespace | Critical | **Critical** | §5.2.2 |
| `IAM-005` | RBAC-05 | Credentials cannot be listed or harvested | Critical | **Critical** | §5.2.5 prohibitive |
| `IAM-006` | RBAC-06 | Services cannot grant themselves more access | Critical | **Critical** | §5.2.6 |
| `IAM-007` | RBAC-07 | Services cannot impersonate others or open shells | Critical | **Critical** | §5.2.6 |
| `IAM-001` | RBAC-01 | Each service has its own identity | Critical | **High** | §5.2.1 prohibitive; no direct outage |
| `SEC-004` | SEC-07 | Containers stay inside their own sandbox | Critical | **Critical** | §11.2.1 prohibitive |
| `SEC-001` | SEC-01a | Containers run as an unprivileged user | Critical | **Critical** | §11.2.1 |
| `SEC-002` | SEC-01b | Containers declare their unprivileged identity | Critical | **Medium** | Platform assigns non-root |
| `NET-001` | NET-05 | External entry points are encrypted | Critical | **Critical** | §11.2.5 |
| `AVAIL-003` | PDB-06 | Updates roll out without downtime | Critical | **High** | §14.2.4 prohibitive; foreseeable event |
| `AVAIL-001` | PDB-01 | Service survives planned maintenance | Critical | **High** | §4.2.1; manifests during maintenance |
| `AVAIL-002` | PDB-03 | Maintenance is never blocked indefinitely | Warning | **High** | §4.2.1; blocks upgrades |
| `STOR-003` | STO-05 | Multiple copies do not fight over one disk | Critical | **Critical** | §16.2.4 |
| `STOR-005` | STO-10 | Stateful services do not store state in temporary space | Warning | **High** | §16.2.1; data loss |
| `HEALTH-001` | PRB-01 | Traffic only reaches ready services | Critical | **High** | §9.2.1 |
| `HEALTH-006` | PRB-06 | Health checks point at a reachable port | Critical | **High** | Narrowed to named ports |
| `META-002` | MTA-03 | Addressing uses stable identifiers | Critical | **Critical** | §10.2.1, once ordinals excluded |
| `SUP-001` | SUP-01a | Deployed images cannot silently change | Critical | **Critical** | §11.2.4 prohibitive |
| `SUP-002` | SUP-01b | Deployed images are exactly reproducible | Critical | **Medium** | §11.2.4 recommending; pipeline-owned |
| `META-001` | MTA-01 | Workloads are identifiable | Critical | **Medium / Advisory** | No runtime impact |
| `HEALTH-004` | PRB-04 | Restart and traffic checks test different things | Critical | **Medium** | §9.2.3 guidance |
| `SCHED-001` | SCH-01 | Copies are spread across failure domains | Critical | **Medium** | §3.2.1 recommends soft |
| `NET-002` | NET-07 | Every internal address points somewhere | Critical | **Medium** | Not statically decidable |
| `CFG-001` | CFG-01 | No credentials in plain configuration | Critical | **Critical** | §7.2.6, once detector is fixed |
| `AVAIL-005` | PDB-07 | Stalled rollouts are detected | Warning | **Medium** | §20.2.2 |
| `AVAIL-004` | PDB-08 | Shutdown time is stated explicitly | Warning | **Advisory** | Platform default adequate |
| `CFG-002` | CFG-03 | Configuration cannot drift after deployment | Warning | **Medium** | §8.2.1 |
| `CFG-003` | CFG-06 | Certificates are delivered as files | Warning | **Medium** | §7.2.4 |
| `HEALTH-002` | PRB-02 | Slow-starting services are given time | Warning | **Medium** | §9.2.1 |
| `HEALTH-003` | PRB-03 | Restart checks are less twitchy | Warning | **Medium** | §9.2.1 |
| `HEALTH-005` | PRB-05 | Health checks respond promptly | Warning | **Medium** | §9.2.4, type-aware |
| `IAM-002` | RBAC-02 | Services without platform access carry no key | Warning | **Medium** | §11.2.2 |
| `SEC-003` | SEC-06 | Containers use the standard system-call filter | Warning | **Medium** | §11.2.1 |
| `STOR-001` | STO-01 | Storage requests name the storage type | Warning | **Medium** | §16.2.2 |
| `STOR-004` | STO-08 | File ownership is set for shared folders | Warning | **Medium** | §16.3.2 |
| `OBS-001` | OBS-01 | Live services publish health metrics | Warning | **Medium** | §17.2.2 |
| `META-004` | MTA-08 | Setup tasks clean up after themselves | Warning | **Medium** | §22.2.2 |
| `LIFE-001` | UPG-07 | All required extensions are installed first | Warning | **Medium** | §19.3.3, with allowlist |
| `META-003` | MTA-05 | Custom labels are namespaced | Informational | **Advisory** | §10.2.2 |
| `STOR-002` | STO-02 | Shared storage is used safely | Warning | **Redesigned** | §16.2 — see 5.3 |
| — | RES-03 | Containers that cap CPU | Informational | **Withdrawn / narrowed** | §13.2.2 is a caution |

## A.2 New policies — summary

| Domain | IDs | Count | Priority 1 |
|---|---|---|---|
| Configuration and secrets | `CFG-004` … `CFG-011` | 8 | 3 |
| Container security | `SEC-005` … `SEC-011` | 7 | 3 |
| Identity and access | `IAM-008` … `IAM-011` | 4 | 1 |
| Availability | `AVAIL-006` … `AVAIL-010` | 5 | 1 |
| Scheduling | `SCHED-002`, `SCHED-003` | 2 | 0 |
| Resources | `RES-001` … `RES-007` | 6 | 0 |
| Networking | `NET-003` … `NET-008` | 6 | 1 |
| Storage | `STOR-006` … `STOR-010` | 5 | 0 |
| Observability | `OBS-002` … `OBS-005` | 4 | 0 |
| Lifecycle and supply chain | `LIFE-002`, `LIFE-003`, `SUP-003`, `SUP-004` | 4 | 0 |
| Health checking | `HEALTH-007` … `HEALTH-009` | 3 | 0 |
| **Total** | | **54** | **9** |

*(Sub-policies such as `SEC-007a`–`c` and `STOR-002a`–`d` are counted once under their parent.)*

---

# Appendix B — Golden-corpus test manifest

A single manifest exercising the principal corrections and additions. A corrected scanner must produce **exactly** the findings listed beneath it. Any additional finding indicates a regression.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: app-namespace
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: app-alpha
  namespace: app-namespace
automountServiceAccountToken: false
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: app-alpha
  namespace: app-namespace
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    resourceNames: ["app-alpha-db"]     # named, read-only: IAM-005/009 pass
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: app-alpha
  namespace: app-namespace
subjects:
  - kind: ServiceAccount
    name: app-alpha                      # specific identity: IAM-010 passes
    namespace: app-namespace
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: app-alpha
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-alpha-config
  namespace: app-namespace
immutable: true                          # CFG-002 passes
data:
  LOG_LEVEL: "INFO"
  SECRET_FETCH_RETRYCOUNT: "5"           # must NOT trigger CFG-001
  TOKEN_CACHE_TTL_SECONDS: "900"         # must NOT trigger CFG-001
  KEYSTORE_PATH: /etc/tls/keystore.jks   # must NOT trigger CFG-001
---
apiVersion: v1
kind: Secret
metadata:
  name: app-alpha-db
  namespace: app-namespace
type: Opaque
data:
  password: UzNjcjN0LVAhc3N3MHJkLTIwMjY=  # MUST trigger CFG-004 and CFG-011
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: app-alpha
  namespace: app-namespace
spec:
  maxUnavailable: 1                      # AVAIL-001/006 pass
  selector:
    matchLabels:
      app.kubernetes.io/name: app-alpha
      app.kubernetes.io/instance: release-one
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
  namespace: app-namespace
spec:
  podSelector: {}                        # NET-003 passes
  policyTypes: ["Ingress"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-alpha
  namespace: app-namespace
  labels:
    app.kubernetes.io/name: app-alpha
    app.kubernetes.io/instance: release-one
    app.kubernetes.io/component: api
    app.kubernetes.io/part-of: platform-example
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/version: "1.4.0"
  annotations:
    example.com/owner-team: platform-example
    example.com/runbook-url: https://runbooks.example.com/app-alpha
    example.com/git-sha: "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c"
spec:
  replicas: 3
  progressDeadlineSeconds: 600           # AVAIL-005 passes
  strategy:
    type: RollingUpdate                  # AVAIL-003 passes
    rollingUpdate: { maxUnavailable: 1, maxSurge: 1 }   # AVAIL-008 passes
  selector:
    matchLabels:
      app.kubernetes.io/name: app-alpha
      app.kubernetes.io/instance: release-one
  template:
    metadata:
      labels:
        app.kubernetes.io/name: app-alpha
        app.kubernetes.io/instance: release-one
        app.kubernetes.io/component: api
      annotations:
        checksum/config: "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f0"  # CFG-010
    spec:
      serviceAccountName: app-alpha      # IAM-001 passes
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 45  # AVAIL-004 passes
      securityContext:
        runAsNonRoot: true               # SEC-001/002 pass
        runAsUser: 10001
        fsGroup: 10001
        seccompProfile: { type: RuntimeDefault }        # SEC-003 passes
      topologySpreadConstraints:         # SCHED-001 passes; SCHED-002 passes
        - maxSkew: 1                     #   (only one soft rule, not stacked)
          topologyKey: topology.kubernetes.io/zone
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: app-alpha
      containers:
        - name: app
          image: registry.example.com/app-alpha:1.4.0   # SUP-001 pass, SUP-002 fail
          ports:
            - name: http
              containerPort: 8080        # no hostPort: SEC-010 passes
            - name: metrics
              containerPort: 9090
          securityContext:
            allowPrivilegeEscalation: false             # SEC-006 passes
            readOnlyRootFilesystem: true                # SEC-008 passes
            capabilities: { drop: ["ALL"] }             # SEC-007 passes
          resources:
            requests: { cpu: "100m", memory: "256Mi" }  # RES-001/002 pass
            limits:   { cpu: "500m", memory: "512Mi" }  # RES-004/005/006 pass
          startupProbe:
            httpGet: { path: /healthz/startup, port: http }
            periodSeconds: 5
            timeoutSeconds: 2
            failureThreshold: 24         # 120s total: HEALTH-008 passes
          readinessProbe:
            httpGet: { path: /healthz/ready, port: http }
            periodSeconds: 5
            timeoutSeconds: 2
          livenessProbe:
            httpGet: { path: /healthz/live, port: http }   # HEALTH-004 passes
            periodSeconds: 10            # less sensitive: HEALTH-003 passes
            timeoutSeconds: 2
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef: { name: app-alpha-db, key: password }  # CFG-008
          volumeMounts:
            - name: tmp
              mountPath: /tmp            # writable area for SEC-008
      volumes:
        - name: tmp
          emptyDir: {}                   # no hostPath: SEC-009 passes
---
apiVersion: v1
kind: Service
metadata:
  name: app-alpha
  namespace: app-namespace
  labels:
    app.kubernetes.io/name: app-alpha
    app.kubernetes.io/instance: release-one
spec:
  type: ClusterIP                        # NET-007 passes
  selector:
    app.kubernetes.io/name: app-alpha
    app.kubernetes.io/instance: release-one
  ports:
    - name: http
      port: 80
      targetPort: http
```

### Expected output

| Policy | Severity | Count | Target |
|---|---|---|---|
| `CFG-004` Credentials are not shipped inside the chart | Critical | 1 | Secret `app-alpha-db` |
| `CFG-011` High-entropy credential value | Critical | 1 | `app-alpha-db` / `password` |
| `SUP-002` Deployed images are exactly reproducible | Medium | 1 | `app-alpha` / `app` |
| `OBS-003` Metrics are reachable by the monitoring system | Medium | 1 | `app-alpha` — metrics port, no monitoring configuration |
| **All other policies** | — | **0** | Pass or Not applicable |

---

# Appendix C — Glossary for non-specialist readers

Included so that a release manager or programme lead can read the report unaided. Every term is glossed on first use inside a finding; this table is the reference.

| Term | Plain meaning |
|---|---|
| **Chart** | A packaged, parameterised description of how to install a piece of software. |
| **Manifest** | The fully expanded instructions produced from a chart, with all parameters filled in. |
| **Container** | An isolated package containing a program and everything it needs to run. |
| **Pod** | One or more containers that start, stop and run together on the same server. |
| **Replica / copy** | One running instance of a service. Multiple copies provide capacity and fault tolerance. |
| **Node** | A physical or virtual server that runs containers. |
| **Zone** | A separately powered and networked area of a data centre. Services are spread across zones so one zone failing does not stop the service. |
| **Namespace** | A named partition of the cluster used to separate teams and applications. |
| **Deployment** | A service where copies are interchangeable and can be replaced freely. |
| **StatefulSet** | A service where each copy has a stable identity and its own storage — typically a database. |
| **Job** | A one-off task that runs to completion and stops. |
| **Service** | A stable internal address routing traffic to whichever copies are currently healthy. |
| **Ingress / Route** | The entry point that makes a service reachable from outside the cluster. |
| **NetworkPolicy** | A firewall rule controlling which services may connect to which. |
| **PodDisruptionBudget** | A rule telling the platform how many copies must stay running during maintenance. |
| **Node drain** | Moving every service off a server so it can be patched or replaced. |
| **Rolling update** | Replacing copies one at a time so the service stays available throughout. |
| **Recreate strategy** | Stopping every copy before starting any new one. Causes a full outage on every update. |
| **Readiness check** | A test the platform runs to decide whether a copy should receive traffic. |
| **Liveness / restart check** | A test the platform runs to decide whether a copy is stuck and should be restarted. |
| **Startup check** | A test allowing a slow-starting service time to initialise before other checks apply. |
| **Resource request / reservation** | The capacity a container asks to have set aside for it. Drives placement. |
| **Resource limit / cap** | The maximum capacity a container may consume. Protects its neighbours. |
| **ConfigMap** | A store for ordinary, non-sensitive settings. |
| **Secret** | A store for sensitive values. Protected by access controls, but **not encrypted by default** — treat anything placed in one as sensitive but not safe from anyone with read access. |
| **ServiceAccount / identity** | The identity a service uses when talking to the platform. |
| **Role / permission rule** | A statement of which actions an identity may perform on which resources. |
| **Privileged container** | A container granted unrestricted control of the server it runs on. |
| **Root user** | The all-powerful administrative account inside a container. |
| **hostPath** | A folder on the server itself, mounted directly into a container. |
| **Persistent volume claim** | A request for storage that survives restarts. |
| **Access mode ReadWriteMany** | Storage several copies can write to at the same time. |
| **Image tag** | A moving label such as `1.4.0` or `latest`. The software behind it can change. |
| **Image digest / fingerprint** | A permanent identifier derived from the image's exact contents. Cannot change. |
| **Custom resource definition** | An extension adding a new resource type to the platform. Must exist before instances can be created. |
| **Helm hook** | A task the chart runs at a defined point during installation or upgrade. |

---

**End of document.**
