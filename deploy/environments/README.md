# environments/ - what is deployed, where, and at which version

Two layers per environment, applied in that order:

```
<env>/
  namespace.yaml            the namespace, with Pod Security labels
  layers.yaml               TWO Flux Kustomizations, and the order between them
  database/cluster.yaml     LAYER 1 - the CloudNativePG Cluster
  platform/helmrelease.yaml LAYER 2 - everything else, as one Helm release
```

`lab/` deploys from branch `lab` into `swgw-lab`; `prod/` from `main` into
`swgw`.

## The order, and why it is real

`layers.yaml` declares `software-gateway-<env>-platform` with
`dependsOn: [software-gateway-<env>-database]`, and both carry `wait: true`.

That is not "applied in sequence" - it is docker-compose's `depends_on`. Flux
holds the platform layer entirely until the database layer reports **Ready**,
which for a CloudNativePG Cluster means initdb finished, the instances joined,
and a primary was elected. Only then is the HelmRelease applied at all.

Two things follow from the database being first, and they are the reason it is
not in the chart:

- **`helm rollback` cannot reach it.** Rolling an application back a version is
  routine; rolling a database back is data loss.
- **The ZITADEL migration can be a Helm pre-install hook.** Helm waits for a
  hook before creating any pod, so no pod is ever created against an unmigrated
  schema. A chart that also deployed its own database could not do this - the
  hook would wait for a Postgres Helm had not created yet.

`git log -p deploy/environments/prod/platform/helmrelease.yaml` is the
production deployment history. Every line of it was written either by a person in a pull
request or by the release pipeline moving `spec.chart.spec.version`, and there
is nothing else to correlate.

## Why the values are inline

They could be a ConfigMap built by `configMapGenerator`, and that is the
arrangement most Flux examples show. Inline is better here for one reason: a
change to an inline value changes the HelmRelease object, so Flux reconciles it
**immediately**. A change to a referenced ConfigMap is picked up on the
HelmRelease's own interval, which means a value edited at 09:00 takes effect at
some point before 09:10 and the person who edited it cannot tell whether it
has. That is a bad property for the file that says which database production
talks to.

The cost is that a value shared by both environments is written twice. That is
a handful of lines, and it buys a file somebody can read top to bottom and know
what an environment is.

## What is NOT here

**The database's password.** CloudNativePG generates it and publishes the
connection as `swgw-db-app`. It is in no values file, no commit and nobody's
password manager, and there is nothing to rotate by hand.

**Secrets.** Not one value. Everything credential-shaped is a reference to a
Secret produced in-cluster from `config/secrets/secrets.yaml` by the Vault
Secrets Operator or the Azure Key Vault CSI driver - see the chart's
`secrets.backend`. What lives here is the NAME of a Secret, which is not a
secret.

**Products, users, roles and policies.** Those are `config/`, they are carried
inside the chart, and they are the same in both environments by construction.
An environment that needs a different product list is not an environment, it is
a different deployment - and it gets its own directory here rather than a fork
of the shared one.

## Changing one

| you want to | change |
|---|---|
| deploy a new release | nothing - the pipeline moves `version` on merge |
| pin or roll back | `platform/helmrelease.yaml`, `spec.chart.spec.version` |
| stop automatic deploys | `spec.suspend: true` on the HelmRelease |
| scale, resize, retarget | the value under `spec.values` |
| resize the database, change its replica count | `database/cluster.yaml` |

A rollback is a pull request that sets `version` back. It is reviewed, it is in
the history, and it takes the same path as a deploy - which is what makes it
something anybody on the team can do at 02:00 rather than a `helm rollback`
that leaves the cluster disagreeing with Git.
