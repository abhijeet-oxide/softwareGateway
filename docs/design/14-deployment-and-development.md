# 14 - Deployment and Development

> **Prerequisites:** [02 - Configuration](02-configuration.md), [09 - API](09-api.md) §9

---

## 1. Repository layout for deployment

Flux-native. Kustomize base plus per-environment overlays.

```
deploy/
├── base/
│   ├── kustomization.yaml
│   ├── namespace.yaml
│   ├── coordinator/{deployment,service,serviceaccount,pdb}.yaml
│   ├── worker/{deployment,serviceaccount,hpa}.yaml
│   ├── postgres/{statefulset,service,pvc}.yaml     # optional; external is preferred
│   ├── config/system-config.yaml
│   └── network/{networkpolicy-coordinator,networkpolicy-worker}.yaml
├── overlays/
│   ├── dev/          replicas 1, SQLite, debug logging
│   ├── staging/
│   └── production/   replicas 2 + HPA, external Postgres, full limits
├── products/                                        # one ConfigMap per product
│   ├── vendor-a-platform.yaml
│   └── vendor-b-database.yaml
├── flux/
│   ├── gitrepository.yaml
│   └── kustomization.yaml
└── observability/
    ├── servicemonitor.yaml
    ├── prometheusrule.yaml                          # alerts from 12 section 7
    └── dashboards/*.json
```

**Products are a sibling of `base`, not part of an overlay.** They are data, and they change on a different cadence and through a different review path than infrastructure - a platform team owns `base/`, product owners own `products/`. Kustomize overlays are for environment differences, not for content.

## 2. Flux

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: softwaregateway, namespace: flux-system}
spec:
  interval: 5m
  path: ./deploy/overlays/production
  prune: true
  sourceRef: {kind: GitRepository, name: softwaregateway}
  healthChecks:
    - {apiVersion: apps/v1, kind: Deployment, name: coordinator, namespace: softwaregateway}
    - {apiVersion: apps/v1, kind: Deployment, name: worker,      namespace: softwaregateway}
  timeout: 5m
```

Secrets are **not** in Git. VSO materializes them from Vault into the namespace; manifests reference them by name ([02](02-configuration.md) §5.5). Rotation propagates through the mounted volume with no restart ([02](02-configuration.md) §3).

## 3. Workloads

### 3.1 Coordinator

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: coordinator, namespace: softwaregateway}
spec:
  replicas: 2                       # HA for the API; one holds the leader lock (04 section 9)
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    spec:
      containers:
        - name: coordinator
          image: ghcr.io/example/softwaregateway-coordinator:1.4.2
          args: ["--config=/etc/softwaregateway/config.yaml"]
          ports: [{name: http, containerPort: 8080}]
          env:
            - name: SWGW_DATABASE_DSN
              valueFrom: {secretKeyRef: {name: postgres-credentials, key: dsn}}
            - name: POD_NAME
              valueFrom: {fieldRef: {fieldPath: metadata.name}}

          # Liveness: process-local ONLY. See the note below.
          livenessProbe:
            httpGet: {path: /healthz, port: http}
            initialDelaySeconds: 10
            periodSeconds: 20
            failureThreshold: 3
          readinessProbe:
            httpGet: {path: /readyz, port: http}
            periodSeconds: 10
            failureThreshold: 2
          # Covers migrations on a cold start without a long liveness delay.
          startupProbe:
            httpGet: {path: /readyz, port: http}
            periodSeconds: 5
            failureThreshold: 30          # up to 150s

          resources:
            requests: {cpu: 200m, memory: 256Mi}
            limits:   {memory: 512Mi}     # no CPU limit -- see note

          volumeMounts:
            - {name: products, mountPath: /etc/softwaregateway/products, readOnly: true}
            - {name: secrets,  mountPath: /etc/softwaregateway/secrets,  readOnly: true}
            - {name: config,   mountPath: /etc/softwaregateway/config.yaml, subPath: config.yaml, readOnly: true}
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
      volumes:
        - name: products
          projected:
            sources:
              - {configMap: {name: product-vendor-a-platform}}
              - {configMap: {name: product-vendor-b-database}}
        - name: secrets
          projected:
            sources:
              - {secret: {name: vendor-a-registry}}
              - {secret: {name: internal-acr}}
              - {secret: {name: teams-webhooks}}
      terminationGracePeriodSeconds: 45
```

> **`/healthz` checks nothing external** ([09](09-api.md) §9.1). A liveness probe that touched the database would restart every Coordinator during a brief Postgres blip - converting a recoverable dependency hiccup into a fleet-wide crash-loop at precisely the moment the process needs to stay alive and retry. Readiness handles "should I get traffic"; liveness handles "am I wedged".

> **No CPU limit, memory limit only.** A CPU limit causes CFS throttling, which on a latency-sensitive control plane produces sporadic multi-hundred-millisecond stalls that look like network problems and are miserable to diagnose. Requests provide scheduling fairness; the limit adds throttling without adding protection. Memory *is* limited, because unbounded memory is a node-level hazard rather than a self-correcting one.

Note the projected volumes are **not** `subPath` mounts. `subPath` mounts do not receive ConfigMap or Secret updates, which would silently break both config reload ([02](02-configuration.md) §6) and VSO credential rotation. The single `config.yaml` uses `subPath` deliberately, since system config genuinely requires a restart.

A `PodDisruptionBudget` with `minAvailable: 1` keeps one Coordinator through node drains.

### 3.2 Worker

```yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: worker, namespace: softwaregateway}
spec:
  replicas: 3                                # HPA takes over (section 4)
  template:
    spec:
      containers:
        - name: worker
          image: ghcr.io/example/softwaregateway-worker:1.4.2
          env:
            - name: SWGW_WORKER_COORDINATOR_ENDPOINT
              value: http://coordinator.softwaregateway.svc:8080
            - name: SWGW_WORKER_ID
              valueFrom: {fieldRef: {fieldPath: metadata.name}}
          ports: [{name: http, containerPort: 8081}]   # probes and metrics only

          livenessProbe:                     # main loop ticking? (11 section 2.1)
            httpGet: {path: /healthz, port: http}
            periodSeconds: 20
            failureThreshold: 3
          readinessProbe:
            httpGet: {path: /readyz, port: http}       # registered with Coordinator
            periodSeconds: 10

          resources:
            requests: {cpu: "1", memory: 256Mi}        # memory from 05 section 4.5
            limits:   {memory: 512Mi}

          volumeMounts:
            - {name: secrets, mountPath: /etc/softwaregateway/secrets, readOnly: true}
          securityContext:
            runAsNonRoot: true
            readOnlyRootFilesystem: true               # nothing is written to disk
            allowPrivilegeEscalation: false
            capabilities: {drop: ["ALL"]}
      terminationGracePeriodSeconds: 120
```

**`readOnlyRootFilesystem: true` with no writable volume, and no `emptyDir`.** This is invariant I5 enforced by the platform rather than by code review: a worker *cannot* buffer a blob to disk, because there is nowhere to put it. Chaos scenario C10 ([11](11-resiliency-and-backpressure.md) §5) validates it.

Workers hold **no database credentials** - a direct consequence of HTTP leasing ([00](00-overview.md) §5.2), and visible here as a shorter secret mount than the Coordinator's.

**`terminationGracePeriodSeconds: 120`** gives a worker time to finish in-flight blobs on `SIGTERM`: stop leasing, drain, exit. If a blob outlives the grace period the pod is killed and the lease expires - also correct, just less efficient ([11](11-resiliency-and-backpressure.md) §2.1). 120 s is a judgement call: long enough for a typical layer, short enough not to stall a node drain. Sites with very large layers should raise it.

### 3.3 Database

**External managed PostgreSQL is the recommendation** - Cloud SQL, RDS, Azure Database. Backups, failover, patching, and PITR are solved problems we should not re-solve, and this is the only stateful component in the system.

The in-cluster `StatefulSet` in `base/postgres/` exists for dev and evaluation. It is a single instance with a PVC and no automated failover, and the manifest says so in a comment so nobody promotes it to production by accident.

### 3.4 Network policy

The only control protecting the unauthenticated API ([09](09-api.md) §10), so it is not optional:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: coordinator, namespace: softwaregateway}
spec:
  podSelector: {matchLabels: {app: coordinator}}
  policyTypes: [Ingress, Egress]
  ingress:
    - from:
        - podSelector: {matchLabels: {app: worker}}
        - namespaceSelector: {matchLabels: {name: platform-tools}}   # CLI users
        - namespaceSelector: {matchLabels: {name: monitoring}}       # Prometheus
      ports: [{port: 8080, protocol: TCP}]
  egress:
    - to: [{podSelector: {matchLabels: {app: postgres}}}]
      ports: [{port: 5432, protocol: TCP}]
    - {}      # registries, SMTP, Teams, Sigstore, OTel -- restrict per site
```

> **No Ingress, no LoadBalancer, no public exposure.** Until §10.2 of [09](09-api.md) is implemented, anyone who can reach port 8080 can create, cancel, and re-prioritize transfers and read the full audit trail. This is stated in the manifest as a comment, not only here.

## 4. Autoscaling

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: worker, namespace: softwaregateway}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: worker}
  minReplicas: 2
  maxReplicas: 50
  metrics:
    - type: External
      external:
        metric: {name: softwaregateway_queue_backlog_per_worker}
        target: {type: AverageValue, averageValue: "20"}
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 30
      policies: [{type: Percent, value: 100, periodSeconds: 30}]   # aggressive: work is waiting
    scaleDown:
      stabilizationWindowSeconds: 300
      policies: [{type: Pods, value: 1, periodSeconds: 60}]        # conservative: 1 pod/min
```

Requires `prometheus-adapter` to expose the metric.

**Asymmetric on purpose.** Scaling up fast is cheap and directly serves throughput. Scaling down fast is expensive: it kills workers holding leases, and a package finishing at 10:00 followed by another starting at 10:02 would otherwise thrash the fleet. A 300 s window and one pod per minute ride out the gaps between packages.

`backlog_per_worker` rather than raw queue depth because a ratio converges and an absolute count does not ([09](09-api.md) §9.2).

`minReplicas: 2`, not 1: a single worker makes a node drain a full stall.

## 5. Local development

> **Requirement: developers can run this without Kubernetes.** The target is `git clone` to a working transfer in under five minutes.

### 5.1 The zero-setup path

**SQLite is the development default** ([03](03-persistence.md) §2), so nothing needs installing:

```bash
git clone … && cd softwareGateway
task dev:registry                     # local OCI registry with a seeded test package
task dev:coordinator                  # SQLite at ./dev/swgw.db
task dev:worker                       # second terminal
go run ./cmd/transferctl health --endpoint http://localhost:8080
```

The identical configuration loader reads `./dev/products/` and `./dev/secrets/` as plain directories ([02](02-configuration.md) §9) - no cluster, no ConfigMaps, no mocking of client-go. That is the payoff for choosing volume mounts over the Kubernetes API ([02](02-configuration.md) §3), and it is a large one for developer experience.

### 5.2 With PostgreSQL

When Postgres-specific behaviour matters - `SKIP LOCKED`, partitioning, advisory locks - which is any change to [04](04-queue-and-scheduling.md):

```bash
docker compose up -d postgres
SWGW_DATABASE_DRIVER=postgres \
SWGW_DATABASE_DSN='postgres://swgw:swgw@localhost:5432/swgw?sslmode=disable' \
  go run ./cmd/coordinator
```

```yaml
# docker-compose.yaml
services:
  postgres:
    image: postgres:16-alpine
    environment: {POSTGRES_USER: swgw, POSTGRES_PASSWORD: swgw, POSTGRES_DB: swgw}
    ports: ["5432:5432"]
    healthcheck: {test: ["CMD-SHELL","pg_isready -U swgw"], interval: 5s}
  registry:
    image: registry:2                    # local OCI registry, transfer source/target
    ports: ["5000:5000"]
  registry-dest:
    image: registry:2
    ports: ["5001:5000"]
```

Two registries so a real end-to-end transfer can be exercised locally.

### 5.3 Tasks

The task runner is [Task](https://taskfile.dev) (`Taskfile.yml`), not make. `task` alone lists everything.

| Task | Does |
|---|---|
| `task build` | All three binaries into `bin/` |
| `task build:all` | Cross-compile linux/darwin/windows × amd64/arm64 into `dist/` |
| `task test` | Unit tests with `-race`, in-process registry, **no Docker** |
| `task test:short` | The same without the race detector |
| `task test:pkg -- <pkg>` | One package |
| `task test:integration` | Postgres + registries via testcontainers |
| `task lint` | `golangci-lint` (v2) |
| `task check` | fmt, vet, lint, test |
| `task ci` | Exactly what the pipeline runs |
| `task dev:coordinator` / `dev:worker` | Run against SQLite |
| `task dev:registry` | Local registry seeded with a multi-arch test package |
| `task validate` | Validate `./dev/products` |

> **Decision - Task over make.**
>
> *Alternative:* keep the Makefile.
>
> *Rejected because* it needed `bash` and `find`, so **PowerShell and cmd could not run it at all** - a Windows developer had to install Git Bash or WSL before their first build, and the "cross-platform" claim was never tested. Task ships its own POSIX shell interpreter (`mvdan/sh`), so one definition runs identically on all three platforms; CI now includes a `windows-latest` job that proves it on every commit.
>
> *It also removed a class of bug.* `go build -o <name>` does not append `.exe` on Windows, which is exactly how binaries shipped unrunnable and had to be renamed by hand. The suffix is now derived from the target platform, and CI asserts it.
>
> *What would change our mind:* nothing likely. The one real cost is a tool to install, and it is a single `go install` - cheaper than the Git Bash prerequisite it replaced.

**`task test` must not require Docker.** Unit tests run against an in-process OCI registry ([06](06-registry-abstraction.md) §8) and SQLite. A test suite that needs containers is a test suite developers run less often, and the difference compounds.

**`CGO_ENABLED=0` is set on build tasks only, never globally.** Shipped binaries are static - SQLite is pure Go - but `go test -race` requires cgo, so a global setting would silently break the entire suite.

## 6. Container images

Distroless, non-root, multi-stage, static:

```dockerfile
FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/coordinator ./cmd/coordinator

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/coordinator /coordinator
USER nonroot:nonroot
ENTRYPOINT ["/coordinator"]
```

`CGO_ENABLED=0` for a static binary on distroless. **This is the one place the SQLite choice has a real cost**: `mattn/go-sqlite3` requires cgo. Resolved by using a pure-Go SQLite driver (`modernc.org/sqlite`) so both dialects build statically - SQLite is a development convenience and must not compromise the production image ([16](16-technology-choices.md)).

`build_info` labels ([12](12-observability-and-audit.md) §2.7) come from the same `VERSION`/`COMMIT` args, so a dashboard can correlate behaviour with a deployment.

## 7. Operational runbook

| Task | Command |
|---|---|
| Add a product | Add YAML to `deploy/products/`, add to the projected volume, merge. Flux applies; reload within ~60 s |
| Rotate a credential | Rotate in Vault. VSO updates the Secret; the mount refreshes; no restart |
| Change rate limits | Edit the product YAML, merge. Applies to new transfers; in-flight keep planned settings ([02](02-configuration.md) §6) |
| Scale workers manually | `kubectl scale deploy/worker --replicas=20` (HPA will reassert) |
| Pause everything | `transferctl transfers list -o name \| xargs -n1 transferctl transfers pause` |
| Investigate a stuck transfer | `transferctl transfers describe <id>` → failed jobs, error classes, workers |
| Recover after a registry outage | `transferctl transfers retry <id>` - resumes; completed jobs stay completed |
| Database restore | Restore Postgres. In-flight transfers resume from the last committed state; at worst some jobs re-run ([11](11-resiliency-and-backpressure.md) §2.4) |
| Emergency stop | `kubectl scale deploy/worker --replicas=0`. Leases expire; nothing is lost |

The last row is worth internalizing: **scaling workers to zero is a safe, complete stop.** No draining protocol, no state to flush, no corruption. Scale back up and everything resumes. That property is the clearest practical expression of the stateless-worker design.
