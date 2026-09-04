# Compliance Scanner — Validation Feedback

**From:** Platform compliance team
**To:** Scanner engineering
**Scan under review:** One product release — 855 rendered objects, 257 workload containers, 63 checks fired, 2,253 findings (481 Critical, 1,127 Warning, 645 Informational)

---

## What we did

We ran the scanner against a production release, then independently re-implemented fourteen check families directly against the same rendered manifest and compared the results row by row.

**Ten of fourteen matched your counts exactly.** Detection quality is good and materially better than the previous cycle. The items below are the remaining gaps, each with the manifest evidence needed to reproduce and fix them.

| Outcome | Count |
|---|---|
| Checks verified accurate | 10 families, exact match |
| Confirmed false positives | 9 findings (8 Critical) |
| Findings needing re-tiering | ~42 (all Critical) |
| Real conditions in our data with no check | 6 |
| Genuine issue missed by an existing check | 1 (Critical) |

**Naming.** All chart, workload, secret and namespace names below are substituted. Template paths, line numbers, field paths, field values and counts are reproduced exactly as your scanner emitted them, so every item is reproducible on your side.

---

# Part A — False positives

## A1. `PDB-01` — workload-to-policy join fails on namespace

**Emitted (3 Critical):**
```
PDB-01 | Critical | Deployment controller-a
  chart-a/templates/controller-a-deployment.yaml, line 1323
  Observed: "no such rule matches this workload"
  Finding:  "controller-a runs 2 copies, and the chart does not tell the
             platform how many must stay running during maintenance."
```

**Manifest — the policy the scanner says does not exist:**
```yaml
# chart-a/templates/controller-a-deployment.yaml, line 1323
kind: Deployment
metadata:
  name: controller-a
  namespace: app-namespace           # <-- namespace declared
spec:
  replicas: 2
  template:
    metadata:
      labels:
        app: controller-a
        release: app-release

---
# chart-a/templates/pdb.yaml, line 2
kind: PodDisruptionBudget
metadata:
  name: controller-a-pdb
                                     # <-- namespace NOT declared
spec:
  maxUnavailable: 50%
  selector:
    matchLabels:
      app: controller-a              # matches the pod labels
      release: app-release           # matches the pod labels
```

**Issue.** The join requires `metadata.namespace` to be equal on both objects. `helm template` emits that field only where a chart hard-codes it — when absent, the object lands in the release namespace at install time. This release mixes both conventions, so one side reads `app-namespace` and the other reads nothing.

Confirmed on all three affected pairs. On the third pair the mismatch runs the **opposite** way (workload absent, policy declared), which rules out a one-directional rendering assumption.

| Workload | Workload ns | Policy | Policy ns | Labels match | ns match |
|---|---|---|---|---|---|
| `controller-a` | `app-namespace` | `controller-a-pdb` | *absent* | Yes | No |
| `controller-b` | `app-namespace` | `controller-b-pdb` | *absent* | Yes | No |
| `ca-service` | *absent* | `ca-service-pdb` | `app-namespace` | Yes | No |

**Fix.**
```
effective_namespace(obj) = obj.metadata.namespace or RELEASE_NAMESPACE
```

**Scope.** This is a join defect, not a rule defect. The same comparison is likely used wherever one object is related to another — service to workload, binding to identity, claim to mount. We recommend auditing every cross-object lookup rather than patching this check.

**Acceptance test.** `PDB-01` must emit **0 findings** against a manifest where a PodDisruptionBudget declares no namespace and its selector matches a namespaced workload's pod labels.

---

## A2. `PDB-09` — same defect, opposite assertion

**Emitted (4 Critical):**
```
PDB-09 | Critical | PodDisruptionBudget controller-a-pdb
  chart-a/templates/pdb.yaml, line 2
  Finding: "this rule protects no workload in the release"
```

**Issue.** `PDB-01` and `PDB-09` fire on the **same three object pairs**, asserting opposite things:

| Check | Assertion |
|---|---|
| `PDB-01` | "this workload has no disruption policy" |
| `PDB-09` | "this disruption policy protects no workload" |

Both cannot be true. Each is the mirror image of the other, and both follow from the single failed namespace comparison in A1.

The fourth finding (`ml-chart/charts/inference/templates/controller-manager-pdb.yaml`, line 55) *does* match its Deployment. It has a real problem — `minAvailable: 0`, see B2 — but is reported for the wrong reason.

**Verified: all 27 PodDisruptionBudgets in this release match at least one workload. This check should emit zero findings.**

**Suggested diagnostic.** Two checks asserting contradictory things about the same object pair almost always indicates one broken join rather than two broken rules. Worth adding as an internal consistency assertion in your test suite.

---

## A3. `PDB-02` — flags the configuration the standard recommends, misses the real deadlock

**Emitted (1 Critical):**
```
PDB-02 | Critical | PodDisruptionBudget agent-service-pdb
  chart-b/templates/agent-service-pdb.yaml, line 2
  Observed: "at least 1 copies must stay running"
  Finding:  "the rule agent-service-pdb demands that every copy stays running,
             so the platform can never get permission to move one. Patching any
             server that hosts this service will wait indefinitely, and a cluster
             upgrade stalls until somebody intervenes."
```

**Manifest:**
```yaml
# chart-b/templates/agent-service-pdb.yaml, line 2
kind: PodDisruptionBudget
metadata:
  name: agent-service-pdb
spec:
  minAvailable: 1
  selector:
    matchLabels:
      app: agent-service
```

The covered workload is a **Deployment with `replicas: 2`**.

```
disruptions_allowed = replicas − minAvailable = 2 − 1 = 1
```

One copy can be evicted. Maintenance proceeds normally. There is no deadlock.

**Issue.** This is not only a false positive — it contradicts the governing standard. §4.2.5:

> *"For replicas=2, a common safe setting is `maxUnavailable: 1` (or `minAvailable: 1`)"*

And §4.4, baseline defaults:

> *"replicas = 2: PDB `maxUnavailable: 1` (or `minAvailable: 1`)"*

The chart implemented exactly what the standard prescribes, and the scanner rates it Critical. A chart author who acts on this finding makes their release worse. Of all defect classes this is the most damaging to trust in the report.

**Root cause.** The rule appears to pattern-match on the literal `minAvailable: 1` rather than computing the disruption allowance, and does not evaluate percentage forms at all.

**Fix.**
```python
def disruptions_allowed(pdb, replicas):
    if 'minAvailable' in pdb:
        v = pdb['minAvailable']
        need = math.ceil(replicas * pct(v)) if is_pct(v) else v
        return replicas - need
    if 'maxUnavailable' in pdb:
        v = pdb['maxUnavailable']
        return math.floor(replicas * pct(v)) if is_pct(v) else v
    return None

fire = (allowed := disruptions_allowed(...)) is not None and allowed <= 0
```

**Acceptance test.** Against this release the check must emit **exactly one** finding — the percentage deadlock described in **B1** — and must not fire on `minAvailable: 1` where replicas ≥ 2.

---

## A4. `CFG-14` — fires on the presence of data, not on what the data is

**Emitted: 56 Critical.** Verified genuine: **14**.

### Correctly flagged

```yaml
# chart-c/templates/service-tls-cert.yaml, line 2
kind: Secret
metadata:
  name: service-tls-cert
data:
  ca.crt:         LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t...   # certificate
  tls.crt:        LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t...   # certificate
  server.pem:     LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0t...   # certificate
  tls.key:        LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVkt...   # RSA PRIVATE KEY
  server-key.pem: LS0tLS1CRUdJTiBSU0EgUFJJVkFURSBLRVkt...   # RSA PRIVATE KEY
```

Two RSA private keys shipped in the chart. Correct Critical.

### Incorrectly flagged

```yaml
# chart-d/templates/db-connection-properties.yaml, line 186
kind: Secret
metadata:
  name: db-connection-properties
stringData:
  db.properties: |
    dataSource.user = svcusr
    dataSource.password =                    # <-- EMPTY
  conf-db.properties: |
    dataSource.user = svcusr
    dataSource.password =                    # <-- EMPTY
```

This is a configuration template with **empty** credential fields, expecting values to be injected at install time. There is nothing exposed. It is rated Critical.

### Classification of all 56

| Class | Count | Assessment |
|---|---|---|
| Contains a PEM `PRIVATE KEY` block | **12** | Genuine Critical |
| Contains a non-empty password value | **2** | Genuine Critical |
| Short opaque value — usernames, hostnames, object names | 32 | Requires value inspection |
| Configuration/properties file, credential fields empty | 10 | Mostly false, as above |

**Fix.** Base64-decode `data`, read `stringData` directly, and classify on content:

| Sub-check | Condition | Severity |
|---|---|---|
| `CFG-14a` | Decoded value contains `-----BEGIN ... PRIVATE KEY-----` | Critical |
| `CFG-14b` | Decoded value is a non-empty credential-shaped string | Critical |
| `CFG-14c` | Secret carries inline data, contents not credential-shaped | **Advisory** |

Also treat an empty right-hand side (`password =`, `password:` with nothing after it) as an explicit non-finding rather than a match.

---

## A5. `CFG-14` — evidence pane does not show the field the finding is about

**Emitted:**
```
CFG-14 | Critical | Secret db-connection-properties
  chart-d/templates/db-connection-properties.yaml
  Evidence window: lines 164–192 of 4,939
  Footer: "data is not in this manifest, which is what the finding says"
```

**Issue.** The secret's content begins at line **186** (`stringData:`) and continues past the bottom of the pane. Lines 164–192 show `apiVersion`, `kind`, `metadata`, `labels` and `annotations` — none of which the finding concerns. A reviewer opening this finding sees no problem, because the problem is not on screen.

The footer note compounds it: the finding is about `stringData`, but the note refers to `data`.

**Fix.** Centre the evidence window on the line resolved from the `Field` column and highlight it. Where `Field` is `data` or `stringData`, resolve to the first key under it.

---

## A6. `CFG-13` — flags secret *names* as though they were secret *values*

**Emitted (subset of 14 Critical):**
```
CFG-13 | Critical | StatefulSet workload-e, container workload-e
  chart-e/templates/statefulset.yaml, line 127
  Field: spec.template.spec.containers[0].env
  Observed: "APP_SESSION_SECRET holds an opaque high-entropy value in a
             field named for a credential"
  Confidence: Confirmed from the chart
```

**Manifest:**
```yaml
# chart-e/templates/statefulset.yaml, line 127
env:
  - name: APP_CA_PASSWORD
    valueFrom:
      secretKeyRef:                          # correct pattern, not flagged
        name: ca-credential
        key: password
  - name: APP_SESSION_SECRET
    value: "session-store-v1"                # flagged
  - name: APP_SESSIONCLUSTER_SECRET
    value: "cluster-session-store-v1"        # flagged
  - name: APP_CACHE_SECRET
    value: "cache-store-v1"                  # flagged
```

**Issue.** The flagged values follow the naming convention of other objects in the release, not the shape of credential material. They are most likely secret object names the application resolves at runtime.

Your wording hedges correctly — *"holds an opaque high-entropy value in a field named for a credential"* — but the row is rated **Critical** with confidence **`Confirmed from the chart`**. A value-shape inference should not carry your highest severity and your highest confidence simultaneously.

**Fix.** Cross-reference the literal against the names of Secrets and ConfigMaps shipped in the release. On match, downgrade to Medium with confidence `Needs someone who knows this workload`.

---

## A7. `STO-10` — correct finding, incorrect evidence

**Emitted (17 Critical):**
```
STO-10 | Critical | StatefulSet workload-a
  chart-f/templates/statefulset.yaml, line 66
  Field: spec.template.spec.volumes
  Observed: "config-mount (a folder on the server), hugepages (scratch space,
             deleted with the copy), devices (a folder on the server),
             drivers (a folder on the server)..."
```

**Manifest:**
```yaml
# chart-f/templates/statefulset.yaml, line 66
kind: StatefulSet
metadata:
  name: workload-a
spec:
  volumeClaimTemplates: []                    # <-- the actual finding
  template:
    spec:
      volumes:
        - name: hugepages
          emptyDir: { medium: HugePages }     # memory allocation, not scratch
        - name: devices
          hostPath: { path: /sys/bus/pci/devices }   # device access, not state
        - name: app-logs
          emptyDir: {}                        # genuinely ephemeral
```

**Issue.** The underlying finding is correct — this is a StatefulSet with **no `volumeClaimTemplates`**, so anything it writes is lost on restart. But the evidence text miscategorises two volume types:

- `emptyDir` with `medium: HugePages` is a **hugepage memory allocation**, a standard mechanism for performance-sensitive workloads. It is not scratch storage and holds no state.
- `hostPath` under `/sys` and `/dev` is **device access**. It is already reported by `SEC-08` and is not application state.

A reviewer reading this evidence will conclude the check does not understand the workload, and will discount the genuine point buried inside it.

**Fix.** Exclude from state analysis: `emptyDir` with `medium: HugePages` or `medium: Memory`; `hostPath` mounts under `/sys`, `/dev`, `/proc`. The finding then reduces to its accurate core:

> *"`workload-a` is a StatefulSet — a workload type chosen for stable identity and durable storage — but declares no `volumeClaimTemplates`. Everything it writes is lost on restart."*

---

## A8. `RBAC-05` — title does not match the rule in 15 of 49 cases

**Emitted (49 Critical):**
```
RBAC-05 | Critical | Role monitor-server-role
  chart-g/charts/monitor-server/templates/monitor-server-rbac.yaml, line 1097
  Title:   "No service can list the namespace's credentials"
  Finding: "a rule can read every secret in the namespace"
```

**Manifest:**
```yaml
# chart-g/charts/monitor-server/templates/monitor-server-rbac.yaml, line 1097
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]              # <-- no 'list'. No resourceNames either.
```

**Issue.** The finding is **correct** — `get` with no `resourceNames` permits fetching any secret in the namespace by name. But the title says *"can **list**"*, and this rule has no `list` verb. A reviewer checking the rule against the title concludes the finding is mistaken and moves on.

Verified across all 49 findings:

| Rule contains | Count | Title accurate |
|---|---|---|
| `list` or `watch` on secrets | 34 | Yes |
| **Only `get`, no `list`** | **15** | **No** |

**Fix.** Split the check so the text matches the rule.

| Sub-check | Condition | Text |
|---|---|---|
| `RBAC-05a` | `list` or `watch` on secrets | "can enumerate every credential in the namespace" |
| `RBAC-05b` | `get` on secrets with no `resourceNames` | "can fetch any credential in the namespace by name" |

---

## A9. `SEC-02` / `SEC-04` — three Critical findings for one configuration decision

**Emitted — three separate Critical rows on the same container:**
```
SEC-03 | Critical | chart-f/templates/statefulset.yaml, line 66
         Field: ...containers[0].securityContext.privileged
SEC-04 | Critical | chart-f/templates/statefulset.yaml, line 66
         Field: ...containers[0].securityContext.capabilities.drop
SEC-02 | Critical | chart-f/templates/statefulset.yaml, line 66
         Field: ...containers[0].securityContext.allowPrivilegeEscalation
```

**Manifest:**
```yaml
# chart-f/templates/statefulset.yaml, line 66
securityContext:
  privileged: true
  readOnlyRootFilesystem: true
```

**Issue.** `privileged: true` is the root cause of all three. When it is set:

- the kernel grants the **full** capability set regardless of `capabilities.drop`, so `SEC-04` is unactionable until `privileged` is removed;
- privilege escalation is permitted regardless of `allowPrivilegeEscalation`, so `SEC-02` is likewise unactionable.

Setting `drop: ["ALL"]` and `allowPrivilegeEscalation: false` on this container changes nothing. The reader receives three Critical findings, fixes two of them, and the security posture is identical.

Worth noting the same manifest sets `readOnlyRootFilesystem: true` — good practice, but largely negated by `privileged: true`, since a privileged process can remount the filesystem. Your report does not mention this interaction either.

**Scale.** 4 of 47 `SEC-02` findings and 4 of 67 `SEC-04` findings sit on privileged containers. The remaining 43 and 63 are genuine standalone findings and should be preserved — `SEC-02` in particular is among the most valuable checks in the pack.

**Fix.** When a container is privileged, emit dependent findings with outcome `Superseded by SEC-03` rather than Critical, and add a line to the `SEC-03` finding noting which other controls it nullifies.

---

# Part B — Conditions in our data that no check detected

## B1. Percentage-based disruption deadlock

**Present in the manifest. No finding emitted.**

```yaml
# chart-h/templates/workload-d-pdb.yaml
kind: PodDisruptionBudget
metadata:
  name: workload-d-pdb
spec:
  maxUnavailable: 10%
  selector:
    matchLabels:
      app: workload-d
```

The covered workload is a **Deployment with `replicas: 1`**.

```
floor(1 × 0.10) = 0 disruptions allowed
```

**Issue.** This is a genuine deadlock. No eviction can ever be approved, so a drain of the hosting node hangs indefinitely and a cluster upgrade stalls until someone intervenes manually.

This is precisely the condition `PDB-02` exists to detect. It did not fire here — it fired instead on the one healthy configuration in the release (A3).

**Check should:** compute the disruption allowance including percentage forms and fire when the result is ≤ 0. Severity Critical.

---

## B2. Disruption policies that permit unlimited eviction

**Three present in the manifest. No finding emitted for this condition.**

```yaml
kind: PodDisruptionBudget
metadata:
  name: helper-a-pdb
spec:
  minAvailable: 0                 # every copy may be evicted at once
  selector:
    matchLabels:
      app: helper-a
```

**Issue.** `minAvailable: 0` is a no-op. The policy exists in the chart, passes review, appears in inventories — and provides zero protection. This is a more dangerous failure mode than a missing policy, because a missing policy is visible and a useless one is not.

One of these three is currently reported under `PDB-09` for the wrong reason ("protects no workload" — it does match a workload).

**Check should:** fire when `minAvailable: 0`, or when the computed allowance equals or exceeds the replica count. Severity Medium.

> Suggested text: *"`helper-a-pdb` declares a disruption policy that allows every copy to be evicted at once. The policy exists but provides no protection — it will not prevent a drain from taking the service fully offline."*

---

## B3. `NET_RAW` granted with no finding from any check

**Three containers in the manifest add capabilities and receive no finding at all.**

```yaml
# chart-h/templates/upgrade-job.yaml
kind: Job
metadata:
  name: upgrade-job-a
spec:
  template:
    spec:
      containers:
        - name: upgrade-job-a
          securityContext:
            capabilities:
              add: ["NET_RAW"]      # no finding produced
```

**Issue.** We counted 22 containers adding capabilities across the release. `SEC-13` flagged 19. The three omitted add `NET_RAW` alone (two containers) and `SYS_NICE` alone (one container).

`SEC-13` appears to key on `NET_ADMIN` and the `SETUID`/`SETGID`/`CHOWN` group. `NET_RAW` on its own is not matched — but it permits raw packet crafting and ARP/DNS spoofing from inside the pod, which is not a low-risk grant.

**Check should:** introduce a middle tier so these surface without inflating Critical.

| Tier | Capabilities | Severity |
|---|---|---|
| High-risk | `SYS_ADMIN`, `SYS_MODULE`, `SYS_PTRACE`, `DAC_READ_SEARCH`, `DAC_OVERRIDE`, `NET_ADMIN` | Critical |
| Privilege-relevant | `SETUID`, `SETGID`, `CHOWN`, `FOWNER`, **`NET_RAW`**, `SYS_CHROOT` | **Warning** |
| Low-risk | `AUDIT_WRITE`, `SYS_NICE`, `KILL` | Advisory |

---

## B4. Writable container root filesystem — no check exists

**37 of 257 containers in this release. No check in the pack detects it.**

```yaml
securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  # readOnlyRootFilesystem not set -> defaults to false
```

**Issue.** With a writable root filesystem, anything that achieves code execution inside the container can overwrite the application binary and persist across restarts. A read-only root filesystem means every restart returns a known-good image.

The check families present are `SEC-01`, `SEC-02`, `SEC-03`, `SEC-04`, `SEC-06`, `SEC-07`, `SEC-08`, `SEC-11`, `SEC-12`, `SEC-13`. None covers this.

**Check should:** fire when `readOnlyRootFilesystem` is not `true`, resolved through pod-level inheritance. Severity Medium.

Pair it with an Advisory for the inverse case — `readOnlyRootFilesystem: true` with no writable `emptyDir` mounted at `/tmp` — which commonly fails at runtime when the application writes temporary files.

---

## B5. A regression to a mutable image tag would not be Critical

**Verified: 0 of 257 images in this release use `:latest` or an untagged reference.** Every image carries an explicit version tag.

**Issue.** `SUP-01` is a single Warning-severity check covering "not pinned by digest", which fired 257 times. There is no separate rule for mutable tags. If a future release introduced `image: app:latest`, it would appear at the same Warning level as 257 existing rows and be invisible.

The two conditions have different severity and different owners. Digest pinning is frequently resolved by the build pipeline at relocation time; a mutable tag is always a chart defect.

**Check should:** split.

| Check | Condition | Severity | Owner |
|---|---|---|---|
| `SUP-01a` | `:latest`, or no tag and no digest | **Critical** | Chart template |
| `SUP-01b` | Tagged but not digest-pinned | Warning | Build pipeline |

Against this release `SUP-01a` emits zero findings — a good result the current design has no way to express.

---

## B6. No pass records

**All 2,253 rows carry `Outcome: Fail`.**

**Issue.** Three consequences:

1. **No denominator.** "2,253 findings" cannot be converted into a compliance percentage, and counts cannot be compared across releases of different sizes.
2. **Correct configuration is invisible.** This release tags all 257 images correctly, ships 6 NetworkPolicies including 4 default-deny, and has zero containers with `allowPrivilegeEscalation: true` explicitly set. None of it is reportable.
3. **A silent check and a passing check look identical.** Nine checks fired on this product that produced nothing on the previous one. With failure-only output there is no way to tell whether they passed or never ran — which also means a regression cannot be detected.

**Check should:** emit a per-check summary alongside the findings.

```csv
Check,Evaluated,Passed,Failed,NotApplicable,ComplianceRate
SUP-01a,257,257,0,0,100.0%
SUP-01b,257,0,257,0,0.0%
SEC-03,257,253,4,0,98.4%
```

---

# Part C — Finding language

Several findings in this scan are already well written. The ones below are the model we would like applied across the pack, because our reviewers understood them without needing to consult anyone.

## C1. What makes these work

Every effective finding does the same four things in order:

| Step | Purpose |
|---|---|
| **1. State the literal** | What the manifest says, quoted |
| **2. Name the plausible reading** | What a competent reader would reasonably assume |
| **3. Give the runtime reality** | What actually happens, and why it differs |
| **4. State the consequence** | What breaks, when, and who notices |

**Step 2 is the one usually missing, and it is the one that lands.** Most misconfigurations are not careless — they look correct. Naming the plausible reading tells the reader why they missed it, which is far more persuasive than restating the rule.

## C2. Worked examples

**Ingress without TLS.** The manifest is:
```yaml
spec:
  tls: null
  rules:
    - http:
        paths:
          - backend:
              servicePort: 443
```

> The backend port is 443, so it **looks** encrypted — but `spec.tls` is null, which means the ingress terminates **plain HTTP** on the outside. Traffic between the client and the ingress controller is unencrypted; only the hop from controller to pod is protected. The 443 backend is what makes this easy to miss in review.

**Capabilities not dropped.**

> Without a `drop` block the container silently inherits the container runtime's **default capability set** — roughly 14 permissions including `CHOWN`, `SETUID`, `SETGID`, `NET_RAW` and `KILL`. Nothing in the chart asks for raw network access, but the container receives it.

**Private key shipped in a chart.**

> Base64 is encoding, not encryption. `echo "LS0t..." | base64 -d` returns the private key in plain text. Anyone with read access to the repository, a registry mirror or a release archive holds the key that authenticates this service — and rotating it means rebuilding and redistributing the release.

**Resource requests missing.**

> CPU and memory **limits** are set but **requests** are not. Kubernetes copies the limit into the request when only a limit is given, so this container silently reserves 3 full CPUs and 6Gi on whatever node it lands on — far more than the author likely intended, and enough to prevent scheduling on a busy cluster.

**Over-broad RBAC rule.**

> One rule grants `pods` and `secrets` together, so every verb applies to both. The workload needs pod access — but the same rule lets it `patch` credentials, meaning a compromise could silently swap a database password for one the attacker controls. The legitimate owner sees no error; the credential simply changes.

**Percentage deadlock.**

> The policy allows 10% of copies to be unavailable, but the workload runs one copy, and 10% of 1 rounds down to zero. It reads as permissive and is absolute — no eviction can ever be approved, so any drain of the hosting node hangs.

## C3. Where the language should come from

Every one of the above is a mismatch between a **declared value** and an **effective value**:

| Source of the gap | Example |
|---|---|
| A Kubernetes default fills the blank | `allowPrivilegeEscalation` absent → `true` |
| One field overrides another | `privileged: true` overrides `drop: ALL` |
| The value is inherited, not local | `runAsNonRoot: false` set at pod level |
| Arithmetic produces a surprise | `floor(1 × 10%)` = 0 |
| A partial setting implies a full one | limits set, requests absent → request = limit |
| Encoding resembles protection | base64 is not encryption |

Once the scanner computes both values, most of this text can be templated:

```
"{field} is {observed_state}, which {plausible_reading}.
 In practice {effective_value} applies, so {consequence}."
```

## C4. Findings to rewrite

| Check | Current | Suggested |
|---|---|---|
| `SEC-02` | *"allowPrivilegeEscalation — not declared, so it defaults to allowed"* | *"This container runs as a non-root user and drops every capability, so it looks well locked down. But `allowPrivilegeEscalation` is not declared, and Kubernetes defaults it to true — a setuid binary inside the image can still escalate to root. One line fixes it, and it breaks nothing unless the image relies on setuid."* |
| `SEC-04` on a privileged container | *"keeps the default set of special permissions"* | *"No `capabilities.drop` is declared, so the container inherits the runtime's 14 default permissions including raw network access. Note this container is also `privileged: true`, which grants the full set regardless — fix that first, because dropping capabilities changes nothing until then."* |
| `CFG-11` | *"asks for Secret app-credential, which this release does not ship"* | *"This workload mounts `Secret app-credential`, which nothing in this release creates. If nothing supplies it before startup, these pods will sit in `CreateContainerConfigError` with no other explanation — the event log will name the missing secret but not why it is expected."* |
| `STO-10` | *"hugepages (scratch space, deleted with the copy)"* | *"This is a StatefulSet — a workload type chosen for stable identity and durable storage — but it declares no `volumeClaimTemplates`. Everything it writes is lost on restart. If it holds no durable state, a Deployment would be the more accurate choice."* |
| `PDB-03` | *"protects a workload that runs a single copy"* | *"This policy protects a workload with one copy. There is no second copy to keep running, so the platform can never be given permission to move it — maintenance on its node will wait. Either run two copies and allow one to be unavailable, or remove the policy and accept a brief restart."* |

---

# Part D — Schema and presentation

## D1. `Value is` is not deterministic

Across 52 `CFG-11` findings referencing the **same** missing secret, from sibling charts with identical reference patterns:

| Chart | Missing reference | `Value is` |
|---|---|---|
| `chart-f` | `Secret app-credential` | Could not be established |
| `chart-i` | `Secret app-credential` | Could not be established |
| `chart-j` | `Secret app-credential` | **Overridable in values** |
| `chart-k` | `Secret app-credential` | **Overridable in values** |
| `chart-l` | `Secret app-credential` | Could not be established |

Split across all 52: **27 `Could not be established`, 25 `Overridable in values`.** Across the whole scan the placeholder appears on **1,060 of 2,253 rows (47%)** — up from 32% on the previous product we scanned.

**Why the column matters.** It answers the single most useful triage question: *can I fix this at install time, or does it need a new chart?* When it works, it separates the findings a platform team can resolve unaided from those that require the chart author. At 47% unresolved it cannot be used for that.

**Why it is failing.** Rendering destroys the link. Once Helm has run, `replicas: 2` carries no record of whether it came from `.Values.replicaCount` or a literal. Recovering that from rendered output is inference, and the inference degrades as chart complexity grows.

**Suggested fix — differential rendering:**
```bash
helm template rel ./chart                     > baseline.yaml
helm template rel ./chart -f perturbed.yaml   > perturbed.yaml
# field differs between the two -> "Overridable in values"
# field identical               -> "Fixed by the chart"
```
One additional render, near-total accuracy for scalar fields. Where still undetermined, leave the cell empty rather than filling it with a placeholder — an empty cell is honest, and 47% placeholders trains readers to ignore the column.

## D2. Add an `effective_value` column

Most of the language improvements in Part C become mechanical once both values are available:

| `observed_value` | `observed_source` | `effective_value` |
|---|---|---|
| `not declared` | platform default | `true (Kubernetes default)` |
| `false` | inherited from pod | `false` |
| `10%` | explicit | `0 pods (floor of 1 × 10%)` |
| `not declared` | platform default | `30s (Kubernetes default)` |

This also resolves a live inaccuracy: `SEC-01` findings currently report a container-level field path on pods where the value is inherited from the pod, sending the reader to the wrong line.

## D3. Group findings by originating rule

One over-broad RBAC rule in this scan produced four separate Critical rows across three check families:

```yaml
# chart-m/templates/ca-service-rbac.yaml, line 2775
- apiGroups: [""]
  resources: ["pods", "secrets"]
  verbs: ["get", "list", "delete", "watch", "patch"]
```

| Verb | Effect on `secrets` | Reported as |
|---|---|---|
| `get` (unscoped) | read any secret by name | `RBAC-05b` |
| `list` / `watch` | enumerate every secret in the namespace | `RBAC-05a` |
| `delete` | destroy credentials | `RBAC-09` |
| `patch` | overwrite a credential with one you control | `RBAC-09` |

All four are correct. But they appear as unrelated rows in different sections of the report, and the reader must reconstruct that they share one root cause and one fix — splitting `pods` and `secrets` into separate rules with scoped verbs.

**Suggested:** add a `root_cause_id` field so findings arising from the same manifest construct can be grouped in the UI.

## D4. Severity should respect confidence

All 52 `CFG-11` findings carry confidence `Likely, unless the platform provides it` — an honest and correct assessment, since these objects may well be supplied by an installer outside the chart. They are all rated **Critical**.

Across the scan, **429 of 481 Critical findings** carry `Confirmed from the chart`, and only 6 findings in the entire report use `Needs someone who knows this workload`.

**Suggested rule:** no finding may exceed High severity unless confidence is `Confirmed from the chart`. This single constraint removes most of the disputes we would otherwise have to arbitrate.

Related: `kube-root-ca.crt` should be allowlisted in `CFG-11`. Kubernetes auto-creates it in every namespace, and it accounts for 2 of the 52.

---

# Part E — Consolidated list

| ID | Item | Type | Findings affected | Priority |
|---|---|---|---|---|
| A1 | `PDB-01` namespace join — **audit all cross-object lookups** | False positive | 3 Critical | **P1** |
| A2 | `PDB-09` — resolves with A1 | False positive | 4 Critical | **P1** |
| A3 | `PDB-02` — compute allowance, handle percentages | False positive | 1 Critical | **P1** |
| A4 | `CFG-14` — decode and classify content; sub-tier a/b/c | False positive | ~42 Critical | **P1** |
| A5 | `CFG-14` evidence pane does not centre on `Field` | Presentation | all 56 | P2 |
| A6 | `CFG-13` — cross-reference against shipped object names | False positive | subset of 14 | P2 |
| A7 | `STO-10` — exclude hugepages and `/sys` `/dev` hostPath | Evidence | 17 | P2 |
| A8 | `RBAC-05` — title says "list" on 15 rules that only have `get` | Wording | 15 of 49 | P2 |
| A9 | `SEC-02`/`SEC-04` — supersede when container is privileged | Noise | 8 | P2 |
| B1 | Percentage-based deadlock not detected | Missed | 1 Critical | **P1** |
| B2 | `minAvailable: 0` no-op policies | Missed | 3 | P2 |
| B3 | `NET_RAW` tier | Missed | 3 containers | P2 |
| B4 | `readOnlyRootFilesystem` — no check exists | Missed | 37 containers | P2 |
| B5 | Split `SUP-01` into mutable-tag / digest | Missed | — | P2 |
| B6 | Emit pass records | Structural | all | P2 |
| C | Adopt the four-part finding structure | Wording | pack-wide | P2 |
| D1 | `Value is` non-deterministic (47% unresolved) | Defect | 1,060 rows | P2 |
| D2 | Add `effective_value` column | Schema | pack-wide | P2 |
| D3 | Group findings by originating rule | Presentation | — | P3 |
| D4 | Cap severity at High unless confidence is `Confirmed` | Calibration | 52+ | P2 |

**Effect on this scan.** 481 Critical becomes approximately **396** — 9 confirmed false findings removed, ~42 `CFG-14` findings re-tiered to Advisory, and 1 genuine deadlock added that the pack currently misses.

**What we would need to re-validate.** A rescan of the same release with A1–A4 and B1 addressed. We will re-run our independent verification against it and report the delta.
