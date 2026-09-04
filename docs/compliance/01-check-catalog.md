# 01 - Check Catalog

> **Ground truth.** Every assertion in [source-standards.md](source-standards.md),
> triaged: what a machine can decide from the artifact alone, what needs a site's
> values, what needs a human reading a document - and for the first group,
> exactly what the check inspects.
>
> **Prerequisite:** [00 - The Compliance Model](00-compliance-model.md) · **Consumed by:** [02 - Authoring Checks](02-authoring-checks.md), [design/23](../design/23-compliance.md)

---

## 1. How to read this

[source-standards.md](source-standards.md) is the organization's list and it
stays the source of truth for *what we require*. It is written for a human
reviewer, so a third of it is not decidable by a machine at all - "backup and
restore procedure documented, with tested restore evidence" is a document
review, not a YAML assertion. Pretending otherwise is how a compliance tool
ends up with a hundred green ticks that mean nothing.

So every ID gets a disposition:

| Column | Values | Meaning |
|---|---|---|
| **Tier** | `T1-C` | Decidable from the chart's own structure - templates, `Chart.yaml`, `values.yaml`, files present. Never affected by a site's values. |
| | `T1-R` | Decidable from the manifests rendered with the chart's default values. Carries a determinacy ([00](00-compliance-model.md) §2, Rule 4). |
| | `T1-X` | Decidable across the whole release - needs more than one chart, or the artifact tree, or another feature of this platform. |
| | `T2` | Needs a site's values file or a cluster fact. Reported as `skip` at tier 1, with the reason. |
| | `EV` | Needs a document or an attestation a human reads. Never automated; tracked as a release checklist item. |
| **Ships** | `v1` | In the built-in `sgw-baseline` pack at first release. |
| | `v1.1` | Second pass - the logic is sound but needs a heuristic tuned against a real corpus first. |
| | `-` | Not automated. `T2`/`EV` rows. |
| **Sev** | `critical` `warning` `inform` | As the source catalog declares, except where noted. Its own legend says `BLOCK`, `WARN` and `INFO`; those are the same three levels under the names that document uses. |

**Where this document lowers a severity, it says so and why.** A `BLOCK` in the
source catalog that can only be decided by a heuristic becomes a `warning` here: a
heuristic that blocks a release will be switched off within a month, and a
switched-off check finds nothing.

**Every check carries a mechanism and a set of search terms.** The titles in
this catalogue are written so that somebody who is not a Kubernetes engineer can
read them, which means most of them do not contain the name of the thing they
are about. `subcategory` and `keywords` carry that vocabulary, they are matched
by the report's search, and about fifty terms are asserted to find the checks
they belong to - see [02](02-authoring-checks.md) §2.2.1.

**Rows marked `v2`** changed after the audit in
[compliance-report.md](compliance-report.md): a severity recalibrated, a
condition narrowed to remove a false positive, or a check split in two because
it was describing two different defects in one sentence. What changed and why is
in [04 - Response to the Audit](04-audit-response.md).

**Rows marked `NEW`** are not in the source catalog. They are failure modes that
recur in Helm-packaged CNF deliveries and that the artifact makes cheap to
catch. They are proposed additions, kept visibly separate so the source catalog
stays recognisable.

---

## 2. Triage

### 2.1 Scheduling & Placement (SCH)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| SCH-01 | T1-R | v2 | warning | Workloads with `replicas >= 2` carry `topologySpreadConstraints` covering `topology.kubernetes.io/zone` **and** `kubernetes.io/hostname`. Determinacy is usually `configurable` (replicas comes from values) - reported as "at chart defaults" unless the template hard-codes it. Source catalog says `BLOCK`; **lowered to `warning`** because the standard advises starting with SOFT spreading and warns about over-constraining more forcefully than about under-constraining. |
| SCH-02 | T1-R | v1 | warning | Every zone/hostname spread constraint has `maxSkew: 1`. |
| SCH-03 | T1-R | v2 | warning | No pod carries all three of: `requiredDuringSchedulingIgnoredDuringExecution` node affinity, required pod anti-affinity, and a spread constraint with `whenUnsatisfiable: DoNotSchedule`. Three hard constraints is how a workload becomes unschedulable after one node is cordoned. **Lowered to `warning`**: it is a resilience gap rather than a prohibited configuration, and it needs a human to say whether the capacity exists. |
| SCH-04 | T1-R | v1 | warning | Label **values** used in `topologySpreadConstraints.labelSelector` and `podAntiAffinity` selectors do not look generated: 7-40 hex characters, an RFC3339 timestamp, or a key in the unstable-key list (`app.kubernetes.io/version`, `helm.sh/chart`, `pod-template-hash`, `*build*`, `*commit*`, `*sha*`). Source catalog says `BLOCK`; **lowered to `warning` here** because the hex test is a heuristic and a genuine 40-character label value exists. |
| SCH-05 | T1-R | v1 | warning | `requiredDuringScheduling` node affinity does not match on `topology.kubernetes.io/zone` with literal zone names. |
| SCH-06 | T1-R | v1.1 | warning | A pod that targets a node pool (a `nodeSelector` or required node affinity on any key other than the well-known OS/arch keys) also declares at least one `toleration`. The *matching* of toleration to taint is a cluster fact and is `T2`; the absence of any toleration at all is not. |
| SCH-07 | T2 | - | critical | "Sufficient for the stated failure tolerance" needs the stated tolerance. Partially covered by SCH-01 and RES-04. |
| SCH-08 | T1-R | v1 | critical | No pod spec tolerates a **NoSchedule node-pressure taint** (`node.kubernetes.io/memory-pressure`, `disk-pressure`, `pid-pressure`, `network-unavailable`, `unschedulable`) unless the workload or its pod template carries a `compliance.softwaregateway.io/toleration-rationale` annotation with a non-empty value. A toleration with no `key` (`{operator: Exists}`) tolerates all of them and counts. The exception exists because a node agent may legitimately have to run on a node under pressure - a DaemonSet collecting logs off a failing node is the usual one - and an undeclared toleration is indistinguishable from a mistake. |
| SCH-09 | T1-R | v1 | warning | Every toleration of `node.kubernetes.io/not-ready` or `node.kubernetes.io/unreachable` on `NoExecute` sets `tolerationSeconds`. Kubernetes supplies both with 300s when a chart says nothing; declaring them **without** a bound replaces that default with an indefinite one, and pods stay bound to a node that has stopped answering. A toleration with no `key` covers both and counts. |

### 2.2 Disruption & Availability (PDB)

The category with the highest ratio of value to effort, and the one most often
wrong in vendor charts.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| PDB-01 | T1-C + T1-R | v1 | critical | Every `Deployment`/`StatefulSet` is selected by some `PodDisruptionBudget` in the same release. See §3.1 - the selector matching is where naive implementations get this wrong. Structural: if the chart ships **no** PDB template at all, determinacy is `fixed` and it blocks regardless of the default replica count. |
| PDB-02 | T1-R | v2 | critical | No PDB forbids every eviction. See §3.2 - `maxUnavailable: 0`, `maxUnavailable: "0%"`, `minAvailable: <replicas>`, `minAvailable: "100%"` are four spellings of the same deadlock. The allowance is COMPUTED - `minAvailable` rounds the required count up, `maxUnavailable` rounds the allowance down - and only over workloads that DECLARE a copy count. A DaemonSet's count is the size of the cluster, a Job's is its `parallelism`, and a bare Pod beside a Deployment is a hook pod: defaulting their absent `replicas` to 1 made `minAvailable: 50%` over two copies read as a deadlock, and only for the policies that happened to have such an object beside them. |
| PDB-03 | T1-R | v2 | critical | No PDB selects a single-replica workload, a `Job`, or a `CronJob`. A PDB over one replica blocks drains forever - and a cluster upgrade stalls until somebody intervenes by hand, which is why this is **raised to `critical`**. |
| PDB-04 | EV | - | warning | Quorum math is prose. |
| PDB-05 | T1-R | v1 | critical | `RollingUpdate` strategy has `maxSurge > 0` **or** `maxUnavailable > 0`. Both zero is a rollout that cannot start. |
| PDB-06 | T1-R | v1 | critical | A workload selected by a `Service` does not use `strategy.type: Recreate`. |
| PDB-07 | T1-R | v1 | warning | `Deployment.spec.progressDeadlineSeconds` is explicitly set. |
| PDB-08 | T1-R | v2 | inform | `terminationGracePeriodSeconds` is explicitly set. **Lowered to `inform` and split**: every finding this produced was an absent value, on ~8% of a real report's rows, and the platform's 30-second default is usually adequate. The observed value now says so rather than implying the pods are killed instantly. |
| PDB-10 `NEW` | T1-R | v2 | warning | `terminationGracePeriodSeconds` is not `0`. Zero really is SIGKILL at eviction, and it is the other half of PDB-08 - a defect rather than a documentation gap. |
| PDB-09 `NEW` | T1-X | v2 | warning | Every PDB **selects something**. An orphan PDB - one whose selector matches no pod template in the release - protects nothing and is invisible until a drain succeeds when it should not have. Cross-chart, because the PDB and its workload are often in different charts. |

### 2.3 Health Probes & Lifecycle (PRB)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| PRB-01 | T1-R | v1 | critical | Every container whose port is a `Service` target has a `readinessProbe`. Applicability is narrowed to *traffic-receiving* containers deliberately: a sidecar with no service port failing this check is noise, and noise is what gets a check switched off. |
| PRB-02 | T1-R | v1 | warning | A container with `livenessProbe.initialDelaySeconds > 30` has a `startupProbe`. The general form ("initialization is non-trivial") is not decidable; this is its observable signature. |
| PRB-03 | T1-R | v1 | warning | Where both exist, liveness is **less** sensitive than readiness: `livenessProbe.periodSeconds >= readinessProbe.periodSeconds` and `failureThreshold >= readinessProbe.failureThreshold`. Note this contradicts [sample-policies/probes.rego](sample-policies/probes.rego), which warns on a *missing* liveness probe - see [03](03-sample-policy-review.md) §3.2. |
| PRB-04 | T1-R | v2 | warning | Liveness and readiness do not use an identical handler. The comparison is on the normalised HANDLER - `probeHandler()` - with the timings dropped and defaults filled in; comparing field by field rendered two different exec commands as the same empty string, and about a third of this check's findings were that bug. **Lowered to `warning`**: the standard offers guidance on endpoint semantics, it does not prohibit a shared handler. Identical probes mean a slow dependency restarts the pod instead of removing it from the endpoint list. The wider assertion - "liveness has no external dependency calls" - is not decidable from a manifest. |
| PRB-05 | T1-R | v2 | warning | Bounds by check TYPE: a request-based check (HTTP, TCP, gRPC) gets `timeoutSeconds` in `[1,3]` and `periodSeconds` in `[5,10]`; a script-based one gets `[1,10]` and `[5,30]`, because starting a shell costs time before the check begins. Holding a script check to a local web request's budget misread ~89% of the checks it flagged. |
| PRB-08 `NEW` | T1-R | v2 | warning | `timeoutSeconds` is below `periodSeconds` on every probe. Type-independent, and it catches the real defect the fixed band was reaching for: overlapping checks make failure-detection time unbounded. |
| PRB-09 `NEW` | T1-R | v2 | warning | No `livenessProbe` or `readinessProbe` on an init container or on a `Job`/`CronJob` container. A restart check can kill a task just before it completes, and the install then fails in a way that does not reproduce. |
| PRB-11 `NEW` | T1-R | v2 | warning | Where a `startupProbe` exists, `failureThreshold × periodSeconds >= 30`. An allowance shorter than the real start-up time looks like protection and still kills the container mid-start. |
| PRB-06 | T1-R | v2 | critical | A probe's **named** `httpGet.port` / `tcpSocket.port` resolves to a port the same container declares. Numeric ports are skipped: `containerPort` is documentation and opens nothing, so a probe aimed at 8443 reaches whatever is listening on 8443 whether or not the number appears in the list. Reporting one was a finding whose only fix was a line that changes nothing. A probe pointing at a sidecar's port passes health checks the application never answers. |
| PRB-07 | T1-R | v1 | warning | Where a `preStop` hook exists it is not a bare `sleep` longer than `terminationGracePeriodSeconds`, which guarantees SIGKILL mid-shutdown. |

### 2.4 Container Security Posture (SEC)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| SEC-01 | T1-R | v2 | critical | The EFFECTIVE user is not root: `runAsUser == 0`, or `runAsNonRoot` explicitly `false`, resolved container-then-pod the way the kubelet resolves it. **Narrowed**: "nothing is declared" is not root - on a cluster enforcing a restricted policy an unstated user is assigned a non-root one - and reporting the two identically made this check's own title untrue of nearly all of its findings. |
| SEC-12 `NEW` | T1-R | v2 | warning | Something is declared: `runAsNonRoot` or `runAsUser`, on the container or the pod. The other half of SEC-01, at the severity the condition deserves. |
| SEC-02 | T1-R | v1 | critical | `allowPrivilegeEscalation: false` on every container. Stands down on a privileged container: the kernel permits escalation there whatever this says. |
| SEC-03 | T1-R | v1 | critical | No container sets `privileged: true`. The finding names the three controls it nullifies - `capabilities.drop`, `allowPrivilegeEscalation` and `readOnlyRootFilesystem` - because those three stand down in its favour and a reader who notices the missing rows is entitled to know why. |
| SEC-04 | T1-R | v1 | critical | `capabilities.drop` contains `ALL`. The finding names what the container gets instead - the runtime's default set, about fourteen permissions including `CHOWN`, `SETUID`, `SETGID`, `NET_RAW` and `KILL` - so the reader sees what a missing `drop` actually grants. Stands down on a privileged container, which gets the full set regardless. |
| SEC-13 `NEW` | T1-R | v2 | critical | Nothing that reaches outside the container is added back: `SYS_ADMIN`, `NET_ADMIN`, `SYS_PTRACE`, `SYS_MODULE`, `SYS_RAWIO`, `SYS_BOOT`, `DAC_OVERRIDE`, `DAC_READ_SEARCH`, `ALL`. Dropping everything and adding one back is the right shape; these particular ones reach outside the container in a way the other settings cannot contain. |
| SEC-14 `NEW` | T1-R | v2 | warning | Nothing over identity, file ownership or raw network traffic is added back: `SETUID`, `SETGID`, `CHOWN`, `FOWNER`, `NET_RAW`, `SYS_CHROOT`, `FSETID`, `SETPCAP`, `SETFCAP`, `LINUX_IMMUTABLE`, `MKNOD`. The middle tier the second validation asked for: a single rule blocked on `SETUID` and said nothing at all about `NET_RAW`, which permits raw packet crafting and ARP and DNS spoofing from inside the pod. |
| SEC-15 `NEW` | T1-R | v2 | inform | Every capability added back is one this standard classifies. Records the ordinary ones on a **pass** (`observeOnPass`) so the release has one complete list of the extra permissions it asks for; fails only on a capability nobody has assessed, which is a gap in the standard rather than in the chart. |
| SEC-05 | T1-R | v2 | warning | `readOnlyRootFilesystem: true` on every container. **Broadened**: it used to skip any workload with a `persistentVolumeClaim` and to look only at main containers. Both exclusions were wrong - a root filesystem and a mounted volume are independent, so a database with a data volume can and should have a read-only root - and they left 37 containers in a real release that no check examined. Stands down on a privileged container, which can remount its own root filesystem. |
| SEC-16 `NEW` | T1-R | v2 | inform | A container with `readOnlyRootFilesystem: true` mounts at least one writable volume. The paired advisory: with nothing writable, the first temporary file the program creates fails at run time, and that is the failure that makes teams turn the read-only setting back off. Confidence `needs-review` - whether the program writes anything is a question for whoever knows it. |
| SEC-06 | T1-R | v1 | warning | `seccompProfile.type` is `RuntimeDefault` or `Localhost`; `Unconfined` fails. |
| SEC-07 | T1-R | v1 | critical | No `hostNetwork`, `hostPID`, `hostIPC`, and no `containerPort.hostPort`. |
| SEC-08 | T1-R | v1 | critical | No `hostPath` volume. A `hostPath` whose path is a container-runtime socket (`/var/run/docker.sock`, `/run/containerd/containerd.sock`, `/run/crio/crio.sock`) is reported with a distinct message: it is cluster-admin equivalence, not a mount. |
| SEC-09 | T2 | - | warning | "Runs under an arbitrary UID" needs the image config, which this platform does not fetch. Partially observable as a chart pinning a specific `runAsUser` with an `fsGroup` - too weak to ship as its own check. |
| SEC-10 | T1-R | v1 | critical | Where the chart ships a `SecurityContextConstraints`, `PodSecurityPolicy` or a binding granting one, its subjects are named ServiceAccounts - not `system:authenticated`, `system:serviceaccounts`, or a group. |
| SEC-11 `NEW` | T1-R | v2 | warning | `emptyDir` with `medium: Memory` declares a `sizeLimit`. Without one the volume is bounded only by node memory, and filling it evicts every pod on the node, not just this one. |

### 2.5 Identity & Access (RBAC)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| RBAC-01 | T1-R | v1 | critical | Every pod template names a `serviceAccountName` that is not `default` and that the release actually ships as a `ServiceAccount`. |
| RBAC-02 | T1-R | v1 | warning | `automountServiceAccountToken: false` on the pod or its ServiceAccount **unless** the release binds that ServiceAccount to a Role or ClusterRole. The exemption is derived, not assumed, which is what makes this precise rather than annoying. |
| RBAC-03 | T1-R | v1 | critical | No `*` in `rules[].verbs`, `resources`, or `apiGroups` of any Role or ClusterRole. |
| RBAC-04 | T1-R | v1 | critical | No `ClusterRole`/`ClusterRoleBinding` without a waiver. Bindings to the built-in `view`/`edit`/`admin`/`cluster-admin` roles are named explicitly in the message, because those are the ones that get copied between charts. |
| RBAC-05 | T1-R | v2 | critical | No rule grants `list` or `watch` on `secrets`. **Split**: it used to cover the unscoped `get` case too, and its title said "can list" against 15 rules in a real release that had only `get`. Both halves were right; only one of the two texts described the rule a reader was looking at. |
| RBAC-11 `NEW` | T1-R | v2 | critical | Any rule granting `get` on `secrets`, **and not `list` or `watch`**, carries a non-empty `resourceNames`. Disjoint from RBAC-05 per RULE: where a rule already permits enumeration an unscoped `get` reaches nothing further, so RBAC-05 owns it. Without that exclusion one rule granting `get, list, watch` produced two blocking findings with one fix between them, on 35 of 39 roles in a real release. A Role with two rules of different shapes still gets both findings, because those are two edits. The other half: a `get` with no names does not limit anything - the caller can fetch any secret in the namespace as long as it knows the name, and names in a release are predictable. |
| RBAC-09 `NEW` | T1-R | v2 | critical | No rule grants `create`, `update`, `patch`, `delete` or `deletecollection` on `secrets`. Reading a credential and being able to overwrite it are different powers, and only the first was ever checked: a service that can rewrite a credential can lock out its owner or substitute one it controls. |
| RBAC-10 `NEW` | T1-R | v2 | critical | Every `RoleBinding`/`ClusterRoleBinding` subject is a named `ServiceAccount`, not a `Group` and not a `system:` name. SEC-10 covers this for security-policy grants only; a permission given to `system:authenticated` applies to every service installed in scope, including ones added months later by another team. |
| RBAC-06 | T1-R | v1 | critical | No write verb (`create`, `update`, `patch`, `delete`, `deletecollection`, `bind`, `escalate`) on `roles`, `rolebindings`, `clusterroles`, `clusterrolebindings`. |
| RBAC-07 | T1-R | v1 | critical | No `impersonate`, and no rule on `pods/exec`, `pods/attach` or `pods/portforward`. |
| RBAC-08 | EV | - | warning | Requires knowing which identity the pipeline uses. |

### 2.6 Configuration & Secrets (CFG)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| CFG-01 | T1-R | v2 | critical | Credential-shaped material in `ConfigMap.data`. See §3.4 - the rule is now value-first, and the previous key-name form produced four findings on a real chart of which all four were wrong. |
| CFG-13 `NEW` | T1-R | v2 | critical | Container `env[].value` holds no value that **is** credential material: a private key, a signed token, a cloud access key, a connection string with the password in it, or a well-known placeholder. Shape only, with no reference to what the variable is called - what the check reads rather than what it infers. |
| CFG-16 `NEW` | T1-R | v2 | warning | The inferred half, rated as an inference: a variable named for a credential holding an opaque literal that has no credential shape and does not name any object this release ships. Confidence `needs-review`, owner `needs-decision`. `APP_SESSION_SECRET: "session-store-v1"` follows the naming convention of the other objects in the release rather than the shape of credential material, and reporting that at the highest severity and the highest confidence at once is how a report loses an argument it should not have started. |
| CFG-14 `NEW` | T1-R | v2 | critical | No `Secret` in the release ships with material in `data` or `stringData`. Values are base64-decoded first. An empty shell, created for a secret store to fill in, passes - shipping the object is correct, shipping the material is not. This is the gap the audit found: the check written to catch credentials produced only false positives, while a private key, a database password and a default administrative credential passed through the same run with no finding at all. The finding names the offending **key**, not the block: a Secret keeping its material under `stringData` used to be reported at `data`, so the path resolved to nothing and the evidence window opened on the object's metadata. |
| CFG-02 | T1-R | v1 | critical | No `args`/`command` element matching `--?(password\|passwd\|token\|secret\|api[-_]?key)[= ]` with a literal value. Container args are world-readable through the API and appear in `ps`. |
| CFG-03 | T1-R | v1 | warning | `ConfigMap`s consumed by a workload are `immutable: true` **and** carry a content-derived name suffix or a `checksum` annotation. |
| CFG-04 | T1-R | v2 | warning | A pod template that mounts or references a `ConfigMap`/`Secret` the same chart templates carries a `checksum/*` annotation over it. Without one, `helm upgrade` changes the config and never restarts the pod - the single most common "the change did not take effect" in Helm-packaged software. **Lowered to `warning`**, confidence `probable`: some workloads reload their configuration without restarting, and the manifest cannot show which. |
| CFG-05 | T1-C | v1 | warning | Template text of any `checksum/*` or pod-template annotation does not call `now`, `randAlphaNum`, `randAscii`, `uuidv4` or `date`. A checksum built from a timestamp restarts every pod on every `helm upgrade`, including the no-op ones. |
| CFG-06 | T1-R | v1 | warning | TLS material (`Secret` of type `kubernetes.io/tls`, or keys matching `*.crt`/`*.key`/`*.pem`) reaches the container as a read-only volume mount, not through `env`/`envFrom`. |
| CFG-07 | T1-R | v1 | warning | `envFrom.secretRef` and whole-`Secret` volume mounts (no `items:`) are reported: the container gets every key, including the ones added later. |
| CFG-08 | T2 | - | critical | "Environment-specific values baked into the image" needs the image. |
| CFG-09 | EV | - | warning | Rotation model is prose. |
| CFG-10 | T1-C + T1-R | v1 | critical | Two assertions with one implementation: every rendered document parses and validates against the Kubernetes schema for its `apiVersion`/`kind`, and **rendering twice produces identical output**. See §3.5. |
| CFG-11 `NEW` | T1-X | v2 | warning | Every `configMapRef`, `secretRef`, `configMapKeyRef`, `secretKeyRef`, `volumes[].configMap` and `volumes[].secret` resolves to an object the release ships, or is explicitly marked optional. A dangling reference is a `CreateContainerConfigError` at install time that no amount of reading the chart reveals. **Lowered from `critical`**, confidence `probable`: the chart is not the only thing that can put an object in a namespace - an installer, an operator or a platform team may supply it - and a finding may not be blocking unless it was read directly from the chart (see [02](02-authoring-checks.md) §2.3). Objects the cluster creates in every namespace itself, `kube-root-ca.crt` among them, are excluded outright. |
| CFG-12 `NEW` | T1-C | v1.1 | inform | The chart ships a `values.schema.json`. It is the only mechanism that makes a values mistake fail at `helm install` instead of at 03:00. |

### 2.7 Resources, Scaling & Performance (RES)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| RES-01 | T1-R | v1 | critical | `resources.requests.cpu` and `resources.requests.memory` on every container, **including `initContainers` and sidecars**. Init containers are the ones every implementation forgets, and an init container without requests is what makes a node's scheduling decision wrong. |
| RES-02 | T1-R | v1 | critical | `resources.limits.memory` set on every container. |
| RES-03 | T1-R | v2 | inform | A `limits.cpu` at least twice `requests.cpu`. **Narrowed**: the inventory form listed every container that caps CPU - about a fifth of a real report, describing no defect - and the condition worth reviewing is a cap with no headroom, where the container is paused during every ordinary burst. |
| RES-10 `NEW` | T1-R | v2 | critical | No `limits` value below its matching `requests` value. Not unwise - invalid: Kubernetes rejects the pod and the service never starts. |
| RES-11 `NEW` | T1-R | v2 | inform | `limits.memory` no more than four times `requests.memory`. A container that reserves a little and may grow a lot is placed where it cannot actually grow. |
| RES-04 | T1-R | v1 | warning | Where an `HorizontalPodAutoscaler` exists its `minReplicas >= 2`; where a `Deployment` is selected by a `Service` and has no HPA, that is reported at `inform`. |
| RES-05 | T2 | - | warning | Needs cluster capacity. |
| RES-06 | EV | - | inform | Needs to know the bottleneck. |
| RES-07..09 | EV | - | warning | Performance targets, concurrency bounds, timeout/retry policy - all declarations in a document. |

### 2.8 Networking (NET)

Rows NET-08 through NET-10 are the ones a generic Kubernetes linter does not
have, and they are the reason this catalog is worth implementing rather than
adopting.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| NET-01 | T1-C | v2 | warning | The release ships at least one `NetworkPolicy` with an empty `podSelector` and `policyTypes: [Ingress]` - a default deny. **Lowered to `warning`**, confidence `probable`: many platforms apply this per namespace centrally, and a blocking finding against a vendor for something the platform team owns is the shape of dispute that costs a report its credibility. Reported with `fixOwner: platform-team` rather than suppressed - silence is worse than a correctly attributed finding. |
| NET-02 | T1-R | v1 | critical | No ingress rule is `from: []`-equivalent (an empty or absent `from` with an empty `podSelector`), and every rule names `ports`. |
| NET-03 | T1-R | v1 | warning | Where `policyTypes` includes `Egress`, at least one rule enumerates destinations; an empty `to` is reported. |
| NET-04 | T1-R | v1 | warning | `NetworkPolicy` selectors use the same stable-label test as SCH-04. |
| NET-05 | T1-R | v1 | critical | Every `Ingress` declares `tls`; every OpenShift `Route` declares `tls.termination`; a `Service` of type `LoadBalancer` exposing port 80 without a matching 443 is reported. |
| NET-06 | T1-R | v1 | critical | No `Ingress`/`Route` path matches `/metrics`, `/debug`, `/actuator`, `/admin`, `/-/`, and no `LoadBalancer` Service exposes a port named `metrics`, `debug`, `admin` or numbered 9090/9100/6060. Covers OBS-10, which is the same assertion from the other side. |
| NET-07 | T1-X | v2 | warning | Every `Service.spec.selector` matches at least one pod template in the release. Selector keys the PLATFORM supplies at pod creation - `statefulset.kubernetes.io/pod-name`, `pod-template-hash`, `controller-revision-hash`, the batch job labels - are skipped, because no chart can contain them and comparing against them reported every per-replica Service in a clustered database as routing into a void. **Lowered to `warning`**: whether an address has a live target is a runtime property, and a rendered chart cannot settle it. |
| NET-11 `NEW` | T1-X | v2 | warning | Every named `targetPort` exists under that name in a container of the workload the Service routes to. Split out of NET-07 so a vendor can fix one without being told about the other. Port names cap at 15 characters, and a name trimmed to fit is the usual cause. |
| NET-13 `NEW` | T1-R | v2 | warning | No ingress rule has an empty `namespaceSelector` or an `ipBlock` of `0.0.0.0/0`. Such a rule NAMES a source, so it satisfies NET-02 and reads as a control while admitting the whole cluster. |
| NET-08 | T1-R | v1 | warning | Every `NetworkAttachmentDefinition` the release ships parses as JSON in `spec.config` and declares `type`, an `ipam` block, and - for `macvlan`/`ipvlan`/`sriov` types - `master` and `mtu`. Every `k8s.v1.cni.cncf.io/networks` annotation names an attachment the release ships or a documented cluster-provided one. |
| NET-09 | T1-R | v1 | warning | A pod with a `k8s.v1.cni.cncf.io/networks` annotation is reported wherever the release also ships a `NetworkPolicy`: Kubernetes `NetworkPolicy` governs the cluster network only, and traffic on a secondary interface is not covered by it. The finding is the statement, and it asks for the compensating control. |
| NET-10 | T1-R | v1 | warning | A container requesting an accelerated-networking resource - any resource name outside `cpu`/`memory`/`ephemeral-storage`/`hugepages-*`, e.g. `intel.com/*`, `openshift.io/*`, `nvidia.com/*` - declares it in **both** `requests` and `limits` (Kubernetes requires equality for extended resources) and the pod carries a `nodeSelector` or node affinity. Hugepages are checked the same way. |
| NET-11 `NEW` | T1-R | v1 | warning | Every `containerPort` a `Service` targets by name exists under that name in the target container, and no two containers in one pod declare the same `containerPort` number. |

### 2.9 Storage & Data (STO)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| STO-01 | T1-R | v1 | warning | Every `PersistentVolumeClaim` and `volumeClaimTemplate` sets `storageClassName` explicitly. Relying on the cluster default makes the same chart behave differently in two clusters, which is exactly the failure that is hardest to diagnose. |
| STO-02 | T1-R | v2 | critical | No workload mounting a `ReadWriteMany` claim runs as root. **Redesigned**: the old rule reported the access mode itself and its remediation suggested abandoning shared storage, which contradicts §16.2 of the standard - that section legitimises shared-write storage and sets terms for it. This checks the terms: non-root execution, and access by group membership rather than ownership (with STO-07 and STO-08 covering the other two). |
| STO-13 `NEW` | T1-R | v2 | inform | A shared-write claim is mounted by more than one workload. Recorded, confidence `needs-review`: concurrent writers are a claim about the software, and somebody has to confirm it rather than inherit it. |
| STO-03 | T1-R (conditional) | v1.1 | warning | Where the **chart ships a `StorageClass`**, its `volumeBindingMode` is `WaitForFirstConsumer`. Where it does not, the class is a cluster fact and the check is `skip`, reason `storage class is cluster-provided (tier 2)`. |
| STO-04 | T1-R | v1 | critical | Every claim declares `resources.requests.storage` as a parseable, non-zero quantity. `allowVolumeExpansion` and `reclaimPolicy` are checked only on a `StorageClass` the chart ships. |
| STO-05 | T1-R | v1 | critical | No `Deployment` with `replicas > 1` mounts a `persistentVolumeClaim`. Two replicas sharing one RWO claim is a scheduling deadlock; sharing one RWX claim is silent data corruption for anything not written for it. |
| STO-06 | EV | - | critical | Restore evidence is a document. |
| STO-07 | T1-R | v1 | critical | No `initContainer` command contains `chown` or `chmod` against a path that is also a `persistentVolumeClaim` mount. On shared and network-backed storage this is a startup that takes hours or fails outright, and it is the standard workaround for not knowing the UID model. |
| STO-08 | T1-R | v1 | warning | A pod mounting a PVC declares `securityContext.fsGroup` or `fsGroupChangePolicy`. |
| STO-09 | EV | - | critical | "Do not disable root squash" is a statement in a document. |
| STO-10 | T1-R | v2 | critical | `hostPath` and `local` volumes on a `StatefulSet`, and `emptyDir` where the StatefulSet declares no `volumeClaimTemplates` at all. **Narrowed and raised**: scratch space beside real storage is legitimate and is no longer reported; a stateful workload with nothing BUT scratch space loses its data on every restart, which is the case it was chosen to prevent. Volumes that hold no data are excluded from both the verdict and the evidence: an `emptyDir` with `medium: HugePages` or `Memory` is a memory allocation, and a `hostPath` under `/sys`, `/dev` or `/proc` is device access, which SEC-08 reports on its own terms. Reading those as lost application state made a correct finding read as though the tool had not understood the workload. |
| STO-11 | EV | - | critical | Cross-zone failover expectation is a declaration. |
| STO-12 `NEW` | T1-R | v1 | warning | A `StatefulSet`'s `volumeClaimTemplates` are never edited by an upgrade - Kubernetes forbids it. A chart that templates the claim size from `.Values` is offering a knob that works exactly once, and the finding says so. |

### 2.10 Observability (OBS)

The category where the catalog is furthest from what an artifact can show. Most
of it is about what the *running software emits*, which is a runtime property.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| OBS-01 | T1-R | v1 | warning | A workload selected by a `Service` either declares a container port named `metrics`/`http-metrics`, or is covered by a `ServiceMonitor`/`PodMonitor` the release ships, or carries `prometheus.io/scrape: "true"`. Source catalog says `BLOCK`; **lowered to `warning`** because three conventions coexist and none is universal. |
| OBS-05 | T1-R | v2 | warning | No container is CONFIGURED to write its logs to a file: an environment variable matching `LOG*(FILE\|PATH\|DIR\|DEST\|OUTPUT)` whose value is not a standard stream. What the running software actually emits is a runtime property; where the chart tells it to send them is not. |
| OBS-02..04, 06..08 | EV | - | - | Golden signals, label cardinality, trace propagation, correlation IDs - runtime properties. A manifest cannot show them and a check that pretends to would be the exact gimmick this catalog is meant to avoid. |
| OBS-09 | T1-C | v1 | warning | Where the release ships `PrometheusRule`s, every rule has a `runbook_url` (or configured equivalent) annotation and a `severity` label. Where it ships none, that is reported once at `inform`. |
| OBS-10 | - | - | critical | Same assertion as NET-06; implemented there, aliased here so the ID resolves. |

### 2.11 Metadata: Labels & Annotations (MTA)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| MTA-01 | T1-R | v2 | warning | `app.kubernetes.io/{name,instance}` present on controllers and Services. **Split and lowered**: these two appear in selectors, spread rules and maintenance rules, so their absence has consequences; the other four do not, and rating a cosmetic gap as blocking accounted for about a fifth of a real report's blocking findings. |
| MTA-09 | T1-R | v2 | inform | `app.kubernetes.io/{component,part-of,managed-by,version}`. Nothing breaks; the release is invisible to every dashboard and cost report, which is discovered during an incident. (This replaces the proposed Helm-adoption check under the same ID, which needs the release name the manifest stream does not carry.) |
| MTA-02 | T1-R | v1 | warning | Label keys are DNS-safe (RFC 1123, optional DNS-subdomain prefix) and values are `<= 63` characters and syntactically valid. Invalid metadata is an install-time rejection, so this is cheap and absolute. |
| MTA-03 | T1-R | v1 | critical | No selector - `Deployment`, `StatefulSet`, `DaemonSet`, `Service`, `NetworkPolicy`, `PDB` - matches on `app.kubernetes.io/version`, `helm.sh/chart`, or a key in the unstable list. A `Deployment.spec.selector` is immutable after creation: a version in it means the next release cannot be upgraded, only deleted and recreated. |
| MTA-04 | T1-R | v1 | warning | No annotation value exceeds 8 KiB, and none matches the credential patterns from CFG-01. |
| MTA-05 | T1-R | v1 | inform | Custom label and annotation keys carry a domain prefix. |
| MTA-06 | T1-R | v1.1 | warning | Provenance annotations - source commit, build ID, release timestamp - present on controllers. Source catalog says `BLOCK`; **lowered to `warning`** and deferred to v1.1 because the key names are an organizational convention that has to be configured before the check can be fair. |
| MTA-07 | T1-R | v1.1 | warning | Ownership annotations - owning team, on-call contact, runbook URL. Same reasoning as MTA-06; the key names are configuration. |
| MTA-08 | T1-R | v1 | warning | Every resource carrying a `helm.sh/hook` annotation also carries `helm.sh/hook-delete-policy`; hook `Job`s set `backoffLimit` and `activeDeadlineSeconds`. An undeleted, unbounded hook Job is what makes the *second* `helm upgrade` fail. |
| MTA-09 `NEW` | T1-R | v1 | warning | Every rendered resource carries `app.kubernetes.io/managed-by: Helm` and the release labels Helm needs to adopt it. A resource created by a template but not labelled for the release is not reclaimed by `helm uninstall`. |

### 2.12 Supply Chain & Release Integrity (SUP)

Four of these are already answered by features this platform has. They are
implemented as **integration checks** - they read the platform's own record
rather than re-deriving it - which is what stops the same question having two
answers on one screen.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| SUP-01 | T1-R | v2 | warning | Every image reference is `name@sha256:…`. See §3.3 for how the reference is assembled from the three fields charts split it across. **Lowered to `warning`**: this was the single largest blocking category in a real run - about a third of all blocking findings - and the standard's prohibitive language is on MOVING tags, which is SUP-11. Fingerprint pinning is also frequently the pipeline's to do rather than the chart author's: where images are relocated into a target registry on the way in, an author-time digest does not survive the rewrite. |
| SUP-11 `NEW` | T1-R | v2 | critical | No image uses a moving label - `latest`, `stable`, `main`, `master`, `edge`, `dev`, `test`, `nightly` - or no tag at all. This is the prohibited half, and separating it lets a vendor fix one without being told about the other. SUP-01 does not apply to an image SUP-11 reports: there is no specific version to pin yet, and two checks on one decision is a doubled row count and a halved credibility. |
| SUP-02 | T1-R | v1 | critical | Every image reference resolves to a registry in the configured allowlist. The allowlist is deployment configuration, not a policy constant. |
| SUP-03 | T1-X | v1 | critical | Integration: a security sync exists for this release, its coverage is complete, and it reports no unwaived critical findings. Reads [design/21](../design/21-security-posture.md)'s stored result; never re-scans. Outcome is `skip` with reason `no scan on record` when there is none - **not** a pass. |
| SUP-04 | T1-X | v1 | warning | Integration: an SBOM document is on record for the release's images. |
| SUP-05 | T1-X | v1 | critical | Integration: signature verification has passed for this package ([design/08](../design/08-verification.md)). |
| SUP-06 | - | - | critical | Structurally guaranteed by this platform: promotion moves the same digest ([design/22](../design/22-promotion.md)). Recorded as an `inform` result stating that, so the report is complete rather than silent. |
| SUP-07 | T1-C | v1 | critical | Every `Chart.yaml` dependency has an exact version (no range, no `*`, no `^`/`~`) **and** is vendored under `charts/`. An unvendored dependency means the render is not reproducible and, in an air-gapped install, not possible. This is also what makes tier-1 rendering work at all - see [design/23](../design/23-compliance.md) §5.3. |
| SUP-08..09 | EV | - | warning | Rollback references and pipeline provenance are documents. |
| SUP-10 | T1-C | v1 | critical | No default, sample or placeholder credential in `values.yaml`, a `Secret` template's literal data, or a container `env` default. The pattern set is the CFG-01 set plus the known placeholders (`changeme`, `change-me`, `admin`, `password`, `secret`, `letmein`, `test123`, `P@ssw0rd`, `example.com` credentials). |

### 2.13 Upgrade & Maintenance Readiness (UPG)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| UPG-01..04 | EV | - | critical | Availability targets, statefulness model, drain and zone-evacuation rehearsals. Evidence, and the most valuable evidence in the catalog - tracked as a release checklist beside the automated results, never faked as a check. |
| UPG-05 | T1-R | v2 | warning | No hook script, `Job` command, or documented procedure in the chart's own text uses `--force`, `--grace-period=0`, or `kubectl drain --disable-eviction`. The manifest half is covered by PDB-02 and PDB-05. |
| UPG-06 | T1-X | v1.1 | warning | Where the release ships an operator (a `Deployment` with an accompanying `CustomResourceDefinition`), objects of the operator's own managed kinds are not also templated directly. |
| UPG-07 | T1-X | v2 | warning | Every custom resource the release ships has a matching `CustomResourceDefinition` in the same release, OR belongs to a group named in `config.platformApiGroups`. **Narrowed**: built-in cluster APIs were being asked to ship a definition of themselves - a finding with no possible fix - and operator-supplied types are the platform team's, which §19.2.1 assigns away from application charts. A CR whose CRD arrives later, or never, is an install that fails on ordering - and this is only decidable across the whole release, which is why it is `T1-X`. |
| UPG-08 `NEW` | T1-R | v2 | inform | Every object carrying `helm.sh/hook` names moments Helm recognises. The observed value lists them, so the report carries an inventory of what the release runs OUTSIDE the ordinary install - which nothing else in a compliance report shows, because hooks do not appear among the deployed objects. It fails only for an unrecognised moment: `post-instal` is ignored, the task never runs, and the upgrade reports success. |
| UPG-09 `NEW` | T1-R | v2 | warning | No object runs ONLY at `pre-rollback` or `post-rollback`. This platform reconciles a declared state forward and never issues `helm rollback`, so such a task never runs at all - not even when the release is moved to an earlier version, which arrives here as an ordinary upgrade. The repair or migration reversal it was written to perform silently does not happen. A hook that runs at a rollback and also at an upgrade is fine and is not reported. |
| UPG-08..09 (source rows) | EV | - | critical | The catalog's own UPG-08 and UPG-09 - migration and rollback PROCEDURES - remain evidence. The two rows above are additions that share the prefix, not implementations of them. |
| UPG-10 | T2 | - | critical | Cross-zone parity is a cluster fact. |
| UPG-11 `NEW` | T1-R | v2 | warning | CRDs placed in a chart's `crds/` directory are installed once and **never upgraded or deleted by Helm**. A chart that ships CRDs there and also changes them between versions has an upgrade path that silently does nothing; one that ships them under `templates/` needs `helm.sh/resource-policy: keep`. The check reports which of the two shapes the chart uses, and what is missing for that shape. |

---

## 3. The checks that are easy to get wrong

Six specifications, written out because a naive implementation of each passes
its own test and misses the real defect. These are the ones to read before
writing any code.

### 3.1 PDB-01 - "every replicated workload has a PDB"

**The naive implementation** matches a PDB to a workload by comparing the PDB's
`matchLabels` to the workload's `metadata.labels`, or worse, by looking for the
workload's *name* among the selector's values. Both are wrong.

**What Kubernetes actually does:** a PDB selects **pods**. The comparison is
between `PodDisruptionBudget.spec.selector` and the workload's
`spec.template.metadata.labels` - the *pod template's* labels, which are
routinely different from the controller's own labels.

**The specification:**

1. Build the pod-template label set for each `Deployment`, `StatefulSet` and
   `DaemonSet` in the **release** (not the chart - a platform chart commonly
   ships PDBs for workloads in sibling charts, so this is `T1-X`).
2. For each PDB, evaluate its `selector` - both `matchLabels` **and**
   `matchExpressions`, with `In`, `NotIn`, `Exists`, `DoesNotExist` - against
   each pod-template label set, using Kubernetes label-selector semantics: an
   empty selector `{}` selects **everything** in the namespace; a `nil` selector
   selects **nothing** (this reversed twice in the Kubernetes API and is still
   the most misread line in the PDB documentation).
3. Namespaces must match. A PDB in one namespace never protects a pod in
   another, and charts that template the namespace get this wrong.
4. A workload with no selecting PDB fails.

**Determinacy:** if the chart contains no PDB template at all, `fixed` - no
values file adds one. If a PDB template exists but is gated on a value
(`{{- if .Values.pdb.enabled }}`), the probe render establishes whether the
default is off; the finding then reads "the chart ships a PDB, disabled by
default", which is a different and much more useful sentence.

### 3.2 PDB-02 - "PDB does not forbid every eviction"

Four spellings of one deadlock, and an implementation that checks only the first
finds roughly a third of them:

| Field | Deadlocking value | Why it is missed |
|---|---|---|
| `maxUnavailable` | `0` | The one everybody checks. |
| `maxUnavailable` | `"0%"` | `IntOrString`. A string, so a numeric comparison silently skips it. |
| `minAvailable` | `<replicas>` | Needs the workload's replica count, so it needs the selector matching from §3.1. |
| `minAvailable` | `"100%"` | Same, plus the string form. |

Also worth reporting at `warning`: `minAvailable` expressed as a percentage that
rounds up to the full replica count at the chart's default replica count -
`minAvailable: "51%"` with `replicas: 2` requires 2 of 2, which is
`maxUnavailable: 0` wearing a disguise. Kubernetes rounds `minAvailable`
percentages **up**, and this is where quorum-sized workloads deadlock.

### 3.3 SUP-01 / SUP-02 - assembling an image reference

Charts almost never write an image reference as one string. The common shape is:

```yaml
image: "{{ .Values.image.registry }}/{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
```

After rendering, the checks see one string and that is what they judge - which
is correct, and it is why these checks are `T1-R` and not `T1-C`. Three rules
keep it accurate:

- **Parse with the OCI reference grammar**, not by splitting on `:`. A registry
  with a port (`registry.example.com:5000/app`) breaks naive splitting, and the
  digest form contains a `:` of its own.
- **A default of `.Chart.AppVersion` is still a tag**, and it renders as one.
  SUP-01 fails it. Determinacy is `configurable` where the tag came from values -
  so the finding reads "ships a mutable tag by default", and where the template
  hard-codes it, `fixed`.
- **`imagePullPolicy` is a separate finding, not part of this one.** `Always`
  with a digest is harmless; `IfNotPresent` with a mutable tag is how two nodes
  end up running different code. That pairing is worth its own `warning` and it
  belongs beside SUP-01 in the report.
- **A MOVING tag and an unpinned fingerprint are two findings, not one.** SUP-11
  is `latest`, `stable`, `main` or no tag at all - the prohibited case, and a
  blocking one. SUP-01 is a specific version that is not a fingerprint, which is
  a warning and is frequently the pipeline's to fix rather than the chart
  author's. Rolling them together made this the largest blocking category in a
  real report while the prohibited half was already satisfied everywhere.

Image references appear in more places than `containers[].image`: `initContainers`,
`ephemeralContainers`, `CronJob.spec.jobTemplate`, and CRs of operators that name
images in their spec. The traversal is over *every string field that parses as an
image reference in a known image-bearing path*, and the check records the path it
found it at - which is what makes a finding in an operator CR actionable.

### 3.4 CFG-01 / CFG-13 / CFG-14 - credential detection without crying wolf

The check that will be switched off first if it is careless, and the one the
audit found had already earned it: on a real chart it produced four findings and
every one of them was wrong, while four unmistakable credentials in the same
chart went unreported.

Both halves came from the same mistake. It asked whether the KEY looked like it
held a password. `SECRET_FETCH_RETRYCOUNT` (a retry counter),
`TOKEN_CACHE_TTL_SECONDS` (a cache lifetime), `PASSWORD_MIN_LENGTH` (a policy
parameter) and `KEYSTORE_PATH` (a file path) all match that question and none of
them is a credential - and they are the ordinary way to name a parameter about
credential handling. Meanwhile `CONNECTION_STRING`, `CLOUD_ACCESS_KEY`,
`SESSION_JWT` and `INITIAL_LOGIN` do not match it, and all four were real.

**So the order is inverted. The value is examined first.**

*1. Shape signals, conclusive whatever the field is called:*

| Pattern | Class reported |
|---|---|
| `-----BEGIN … PRIVATE KEY-----` | a private key |
| `eyJ…` with two dot-separated segments | a signed token (JWT) |
| `AKIA`/`ASIA`/`AGPA`/`AIDA`/`AROA` + 16 uppercase | a cloud access key |
| `scheme://user:pass@host` | a connection string with the password written into it |
| `Bearer …` / `Basic …`, 16+ characters | a ready-made authorization header |
| `ghp_`, `github_pat_`, `xox[baprs]-`, `sk-`, `glpat-`, `AIza…` | an API access token |
| `changeme`, `passw0rd`, `letmein`, `qwerty`, `123456`, … | a placeholder somebody was meant to replace |

*2. Exclusions, applied before the field name is trusted at all:*

- **Reference-shaped keys** - `secretName`, `secretKeyRef`, `existingSecret`,
  `authUrl`, `passwordPolicy`, `tokenTtl`. A reference to a credential is the
  correct pattern and flagging it punishes the right answer.
- **Operational-parameter keys** - anything ending in a unit, a count, a
  location or a setting: `_COUNT`, `_INTERVAL`, `_SEC`, `_TTL`, `_TIMEOUT`,
  `_PATH`, `_FILE`, `_URL`, `_HOST`, `_PORT`, `_CLASS`, `_PROVIDER`,
  `_ALGORITHM`, `_POLICY`, `_LENGTH`, `_ENABLED`, and the rest.
- **Values that cannot be the credential their key is named for** - integers,
  durations, absolute paths, bare hostnames, URLs with no credentials in them,
  class or package identifiers, UUIDs, digests, and single-word settings
  (`none`, `oidc`, `true`).
- **Public certificates.** A `BEGIN CERTIFICATE` block is long, base64 and
  alarming-looking, and it is the single most common false positive in any
  entropy-based detector. It is meant to be public.

*3. Corroborated signal:* a field named for a credential holding a literal that
survived every exclusion above.

*4. Entropy is never a signal on its own.* It flags every checksum, UUID and
base64-encoded certificate chain in a chart. It is used only to sharpen the
WORDING of a finding that already matched on its key name - "an opaque
high-entropy value" rather than "a literal value".

**Secret values are base64-decoded before analysis.** Base64 is an encoding, not
protection, and a detector reading the encoded form finds nothing at all. In the
audited run a private key, a database password and a default administrative
credential were all plainly readable after a single decode, and all three were
reported as no finding.

**A `Secret` shipping material is CFG-14, not CFG-01.** The defect is not that a
credential is in a Secret - that is what Secrets are for - it is that the Secret
travels inside the chart, into version control, into every mirror and into every
archive anybody has taken. An empty Secret, created for a secret store to fill
in at install time, passes.

**No finding prints the value.** It names the object, the key and the class. A
compliance report is itself something that gets forwarded into a ticket, an
email and a shared drive; a report quoting the password it found has copied the
exposure rather than described it.

The pattern sets are configuration, not constants, so a deployment can add its
own and suppress a false positive without a rebuild. The table above is asserted
in `internal/compliance/cel/heuristics_test.go`, row by row, with the reason each
row is there.

### 3.5 CFG-10 - schema validity and render determinism

Two assertions, one implementation, and the second is the one nobody checks.

**Schema validity:** every rendered document is parsed and validated against the
Kubernetes OpenAPI schema for its `apiVersion`/`kind`, from a schema set vendored
at a pinned Kubernetes version ([design/23](../design/23-compliance.md) §5.5).
Unknown kinds - CRs of operators the release ships - are validated against the
`CustomResourceDefinition`'s own `openAPIV3Schema` where the release ships one,
and reported as `skip` with reason `no schema available` where it does not.

**Render determinism:** render the chart twice with identical inputs and compare
byte for byte. A difference means the chart contains `now`, `randAlphaNum`,
`uuidv4` or similar, and that means:

- every `helm upgrade` restarts pods that did not change,
- a GitOps diff never converges,
- and every other result in this run is unreproducible, which is why this check
  is `critical` and why a chart that fails it has its whole result set marked
  `determinacy: unknown`.

This is also the cheapest possible implementation of "reproducible" from
[00](00-compliance-model.md) Rule 5 - the same mechanism that establishes the
property tests it.

### 3.6 NET-07 - a Service that selects nothing

The failure this catches is a chart that renames a pod-template label and forgets
the Service. `helm install` succeeds, every pod is `Running` and `Ready`, the
rollout is green, and every request gets a connection refused. Nothing in
Kubernetes reports it: a Service with no endpoints is a legal Service.

**The specification:**

1. For each `Service` with a non-empty `spec.selector`, find pod templates in the
   release whose labels are a superset of the selector, in the same namespace.
   Zero matches is a failure.
2. **Selector keys the platform supplies at pod creation are skipped.** This is
   the correction the audit forced, and it was 100% of this check's findings on a
   real chart. A headless Service addressing one member of a clustered database
   selects on `statefulset.kubernetes.io/pod-name: db-0`; Kubernetes writes that
   label when it creates the pod, and no chart contains it or ever will.
   Comparing against it concluded the Service routed into a void, at blocking
   severity, while the tool's own output printed the workload it pointed at. The
   allowlist is `statefulset.kubernetes.io/pod-name`,
   `apps.kubernetes.io/pod-index`, `controller-revision-hash`,
   `pod-template-hash`, `pod-template-generation`,
   `batch.kubernetes.io/job-name`, `batch.kubernetes.io/controller-uid`,
   `job-name`, `controller-uid`.
3. **`targetPort` resolution is NET-11**, not this check. They are different
   defects with different fixes, and a vendor should be able to fix one without
   being told about the other. A number always resolves; a name has to exist in
   a container of a matched workload, and port names cap at 15 characters, so a
   name trimmed to fit is a real and common cause.
4. A `Service` with no selector is skipped with reason
   `headless or externally managed endpoints`, not failed - that is a legitimate
   shape (`ExternalName`, manually managed `EndpointSlice`).

**Severity is `warning`, not `critical`.** Whether an address has a live target is a
runtime property. A rendered chart is strong evidence and not proof, and the
audit's rule holds: a finding that is not decidable from the artifact alone does
not decide a verdict on its own.

---

## 4. What ships, counted

| Category | Source rows | Shipping | v1.1 | Tier 2 | Evidence |
|---|---|---|---|---|---|
| SCH | 7 | 7 | 1 | 1 | 0 |
| PDB | 8 | 9 | 0 | 0 | 1 |
| PRB | 7 | 10 | 0 | 0 | 0 |
| SEC | 10 | 12 | 0 | 1 | 0 |
| RBAC | 8 | 9 | 0 | 0 | 1 |
| CFG | 10 | 9 | 1 | 1 | 1 |
| RES | 9 | 6 | 0 | 1 | 4 |
| NET | 10 | 11 | 0 | 0 | 0 |
| STO | 11 | 8 | 1 | 0 | 4 |
| OBS | 10 | 3 | 0 | 0 | 6 |
| MTA | 8 | 7 | 2 | 0 | 0 |
| SUP | 10 | 3 | 0 | 0 | 2 |
| UPG | 10 | 5 | 1 | 1 | 6 |
| **Total** | **118** | **99** | **6** | **5** | **25** |

"Shipping" exceeds the source rows in five categories because a source
assertion sometimes describes two defects with two different fixes and two
different severities, and one check cannot honestly carry both. `SEC-01`
described running as root and saying nothing about the user; `SUP-01` described
a moving tag and an unpinned fingerprint; `PDB-08` described an absent shutdown
time and a zero one. Each pair is now two checks, and a vendor can fix one
without being told about the other.

Of the 118 assertions in the source catalog plus the additions:
**99 ship as automated tier-1 checks**, 6 follow once their heuristics are tuned
against a real corpus, 5 are genuinely tier-2, and **25 are evidence** - document
reviews that this feature tracks as a checklist beside the automated results and
never reports as passed.

That last number is the honest one. A tool claiming to automate 118 of 118 would
be claiming to have read the vendor's restore-test report.

### 4.1 What the audit changed about these numbers

[04 - Response to the Audit](04-audit-response.md) has the full account. The
short version, on a representative run:

- Thirteen classes of false positive removed, each with the fixture that stops it
  coming back.
- The blocking set recalibrated by the rubric in
  [02](02-authoring-checks.md) §2.3. Image digest pinning, identifying labels,
  probe-handler comparison, spreading rules, Service selectors and default-deny
  network policy leave it; the maintenance deadlocks, shared-storage-as-root and
  scratch-space data loss join it. Between them those six accounted for the
  large majority of a real report's blocking findings, and close to none of its
  actionable defects.
- Twenty-six checks added, of which the largest gap - a chart shipping
  credentials inside it - was invisible to every check in the pack.
