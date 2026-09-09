# 27. config/ - one directory, two deployment paths

## 1. What was wrong

Four inputs decided what a deployment was, and each arrived a different way:

| input | where it lived | how it was applied |
|---|---|---|
| products | `deploy/products/*.yaml` | mounted, watched, hot-reloaded |
| the product list for access | `GATEWAY_PRODUCTS` in `.env` | an environment variable |
| roles | `GATEWAY_ORG_ROLES`, `GATEWAY_PRODUCT_ROLES` | two more |
| people | `deploy/zitadel/users.json` | a JSON file, mounted separately |
| permissions | `deploy/cerbos/policies/*.yaml` | a third mount |
| registry credentials | nowhere | not mounted at all |

Three formats, five places, and no relationship between them that anything
checked. The consequences were not theoretical:

- a product could be replicated with no ZITADEL project, so nobody could be
  granted access to it, and nothing said so;
- a product could have a project and no owner, so nobody could approve its
  downloads, and nothing said so;
- roles were named in `.env` and the policies deciding what they may do were in
  a directory two levels away, in a different format;
- `credentialsRef` in a product document resolved to `/etc/softwaregateway/
  secrets/<name>/`, which `docker-compose.yml` never mounted, so a product with
  real credentials could not work locally at all.

And none of it converged with the cluster. Flux would reconcile some of these as
files and the rest would be environment on a Deployment, which is a second
format for the same decisions.

## 2. The split that replaced it

**`config/` is content. `deploy/` is machinery.**

Content changes without a release: which products exist, who may use them, what
a role means, what a credential is. Machinery changes with the code:
Dockerfiles, nginx configuration, the seeder, the database's init script.

```
config/
  access/
    roles.yaml          the roles that exist: tenant-wide, and per product
    policies/           Cerbos policies - what each role may actually do
  users/
    users.yaml          who may sign in, and their role on each product
  products/
    *.yaml              what this gateway replicates, one file per product
  secrets/
    manifests/          VSO / ExternalSecret documents. Committed, no values.
    local/              the same layout filled in by hand. Never committed.
```

One directory, and the two deployment paths differ only in how it arrives:

| | local | cluster |
|---|---|---|
| arrives by | `docker-compose.yml` bind-mounts `${CONFIG_DIR:-./config}` | Flux reconciles it |
| applied by | `docker compose run --rm zitadel-init` | the seeding Job, or a CI step |
| read by | the same binaries, at the same paths | the same binaries, at the same paths |

No rendering step, no per-environment fork, no second format.

## 3. The rule that ties products to people

**Every product in `config/products` must have an owner in `config/users/users.yaml`.**

The owner role is named once, in `config/access/roles.yaml` as
`product.ownerRole`, so the rule is configuration rather than a constant in the
seeder.

It is checked twice, deliberately:

- **`go test ./deploy/...`**, on the pull request, which is where a product with
  no owner should be caught. The failure names the product, the file, and the
  two lines to add.
- **the seeder**, before its first write, which is the backstop for anything
  applied outside that path. It refuses the whole run rather than half-applying
  one: a deployment that is missing an owner is a deployment somebody has to fix
  anyway, and leaving projects created and grants not is worse than leaving
  nothing.

## 4. Why the seeder parses YAML by hand

`deploy/zitadel/bootstrap.mjs` runs in a plain `node:alpine` container.
Depending on `js-yaml` means an npm install at deploy time, which means a
registry reachable from the deployment, which the air-gapped estates this
product ships into do not have.

So it carries a **subset reader**: maps, lists, lists of maps, inline `[]` and
`{}`, quoted and plain scalars, comments. Not anchors, not multi-line scalars,
not multiple documents. `config/access/roles.yaml` and `config/users/users.yaml` are
written inside that subset.

Two things keep that honest. The Go test parses both files with the real
library, so a document the subset cannot read fails in CI. And **product
documents are never parsed here at all**: their schema belongs to the Go side,
which validates and hot-reloads them, and all the seeder needs is
`metadata.name` - so it takes the name and refuses the file if it cannot find
one, rather than guessing.

## 5. Secrets

`internal/product/secrets.go` resolves `credentialsRef.secretName` to
`<config dir>/secrets/<secretName>/<key>`, reading plain files. That is a
projected Kubernetes Secret volume, and it is deliberately not the Kubernetes
API: no client-go, no cluster-wide Secret read permission, no API-server load,
and the same code path works against a directory on a laptop.

`config/secrets/manifests/` holds the documents that produce those Secrets in a
cluster - VaultStaticSecret, ExternalSecret, SealedSecret. They carry a
reference to a value and never a value, which is what makes them reviewable.

`config/secrets/local/` is the same layout filled in by hand, bind-mounted by
compose at the same path, and never committed. A developer gets production's
layout without a Vault.

## 6. The worker no longer waits for any of it

A worker used to refuse to START without product configuration. The rule behind
that is sound and unchanged - a worker that cannot execute the work must not
lease it, because attempts are counted when a job is handed out, and a worker
with no configuration once leased two and a half thousand jobs and failed every
one.

But "must not lease" was implemented as "must not start", and those are
different. Refusing to start makes the data plane depend on configuration
arriving first, which inverts the order a rollout actually happens in.

So the loop gates instead (`worker.Options.CanLease`). A worker with no products
starts, reports it in readiness as DEGRADED rather than DOWN so the container
stays green, logs the reason once, leases nothing, and begins working the moment
the watcher loads a product. Measured: a worker started against an empty
directory stayed up, and a product file copied in was leasing 21 seconds later
with no restart.

## 7. A worker is named, not numbered

`os.Hostname()` is the POD name under Kubernetes and reads perfectly. Under
Docker and Podman it is the CONTAINER ID, so the fleet list read `16b81c38de5c`
and nothing on the screen said which container that was or even that it was a
worker.

`SWGW_WORKER_NAME` supplies a name and the host keeps replicas apart:
`worker-16b81c38de5c` under compose. Under Kubernetes the same variable is set
from the downward API to the pod name, which already contains the hostname, and
no suffix is added - appending the pod name to itself would be worse than the id
ever was.

## 8. Files

- [`config/README.md`](../../config/README.md) - the directory, for the person who edits it
- [`deploy/zitadel/bootstrap.mjs`](../../deploy/zitadel/bootstrap.mjs) - the reader, the checks, the seeding
- [`deploy/deploy_test.go`](../../deploy/deploy_test.go) - `TestEveryProductHasAnOwner`
- [`cmd/worker/main.go`](../../cmd/worker/main.go) - `workerName`, `CanLease`, the readiness check
- [`internal/worker/loop.go`](../../internal/worker/loop.go) - the lease gate
- [`docs/design/24-identity-and-access.md`](24-identity-and-access.md) - the identity model this feeds
