# Deploying Software Gateway

Three ways to run this, and they are the same product configured three times
over. Which one you want depends on what you are doing, not on what environment
you are in.

| | what it is | what configures it | who it is for |
|---|---|---|---|
| **`task run`** | the binaries on your machine, against SQLite and no identity provider | `config/` and `.env` | writing code |
| **`docker compose up`** | the whole stack on your machine: PostgreSQL, ZITADEL, Cerbos, the three services | `config/` and `.env` | checking a change against real sign-in |
| **Helm, reconciled by Flux** | a cluster — lab, non-production, production, AKS or OpenShift | `config/` and **one values file** | everything that serves other people |

All three read the same `config/` directory — the products, the people, the
roles, the Cerbos policies and the credential inventory. That is the thing that
makes them one product rather than three: a product added in `config/products`
is added everywhere, and nothing about it is restated per deployment.

What differs between them is only how the process is started and where it finds
its credentials. Local runs answer that with `.env`; a cluster answers it with
one values file.

## `task run` — writing code

```sh
task run
```

SQLite, authentication off, everything in one process tree. `.env.example`
documents every variable; `docs/DEVELOPER-GUIDE.md` is the whole story.

## `docker compose up` — the full stack locally

```sh
docker compose up -d
# http://localhost:8000
```

PostgreSQL, ZITADEL and its sign-in screens, Cerbos, the coordinator, the workers
and the web tier — seeded and authenticated, with no configuration. This is where
a change to sign-in, to roles, or to the seeder is checked, because it is the
same seeder and the same ZITADEL a cluster runs.

[`QUICKSTART.md`](../QUICKSTART.md) covers the variables, adding people and
products, running under Podman, and getting through a corporate proxy.

## A cluster — one values file

```
deploy/flux/instances/<instance>/values/values.yaml
```

That file is the whole of what a deployment differs in. A `configMapGenerator`
turns it into a ConfigMap, and both HelmReleases — the database layer and the
application layer — read that one ConfigMap. There is no second place to look.

Start from the closest [example](../deploy/examples/README.md), then:

- [`deploy/flux/README.md`](../deploy/flux/README.md) — the layout, bootstrapping
  a cluster, and what to do when a release goes wrong
- [`deploy/charts/software-gateway/README.md`](../deploy/charts/software-gateway/README.md)
  — what the chart takes and what it refuses
- [`docs/deployment-troubleshooting.md`](deployment-troubleshooting.md) — the
  failures this has actually hit in a cluster, and what each one really was

### Without Flux

The chart is an ordinary Helm chart. Install the two layers in order:

```sh
task chart:stage
helm install swgw-db deploy/charts/software-gateway --namespace swgw --create-namespace \
  --values my-values.yaml --set layers.database=true --set layers.application=false
helm install swgw    deploy/charts/software-gateway --namespace swgw \
  --values my-values.yaml
```

The same one values file, twice. Point `database.cluster.name: ""` at a database
somebody else runs and the first command has nothing to do.

## Why the database is its own release

It is the only thing in this deployment that cannot be recreated, so three things
have to be true of it and none of them can be true if it is part of the
application release:

- **`helm rollback` must not be able to reach it.** Rolling an application back a
  version is routine; rolling a database back is data loss.
- **`helm uninstall` must not be able to delete it.** The `Cluster` also carries
  `helm.sh/resource-policy: keep`.
- **It must exist before the application installs.** That is what lets the
  ZITADEL migration be a Helm pre-install hook: Helm waits for the hook, so no
  pod is ever created against an unmigrated schema. A chart that deployed its own
  database in the same release would deadlock — the hook would wait for a
  PostgreSQL that Helm had not created yet.

Ordering continues below Helm. Every workload has a `wait-for-<dependency>` init
container, so a pod whose dependency is not ready sits in `Init` and logs what it
is waiting for, rather than crash-looping. A restart count above zero in this
deployment means something actually went wrong.

## What a change costs

| change | cost |
|---|---|
| a product, a Cerbos policy | nothing restarts — the coordinator, the workers and Cerbos watch their directories |
| a person, a role grant | one Job runs for a few seconds; no pod is touched |
| `config.yaml`, a new image | a rolling update with `maxUnavailable: 0` |

A replica is removed only after its replacement reports ready, and readiness on
the coordinator means the database answers and the schema matches the build. So a
bad image never takes capacity away: the rollout stalls with the old pods serving
until Flux's remediation rolls it back.
