# 04 - Response to the compliance audit

> **What changed, and why.** [compliance-report.md](compliance-report.md) is an
> audit of this tool against a real Helm chart. This document answers it clause
> by clause: what was accepted, what was implemented differently, and what was
> declined with a reason.
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
three: `block`, `warn`, `info`, and they are not decoration - `block` decides
the run's verdict, and the verdict decides whether a release is accepted. A
fourth level would have to answer "does this fail the release or not", and both
answers make it a synonym for one of the three that already exist.

The mapping applied, per policy, from the audit's Appendix A:

| Audit | Here | Meaning |
|---|---|---|
| Critical | `block` | Do not accept the release |
| High | `block` | Do not accept the release |
| Medium | `warn` | Fix in the next release |
| Advisory | `info` | Recorded; no action required |

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
| §5.1.1 | A Service selecting on a label the platform adds at pod creation was reported as routing into a void - at blocking severity, while the tool's own output named the workload it pointed at | `selectedBy` skips runtime-supplied selector keys; `NET-07` is `warn` | `good-app` per-replica Service |
| §5.1.2 | A pod's ordinal was classified as release-varying. It is the documented way to address one member of a clustered database, and it is stable for that member's whole life | `IsUnstableLabelKey` separates a stable runtime identity from a template hash | `cel/heuristics_test.go` |
| §5.1.3 | Credential detection matched key NAMES. Four findings, four wrong: a retry counter, a cache lifetime, a minimum length, a file path. Four real credentials in the same chart went unreported | Rewritten value-first, with §4.2.1 of [02](02-authoring-checks.md) as its stated budget | `bad-config` `credential-parameters` - six keys that name a credential and hold none |
| §5.1.4 | Two exec probes were compared by rendering their commands as the empty string, so any two shell health checks looked identical | `probeHandler()` normalises the handler and drops the timings | `bad-probes` `same-handler` |
| §5.1.5 | A numeric probe port was reported as unreachable. `containerPort` is documentation; a probe aimed at 8443 reaches whatever is listening on 8443 | Narrowed to named ports, where an unresolvable name really does stop a copy becoming ready | `bad-probes` `numeric-probe-port` |
| §5.1.6 | Built-in and operator-supplied types were asked to ship a definition of themselves | `builtinApiGroup()`, plus a configured list of platform prerequisites | `cel/heuristics_test.go` |
| §5.2.2 | Root detection did not resolve pod-to-container inheritance, so "runAsNonRoot not set" appeared on pods that had explicitly set it to `false` | `securityValue` / `securitySource` resolve it the way the kubelet does, and name the provenance | `bad-cronjob`, `bad-security` |
| §5.3 | The shared-storage rule told teams to stop using shared storage, which is the opposite of what the standard says | `STO-02` now checks the terms the standard sets: not written by root; `STO-13` records more than one writer | `bad-storage` `shared-rwx` |
| §5.4.1 | An absent shutdown time was described as zero, on ~8% of the report's rows | `PDB-08` is `info` and says "not declared, so the platform allows 30 seconds"; explicit zero is `PDB-10` at `warn` | `bad-pdb` `instant-kill` |
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
| Pass records in the vendor report | The runs already emit and store passes, skips and not-applicable outcomes; the export writes only failures. Making the "full inventory" artifact of §8.4 real is an export change, not a policy one. |
| `CFG-05` - checksum inputs are stable | Needs the chart's template TEXT, not its rendered output: the defect is a call to `now` or `randAlphaNum` inside the checksum, which renders to a value that looks perfectly stable. The tier-1 renderer does not read templates. |
| `SUP-07` - dependencies pinned and vendored | Same: reads `Chart.yaml`, which the manifest stream does not contain. |
| `STO-03`, `STO-07` (reclaim policy), `STO-14` | Properties of a StorageClass the cluster provides, not of the chart. Reporting them from a chart alone would be a guess about somebody else's cluster. |
| `MTA-06`, `MTA-07` - provenance and ownership annotations | The key names are an organizational convention. The check is fair only once the convention is configured, and configuring it is a deployment decision rather than a policy one. |
| `HEALTH-007` - readiness does not depend on distant systems | The observable signature is a path name, and the catalogue was already right that this is not decidable from a manifest. A check keyed on `/health/full` would be a guess wearing a severity. |
