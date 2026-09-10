# software-gateway

The whole stack: the coordinator, the worker fleet, the web tier, the policy
decision point, and the identity provider with its sign-in screens and front
door. It is the same software `docker compose up` runs, reading the same
`config/` directory, with the same file at the same paths.

## The shortest install that works

Three values have no safe default and the chart refuses to render without them.
That is deliberate: a generated password that nothing stores is a database
nobody can open after the next `helm upgrade`.

```sh
helm install swgw oci://artifactory.internal.example.com/swgw/software-gateway \
  --version 1.4.3 --namespace swgw --create-namespace \
  --set postgresql.password.value="$(openssl rand -base64 24)" \
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

# Managed PostgreSQL. The bundled StatefulSet is one instance with no failover
# and no backups; this is the only stateful component in the system.
postgresql:
  enabled: false
  external:
    enabled: true
    existingSecret: swgw-postgres     # keys: dsn, host, port, user, password, sslmode, database

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
| `zitadel` + `zitadel-login` + `zitadel-proxy` | the identity provider, its sign-in screens, and the one origin they share | three Deployments, a setup Job |
| `postgres` | evaluation and lab only | StatefulSet, one PVC |
| the seeding Job | people, roles, projects, grants | a Job, when identity input changes |

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

A chart pulled from the registry is already staged and needs nothing.

## Where the rest is written down

- `docs/design/30-continuous-delivery.md` - why it is built this way
- `deploy/flux/README.md` - bootstrapping a cluster
- `deploy/environments/README.md` - what is deployed, where
- `config/README.md` - the directory an administrator manages
- `deploy/STACK.md` - the compose stack, and the Entra registration
