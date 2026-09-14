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

The four things a real deployment changes:

```yaml
access:
  web:      {host: 10.20.30.40, port: 80}   # what a browser types for the UI
  identity: {host: 10.20.30.41, port: 80}   # where it is redirected to sign in
image:    {registry: registry.example.internal}
images:   {mirror: registry.example.internal/docker}
imagePullSecrets: [registry-pull]
```

### The two addresses

`access.web.host` is **what somebody types to reach Software Gateway**.
`access.identity.host` is **where their browser is sent to sign in** — ZITADEL.
They are two origins, not one host with two paths, for two reasons that both
bite late:

- ZITADEL stamps `access.identity` into every token's `iss`, and the coordinator
  refuses a token whose issuer is not the one it was configured with.
- ZITADEL answers 404 to a Host header it does not recognise as its own.

So both must be the address a browser **really** reaches, character for
character. A hostname, an IP, with or without a port — no DNS is required. Two
IPs, or one IP on two ports, are both fine.

`access.scheme` is what the BROWSER speaks to whatever is in front. Leave it
`http` unless something really terminates TLS: claiming `https` over a plain
HTTP entry point is a stack that comes up green and refuses every sign-in, and
the error names ZITADEL three services away from the line that caused it.

### Turning features on

Each is one flag, and each is the only thing that adds a Secret:

| flag | default | what turning it on needs |
|---|---|---|
| `identity.sso.enabled` | off | an app registration, and one Secret for its client secret — [entra-app-registration.md](entra-app-registration.md) |
| `access.expose.ingress.tls.enabled` | off | two certificate Secrets. Not needed when something in front terminates TLS |
| `database.cluster.backup.enabled` | off | object storage and its credentials |
| `networkPolicy.enabled` | off | nothing |
| `metrics.serviceMonitor.enabled` | off | the Prometheus operator |
| `identity.enabled` | **on** | off points at an identity provider you already run |
| Flux notifications | off | uncomment one line in `clusters/<cluster>/0-sources/kustomization.yaml`, and a Secret holding the webhook |

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

For a private registry and nothing else turned on, that is **one Secret**.
Export the values first so nothing lands in shell history, then paste:

```sh
export REGISTRY_HOST=registry.example.internal
export REGISTRY_USERNAME=... REGISTRY_TOKEN=...

kubectl create namespace swgw-lab

kubectl -n swgw-lab create secret docker-registry registry-pull \
  --docker-server="$REGISTRY_HOST" \
  --docker-username="$REGISTRY_USERNAME" \
  --docker-password="$REGISTRY_TOKEN"
```

Turning a feature on adds to that list, and `task flux:secrets` shows it the
moment the values file says so. It also prints the credentials declared in
`config/secrets/secrets.yaml` that no product references yet, separately — those
are a shape to copy when a product stops being anonymous, and a missing one
takes **that product** out of service rather than the deployment.
[`config/secrets/README.md`](../config/secrets/README.md) walks through adding
one end to end.

### Two credentials you do not create

**ZITADEL's master key and root password.** The chart generates them on first
install and reads them back from the cluster on every upgrade, so they never
rotate and there is nothing to type. Read one at any time:

```sh
kubectl -n swgw-lab get secret swgw-software-gateway-identity \
  -o jsonpath='{.data.masterkey}' | base64 -d
```

**Keep a copy of that master key somewhere deleting the namespace cannot
reach.** ZITADEL encrypts its own database with it, so a database restored into
a fresh namespace without it is bytes nothing can read. The Secret carries
`helm.sh/resource-policy: keep`, so `helm uninstall` leaves it — but deleting
the namespace takes the key and the database together.

Point `identity.masterkey.existingSecret` at a Secret of your own to manage it
yourself instead.

**The database password.** CloudNativePG generates it and publishes the
connection as `<cluster>-app`. It is in no values file, no commit and nobody's
password manager, and nothing rotates it by hand.

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
| `swgw-software-gateway-identity` | the chart, on first install | ZITADEL's master key and root password, generated once and read back on every upgrade |

The `state` Secret is the cluster's replacement for the shared volumes
`docker-compose.yml` uses. The seeder's ServiceAccount can `get` and `patch`
exactly that one Secret by name, and nothing long-running uses that account.

---

## Behind your own reverse proxy

The arrangement most internal labs end up with: one nginx, one private IP, a
self-signed certificate, and no DNS anywhere.

Set `access.expose.type: none` — the chart then publishes nothing and every
Service stays ClusterIP. Your nginx reaches `web:80` and `zitadel-proxy:80` by
name, and its own Service holds the address. `access.web` and `access.identity`
are what the browser types, so two ports on one IP works:

```yaml
access:
  scheme: https                         # what the BROWSER speaks to nginx
  web:      {host: 10.20.30.40, port: 443}
  identity: {host: 10.20.30.40, port: 8443}
  expose:   {type: none}
```

ZITADEL keeps serving plain HTTP behind nginx; that is a different question from
the scheme above and does not change the issuer.

Three things the proxy must get right, each of which fails late:

- **`proxy_set_header Host $http_host`**, not `$host`. It preserves the port,
  and ZITADEL matches on the whole thing.
- **`proxy_set_header X-Forwarded-Proto https`**, or the sign-in screens build
  `http://` links behind an `https://` front door.
- **`proxy_buffer_size 32k`** on the identity server. A privileged user's token
  carries org-wide roles and does not fit the default header buffers; without it
  the sign-in returns a 400 the application never sees.

A self-signed certificate is fine. A browser warns once per **origin**, and
these are two, so a first sign-in accepts the warning twice. Nothing inside the
cluster sees that certificate — the coordinator fetches ZITADEL's keys over
plain HTTP at `zitadel:8080`.

The whole thing, including the nginx config:
[`deploy/examples/reverse-proxy-no-dns.yaml`](../deploy/examples/reverse-proxy-no-dns.yaml).

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
