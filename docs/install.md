# Installing Software Gateway

Three ways to run it, same product, same `config/` directory:

| | what it is | configured by |
|---|---|---|
| `task run` | the binaries on your machine, SQLite, no sign-in | `config/` + `.env` |
| `docker compose up -d` | the whole stack locally, with real sign-in | `config/` + `.env` |
| **Flux** | a cluster | `config/` + **one values file** |

The first two are [`QUICKSTART.md`](../QUICKSTART.md). This page is the third,
start to finish, for a cluster that has nothing on it yet.

---

## What you need first

- `kubectl` with cluster-admin, and `flux` ([install](https://fluxcd.io/flux/installation/))
- A container registry holding this product's images and its chart, and a
  credential that can read it
- A namespace name, and the two addresses a browser will use

Nothing else. There is no credentials operator in this path: the Secrets are
created once, by hand, and nothing in Git ever holds a value.

---

## 1. Write the instance

```sh
cp -r deploy/flux/instances/lab deploy/flux/instances/myinstance
$EDITOR deploy/flux/instances/myinstance/values/values.yaml
$EDITOR deploy/flux/instances/myinstance/namespace.yaml    # the namespace name
$EDITOR deploy/flux/instances/myinstance/kustomization.yaml # the same name
```

That values file is the whole of what this deployment differs in. Everything
else is a chart default — `deploy/charts/software-gateway/values.yaml` lists
them, and restating one there is a test failure.

The five things a real deployment changes:

```yaml
access:
  web:      {host: 10.20.30.40, port: 80}   # what a browser types
  identity: {host: 10.20.30.41, port: 80}
image:    {registry: registry.example.internal}
images:   {mirror: registry.example.internal/docker}
imagePullSecrets: [registry-pull]
identity:
  masterkey:    {existingSecret: swgw-zitadel}
  rootPassword: {existingSecret: swgw-zitadel}
```

Turning on single sign-on needs one app registration in the directory:
[entra-app-registration.md](entra-app-registration.md).

`access.scheme` defaults to `http`. Leave it there unless something in front
really terminates TLS: it is stamped into every token's `iss` and the
coordinator checks it, so claiming `https` over a plain HTTP entry point is a
stack that comes up green and refuses every sign-in.

Tell the cluster about the instance:

```sh
cp deploy/flux/clusters/lab/1-instances/lab.yaml \
   deploy/flux/clusters/lab/1-instances/myinstance.yaml
$EDITOR deploy/flux/clusters/lab/1-instances/myinstance.yaml          # name, path, namespace
$EDITOR deploy/flux/clusters/lab/1-instances/kustomization.yaml       # add the file
$EDITOR deploy/flux/clusters/lab/0-sources/gitrepository.yaml         # your repository URL
$EDITOR deploy/flux/clusters/lab/0-sources/helmrepository.yaml        # your chart registry
```

Check it before pushing:

```sh
task flux:build                       # every kustomization
task chart:template -- myinstance     # both layers, exactly as Flux renders them
```

Then commit and push. Flux reads the branch named in `gitrepository.yaml`.

---

## 2. Create the Secrets

The list comes from your values file, so it cannot go stale:

```sh
task flux:secrets -- myinstance
```

It prints something like this. Export the values first so nothing lands in
shell history, then paste:

```sh
export REGISTRY_HOST=registry.example.internal
export REGISTRY_USERNAME=... REGISTRY_TOKEN=...
export SWGW_ZITADEL_MASTERKEY=$(openssl rand -hex 16)      # EXACTLY 32 characters
export SWGW_ZITADEL_ROOTPASSWORD=$(openssl rand -base64 24)

kubectl create namespace swgw-lab

kubectl -n swgw-lab create secret docker-registry registry-pull \
  --docker-server="$REGISTRY_HOST" \
  --docker-username="$REGISTRY_USERNAME" \
  --docker-password="$REGISTRY_TOKEN"

kubectl -n swgw-lab create secret generic swgw-zitadel \
  --from-literal=masterkey="$SWGW_ZITADEL_MASTERKEY" \
  --from-literal=rootPassword="$SWGW_ZITADEL_ROOTPASSWORD"
```

**Keep the master key.** ZITADEL encrypts its own database with it, and a
database whose key is gone cannot be decrypted by anything.

Every other credential comes from `config/secrets/secrets.yaml` — the list of
names and keys the products reference. `task flux:secrets` prints those too. A
missing one takes **one product** out of service and says which file it looked
for; it does not stop the deployment.

**The database password is not on this list and never will be.** CloudNativePG
generates it and publishes the connection as `<cluster>-app`, so it is in no
values file, no commit and nobody's password manager.

---

## 3. Install Flux and its two credentials

```sh
flux install --namespace flux-system

# Read access to this repository
flux create secret git software-gateway-git \
  --namespace flux-system \
  --url https://github.com/abhijeet-oxide/softwareGateway \
  --username git --password "$GIT_TOKEN"

# Pulling the chart. A different object from the kubelet's pull secret above -
# same registry, different namespace, different reader.
flux create secret oci chart-registry \
  --namespace flux-system \
  --url "$REGISTRY_HOST" \
  --username "$REGISTRY_USERNAME" --password "$REGISTRY_TOKEN"
```

---

## 4. Point the cluster at the repository

```sh
kubectl apply -k deploy/flux/clusters/lab
```

That is the last manual step. Everything after it is a pull request.

```sh
flux -n flux-system reconcile kustomization software-gateway-cluster-lab --with-source
```

---

## 5. Watch it come up

```sh
flux -n flux-system get kustomizations
flux -n swgw-lab get helmreleases
kubectl -n swgw-lab get pods -w
```

First install takes a few minutes and arrives in this order:

| | what is happening | how long |
|---|---|---|
| `platform-operators` Ready | CloudNativePG installed, CRDs registered | ~1 min |
| `swgw-db-1` Running | initdb, the `swgw` and `zitadel` databases, Secret `swgw-db-app` published | ~1 min |
| `zitadel-setup` Completed | ZITADEL's schemas and migrations, the first instance | ~2 min |
| `zitadel` Running | the identity provider serving | ~1 min |
| `seed` Completed | people, projects, role grants, the OIDC clients | ~30 s |
| everything else Ready | the coordinator, workers, web, Cerbos, the sign-in screens | ~1 min |

**A pod in `Init` is waiting, not failing.** Every workload has a
`wait-for-<dependency>` init container, so ordered startup is visible as pods
that have not begun:

```sh
kubectl -n swgw-lab logs <pod> -c wait-for-database
#   waiting for database at swgw-db-rw:5432
```

A restart count above zero means something actually went wrong.

Read the seeding Job first — it prints what it provisioned, and every account
that **cannot** sign in and why:

```sh
kubectl -n swgw-lab logs -l app.kubernetes.io/component=seed --tail=-1
```

Then open `access.web.host`.

---

## What the release creates for itself

Nothing on the list above is created twice. For reference, these appear without
anybody making them:

| | made by | holds |
|---|---|---|
| `swgw-db-app` | CloudNativePG | the database user, password and a ready-made `uri` |
| `swgw-software-gateway-state` | the setup Job, then the seeder | ZITADEL's machine token, the sign-in screens' token, the SPA's OIDC client id, the worker fleet's credentials |
| `swgw-software-gateway-identity` | the chart | only when a ZITADEL value is given inline instead of named in a Secret |

The `state` Secret is the cluster's replacement for the shared volumes
`docker-compose.yml` uses. The seeder's ServiceAccount can `get` and `patch`
exactly that one Secret by name, and nothing long-running uses that account.

---

## How the pieces reach each other

Everything internal is a Service name in one namespace. None of it is
configurable, because there is nothing to decide.

```
browser ──▶ web:80 ──▶ controller:8080 ──▶ cerbos:3592
                                       └─▶ zitadel:8080        (OIDC keys)
                                       └─▶ swgw-db-rw:5432
browser ──▶ zitadel-proxy:80 ─┬─▶ zitadel:8080
                              └─▶ zitadel-login:3000 ──▶ zitadel:8080
worker ───▶ controller:8080   and the registries it moves bytes between
```

The browser reaches exactly two of them. `/api/v1` is proxied by the web tier
rather than published, which keeps it same-origin — the coordinator ships no
CORS middleware.

---

## Changing it afterwards

| you want to | change |
|---|---|
| deploy a new release | nothing — the pipeline moves the pointer on merge |
| pin or roll back | `task flux:version -- myinstance 1.4.2`, in a pull request |
| resize, retarget, turn on SSO | `values/values.yaml` |
| add a product, a person, a policy | `config/` — nothing restarts |
| stop deploying, right now | `flux -n swgw-lab suspend helmrelease software-gateway` |

`flux suspend` is the emergency brake and not a fix: Git and the cluster now
disagree and nothing will say so later.

A values edit is applied at the next reconciliation (5 minutes), or at once:

```sh
flux -n swgw-lab reconcile helmrelease software-gateway --with-source
```

---

## When something is wrong

[`deployment-troubleshooting.md`](deployment-troubleshooting.md) — the failures
this has actually hit in a cluster, what each one really was, and what now stops
it.
