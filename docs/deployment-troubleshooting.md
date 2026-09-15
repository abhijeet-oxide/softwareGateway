# Cluster deployment: the failures this has actually hit

Every entry here happened in a real cluster. Each says what the symptom was, what
it really was, and what in the chart now stops it — because a fix nobody can find
again is a fix that gets undone.

## The issuer mismatch

**Symptom.** The coordinator refuses to start:

```
authentication is enabled but not usable: issuer mismatch
  configured issuer: https://id.example.internal
  reported issuer:   http://id.example.internal
```

Everything else is green. ZITADEL is running, the database is healthy, the seeder
completed.

**What it is.** The coordinator verifies tokens offline and checks that the `iss`
claim matches the issuer it was configured with. `https` and `http` are different
issuers, so it exits rather than accept tokens it cannot place.

The scheme ZITADEL reports comes from `ZITADEL_EXTERNALSECURE`, which it also
persists into the instance's domain settings **at first setup**. So changing the
scheme after the first install is not enough on its own: the stored instance
domain still says what it said the day it was created.

**What the chart does now.** Every browser-facing URL is derived from one
`access` block:

```yaml
access:
  scheme: http
  web:      {host: 10.20.30.40, port: 80}
  identity: {host: 10.20.30.41, port: 80}
```

`SWGW_AUTH_ISSUER`, `ZITADEL_EXTERNALSECURE`, `ZITADEL_EXTERNALDOMAIN`,
`ZITADEL_EXTERNALPORT`, the login screens' `X-Forwarded-Proto` and the SPA's
redirect URI all come from those four values. They cannot disagree, because there
is nothing to state twice.

**The rule that matters.** `access.scheme` is a statement about what a browser
really speaks to the entry point, not an aspiration. In a lab with no
certificates that is `http`, and saying so is what makes sign-in work. TLS
terminating at an ingress or a reverse proxy is `https` — ZITADEL still serves
plain HTTP behind it (`--tlsMode disabled`), which is a different question and
does not change the issuer.

**If an instance already stored the wrong scheme**, change `access.scheme`, then
correct the stored domain — ZITADEL's admin console, *Instance → Domains* — or
re-run the setup against an empty ZITADEL database. Nothing else clears it.

## `'Port' cannot parse value as 'uint16'`

**Symptom.** ZITADEL crash-loops at startup with a parse error naming a setting
nobody set.

**What it is.** Kubernetes injects an environment variable per Service in the
namespace: a Service named `zitadel` produces `ZITADEL_PORT=tcp://10.0.1.5:8080`.
ZITADEL reads `ZITADEL_PORT` as its own `Port` setting and rejects it.

**What the chart does now.** Every pod sets `enableServiceLinks: false`. It is
not hygiene on the ZITADEL deployment alone — the whole class of collision
between a Service name and a configuration key is removed everywhere.

## Pods in `ImagePullBackOff` from a private registry

**Symptom.** Some pods pull and some do not, or all of them fail against a
private registry.

**What it is.** `imagePullSecrets` reached some pod specs and not others — a Job,
an init container's pod, the database cluster.

**What the chart does now.** `imagePullSecrets` is a top-level list of Secret
names, applied to **every** pod the chart renders and to the CloudNativePG
`Cluster`. `secrets.registryPullSecret` additionally produces that Secret from
`config/secrets/secrets.yaml` through whichever credentials backend the cluster
runs, and adds its name to the same list — so the credential the pipeline pushes
with is the one the cluster pulls with, held in one place.

The chart refuses to render when `registryPullSecret.enabled` names an inventory
entry that does not exist, which would otherwise be a whole release in
`ImagePullBackOff` waiting on a Secret nothing was ever going to create.

## `read-only file system` when staging compliance bundles

**Symptom.** The coordinator starts and then fails:

```
mkdir /tmp/sgw-baseline-xxxx: read-only file system
```

**What it is.** The coordinator runs with `readOnlyRootFilesystem: true`, which
is correct, and stages compliance bundles under `/tmp`, which is also correct.

**What the chart does now.** The coordinator mounts an `emptyDir` at `/tmp`. The
root filesystem stays read-only.

## `permission denied to create database` during ZITADEL setup

**Symptom.** The ZITADEL setup Job fails with `SQLSTATE 42501`, or — if the
init step is skipped — with `relation "eventstore.events" does not exist`.

**What it is.** `zitadel init` wants to `CREATE DATABASE` and `CREATE ROLE`, which
the CloudNativePG application account deliberately cannot do. CloudNativePG has
already created the database and its owner; what is missing is only ZITADEL's own
schemas.

**What the chart does now.** The setup Job runs `zitadel init zitadel`, which
creates the internal schemas and nothing else, then `zitadel setup` for the
migrations. The `zitadel` database itself is created by the `Cluster`'s
`postInitSQL`, owned by the application account.

## Flux is not picking up a new chart version

**Symptom.** `spec.chart.spec.version` says one thing and `lastAttemptedRevision`
says another, release after release.

**What it is.** The HelmRelease is still remediating a previous failed install.
It is not stuck on the version; it is stuck on the release.

```sh
flux -n <namespace> get helmreleases            # read the message, not the version
flux -n <namespace> reconcile helmrelease software-gateway --with-source --force
```

**Worth knowing.** The failure that produced this is upstream of Flux almost
every time. Read the HelmRelease's condition message first — `flux get` prints
the Helm error verbatim, and it usually names a hook Job whose log says exactly
what happened.

## A pod sitting in `Init`

Not a failure. Every workload has a `wait-for-<dependency>` init container, so a
pod whose dependency is not ready has not started rather than started and
crashed:

```sh
kubectl -n <namespace> logs <pod> -c wait-for-database
#   waiting for database at swgw-db-rw:5432
```

The container it gates has not run, so its restart count is still zero — which is
the point. A restart count above zero in this deployment means something actually
went wrong.
