# Source standards catalog

> **The source catalog, unchanged.** This is what the organization requires.
> How each entry is turned into a machine-checkable assertion - and which of
> them cannot be - is triaged in [01 - Check Catalog](01-check-catalog.md);
> the rules a check has to follow are in [00 - The Compliance Model](00-compliance-model.md);
> the implementation plan is [design/23](../design/23-compliance.md).
> Start at [README.md](README.md).

Validation catalog for automated scanning of a delivered software package (container images, Helm charts, Kubernetes manifests, and accompanying metadata). Each item is expressed as a machine-checkable assertion so a scanner can emit PASS / FAIL / WARN.

**Scan inputs:** rendered manifests (Helm template / kustomize build), chart metadata and values, image references and manifests/SBOM, CRs for operator-managed components, and release documentation.

**Severity legend:** `BLOCK` = fail the release, `WARN` = flag for review, `INFO` = record only.

---

## 1. Scheduling & Placement

| ID | Check | Severity |
|---|---|---|
| SCH-01 | Topology spread constraints present on workloads with replicas ≥ 2, across `topology.kubernetes.io/zone` and `kubernetes.io/hostname` | BLOCK |
| SCH-02 | `maxSkew: 1` used for zone/hostname spread | WARN |
| SCH-03 | No stacking of multiple hard rules (required nodeAffinity + required antiAffinity + `DoNotSchedule` spread) | BLOCK |
| SCH-04 | Selectors in spread/anti-affinity use stable identity labels only (no commit SHA, build ID, timestamp) | BLOCK |
| SCH-05 | Zones not hardcoded in nodeAffinity; node pool targeting uses pool labels | WARN |
| SCH-06 | Dedicated-node workloads declare matching tolerations for the intended taints | WARN |
| SCH-07 | Replica count sufficient for stated failure tolerance (≥ 2 for node failure, ≥ 3 for single-zone loss) | BLOCK |

## 2. Disruption & Availability

| ID | Check | Severity |
|---|---|---|
| PDB-01 | PodDisruptionBudget defined for every replicated production workload (replicas ≥ 2) | BLOCK |
| PDB-02 | PDB does not permit zero disruptions (`minAvailable` != `replicas`; not `maxUnavailable: 0`) | BLOCK |
| PDB-03 | No PDB attached to single-replica workloads or short-lived Jobs/CronJobs | WARN |
| PDB-04 | Quorum-based workloads document quorum math and PDB is consistent with it | WARN |
| PDB-05 | Rollout strategy compatible with PDB: at least one of `maxSurge > 0` or `maxUnavailable > 0` | BLOCK |
| PDB-06 | `RollingUpdate` used for traffic-serving workloads (`Recreate` requires declared outage window) | BLOCK |
| PDB-07 | `progressDeadlineSeconds` explicitly set | WARN |
| PDB-08 | `terminationGracePeriodSeconds` explicitly set and > 0 | WARN |

## 3. Health Probes & Lifecycle

| ID | Check | Severity |
|---|---|---|
| PRB-01 | Readiness probe defined on every traffic-receiving container | BLOCK |
| PRB-02 | Startup probe defined where initialization is non-trivial; budget = `failureThreshold × periodSeconds` covers worst-case start | WARN |
| PRB-03 | Liveness probe, if present, is less sensitive than readiness (longer period / higher threshold) | WARN |
| PRB-04 | Liveness and readiness use distinct endpoints; liveness has no external dependency calls | BLOCK |
| PRB-05 | Probe timeouts bounded (typically 1–3 s) and periods bounded (typically 5–10 s) | WARN |
| PRB-06 | Probes target the correct container/port when sidecars or proxies are present | BLOCK |
| PRB-07 | Graceful shutdown declared (SIGTERM handling and/or `preStop`); no unjustified long sleeps | WARN |

## 4. Container Security Posture

| ID | Check | Severity |
|---|---|---|
| SEC-01 | `runAsNonRoot: true`; no UID 0 | BLOCK |
| SEC-02 | `allowPrivilegeEscalation: false` | BLOCK |
| SEC-03 | `privileged: true` absent (or an approved, documented exception) | BLOCK |
| SEC-04 | `capabilities.drop: ["ALL"]`; any added capability is enumerated and justified | BLOCK |
| SEC-05 | `readOnlyRootFilesystem: true` for stateless workloads; writable paths are mounted volumes | WARN |
| SEC-06 | `seccompProfile.type: RuntimeDefault` (or stricter) | WARN |
| SEC-07 | `hostNetwork`, `hostPID`, `hostIPC`, `hostPort` not used without exception | BLOCK |
| SEC-08 | No `hostPath` volumes and no container-runtime socket mounts | BLOCK |
| SEC-09 | Image does not assume a fixed UID/GID or the presence of a passwd entry (runs under arbitrary UID) | WARN |
| SEC-10 | Elevated security-policy requirements are scoped to a named ServiceAccount and namespace, not granted broadly | BLOCK |

## 5. Identity & Access (RBAC)

| ID | Check | Severity |
|---|---|---|
| RBAC-01 | Dedicated ServiceAccount per workload; default ServiceAccount not used | BLOCK |
| RBAC-02 | `automountServiceAccountToken: false` unless API access is required | WARN |
| RBAC-03 | No wildcard `*` in verbs, resources, or apiGroups | BLOCK |
| RBAC-04 | Namespace-scoped Role/RoleBinding preferred; ClusterRole/ClusterRoleBinding requires justification | BLOCK |
| RBAC-05 | Secret access limited to `get` on named `resourceNames`; no `list`/`watch` on all Secrets | BLOCK |
| RBAC-06 | No write verbs on roles, rolebindings, clusterroles, clusterrolebindings | BLOCK |
| RBAC-07 | No `impersonate`, `pods/exec`, or `pods/portforward` for runtime identities | BLOCK |
| RBAC-08 | Runtime identity separate from deployment/pipeline identity | WARN |

## 6. Configuration & Secrets

| ID | Check | Severity |
|---|---|---|
| CFG-01 | No credentials, tokens, keys, or certificates in ConfigMaps, manifests, values files, or images | BLOCK |
| CFG-02 | Secrets not passed as command-line arguments | BLOCK |
| CFG-03 | ConfigMaps versioned (name suffix or content hash) and marked `immutable: true` for production | WARN |
| CFG-04 | Config/secret changes trigger a rollout (checksum annotation on pod template or versioned reference) | BLOCK |
| CFG-05 | Checksum inputs are stable (no timestamps or random values causing churn) | WARN |
| CFG-06 | TLS material and file-based config delivered via read-only volume mounts, not env vars | WARN |
| CFG-07 | Only required keys mounted; whole-Secret mounts flagged | WARN |
| CFG-08 | No environment-specific values baked into the image | BLOCK |
| CFG-09 | Secret rotation model documented as create-new-then-roll, not mutate-in-place | WARN |
| CFG-10 | Manifests pass schema validation and policy checks; render is deterministic | BLOCK |

## 7. Resources, Scaling & Performance

| ID | Check | Severity |
|---|---|---|
| RES-01 | CPU and memory requests set on every container, including sidecars and init containers | BLOCK |
| RES-02 | Memory limit set with headroom above steady-state usage | BLOCK |
| RES-03 | CPU limits reviewed for latency-sensitive services (throttling risk) | WARN |
| RES-04 | HPA present for elastic stateless services; `minReplicas` ≥ 2 for critical services | WARN |
| RES-05 | HPA `maxReplicas` schedulable under declared failure scenarios | WARN |
| RES-06 | HPA metric matches the real bottleneck (custom/app metric where CPU is not representative) | INFO |
| RES-07 | Declared performance targets present: p95/p99 latency, throughput, error rate, startup time | WARN |
| RES-08 | Concurrency bounded per instance (thread/worker/connection pool limits declared) | WARN |
| RES-09 | Outbound calls declare timeouts; retries use bounded backoff with jitter | WARN |

## 8. Networking

| ID | Check | Severity |
|---|---|---|
| NET-01 | Default-deny ingress NetworkPolicy present for the application namespace | BLOCK |
| NET-02 | Allow rules are explicit (named ports, specific pod/namespace selectors); no allow-from-all | BLOCK |
| NET-03 | Egress destinations enumerated; unrestricted egress flagged for sensitive workloads | WARN |
| NET-04 | Policy selectors use stable identity labels | BLOCK |
| NET-05 | External exposure declares TLS termination mode explicitly | BLOCK |
| NET-06 | No admin, debug, or metrics endpoints exposed externally | BLOCK |
| NET-07 | Service selectors match pod template labels and are stable across releases | BLOCK |
| NET-08 | Secondary-network attachments are version-controlled, and MTU/VLAN/IPAM are declared and consistent per zone | WARN |
| NET-09 | Secondary networks are not assumed to be covered by NetworkPolicy; compensating controls documented | WARN |
| NET-10 | Hardware-accelerated networking requests devices via resource requests, is isolated to labeled node pools, and declares per-zone capacity and driver/firmware alignment | WARN |

## 9. Storage & Data

| ID | Check | Severity |
|---|---|---|
| STO-01 | Storage type matches workload behavior; storage class named explicitly (no reliance on cluster default) | WARN |
| STO-02 | Access mode justified; `ReadWriteMany` used only when concurrent mount is required | WARN |
| STO-03 | Volume binding mode is `WaitForFirstConsumer` for zonal backends | WARN |
| STO-04 | PVC size, expansion capability, and reclaim policy declared; `Retain` for critical data | WARN |
| STO-05 | Persistent, identity-bearing workloads use StatefulSet, not Deployment with shared PVC | BLOCK |
| STO-06 | Backup, snapshot, and restore procedure documented, with tested restore evidence | BLOCK |
| STO-07 | No startup-time `chown`/`chmod` on shared volumes; access via documented UID/GID and supplemental groups | BLOCK |
| STO-08 | UID/GID and permission model documented per persistent mount | WARN |
| STO-09 | No requirement to disable server-side storage protections (e.g. root squash) as a workaround | BLOCK |
| STO-10 | Node-local storage used only for disposable data or with app-level replication; drain behavior documented | WARN |
| STO-11 | Cross-zone failover expectation for volumes stated explicitly (zonal vs replicated vs pinned) | BLOCK |

## 10. Observability

| ID | Check | Severity |
|---|---|---|
| OBS-01 | Metrics endpoint exposed in a standard scrape format on a dedicated port/path | BLOCK |
| OBS-02 | Golden signals emitted: traffic, errors, latency histograms, saturation | BLOCK |
| OBS-03 | Metric labels are low-cardinality (no request IDs, user IDs, unbounded paths) | BLOCK |
| OBS-04 | Metrics and logs carry stable identifiers: service, version, namespace, instance | WARN |
| OBS-05 | Logs written to stdout/stderr, structured (JSON preferred), with severity and timestamp | BLOCK |
| OBS-06 | No secrets, tokens, or credentials in logs; no environment dumps at startup | BLOCK |
| OBS-07 | Trace context propagated end-to-end; sampling strategy declared | WARN |
| OBS-08 | Correlation/request IDs present in logs and traces | WARN |
| OBS-09 | Alert definitions and runbook links shipped with the release | WARN |
| OBS-10 | Metrics endpoint not publicly exposed | BLOCK |

## 11. Metadata: Labels & Annotations

| ID | Check | Severity |
|---|---|---|
| MTA-01 | Standard app labels present on controllers, pod templates, and services: `name`, `instance`, `component`, `part-of`, `managed-by`, `version` | BLOCK |
| MTA-02 | Label keys DNS-safe; values short, lowercase, and stable | WARN |
| MTA-03 | Only identity labels used in selectors; operational labels (version, build metadata) excluded | BLOCK |
| MTA-04 | No PII, secrets, or large payloads in labels or annotations | BLOCK |
| MTA-05 | Custom labels use an owned domain prefix and are limited in number | WARN |
| MTA-06 | Provenance annotations present: source commit, build ID, release timestamp | BLOCK |
| MTA-07 | Ownership annotations present: owning team, on-call contact, runbook URL | BLOCK |
| MTA-08 | Chart lifecycle hooks used only for bounded one-time tasks; retain-on-uninstall policies justified | WARN |

## 12. Supply Chain & Release Integrity

| ID | Check | Severity |
|---|---|---|
| SUP-01 | Images pinned by immutable digest; no mutable or floating tags in production manifests | BLOCK |
| SUP-02 | All image references resolve to approved registries | BLOCK |
| SUP-03 | Vulnerability scan results attached; critical findings blocked or formally waived | BLOCK |
| SUP-04 | SBOM present and matches the shipped artifact | WARN |
| SUP-05 | Artifact signatures present and verifiable | BLOCK |
| SUP-06 | Same artifact promoted across environments (build once, deploy many) | BLOCK |
| SUP-07 | Chart and dependency versions pinned; rendering reproducible | BLOCK |
| SUP-08 | Rollback reference (previous digest/version) recorded | WARN |
| SUP-09 | Declarative deployment source is version-controlled; no imperative apply steps required | WARN |
| SUP-10 | No default, sample, or placeholder credentials anywhere in the package | BLOCK |

## 13. Upgrade & Maintenance Readiness

| ID | Check | Severity |
|---|---|---|
| UPG-01 | Availability target and maintenance tolerance declared per workload | BLOCK |
| UPG-02 | Statefulness model declared: stateless, leader/follower, or quorum | BLOCK |
| UPG-03 | Drain rehearsal evidence provided: pods evict, reschedule, and become ready within target | BLOCK |
| UPG-04 | Zone-evacuation rehearsal evidence provided where cross-zone failover is required | WARN |
| UPG-05 | No configuration that requires forced eviction or bypassing disruption budgets | BLOCK |
| UPG-06 | Operator/controller-managed components expose configuration through their custom resource, not by editing generated objects | WARN |
| UPG-07 | Custom resources version-controlled and schema-validated against the declared operator version | WARN |
| UPG-08 | Schema/data migrations are explicit, ordered, and reversible or checkpointed | BLOCK |
| UPG-09 | Rollback procedure documented and independent of the failing deployment path | BLOCK |
| UPG-10 | Cross-zone dependency parity declared: any capability required in one zone exists in all target zones, or pinning is documented as an availability constraint | BLOCK |

---

## Scoring Model

| Outcome | Condition |
|---|---|
| **Pass** | Zero `BLOCK` failures; `WARN` items acknowledged |
| **Conditional** | Zero `BLOCK` failures; one or more unacknowledged `WARN` items |
| **Fail** | One or more `BLOCK` failures without an approved, time-bound exception |

**Exception record (required for any waived BLOCK):** check ID, workload, reason, compensating control, approver, expiry date.

**Report fields per finding:** check ID, category, severity, resource kind/name/namespace, manifest path and line, observed value, expected value, remediation hint.
