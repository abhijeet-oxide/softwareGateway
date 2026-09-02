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
| **Sev** | `block` `warn` `info` | As the source catalog declares, except where noted. |

**Where this document lowers a severity, it says so and why.** A `BLOCK` in the
source catalog that can only be decided by a heuristic becomes a `warn` here: a
heuristic that blocks a release will be switched off within a month, and a
switched-off check finds nothing.

**Rows marked `NEW`** are not in the source catalog. They are failure modes that
recur in Helm-packaged CNF deliveries and that the artifact makes cheap to
catch. They are proposed additions, kept visibly separate so the source catalog
stays recognisable.

---

## 2. Triage

### 2.1 Scheduling & Placement (SCH)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| SCH-01 | T1-R | v1 | block | Workloads with `replicas >= 2` carry `topologySpreadConstraints` covering `topology.kubernetes.io/zone` **and** `kubernetes.io/hostname`. Determinacy is usually `configurable` (replicas comes from values) - reported as "at chart defaults" unless the template hard-codes it. |
| SCH-02 | T1-R | v1 | warn | Every zone/hostname spread constraint has `maxSkew: 1`. |
| SCH-03 | T1-R | v1 | block | No pod carries all three of: `requiredDuringSchedulingIgnoredDuringExecution` node affinity, required pod anti-affinity, and a spread constraint with `whenUnsatisfiable: DoNotSchedule`. Three hard constraints is how a workload becomes unschedulable after one node is cordoned. |
| SCH-04 | T1-R | v1 | warn | Label **values** used in `topologySpreadConstraints.labelSelector` and `podAntiAffinity` selectors do not look generated: 7-40 hex characters, an RFC3339 timestamp, or a key in the unstable-key list (`app.kubernetes.io/version`, `helm.sh/chart`, `pod-template-hash`, `*build*`, `*commit*`, `*sha*`). Source catalog says `BLOCK`; **lowered to `warn` here** because the hex test is a heuristic and a genuine 40-character label value exists. |
| SCH-05 | T1-R | v1 | warn | `requiredDuringScheduling` node affinity does not match on `topology.kubernetes.io/zone` with literal zone names. |
| SCH-06 | T1-R | v1.1 | warn | A pod that targets a node pool (a `nodeSelector` or required node affinity on any key other than the well-known OS/arch keys) also declares at least one `toleration`. The *matching* of toleration to taint is a cluster fact and is `T2`; the absence of any toleration at all is not. |
| SCH-07 | T2 | - | block | "Sufficient for the stated failure tolerance" needs the stated tolerance. Partially covered by SCH-01 and RES-04. |
| SCH-08 | T1-R | v1 | block | No pod spec tolerates a **NoSchedule node-pressure taint** (`node.kubernetes.io/memory-pressure`, `disk-pressure`, `pid-pressure`, `network-unavailable`, `unschedulable`) unless the workload or its pod template carries a `compliance.softwaregateway.io/toleration-rationale` annotation with a non-empty value. A toleration with no `key` (`{operator: Exists}`) tolerates all of them and counts. The exception exists because a node agent may legitimately have to run on a node under pressure - a DaemonSet collecting logs off a failing node is the usual one - and an undeclared toleration is indistinguishable from a mistake. |
| SCH-09 | T1-R | v1 | warn | Every toleration of `node.kubernetes.io/not-ready` or `node.kubernetes.io/unreachable` on `NoExecute` sets `tolerationSeconds`. Kubernetes supplies both with 300s when a chart says nothing; declaring them **without** a bound replaces that default with an indefinite one, and pods stay bound to a node that has stopped answering. A toleration with no `key` covers both and counts. |

### 2.2 Disruption & Availability (PDB)

The category with the highest ratio of value to effort, and the one most often
wrong in vendor charts.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| PDB-01 | T1-C + T1-R | v1 | block | Every `Deployment`/`StatefulSet` is selected by some `PodDisruptionBudget` in the same release. See §3.1 - the selector matching is where naive implementations get this wrong. Structural: if the chart ships **no** PDB template at all, determinacy is `fixed` and it blocks regardless of the default replica count. |
| PDB-02 | T1-R | v1 | block | No PDB forbids every eviction. See §3.2 - `maxUnavailable: 0`, `maxUnavailable: "0%"`, `minAvailable: <replicas>`, `minAvailable: "100%"` are four spellings of the same deadlock. |
| PDB-03 | T1-R | v1 | warn | No PDB selects a single-replica workload, a `Job`, or a `CronJob`. A PDB over one replica blocks drains forever. |
| PDB-04 | EV | - | warn | Quorum math is prose. |
| PDB-05 | T1-R | v1 | block | `RollingUpdate` strategy has `maxSurge > 0` **or** `maxUnavailable > 0`. Both zero is a rollout that cannot start. |
| PDB-06 | T1-R | v1 | block | A workload selected by a `Service` does not use `strategy.type: Recreate`. |
| PDB-07 | T1-R | v1 | warn | `Deployment.spec.progressDeadlineSeconds` is explicitly set. |
| PDB-08 | T1-R | v1 | warn | `terminationGracePeriodSeconds` is explicitly set and `> 0`. Zero is SIGKILL at eviction. |
| PDB-09 `NEW` | T1-X | v1 | warn | Every PDB **selects something**. An orphan PDB - one whose selector matches no pod template in the release - protects nothing and is invisible until a drain succeeds when it should not have. Cross-chart, because the PDB and its workload are often in different charts. |

### 2.3 Health Probes & Lifecycle (PRB)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| PRB-01 | T1-R | v1 | block | Every container whose port is a `Service` target has a `readinessProbe`. Applicability is narrowed to *traffic-receiving* containers deliberately: a sidecar with no service port failing this check is noise, and noise is what gets a check switched off. |
| PRB-02 | T1-R | v1 | warn | A container with `livenessProbe.initialDelaySeconds > 30` has a `startupProbe`. The general form ("initialization is non-trivial") is not decidable; this is its observable signature. |
| PRB-03 | T1-R | v1 | warn | Where both exist, liveness is **less** sensitive than readiness: `livenessProbe.periodSeconds >= readinessProbe.periodSeconds` and `failureThreshold >= readinessProbe.failureThreshold`. Note this contradicts [sample-policies/probes.rego](sample-policies/probes.rego), which warns on a *missing* liveness probe - see [03](03-sample-policy-review.md) §3.2. |
| PRB-04 | T1-R | v1 | block | Liveness and readiness do not use an identical handler (same scheme, port and path, or the same exec command). Identical probes mean a slow dependency restarts the pod instead of removing it from the endpoint list. The wider assertion - "liveness has no external dependency calls" - is not decidable from a manifest. |
| PRB-05 | T1-R | v1 | warn | `timeoutSeconds` within `[1,3]`, `periodSeconds` within `[5,10]`. Bounds configurable per deployment. |
| PRB-06 | T1-R | v1 | block | A probe's `httpGet.port` / `tcpSocket.port` resolves to a port the **same container** declares - by number or by name. A probe pointing at a sidecar's port passes health checks the application never answers. |
| PRB-07 | T1-R | v1 | warn | Where a `preStop` hook exists it is not a bare `sleep` longer than `terminationGracePeriodSeconds`, which guarantees SIGKILL mid-shutdown. |

### 2.4 Container Security Posture (SEC)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| SEC-01 | T1-R | v1 | block | `runAsNonRoot: true` at pod or container level, and `runAsUser != 0` wherever set. |
| SEC-02 | T1-R | v1 | block | `allowPrivilegeEscalation: false` on every container. |
| SEC-03 | T1-R | v1 | block | No container sets `privileged: true`. |
| SEC-04 | T1-R | v1 | block | `capabilities.drop` contains `ALL`; every entry in `capabilities.add` is reported in the finding so the review is about a named list rather than a boolean. |
| SEC-05 | T1-R | v1 | warn | `readOnlyRootFilesystem: true` on containers in workloads that mount no `persistentVolumeClaim`. |
| SEC-06 | T1-R | v1 | warn | `seccompProfile.type` is `RuntimeDefault` or `Localhost`; `Unconfined` fails. |
| SEC-07 | T1-R | v1 | block | No `hostNetwork`, `hostPID`, `hostIPC`, and no `containerPort.hostPort`. |
| SEC-08 | T1-R | v1 | block | No `hostPath` volume. A `hostPath` whose path is a container-runtime socket (`/var/run/docker.sock`, `/run/containerd/containerd.sock`, `/run/crio/crio.sock`) is reported with a distinct message: it is cluster-admin equivalence, not a mount. |
| SEC-09 | T2 | - | warn | "Runs under an arbitrary UID" needs the image config, which this platform does not fetch. Partially observable as a chart pinning a specific `runAsUser` with an `fsGroup` - too weak to ship as its own check. |
| SEC-10 | T1-R | v1 | block | Where the chart ships a `SecurityContextConstraints`, `PodSecurityPolicy` or a binding granting one, its subjects are named ServiceAccounts - not `system:authenticated`, `system:serviceaccounts`, or a group. |
| SEC-11 `NEW` | T1-R | v1 | warn | `emptyDir` with `medium: Memory` declares a `sizeLimit`. Without one the volume is bounded only by node memory, and filling it evicts every pod on the node, not just this one. |

### 2.5 Identity & Access (RBAC)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| RBAC-01 | T1-R | v1 | block | Every pod template names a `serviceAccountName` that is not `default` and that the release actually ships as a `ServiceAccount`. |
| RBAC-02 | T1-R | v1 | warn | `automountServiceAccountToken: false` on the pod or its ServiceAccount **unless** the release binds that ServiceAccount to a Role or ClusterRole. The exemption is derived, not assumed, which is what makes this precise rather than annoying. |
| RBAC-03 | T1-R | v1 | block | No `*` in `rules[].verbs`, `resources`, or `apiGroups` of any Role or ClusterRole. |
| RBAC-04 | T1-R | v1 | block | No `ClusterRole`/`ClusterRoleBinding` without a waiver. Bindings to the built-in `view`/`edit`/`admin`/`cluster-admin` roles are named explicitly in the message, because those are the ones that get copied between charts. |
| RBAC-05 | T1-R | v1 | block | No rule grants `list` or `watch` on `secrets`; `get` on `secrets` carries a non-empty `resourceNames`. |
| RBAC-06 | T1-R | v1 | block | No write verb (`create`, `update`, `patch`, `delete`, `deletecollection`, `bind`, `escalate`) on `roles`, `rolebindings`, `clusterroles`, `clusterrolebindings`. |
| RBAC-07 | T1-R | v1 | block | No `impersonate`, and no rule on `pods/exec`, `pods/attach` or `pods/portforward`. |
| RBAC-08 | EV | - | warn | Requires knowing which identity the pipeline uses. |

### 2.6 Configuration & Secrets (CFG)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| CFG-01 | T1-C + T1-R | v1 | block | Credential-shaped material in `ConfigMap.data`, `values.yaml`, or container `env[].value`. See §3.4 for the detection rule and its false-positive budget. |
| CFG-02 | T1-R | v1 | block | No `args`/`command` element matching `--?(password\|passwd\|token\|secret\|api[-_]?key)[= ]` with a literal value. Container args are world-readable through the API and appear in `ps`. |
| CFG-03 | T1-R | v1 | warn | `ConfigMap`s consumed by a workload are `immutable: true` **and** carry a content-derived name suffix or a `checksum` annotation. |
| CFG-04 | T1-C | v1 | block | A pod template that mounts or references a `ConfigMap`/`Secret` the same chart templates carries a `checksum/*` annotation over it. Without one, `helm upgrade` changes the config and never restarts the pod - the single most common "the change did not take effect" in Helm-packaged software. |
| CFG-05 | T1-C | v1 | warn | Template text of any `checksum/*` or pod-template annotation does not call `now`, `randAlphaNum`, `randAscii`, `uuidv4` or `date`. A checksum built from a timestamp restarts every pod on every `helm upgrade`, including the no-op ones. |
| CFG-06 | T1-R | v1 | warn | TLS material (`Secret` of type `kubernetes.io/tls`, or keys matching `*.crt`/`*.key`/`*.pem`) reaches the container as a read-only volume mount, not through `env`/`envFrom`. |
| CFG-07 | T1-R | v1 | warn | `envFrom.secretRef` and whole-`Secret` volume mounts (no `items:`) are reported: the container gets every key, including the ones added later. |
| CFG-08 | T2 | - | block | "Environment-specific values baked into the image" needs the image. |
| CFG-09 | EV | - | warn | Rotation model is prose. |
| CFG-10 | T1-C + T1-R | v1 | block | Two assertions with one implementation: every rendered document parses and validates against the Kubernetes schema for its `apiVersion`/`kind`, and **rendering twice produces identical output**. See §3.5. |
| CFG-11 `NEW` | T1-X | v1 | block | Every `configMapRef`, `secretRef`, `configMapKeyRef`, `secretKeyRef`, `volumes[].configMap` and `volumes[].secret` resolves to an object the release ships, or is explicitly marked optional. A dangling reference is a `CreateContainerConfigError` at install time that no amount of reading the chart reveals. |
| CFG-12 `NEW` | T1-C | v1.1 | info | The chart ships a `values.schema.json`. It is the only mechanism that makes a values mistake fail at `helm install` instead of at 03:00. |

### 2.7 Resources, Scaling & Performance (RES)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| RES-01 | T1-R | v1 | block | `resources.requests.cpu` and `resources.requests.memory` on every container, **including `initContainers` and sidecars**. Init containers are the ones every implementation forgets, and an init container without requests is what makes a node's scheduling decision wrong. |
| RES-02 | T1-R | v1 | block | `resources.limits.memory` set on every container. |
| RES-03 | T1-R | v1 | info | Reports containers that set `limits.cpu`, so the throttling review the catalog asks for has a list to work from. Informational by construction - a CPU limit is not a defect. |
| RES-04 | T1-R | v1 | warn | Where an `HorizontalPodAutoscaler` exists its `minReplicas >= 2`; where a `Deployment` is selected by a `Service` and has no HPA, that is reported at `info`. |
| RES-05 | T2 | - | warn | Needs cluster capacity. |
| RES-06 | EV | - | info | Needs to know the bottleneck. |
| RES-07..09 | EV | - | warn | Performance targets, concurrency bounds, timeout/retry policy - all declarations in a document. |

### 2.8 Networking (NET)

Rows NET-08 through NET-10 are the ones a generic Kubernetes linter does not
have, and they are the reason this catalog is worth implementing rather than
adopting.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| NET-01 | T1-C | v1 | block | The release ships at least one `NetworkPolicy` with an empty `podSelector` and `policyTypes: [Ingress]` - a default deny. |
| NET-02 | T1-R | v1 | block | No ingress rule is `from: []`-equivalent (an empty or absent `from` with an empty `podSelector`), and every rule names `ports`. |
| NET-03 | T1-R | v1 | warn | Where `policyTypes` includes `Egress`, at least one rule enumerates destinations; an empty `to` is reported. |
| NET-04 | T1-R | v1 | warn | `NetworkPolicy` selectors use the same stable-label test as SCH-04. |
| NET-05 | T1-R | v1 | block | Every `Ingress` declares `tls`; every OpenShift `Route` declares `tls.termination`; a `Service` of type `LoadBalancer` exposing port 80 without a matching 443 is reported. |
| NET-06 | T1-R | v1 | block | No `Ingress`/`Route` path matches `/metrics`, `/debug`, `/actuator`, `/admin`, `/-/`, and no `LoadBalancer` Service exposes a port named `metrics`, `debug`, `admin` or numbered 9090/9100/6060. Covers OBS-10, which is the same assertion from the other side. |
| NET-07 | T1-X | v1 | block | Every `Service.spec.selector` matches at least one pod template in the release, and `targetPort` resolves to a declared `containerPort` (by name or number). A Service selecting nothing is a 100% error rate with a green rollout. |
| NET-08 | T1-R | v1 | warn | Every `NetworkAttachmentDefinition` the release ships parses as JSON in `spec.config` and declares `type`, an `ipam` block, and - for `macvlan`/`ipvlan`/`sriov` types - `master` and `mtu`. Every `k8s.v1.cni.cncf.io/networks` annotation names an attachment the release ships or a documented cluster-provided one. |
| NET-09 | T1-R | v1 | warn | A pod with a `k8s.v1.cni.cncf.io/networks` annotation is reported wherever the release also ships a `NetworkPolicy`: Kubernetes `NetworkPolicy` governs the cluster network only, and traffic on a secondary interface is not covered by it. The finding is the statement, and it asks for the compensating control. |
| NET-10 | T1-R | v1 | warn | A container requesting an accelerated-networking resource - any resource name outside `cpu`/`memory`/`ephemeral-storage`/`hugepages-*`, e.g. `intel.com/*`, `openshift.io/*`, `nvidia.com/*` - declares it in **both** `requests` and `limits` (Kubernetes requires equality for extended resources) and the pod carries a `nodeSelector` or node affinity. Hugepages are checked the same way. |
| NET-11 `NEW` | T1-R | v1 | warn | Every `containerPort` a `Service` targets by name exists under that name in the target container, and no two containers in one pod declare the same `containerPort` number. |

### 2.9 Storage & Data (STO)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| STO-01 | T1-R | v1 | warn | Every `PersistentVolumeClaim` and `volumeClaimTemplate` sets `storageClassName` explicitly. Relying on the cluster default makes the same chart behave differently in two clusters, which is exactly the failure that is hardest to diagnose. |
| STO-02 | T1-R | v1 | warn | `ReadWriteMany` is reported with the workload that requested it, so the concurrent-mount claim gets reviewed. |
| STO-03 | T1-R (conditional) | v1.1 | warn | Where the **chart ships a `StorageClass`**, its `volumeBindingMode` is `WaitForFirstConsumer`. Where it does not, the class is a cluster fact and the check is `skip`, reason `storage class is cluster-provided (tier 2)`. |
| STO-04 | T1-R | v1 | block | Every claim declares `resources.requests.storage` as a parseable, non-zero quantity. `allowVolumeExpansion` and `reclaimPolicy` are checked only on a `StorageClass` the chart ships. |
| STO-05 | T1-R | v1 | block | No `Deployment` with `replicas > 1` mounts a `persistentVolumeClaim`. Two replicas sharing one RWO claim is a scheduling deadlock; sharing one RWX claim is silent data corruption for anything not written for it. |
| STO-06 | EV | - | block | Restore evidence is a document. |
| STO-07 | T1-R | v1 | block | No `initContainer` command contains `chown` or `chmod` against a path that is also a `persistentVolumeClaim` mount. On shared and network-backed storage this is a startup that takes hours or fails outright, and it is the standard workaround for not knowing the UID model. |
| STO-08 | T1-R | v1 | warn | A pod mounting a PVC declares `securityContext.fsGroup` or `fsGroupChangePolicy`. |
| STO-09 | EV | - | block | "Do not disable root squash" is a statement in a document. |
| STO-10 | T1-R | v1 | warn | `emptyDir`, `hostPath` and `local` volumes on a `StatefulSet` are reported. |
| STO-11 | EV | - | block | Cross-zone failover expectation is a declaration. |
| STO-12 `NEW` | T1-R | v1 | warn | A `StatefulSet`'s `volumeClaimTemplates` are never edited by an upgrade - Kubernetes forbids it. A chart that templates the claim size from `.Values` is offering a knob that works exactly once, and the finding says so. |

### 2.10 Observability (OBS)

The category where the catalog is furthest from what an artifact can show. Most
of it is about what the *running software emits*, which is a runtime property.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| OBS-01 | T1-R | v1 | warn | A workload selected by a `Service` either declares a container port named `metrics`/`http-metrics`, or is covered by a `ServiceMonitor`/`PodMonitor` the release ships, or carries `prometheus.io/scrape: "true"`. Source catalog says `BLOCK`; **lowered to `warn`** because three conventions coexist and none is universal. |
| OBS-02..08 | EV | - | - | Golden signals, label cardinality, log format, trace propagation, correlation IDs - runtime properties. A manifest cannot show them and a check that pretends to would be the exact gimmick this catalog is meant to avoid. |
| OBS-09 | T1-C | v1 | warn | Where the release ships `PrometheusRule`s, every rule has a `runbook_url` (or configured equivalent) annotation and a `severity` label. Where it ships none, that is reported once at `info`. |
| OBS-10 | - | - | block | Same assertion as NET-06; implemented there, aliased here so the ID resolves. |

### 2.11 Metadata: Labels & Annotations (MTA)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| MTA-01 | T1-R | v1 | block | `app.kubernetes.io/{name,instance,component,part-of,managed-by,version}` present on controllers, pod templates and Services. |
| MTA-02 | T1-R | v1 | warn | Label keys are DNS-safe (RFC 1123, optional DNS-subdomain prefix) and values are `<= 63` characters and syntactically valid. Invalid metadata is an install-time rejection, so this is cheap and absolute. |
| MTA-03 | T1-R | v1 | block | No selector - `Deployment`, `StatefulSet`, `DaemonSet`, `Service`, `NetworkPolicy`, `PDB` - matches on `app.kubernetes.io/version`, `helm.sh/chart`, or a key in the unstable list. A `Deployment.spec.selector` is immutable after creation: a version in it means the next release cannot be upgraded, only deleted and recreated. |
| MTA-04 | T1-R | v1 | warn | No annotation value exceeds 8 KiB, and none matches the credential patterns from CFG-01. |
| MTA-05 | T1-R | v1 | info | Custom label and annotation keys carry a domain prefix. |
| MTA-06 | T1-R | v1.1 | warn | Provenance annotations - source commit, build ID, release timestamp - present on controllers. Source catalog says `BLOCK`; **lowered to `warn`** and deferred to v1.1 because the key names are an organizational convention that has to be configured before the check can be fair. |
| MTA-07 | T1-R | v1.1 | warn | Ownership annotations - owning team, on-call contact, runbook URL. Same reasoning as MTA-06; the key names are configuration. |
| MTA-08 | T1-R | v1 | warn | Every resource carrying a `helm.sh/hook` annotation also carries `helm.sh/hook-delete-policy`; hook `Job`s set `backoffLimit` and `activeDeadlineSeconds`. An undeleted, unbounded hook Job is what makes the *second* `helm upgrade` fail. |
| MTA-09 `NEW` | T1-R | v1 | warn | Every rendered resource carries `app.kubernetes.io/managed-by: Helm` and the release labels Helm needs to adopt it. A resource created by a template but not labelled for the release is not reclaimed by `helm uninstall`. |

### 2.12 Supply Chain & Release Integrity (SUP)

Four of these are already answered by features this platform has. They are
implemented as **integration checks** - they read the platform's own record
rather than re-deriving it - which is what stops the same question having two
answers on one screen.

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| SUP-01 | T1-R | v1 | block | Every image reference in every rendered manifest is `name@sha256:…`. A tag - any tag, including a semver one - fails. See §3.3 for how the reference is assembled from the three fields charts split it across. |
| SUP-02 | T1-R | v1 | block | Every image reference resolves to a registry in the configured allowlist. The allowlist is deployment configuration, not a policy constant. |
| SUP-03 | T1-X | v1 | block | Integration: a security sync exists for this release, its coverage is complete, and it reports no unwaived critical findings. Reads [design/21](../design/21-security-posture.md)'s stored result; never re-scans. Outcome is `skip` with reason `no scan on record` when there is none - **not** a pass. |
| SUP-04 | T1-X | v1 | warn | Integration: an SBOM document is on record for the release's images. |
| SUP-05 | T1-X | v1 | block | Integration: signature verification has passed for this package ([design/08](../design/08-verification.md)). |
| SUP-06 | - | - | block | Structurally guaranteed by this platform: promotion moves the same digest ([design/22](../design/22-promotion.md)). Recorded as an `info` result stating that, so the report is complete rather than silent. |
| SUP-07 | T1-C | v1 | block | Every `Chart.yaml` dependency has an exact version (no range, no `*`, no `^`/`~`) **and** is vendored under `charts/`. An unvendored dependency means the render is not reproducible and, in an air-gapped install, not possible. This is also what makes tier-1 rendering work at all - see [design/23](../design/23-compliance.md) §5.3. |
| SUP-08..09 | EV | - | warn | Rollback references and pipeline provenance are documents. |
| SUP-10 | T1-C | v1 | block | No default, sample or placeholder credential in `values.yaml`, a `Secret` template's literal data, or a container `env` default. The pattern set is the CFG-01 set plus the known placeholders (`changeme`, `change-me`, `admin`, `password`, `secret`, `letmein`, `test123`, `P@ssw0rd`, `example.com` credentials). |

### 2.13 Upgrade & Maintenance Readiness (UPG)

| ID | Tier | Ships | Sev | What the check inspects |
|---|---|---|---|---|
| UPG-01..04 | EV | - | block | Availability targets, statefulness model, drain and zone-evacuation rehearsals. Evidence, and the most valuable evidence in the catalog - tracked as a release checklist beside the automated results, never faked as a check. |
| UPG-05 | T1-R | v1 | warn | No hook script, `Job` command, or documented procedure in the chart's own text uses `--force`, `--grace-period=0`, or `kubectl drain --disable-eviction`. The manifest half is covered by PDB-02 and PDB-05. |
| UPG-06 | T1-X | v1.1 | warn | Where the release ships an operator (a `Deployment` with an accompanying `CustomResourceDefinition`), objects of the operator's own managed kinds are not also templated directly. |
| UPG-07 | T1-X | v1 | warn | Every custom resource the release ships has a matching `CustomResourceDefinition` in the same release, at a `version` the CRD serves. A CR whose CRD arrives later, or never, is an install that fails on ordering - and this is only decidable across the whole release, which is why it is `T1-X`. |
| UPG-08..09 | EV | - | block | Migration and rollback procedures. |
| UPG-10 | T2 | - | block | Cross-zone parity is a cluster fact. |
| UPG-11 `NEW` | T1-C | v1 | warn | CRDs placed in a chart's `crds/` directory are installed once and **never upgraded or deleted by Helm**. A chart that ships CRDs there and also changes them between versions has an upgrade path that silently does nothing; one that ships them under `templates/` needs `helm.sh/resource-policy: keep`. The check reports which of the two shapes the chart uses, and what is missing for that shape. |

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

Also worth reporting at `warn`: `minAvailable` expressed as a percentage that
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
  end up running different code. That pairing is worth its own `warn` and it
  belongs beside SUP-01 in the report.

Image references appear in more places than `containers[].image`: `initContainers`,
`ephemeralContainers`, `CronJob.spec.jobTemplate`, and CRs of operators that name
images in their spec. The traversal is over *every string field that parses as an
image reference in a known image-bearing path*, and the check records the path it
found it at - which is what makes a finding in an operator CR actionable.

### 3.4 CFG-01 / SUP-10 - credential detection without crying wolf

The check that will be switched off first if it is careless, so its rule is
stated precisely and its false-positive budget is explicit.

**A value is reported when it matches at least one of:**

1. **Key-name signal** - the key matches
   `(?i)(pass(word|wd)?|secret|token|apikey|api_key|private_key|credential|auth)`
   **and** the value is a non-empty literal that is not obviously a reference
   (`{{`, `$(`, `${`, `valueFrom`, a path, or one of the known placeholder-but-empty
   forms `""`, `null`).
2. **Shape signal** - the value matches a known credential shape: a PEM block
   header, a JWT (`eyJ` + two dot-separated base64 segments), an AWS access key
   (`AKIA[0-9A-Z]{16}`), a GitHub token prefix, a Docker `config.json` `auth`
   field, or a base64 blob that decodes to `user:password`.
3. **Placeholder signal** (SUP-10 only) - the value is one of the known
   placeholder credentials.

**Deliberately not used:** raw Shannon entropy on its own. It flags every
checksum, UUID, generated hostname and base64-encoded certificate *chain* in a
chart, and a check with a 60% false-positive rate teaches its readers to ignore
it. Entropy is used only as a *tiebreaker* to raise the severity of a value that
already matched signal 1.

**A `Secret` template's `stringData` is not a finding by itself** - that is what
Secrets are for. It is a finding when the value is a literal in the chart rather
than a reference to something injected, which is signal 1 applied to a Secret.

The pattern sets are configuration, not constants, so a deployment can add its
own and suppress a false positive without a rebuild.

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
  is `block` and why a chart that fails it has its whole result set marked
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
   Zero matches is a `block` failure.
2. For each `Service.spec.ports[]`, resolve `targetPort`:
   - a number must appear as a `containerPort` on some matched container;
   - a **string** must appear as a `containerPort.name` on some matched
     container - and port names are limited to 15 characters, so a truncated
     name is a real and common cause;
   - absent `targetPort` defaults to `port`, and the same rules apply.
3. A `Service` with no selector is skipped with reason
   `headless or externally managed endpoints`, not failed - that is a legitimate
   shape (`ExternalName`, manually managed `EndpointSlice`).

---

## 4. What ships, counted

| Category | Rows | v1 | v1.1 | Tier 2 | Evidence | Other |
|---|---|---|---|---|---|---|
| SCH | 7 | 5 | 1 | 1 | 0 | 0 |
| PDB | 9 | 8 | 0 | 0 | 1 | 0 |
| PRB | 7 | 7 | 0 | 0 | 0 | 0 |
| SEC | 11 | 10 | 0 | 1 | 0 | 0 |
| RBAC | 8 | 7 | 0 | 0 | 1 | 0 |
| CFG | 12 | 9 | 1 | 1 | 1 | 0 |
| RES | 9 | 4 | 0 | 1 | 4 | 0 |
| NET | 11 | 11 | 0 | 0 | 0 | 0 |
| STO | 12 | 8 | 1 | 0 | 3 | 0 |
| OBS | 10 | 2 | 0 | 0 | 7 | 1 |
| MTA | 9 | 7 | 2 | 0 | 0 | 0 |
| SUP | 10 | 7 | 0 | 0 | 2 | 1 |
| UPG | 11 | 3 | 1 | 1 | 6 | 0 |
| **Total** | **126** | **88** | **6** | **5** | **25** | **2** |

`Other` is the two rows that are neither automated nor manual: OBS-10, which is
the same assertion as NET-06 and is implemented there, and SUP-06, which this
platform guarantees structurally by promoting digests.

Of the 118 assertions in the source catalog plus 8 proposed additions:
**88 ship as automated tier-1 checks**, 6 follow once their heuristics are tuned
against a real corpus, 5 are genuinely tier-2, and **25 are evidence** - document
reviews that this feature tracks as a checklist beside the automated results and
never reports as passed.

That last number is the honest one. A tool claiming to automate 118 of 118 would
be claiming to have read the vendor's restore-test report.
