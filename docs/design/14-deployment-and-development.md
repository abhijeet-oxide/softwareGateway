# 14 - Deployment and Development

> **Prerequisites:** [02 - Configuration](02-configuration.md), [09 - API](09-api.md) §9

---

## 1. Repository layout for deployment

> **This section described a Kustomize base with per-environment overlays. It
> was not built that way, and [30 - Continuous delivery](30-continuous-delivery.md)
> is the design that was.** The reason is recorded here rather than deleted,
> because the alternative is worth knowing about.
>
> Overlays would have meant `deploy/products/` as a sibling of `base/` - a
> second copy of what `config/products` already holds - and a `config/system-config.yaml`
> beside the one in `config/`. Two descriptions of one deployment, which is the
> arrangement [27 - Configuration as data](27-configuration-as-data.md) was
> written to end. A chart takes `config/` whole, so there is one.

```
deploy/
├── charts/software-gateway/     the chart: every workload, and config/ copied
│   ├── templates/               in at package time by deploy/chartstage
│   └── files/                   staged, never committed
├── environments/
│   ├── lab/helmrelease.yaml     WHAT IS DEPLOYED. One line per environment.
│   └── prod/helmrelease.yaml
├── flux/clusters/{lab,prod}/    how a cluster finds the two above
├── build/                       the Dockerfiles
└── zitadel/ cerbos/ web/ postgres/
                                 the machinery the chart carries in
```

Products are **not** a sibling of the chart and not an overlay. They are
`config/products`, they are read by `task run` and `docker compose` unchanged,
and the chart carries them - so a platform team owning the templates and a
product owner adding a product still change different files, which was the
property overlays were meant to buy.

## 2. Flux

Two objects per cluster: a `GitRepository` on the environment's branch and a
`Kustomization` pointing at `deploy/environments/<env>`, which holds one
`HelmRelease`.

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: software-gateway, namespace: flux-system}
spec:
  interval: 5m
  path: ./deploy/environments/prod
  prune: true
  wait: true
  sourceRef: {kind: GitRepository, name: software-gateway}
  healthChecks:
    - {apiVersion: helm.toolkit.fluxcd.io/v2, kind: HelmRelease, name: software-gateway, namespace: swgw}
```

The **branch is the environment** - `lab` and `main` - so promotion is the pull
request the team already reviews rather than a second mechanism.

The HelmRelease carries `upgrade.remediation.strategy: rollback`, which is what
replaces the health-checked rollback an overlay would have needed a person for:
`maxUnavailable: 0` means a bad image stalls a rollout instead of taking
capacity away, the timeout fires, and the previous release goes back with
nothing having gone down.

Secrets are **not** in Git. VSO or the Azure Key Vault CSI driver materializes
them from `config/secrets/secrets.yaml` into the namespace; manifests reference
them by name ([02](02-configuration.md) §5.5). Rotation propagates through the
mounted volume with no restart ([02](02-configuration.md) §3).

## 3. Workloads

> **The chart is what actually runs; the manifests below are the reasoning.**
> Every decision in this section is implemented in
> `deploy/charts/software-gateway/templates`, and where a detail differs the
> chart is right - it is rendered, schema-validated and installed on every pull
> request, and this page is not. Read it for *why* a liveness probe touches
> nothing external and why the worker has no writable volume; read the
> templates for what the field is called.

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

> **This section recommended external managed PostgreSQL - Cloud SQL, RDS, Azure Database - on the grounds that backups, failover, patching and PITR are solved problems we should not re-solve. That recommendation was not taken, and [30 - Continuous delivery](30-continuous-delivery.md) §7.1 is what replaced it.**

**PostgreSQL runs in-cluster, in every environment, and there is no managed-database path in this repository.** The reasoning above was sound about the problem and wrong about the only way to solve it: CloudNativePG solves the same four things in-cluster, declaratively, and it is the operator's job to keep solving them rather than ours.

What is deployed is a `Cluster` in `deploy/environments/<env>/database` - three instances with synchronous replication in production, two in lab, automatic failover in seconds, and rolling minor-version upgrades. It is **not** part of the application chart, for three reasons set out in [30](30-continuous-delivery.md) §5.3: `helm rollback` must not be able to reach it, it is upgraded on its own schedule, and its existing first is what lets the ZITADEL migration be a Helm pre-install hook.

The one thing the managed option would still have given for free is backups, and that is honestly an open gap: `spec.backup` ships unset, with the shape of the answer in a comment and the target unchosen. A replicated cluster is not a backup - it replicates a `DROP TABLE` faithfully and immediately.

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

The identical configuration loader reads `./config/products/` and `./config/secrets/local/` as plain directories ([02](02-configuration.md) §9) - no cluster, no ConfigMaps, no mocking of client-go. That is the payoff for choosing volume mounts over the Kubernetes API ([02](02-configuration.md) §3), and it is a large one for developer experience.

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
| `task build` | Coordinator, worker, and the web production bundle |
| `task build:backend` | Coordinator and worker into `bin/` |
| `task build:frontend` | Web production bundle into `web/dist/` |
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
| `task validate` | Validate `./config/products` |
| `task chart:stage` | Copy `config/` into the chart, so Helm can package it |
| `task chart:lint` / `chart:template -- prod` | Lint, and render as an environment really deploys |
| `task chart:package -- 1.4.3` | The archive CD publishes |
| `task secrets:scaffold` | Write `config/secrets/local` from the inventory |
| `task release:version` | What a release from this commit would publish |

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

### 6.1 Build credentials, and why a default secret file must be zero bytes

Registry credentials reach a build as a **mounted secret**, never as a build
argument: a build argument is recorded in the build request, printed in full in
any error the build reports, and kept in the build cache. The mount exists only
for the lifetime of one `RUN` and touches no layer.

That is correct, and under podman it has a consequence that has to be written
down, because the symptom names none of the parts involved:

```
archive/tar: write too long
Error: Post "http://d/v5.5.2/libpod/build?...&secrets=["id=netrc,src=podman-build-secret311835887"]": io: read/write on closed pipe
```

**podman's remote client cannot pass a secret to the server over the wire.**
`pkg/bindings/images/build.go` does `os.CreateTemp(options.ContextDirectory,
"podman-build-secret")`, copies the secret into it, and keeps the handle open
(`defer tmpSecretFile.Close()` fires when the whole build returns): every build
secret is written INTO THE BUILD CONTEXT and shipped inside the context tar.
That is every Windows and macOS host, because those run podman machine and
therefore always build remotely.

`nTar` then takes each file's size from the directory walk, writes a header
carrying it, and only then copies the file. On Windows the walk's size comes
from the directory entry, and a directory entry is not updated while a handle
holds unflushed writes - so the header promises zero and the copy delivers the
whole secret. Reproduced with the same three calls:

```
walker stats podman-build-secret-2446760542 at 0 bytes
owner finishes writing 218 bytes
walker copied 0 bytes -> archive/tar: write too long
```

**A zero byte secret is immune**, because zero is what the header promised.
That is the whole rule, and it is why `deploy/npm/npmrc.default` and
`deploy/go/netrc.default` are zero bytes and `deploy/deploy_test.go` fails if
either grows. Both previously carried a comment reading "Intentionally empty"
while being 423 and 215 bytes; on a Windows checkout the netrc one is 218 bytes
after line-ending conversion, and it broke every build. A file that documents
its own emptiness by not being empty is exactly the kind of thing no reviewer
looks at twice, which is why the assertion is a test rather than a comment.

Upstream: containers/podman#26914 ("empty secret files work, any non-empty file
causes the build to fail"), #17899, #23815. The bug is open, so a CREDENTIALED
secret still fails under podman on Windows; `deploy/STACK.md` lists what to do
about that.

Three things that look like fixes and are not:

- **Building serially** (`--parallel 1`). The failure needs no concurrency at
  all: a single build fails on its own. Two services sharing a build context
  and a secret looked like a race, and it is not one.
- **A smaller build context.** Size decides how long the walk takes. It has no
  bearing on whether one file's header disagrees with its contents.
- **Ignoring `podman-build-secret*`.** podman delivers the secret to the server
  as part of the context, so excluding it means the build cannot find its
  secret at all (containers/podman#25314).

Docker is unaffected: buildx streams secrets to the builder over its session
rather than through the context, so nothing is written into the directory being
tarred.

## 7. Operational runbook

| Task | Command |
|---|---|
| Add a product | Add YAML to `config/products/`, add to the projected volume, merge. Flux applies; reload within ~60 s |
| Rotate a credential | Rotate in Vault. VSO updates the Secret; the mount refreshes; no restart |
| Change rate limits | Edit the product YAML, merge. Applies to new transfers; in-flight keep planned settings ([02](02-configuration.md) §6) |
| Scale workers manually | `kubectl scale deploy/worker --replicas=20` (HPA will reassert) |
| Pause everything | `transferctl transfers list -o name \| xargs -n1 transferctl transfers pause` |
| Investigate a stuck transfer | `transferctl transfers describe <id>` → failed jobs, error classes, workers |
| Recover after a registry outage | `transferctl transfers retry <id>` - resumes; completed jobs stay completed |
| Database restore | Restore Postgres. In-flight transfers resume from the last committed state; at worst some jobs re-run ([11](11-resiliency-and-backpressure.md) §2.4) |
| Emergency stop | `kubectl scale deploy/worker --replicas=0`. Leases expire; nothing is lost |

The last row is worth internalizing: **scaling workers to zero is a safe, complete stop.** No draining protocol, no state to flush, no corruption. Scale back up and everything resumes. That property is the clearest practical expression of the stateless-worker design.
