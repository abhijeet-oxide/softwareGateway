# 04 - Response to the compliance audit

> **What changed, and why.** [compliance-report.md](compliance-report.md) is an
> audit of this tool against a real Helm chart. This document answers it clause
> by clause: what was accepted, what was implemented differently, and what was
> declined with a reason.
>
> A second validation followed, in
> [complaince-report_2.md](complaince-report_2.md), against a larger release.
> §9 answers that one the same way.
>
> **Prerequisite:** [00 - The Compliance Model](00-compliance-model.md) · **See also:** [01 - Check Catalog](01-check-catalog.md), [02 - Authoring Checks](02-authoring-checks.md)

---

## 1. What the audit got right, in one paragraph

The findings were accurate and almost nobody outside the team could read them.
The categories producing the most rows produced the fewest real defects, and one
check - the one written to find leaked credentials - produced four findings of
which all four were wrong, while a private key, a database password and a
default administrative credential passed through the same run with no finding at
all. Those are not presentation problems. A report whose largest category is
noise trains its readers to skim, and a reader who skims misses the row that
mattered.

Everything below follows from that.

## 2. The two decisions the audit proposed that were made differently

### 2.1 Check IDs are not renumbered

The audit proposes renaming every policy: `PDB-01` becomes `AVAIL-001`, `SEC-01`
becomes `SEC-001`, and so on, with a `Was` column to reconcile the two.

Declined, and the reason is the audit's own §1.4: *"Stable identifier. Never
reused, never renumbered."* An ID here is not a label on a row. It is the key a
waiver is written against, the key a release-over-release comparison joins on,
the key in a vendor's ticket from eight months ago, and the row in
[source-standards.md](source-standards.md) that says what this organization
requires. Renumbering breaks all four to make the prefixes read better.

What the audit is actually asking for - that a reader should not have to know
what `PDB-01` means - is delivered by the title instead. `PDB-01` is now titled
"A service with more than one copy survives planned maintenance", and the
identifier is a small grey string beside it. Nobody has to translate anything,
and the waivers still resolve.

Where the audit proposes splitting one policy into two, the split is real: the
existing ID keeps the condition it always had, and the new condition gets a new
ID. `SEC-01` still means "runs as root" and now means only that; the "says
nothing about which user it runs as" half is `SEC-12`, which is a different
finding with a different fix and a lower severity.

### 2.2 Four severities became three, deliberately

The audit proposes `Critical / High / Medium / Advisory`. This platform has
three: `critical`, `warning`, `inform`, and they are not decoration - `critical` decides
the run's verdict, and the verdict decides whether a release is accepted. A
fourth level would have to answer "does this fail the release or not", and both
answers make it a synonym for one of the three that already exist.

The mapping applied, per policy, from the audit's Appendix A:

| Audit | Here | Meaning |
|---|---|---|
| Critical | `critical` | Do not accept the release |
| High | `critical` | Do not accept the release |
| Medium | `warning` | Fix in the next release |
| Advisory | `inform` | Recorded; no action required |

The audit's headline effect survives the mapping, because most of its
reclassifications are Critical → Medium rather than Critical → High. Image
digest pinning, identifying labels, probe-handler comparison, spreading rules,
Service selectors and default-deny network policy all leave the blocking set;
the maintenance deadlocks and the scratch-space data loss move into it.

## 3. False positives

Every one the audit names is fixed, and every fix has a fixture that would have
caught it. The fixture is named because "we fixed it" and "it cannot come back"
are different claims.

| Audit | What was wrong | What changed | Fixture |
|---|---|---|---|
| §5.1.1 | A Service selecting on a label the platform adds at pod creation was reported as routing into a void - at blocking severity, while the tool's own output named the workload it pointed at | `selectedBy` skips runtime-supplied selector keys; `NET-07` is `warning` | `good-app` per-replica Service |
| §5.1.2 | A pod's ordinal was classified as release-varying. It is the documented way to address one member of a clustered database, and it is stable for that member's whole life | `IsUnstableLabelKey` separates a stable runtime identity from a template hash | `cel/heuristics_test.go` |
| §5.1.3 | Credential detection matched key NAMES. Four findings, four wrong: a retry counter, a cache lifetime, a minimum length, a file path. Four real credentials in the same chart went unreported | Rewritten value-first, with §4.2.1 of [02](02-authoring-checks.md) as its stated budget | `bad-config` `credential-parameters` - six keys that name a credential and hold none |
| §5.1.4 | Two exec probes were compared by rendering their commands as the empty string, so any two shell health checks looked identical | `probeHandler()` normalises the handler and drops the timings | `bad-probes` `same-handler` |
| §5.1.5 | A numeric probe port was reported as unreachable. `containerPort` is documentation; a probe aimed at 8443 reaches whatever is listening on 8443 | Narrowed to named ports, where an unresolvable name really does stop a copy becoming ready | `bad-probes` `numeric-probe-port` |
| §5.1.6 | Built-in and operator-supplied types were asked to ship a definition of themselves | `builtinApiGroup()`, plus a configured list of platform prerequisites | `cel/heuristics_test.go` |
| §5.2.2 | Root detection did not resolve pod-to-container inheritance, so "runAsNonRoot not set" appeared on pods that had explicitly set it to `false` | `securityValue` / `securitySource` resolve it the way the kubelet does, and name the provenance | `bad-cronjob`, `bad-security` |
| §5.3 | The shared-storage rule told teams to stop using shared storage, which is the opposite of what the standard says | `STO-02` now checks the terms the standard sets: not written by root; `STO-13` records more than one writer | `bad-storage` `shared-rwx` |
| §5.4.1 | An absent shutdown time was described as zero, on ~8% of the report's rows | `PDB-08` is `inform` and says "not declared, so the platform allows 30 seconds"; explicit zero is `PDB-10` at `warning` | `bad-pdb` `instant-kill` |
| §5.4.2 | Script-based health checks were held to a web request's latency budget | `PRB-05` bounds are per check type | `bad-probes` |
| §5.4.3 | An absent token mount read as compliant beside an explicit `true`, though both mount a key | `RBAC-02` observed value says so | `bad-config` |
| §5.4.4 | Two checks emitted findings with an empty observed value | Fixed, and asserted for every finding in every fixture | `contract_test.go` |
| §5.2.4 | The CPU-cap inventory listed every container that caps CPU - about a fifth of a real report, describing no defect | `RES-03` is narrowed to caps with no headroom above the request, which is where throttling actually bites | `bad-resources` |

## 4. New checks

Twenty-six, covering the audit's Part 6 and Part 7 and the rows
[01](01-check-catalog.md) already listed as shipping but which had never been
implemented.

| ID | Title | Audit |
|---|---|---|
| `CFG-04` | A change to the settings actually restarts the service | CFG-010 |
| `CFG-11` | Every configuration reference points at something that exists | catalogue |
| `CFG-13` | No credential is written into a container's environment | CFG-001 |
| `CFG-14` | The chart does not carry credentials inside it | CFG-004/005/006 |
| `MTA-09` | Workloads carry the labels reports and dashboards group by | §5.2.3 split |
| `NET-11` | A Service's target port exists in the container it routes to | catalogue |
| `NET-13` | No rule lets in the whole cluster | NET-004 |
| `OBS-05` | Logs go where the platform can collect them | OBS-002 |
| `PDB-09` | Every maintenance rule protects a real service | AVAIL-007 |
| `PDB-10` | No service is stopped instantly | §5.4.1 split |
| `PRB-08` | A health check finishes before the next one starts | §5.4.2 |
| `PRB-09` | One-off tasks do not carry checks meant for long-running services | HEALTH-009 |
| `PRB-11` | A startup allowance is long enough to be useful | HEALTH-008 |
| `RBAC-09` | No service can change the credentials it reads | IAM-008 |
| `RBAC-10` | Permissions are given to a named identity, not to everybody | IAM-010 |
| `RES-10` | No container is allowed less than it asks for | RES-006 |
| `RES-11` | A memory ceiling is close enough to the request to be believable | RES-005 |
| `SEC-11` | Memory-backed scratch space has a size limit | catalogue |
| `SEC-12` | Every container states which user it runs as | §5.2.2 split |
| `SEC-13` | No container takes back a high-risk system permission | SEC-007a |
| `STO-13` | Shared storage has one writer unless it is meant to have more | STOR-002d |
| `SUP-11` | No image can change without anybody noticing | SUP-001 |
| `UPG-05` | Setup tasks do not force their way past the platform's safeguards | catalogue |
| `UPG-08` | Every task Helm runs is named and understood | new - see §5 |
| `UPG-09` | No task depends on Helm rolling back | new - see §5 |
| `UPG-11` | Extension definitions survive an uninstall | catalogue |

## 5. Helm hooks, and the rollback trap

Not in the audit. Requested alongside it, and the more important of the two by
some distance.

A Helm hook is a task a chart asks Helm to run at a defined moment - before an
install, after an upgrade. Hooks are ordinary and often necessary, and nothing
here is against them. Two things about them are worth reporting.

**`UPG-08` records every one of them.** Hooks are the part of a release that does
not appear in the deployed objects, so nothing else in a compliance report shows
them: a reader can see every Deployment a release ships and have no idea it also
runs three tasks around every upgrade. The check's observed value lists the
moments each hook runs at, so the report carries that inventory. It only *fails*
when a hook names a moment Helm does not recognise, which is a real and silent
defect: `post-instal` is ignored, the task never runs, and the upgrade reports
success anyway.

**`UPG-09` is the warning.** A task marked to run only at `pre-rollback` or
`post-rollback` never runs on this platform at all. The deployment model here is
declarative: the platform reconciles the cluster towards a declared state, and it
does not issue `helm rollback`. Moving a release to an earlier version arrives as
an ordinary upgrade, and the rollback hooks are skipped along with it. So the
repair, cleanup or migration reversal that task was written to perform silently
does not happen, and the release comes back up in a state nobody has tested.

That is why it is a warning rather than an observation. It is not a style
preference about hooks - it is a task that cannot run, in a chart that believes
it will, discovered on the day somebody needs it. A hook that runs at a rollback
*and* at an upgrade is fine and is not reported; the remediation is to add the
upgrade moment, or to move the work into the application's own start-up so that
it happens whichever direction the version moves in.

## 5.1 What the plain language cost, and how it is paid back

Not in the audit, and found by reading the result of acting on it.

Rewriting every title and message so that a release manager can act on it takes
the technical vocabulary out of the report. "A service with more than one copy
survives planned maintenance" is a better sentence than "Replicated workloads
are covered by a PodDisruptionBudget" for the person deciding whether to ship,
and it is a worse one for the engineer who opens the report and searches for
`PodDisruptionBudget`, or `toleration`, or `maxUnavailable`, or `RWX`. The
audit's §3.2 is right about the prose and silent about what the prose displaces.

So the vocabulary is carried on the finding rather than left to whatever words
the sentence happens to use:

| Field | Example |
|---|---|
| `subcategory` | `PodDisruptionBudget`, `Taints & tolerations`, `Seccomp`, `Shared storage` |
| `keywords` | `PodDisruptionBudget PDB policy/v1 eviction "node drain" minAvailable maxUnavailable replicas` |

Both are stored on every result and matched by the report's search, next to the
resource name, the chart, the file, the field path and the message. The finding
row shows the mechanism and clicking it searches for it; the findings table has
a mechanism filter; the export has "Mechanism" and "Search terms" columns; the
policy catalogue is searchable by the same terms. One report, two vocabularies,
and neither reader has to learn the other's.

The subcategory list is closed and asserted, because free text drifts into
"Helm hooks" and "Helm hook" within a month and a filter offering both hides
half the findings under each. About fifty technical terms are asserted to find
the checks they belong to, so a future rewrite of a description cannot quietly
take them away again - which is precisely what this rewrite did before the
fields existed.

## 6. Findings schema

The audit's Part 2 proposes 24 fields. Five were added; the rest were already
present under other names, or were declined.

**Added** (migrations `00048` and `00049`, stored per result so an exported
report keeps saying what it said): `confidence`, `whenItBites`, `fixOwner`,
`fixEffort`, `fixExample` (see [02](02-authoring-checks.md) §2.1), and
`subcategory` and `keywords` (see §5.1 above and [02](02-authoring-checks.md)
§2.2.1).

**Already present**: `policy_id` (`check`), `policy_name` (`title`), `severity`,
`outcome`, `chart`, `chart_version`, `source_file`, `source_line`,
`resource_kind`, `resource_name`, `container`, `container_role`
(`containerType`), `field_path` (`locus`), `observed_value`, `expected_value`,
`what_we_found` (`message`), `why_it_matters` (`rationale`, on the check),
`fix_summary` (`remediation`), `standard_reference` (`reference`), `waiver_id`,
`waiver_expires`, `fingerprint`.

**Declined**:

- `observed_source` as a separate column. The provenance is in the observed
  value's own words - "set on the pod, and inherited by every container in it" -
  which is where a reader is already looking. A separate enum column would be
  read by the exporter and by nobody else.
- `blast_radius`. It is derivable from the address: a finding on a container is
  a container, on a ClusterRoleBinding it is the cluster. A stored enum would
  drift from the address that determines it.
- `policy_version`. The run already records the `bundleDigest` of the whole
  rulebook, which answers "did the rule change or did the chart change" for
  every check at once and cannot be forgotten on one of them.
- `standard_quote`. The `reference` names the clause and
  [source-standards.md](source-standards.md) is in this repository. Copying the
  sentence into every result would duplicate the standard into the database,
  where the two can disagree.

## 7. Standing practices

The audit's §8.5 asks for engineering practices rather than fixes. Three now
exist as tests rather than as intentions - see [02](02-authoring-checks.md) §6.1:
a pack that stops compiling fails the build instead of silently removing its
checks from a run; a finding may not name a field and leave the value blank; and
every check must carry the triage block, a rationale, a reference, and a severity
consistent with its own confidence.

The false-positive budget the audit asks for is the good fixture, which predates
this work and is what made the audit's own §5 findings visible as a short list
rather than a survey: every check in the baseline runs against one realistic,
correct release, and it must produce zero failures.

## 8. What is still open

Stated rather than quietly dropped.

| Audit | Why not yet |
|---|---|
| `CFG-05` - checksum inputs are stable | Needs the chart's template TEXT, not its rendered output: the defect is a call to `now` or `randAlphaNum` inside the checksum, which renders to a value that looks perfectly stable. The tier-1 renderer does not read templates. |
| `SUP-07` - dependencies pinned and vendored | Same: reads `Chart.yaml`, which the manifest stream does not contain. |
| `STO-03`, `STO-07` (reclaim policy), `STO-14` | Properties of a StorageClass the cluster provides, not of the chart. Reporting them from a chart alone would be a guess about somebody else's cluster. |
| `MTA-06`, `MTA-07` - provenance and ownership annotations | The key names are an organizational convention. The check is fair only once the convention is configured, and configuring it is a deployment decision rather than a policy one. |
| `HEALTH-007` - readiness does not depend on distant systems | The observable signature is a path name, and the catalogue was already right that this is not decidable from a manifest. A check keyed on `/health/full` would be a guess wearing a severity. |

---

## 9. Response to the second validation

[complaince-report_2.md](complaince-report_2.md) is an independent verification
of the work above, against a release of roughly 250 containers across a dozen
charts. Its verdict was that the findings were largely correct and that the
remaining problems were of a different kind from the first round: not rules that
were wrong, but rules that were right and unusable - a title that did not
describe the rule it fired on, an evidence pane that opened on the wrong
screenful, three blocking findings where one decision was to blame.

Every item is answered below. Where an item was already implemented before the
second validation ran, that is said rather than claimed as new work.

### 9.1 False positives and misdirected evidence

| Item | What it found | What was done |
|---|---|---|
| A1, A2 | The workload-to-policy join failed whenever one object declared a namespace and the other did not, producing three findings saying "this workload has no policy" and four saying "this policy protects nothing" - about the same object pairs. | Fixed before the second report arrived, in the first round of this work. `sameNamespace()` treats an absent namespace as "wherever this is installed", which is what `helm template` means by it, and `TestAnAbsentNamespaceDoesNotBreakAJoin` holds the pair of mirror checks to it. |
| A3, B1, B2 | `PDB-02` matched the literal `minAvailable: 1` - the configuration this organization's own standard recommends for a two-copy service - and missed `maxUnavailable: 10%` over one copy, which is a real deadlock. | Also first round. `disruptionsAllowed()` computes the allowance, including the percentage forms and their rounding, so the check is about the arithmetic rather than the spelling. |
| A4 | `CFG-14` fired on the presence of `data`, not on what the data was: 56 findings of which 14 were genuine. Usernames, hostnames, object names and a configuration template whose password fields are deliberately empty were all rated blocking. | First round. `secretMaterialClass()` decodes base64 first and reads a shipped configuration file field by field, so an empty password field passes. |
| A5 | The evidence window opened on `apiVersion`, `kind` and `metadata` - none of which the finding concerned - and the footer said `data` about a finding whose subject was `stringData`. | `CFG-14` now names the offending key, through the new `assert.locusExpr`. That exposed a second defect underneath: the locus walk split paths on every dot, so a bracketed key containing one - `metadata.annotations[helm.sh/hook]`, `stringData[db.properties]` - could never resolve at all. Dotted keys are ordinary in Kubernetes, and every finding about one was landing on the wrong screenful. |
| A6 | `CFG-13` reported `APP_SESSION_SECRET: "session-store-v1"` as a leaked credential at the highest severity AND the highest confidence at once. It is far more likely the name of something the application looks up. | Split. `CFG-13` now asserts only on what a value **is** - a private key, a signed token, a cloud access key, a connection string with the password in it - with no reference to the field's name. The inferred case is `CFG-16`, a warning with confidence `needs-review`, and a value matching the name of a Secret or ConfigMap the release ships is treated as a reference and not reported at all. |
| A7 | `STO-10`'s finding was correct and its evidence miscategorised two volumes: an `emptyDir` with `medium: HugePages` is a memory allocation, and a `hostPath` under `/sys` is device access. Neither holds application state. | `volumeStateLabel()` decides what a volume means for durability and returns nothing for the volumes that mean nothing. The finding reduces to its accurate core, and the device mount stays with `SEC-08`, which exists for it. |
| A8 | `RBAC-05`'s title said "can **list** the namespace's credentials" against 15 rules that had only `get`. Both the finding and the title were defensible; only one of them described the rule. | Split into `RBAC-05` (list or watch - can enumerate every credential at once) and `RBAC-11` (get with no `resourceNames` - can fetch any credential by name). |
| A9 | A privileged container produced three blocking findings, two of which could not be acted on: the kernel grants the full capability set whatever `capabilities.drop` says, and permits escalation whatever `allowPrivilegeEscalation` says. | New `assert.supersededBy`. `SEC-02`, `SEC-04` and `SEC-05` stand down on a privileged container and are recorded as **skips naming SEC-03**, not dropped - a missing row and a passing row look the same to a reader, and neither is true. `SEC-03` now says which three controls it nullifies. `TestAPrivilegedContainerGetsOneFinding` also asserts the standalone cases keep firing, because those are the great majority of both checks. |

### 9.2 Conditions no check detected

| Item | What was done |
|---|---|
| B3 - `NET_RAW` granted with no finding | The single capability rule was both too loud and too quiet: blocking on `SETUID`, silent on `NET_RAW`, which permits raw packet crafting and ARP and DNS spoofing from inside the pod. Three tiers now. `SEC-13` blocks on the permissions that reach outside the container; `SEC-14` warns on those over identity, file ownership and raw network traffic; `SEC-15` records the ordinary ones on a **pass** and fails only on a capability this standard has never classified - which is a gap in the standard rather than in the chart, and worth knowing before the release ships. |
| B4 - writable container root filesystem | `SEC-05` existed and skipped any workload with persistent storage, and only ever looked at main containers. Both exclusions were wrong: a root filesystem and a mounted volume are independent, so a database with a data volume can and should have a read-only root. 37 containers that no check examined are now examined. New `SEC-16` pairs with it for the inverse the report asked for - read-only program files with nowhere writable at all, which is the failure that makes teams turn the setting back off. |
| B5 - a mutable tag would not be blocking | Already implemented, and invisible for the reason B6 names. `SUP-11` blocks on a moving label (`latest`, `stable`, `main`, `edge`, or no label at all) and `SUP-01` warns on "tagged but not pinned by digest", exactly the split proposed. `SUP-11` emitted zero findings against that release, which is the good result the failure-only export had no way to show. The Rulebook sheet now shows it. |
| B6 - no pass records | Passes, skips and not-applicable outcomes have always been evaluated and stored; the **Rulebook** sheet has carried per-check pass and fail counts. It now also carries the evaluated total and a rate. The rate's denominator is the subjects the check actually decided, so a chart that failed to render moves the "Not decided" column rather than the compliance rate - a rendering problem must not read as a compliance one, in either direction. |

### 9.3 Language

Part C asked for a four-part shape: state the literal, name the plausible
reading, give the runtime reality, state the consequence. The second step is the
one that lands, and it was the one missing.

The report is right that most of it is mechanical once both values exist, and
that is how it is implemented rather than as a writing convention: `SEC-02`,
`SEC-04`, `CFG-11`, `STO-10` and `PDB-03` are rewritten, and the runtime half of
each now comes from the new `assert.effective` (below) rather than from prose an
author has to remember to keep true.

### 9.4 Schema

| Item | What was done |
|---|---|
| D1 - `Value is` is not deterministic | The determinacy is left **empty** where the differential render could not settle it, in the export and in the interface. 1,060 of 2,253 rows reading "Could not be established" is how a reader learns to skip a column on the rows where it works. The run summary states the coverage once - how many findings the column speaks for - which is the honest place for it. |
| D2 - add an `effective_value` column | New `assert.effective`, stored, exported as **In practice**, and shown on the finding where it differs from what the manifest says. Supplied by `SEC-01`, `SEC-02`, `SEC-04`, `SEC-05`, `RES-01`, `PDB-02`, `PDB-03`, `CFG-11` and `STO-10`. The item's second half - `SEC-01` naming a container field path where the value is inherited from the pod - is fixed by `assert.locusExpr` and `securityLocus()`, which send the reader to the line they actually have to edit. |
| D3 - group findings by originating rule | Partly answered and partly open. `supersededBy` is the mechanism for the case the report leads with, and it is deliberately narrower than a general `root_cause_id`: it records that one finding cannot be acted on while another stands, which is a fact the engine can establish. Grouping four correct findings that share one manifest construct is a presentation question over results that are all individually true, and it is listed as open below. |
| D4 - severity should respect confidence | Adopted as a load-time rule, not a convention: a check may not be `critical` unless its confidence is `confirmed`. `CFG-11` was the only violation - it rated itself `probable` and blocked the release 52 times - and is a warning now. Its rationale says why: the chart is not the only thing that can put an object in a namespace. `kube-root-ca.crt` and its OpenShift equivalent are excluded outright, since no chart can ship what the cluster creates in every namespace itself. |

### 9.5 Still open from the second validation

| Item | Why not yet |
|---|---|
| D3 - `root_cause_id` grouping | Wants findings arising from one manifest construct - a single over-broad RBAC rule producing four correct rows across three check families - grouped in the interface. All four are true and each names a different power the rule grants, so this is a presentation change over correct results rather than a defect, and it belongs with a wider look at how the findings table groups. `supersededBy` covers the case where the rows are not independent. |
| The `Value is` coverage itself | The differential render establishes determinacy for scalar fields it can perturb, and the report is right that the inference degrades as chart complexity grows. Leaving the cell empty is honest about that; improving the probe's reach is separate work with its own measurement. |
