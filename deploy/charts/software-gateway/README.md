# software-gateway

The whole stack: the coordinator, the worker fleet, the web tier, the policy
decision point, and the identity provider with its sign-in screens and front
door. It is the same software `docker compose up` runs, reading the same
`config/` directory, with the same file at the same paths.

## This chart does not deploy a database

Deliberately, and it is the first thing to know about installing it. Three
things follow from the database being applied first, by something else:

- **`helm rollback` cannot reach it.** Rolling an application back a version is
  routine; rolling a database back is data loss.
- **The ZITADEL migration is a Helm pre-install hook.** Helm waits for a hook
  before creating any pod, so no pod is ever created against an unmigrated
  schema. A chart that deployed its own database could not do that - the hook
  would wait for a Postgres Helm had not created yet.
- **It is upgraded on its own schedule**, by somebody who meant to.

Create one first. CloudNativePG is what the GitOps path uses, and this is the
whole of it:

```sh
kubectl apply -n swgw -f - <<'YAML'
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: swgw-db}
spec:
  instances: 3
  storage: {size: 100Gi}
  walStorage: {size: 20Gi}
  bootstrap:
    initdb:
      database: swgw
      owner: swgw
      postInitSQL: ["CREATE DATABASE zitadel OWNER swgw"]
YAML
```

The operator generates the owner's password and publishes the connection as
`swgw-db-app`, which is the Secret this chart reads. It is in no values file and
no commit, and there is nothing to rotate by hand.

## The shortest install that works

Two values have no safe default and the chart refuses to render without them: a
ZITADEL masterkey nobody kept is a database nobody can decrypt.

```sh
helm install swgw oci://artifactory.internal.example.com/swgw/software-gateway \
  --version 1.4.3 --namespace swgw --create-namespace \
  --set identity.masterkey.value="$(openssl rand -hex 16)" \
  --set identity.rootPassword.value="$(openssl rand -base64 18)" \
  --set identity.bootstrapAdmin.password='Passw0rd!'
```

`identity.masterkey.value` must be **exactly 32 bytes** - `openssl rand -hex 16`
produces 32 characters. ZITADEL refuses to start otherwise and says so from
inside a migration failure, so the symptom names neither the setting nor its
length. The chart checks it before rendering.

That gives a working stack on a `ClusterIP`, with the products in `config/` and
the people in `config/users/users.yaml`. Reach it with `kubectl port-forward
svc/web 8000:80` and set `externalUrls` when it gets a real address.

## The values a real deployment states

```yaml
externalUrls:
  # NOT decoration. externalUrls.identity is stamped into every token's `iss`
  # and is what ZITADEL matches the Host header against; externalUrls.web is
  # what the seeder registers as the SPA's OIDC redirect URI. The chart refuses
  # to render if either disagrees with the ingress host below.
  web: https://swgw.example.com
  identity: https://id-swgw.example.com

ingress:
  enabled: true
  className: nginx
  app:      {host: swgw.example.com,    tls: {enabled: true, secretName: swgw-tls}}
  identity: {host: id-swgw.example.com, tls: {enabled: true, secretName: id-tls}}

image:
  registry: artifactory.internal.example.com
  repository: swgw

# The database, which this chart does not deploy. CloudNativePG publishes the
# connection as `<cluster>-app`; `host` is its read-write endpoint, which
# follows the primary through a failover.
database:
  existingSecret: swgw-db-app
  host: swgw-db-rw

secrets:
  backend: vault                      # or `azure`, or `none`
  vault:
    authRef: vault-auth
    mount: kv
    pathPrefix: softwaregateway/prod

identity:
  masterkey:    {existingSecret: swgw-zitadel, key: masterkey}
  rootPassword: {existingSecret: swgw-zitadel, key: rootPassword}
  sso:
    issuer: https://login.microsoftonline.com/<tenant>/v2.0
    clientId: <application id>
    existingSecret: swgw-sso          # the client secret is never a value here
```

`helm show values` prints the rest, with the reason for each beside it.

## What it deploys

| | | |
|---|---|---|
| `coordinator` | the API and the control plane | Deployment, Service `controller`, PDB |
| `worker` | the data plane | Deployment, headless Service, optional HPA |
| `web` | the SPA, and the API on the same origin | Deployment, Service `web` |
| `cerbos` | the policy decision point | Deployment, Service `cerbos` |
| `zitadel` + `zitadel-login` + `zitadel-proxy` | the identity provider, its sign-in screens, and the one origin they share | three Deployments, plus a migration hook |
| the seeding Job | people, roles, projects, grants | a Job, when identity input changes |

## Nothing crash-loops

Every workload has a `wait-for-<dependency>` init container. A pod that is not
yet able to start sits in `Init:0/1` and logs what it is waiting for:

```
$ kubectl -n swgw logs swgw-coordinator-xxx -c wait-for-database
waiting for database at swgw-db-rw:5432
```

Its application container has not run, so **a restart count above zero in this
release means something actually went wrong**. That is the point of it: a pod in
CrashLoopBackOff looks the same whether it is waiting for its database or is
genuinely broken, and treating that as normal during a deployment trains
everybody to ignore the one state that should never be ignored.

The waits are soft where the container is useful without its dependency - the
web tier renders the page that explains an outage, so it must come up during
one - and hard where it is not.

**Service names are not release-prefixed**, so **one release per namespace**.
`deploy/web/nginx.conf` proxies to `controller:8080` and
`deploy/zitadel/nginx.conf` to `zitadel:8080`; those files are baked into images
and mounted by `docker-compose.yml`, and prefixing here would fork them.

Set `identity.enabled: false` to use an identity provider you already run: the
three ZITADEL workloads and the seeding Job are not deployed, and
`externalUrls.identity` points at yours.

## What a change costs

| you change | what happens | restarts |
|---|---|---|
| a product, a Cerbos policy | the watchers reload in place | **none** |
| `config/users/users.yaml` | one Job runs for ten seconds | **none** |
| a credential in the vault | the operator rewrites the Secret, the kubelet updates the volume | **none** |
| `config/config.yaml` | a checksum annotation changes | rolling |
| the code | a new image tag | rolling |

A rolling update here uses `maxUnavailable: 0`: a replica is removed only after
its replacement is **ready**, and ready on the coordinator means the database
answers, its schema matches this build and the products loaded. A bad image
therefore stalls the rollout with the old pods serving rather than taking
capacity away.

**Migrations must be backwards compatible with the previous release.** Both
binaries run against one database during every rollout. Add a column, do not
rename one; drop only in a later release.

ZITADEL's own migration is different and is handled: it runs as a pre-install
hook that Helm waits for, so its new schema is in place before any new ZITADEL
pod is created.

## Credentials

`config/secrets/secrets.yaml` - carried inside this chart - lists what the
deployment needs: a name, its keys, and a relative path. No values.
`secrets.backend` chooses the operator that turns each entry into a Kubernetes
Secret, and **the Secret is identical either way**, so nothing above that
setting knows which was used.

```
vault   VaultStaticSecret   - Vault Secrets Operator. Produced whether or not a
                              pod runs; refreshed on a timer. Recommended.
azure   SecretProviderClass - Azure Key Vault CSI driver. Produced only while a
                              pod mounts the class, which the coordinator does.
none    the Secrets already exist in the namespace.
```

The operator itself is a cluster prerequisite and is not installed here - a
chart that installs its own secrets operator is a chart that has to hold a Vault
token. See `deploy/flux/README.md`.

## Reading a deployment

```sh
kubectl -n swgw rollout status deploy/swgw-software-gateway-coordinator
kubectl -n swgw logs -l app.kubernetes.io/component=seed --tail=-1
kubectl -n swgw get pods -L app.kubernetes.io/version
```

The seeding Job's log is the one to read first. It prints what it provisioned -
the people, their grants - and names every account that **cannot** sign in and
why, which is the question somebody is usually asking.

## Installing it from a git checkout

`config/` lives outside the chart, and Helm cannot package files outside a chart
directory, so a checkout needs one step first:

```sh
task chart:stage
helm install swgw deploy/charts/software-gateway --values my-values.yaml
```

(And create the database first - see the top of this file.)

A chart pulled from the registry is already staged and needs nothing.

## Where the rest is written down

- `docs/design/30-continuous-delivery.md` - why it is built this way
- `deploy/flux/README.md` - bootstrapping a cluster
- `deploy/environments/README.md` - what is deployed, where
- `config/README.md` - the directory an administrator manages
- `deploy/STACK.md` - the compose stack, and the Entra registration
