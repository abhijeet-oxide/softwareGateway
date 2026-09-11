# 30 - Continuous delivery

> **Prerequisites:** [14 - Deployment and Development](14-deployment-and-development.md), [27 - Configuration as data](27-configuration-as-data.md)

---

## 1. What this had to solve

There were two ways to run this software - `task run` and `docker compose` - and
both read `config/`. A third was described in [14](14-deployment-and-development.md)
and did not exist: Kustomize bases, per-environment overlays, a products
directory beside them. Writing it as described would have produced a **second
description of the same deployment**, which is the arrangement
[27](27-configuration-as-data.md) was written to end.

So the requirement was not "add Kubernetes manifests". It was:

1. **One `config/`.** The products, the people, the roles, the policies and the
   credential inventory are read by a laptop, a compose stack and a cluster,
   from the same files, with no rendering step and no per-environment fork.
2. **A change costs what it should.** Adding a product must not restart
   anything. Adding a person must not restart anything. Changing the code
   must roll the fleet, and must do it without dropping a request.
3. **Two environments, one artefact.** Production must run the bits lab ran,
   provably, with no promotion step that can be got wrong.
4. **Credentials from Vault or from Azure Key Vault**, chosen per environment,
   with everything above that choice unaware of it.
5. **Few secrets to hold.** Three values in GitHub, one credential in the
   cluster.
6. **Ordered, quiet startup.** Nothing starts before what it needs, and a pod
   that is waiting says so rather than crash-looping to find out.

## 2. The shape

```
config/                     CONTENT. products, users, roles, policies, and the
                            credential inventory. No values, ever.
deploy/charts/              the chart. config/ is copied in at package time.
deploy/environments/<env>/
  layers.yaml               TWO Flux Kustomizations, and the order between them
  database/cluster.yaml     LAYER 1 - the CloudNativePG Cluster
  platform/helmrelease.yaml LAYER 2 - WHAT IS DEPLOYED. One line per environment.
deploy/flux/platform/       CloudNativePG, pinned and Flux-managed. Cluster
                            scoped, applied once per cluster.
deploy/flux/clusters/{lab,prod}/
                            how a cluster finds the above, and the Alerts that
                            say what it is doing.
.github/workflows/cd.yml    build, publish, move the pointer. No cluster access.
```

**The pipeline does not deploy.** It pushes to the registry and moves one line
in Git; Flux does the rest. So CD holds no kubeconfig, no cluster credential and
no network path into either environment, and a compromise of the pipeline
cannot reach a cluster except through a commit somebody can see.

**`git log -p deploy/environments/prod/helmrelease.yaml` is the production
deployment history.** Every line was written by a person in a pull request or by
the pipeline moving a version, and there is nothing else to correlate.

## 3. The version, and why there are two of them

Two numbers come out of `.github/scripts/version.sh`:

| | moves when | is |
|---|---|---|
| `chart_version` | every merge | the release. What a HelmRelease points at. |
| `image_version` | only when **code** changes | the tag on all three images. |

They diverge on purpose, and the divergence is the mechanism behind requirement
2. A release that adds a product is a new chart carrying new ConfigMaps and
pointing at **the images already running**, so the Deployments' pod specs do not
change and Kubernetes replaces no pod.

### 3.1 Finding `image_version` without storing anything

The obvious implementation remembers the last version that had a build. That
means a file a bot edits, which means merge conflicts between `lab` and `main`
and a value that can be wrong. The information is already in the repository:

1. `CODE_REV` = the last commit that touched anything an image is built from.
2. The releases containing it are `git tag --contains $CODE_REV`.
3. The **oldest** of those is the release that first shipped this code - so its
   images exist, and they are these images.
4. If no tag contains it, this release is the first to ship it. Build.

Nothing is stored, nothing can drift, and two people running it on the same
commit get the same answer.

> **Decision - derive the image version from tags rather than record it.**
>
> *Alternative:* a `VERSION` file, or an `appVersion` in `Chart.yaml`, written
> back by the pipeline.
>
> *Rejected because* it is state, and state on two branches is state that
> conflicts. Every merge from `lab` to `main` would touch it, every conflict
> would be resolved by whoever was merging, and a wrong resolution ships a chart
> that names an image version that was never built - which fails at
> `ImagePullBackOff`, after the release, in the cluster.
>
> *What would change our mind:* a repository where release tags are deleted or
> rewritten. The derivation assumes tags are immutable, which is why the
> pipeline refuses to overwrite an image tag rather than trusting it.

### 3.2 Production runs the bits lab ran

Lab publishes releases too, as prereleases: `v1.4.3-lab.87`. SemVer orders a
prerelease **before** the release it previews, which is exactly what it is.

When that commit reaches `main`, step 3 finds the lab tag - older and lower - so
production's chart names `1.4.3-lab.87` and deploys **the image lab tested**.
Nothing is rebuilt on the way in, so there is nothing that could differ. There
is no promotion step to get wrong because there is nothing to promote.

**The one case where this does not hold is a squash merge.** It creates a new
commit, no tag contains it, and the images are rebuilt. The source is identical
so the software is, but the digests differ. Merge `lab` into `main` with a merge
commit if you want artefact identity end to end.

### 3.3 Which number moves is a choice a person makes

A `Release: minor` or `Release: major` trailer in a commit message on the way in
bumps that field; anything else is a patch. A trailer rather than a label: it is
reviewable, it lives with the change that earned it, and it cannot be added
after the fact.

## 4. One repository for images and charts

`vars.JFROG_REGISTRY` and `vars.JFROG_REPOSITORY` name one OCI repository
holding `software-gateway-coordinator`, `-worker`, `-web` and the
`software-gateway` chart. One credential (`JFROG_USERNAME`, `JFROG_TOKEN`)
pushes all four, Flux pulls the chart with it, and the kubelet pulls the images
with it - the chart's `secrets.registryPullSecret` puts the same credential in
the namespace from the same vault entry.

That is the whole credential surface: **three secrets in GitHub, one in the
cluster.**

**An image tag is written once.** The pipeline checks the registry before it
builds and fails if the tag exists. Overwriting one would make every recorded
deployment ambiguous - a rollback to 1.4.2 would fetch whatever 1.4.2 means
today.

### 4.1 Everything comes from it, including the parts nobody remembers

`image.registry` covers this product's three images. `images.mirror` covers the
other five - ZITADEL, its sign-in screens, nginx, Cerbos and the node image two
init containers use for a few seconds. The CloudNativePG `Cluster` names its
PostgreSQL image, and the operator's own HelmRelease names the operator's.

**Setting one and not the other is refused at render.** Half-mirrored is the
worst of the three states: a cluster pulling three images from Artifactory and
five from the internet is not an air-gapped deployment, and is
indistinguishable from one until a node without egress tries to schedule a pod,
or until Docker Hub rate-limits the whole cluster on a Tuesday.

The forgettable one is CloudNativePG's PostgreSQL image. A cluster missing it
does not fail when it is deployed - it fails **during a failover**, which is
the worst possible moment to learn that a mirror was never configured. So
`TestNothingIsPulledFromThePublicInternet` collects every reference a deployed
environment produces, from the rendered chart AND from the two Flux layers that
are not Helm, and asserts each names the internal registry. It is an allowlist
rather than a denylist of public hostnames, because a denylist passes the first
registry nobody thought of.

## 5. What a change costs

This is the requirement the design is actually built around, and it is a
property of `templates/configmaps.yaml`. Configuration is split into four
ConfigMaps by **what applying a change costs**, which is invisible if they are
one object:

| change | ConfigMap | what happens | restart |
|---|---|---|---|
| a product | `-products` | the Coordinator and every Worker watch the directory and reload | none |
| a Cerbos policy | `-policies` | `watchForChanges` on the disk store | none |
| a person, a role | `-identity` | a Job runs for ten seconds | none |
| a credential rotates | the projected Secret volume | the operator rewrites it, the kubelet updates the volume, the process reads the file when it next needs a token | none |
| `config.yaml` | `-system` | a checksum annotation changes | rolling |
| the code | the image tag changes | a new pod spec | rolling |

**Only `-system` carries a checksum annotation**, and the absence of one
elsewhere is the design rather than an omission. `products/` and `secrets/` are
mounted as **whole directories, never `subPath`**: a `subPath` mount does not
receive ConfigMap or Secret updates, which would silently switch off both the
product watcher and credential rotation - the two things this deployment relies
on to change without a restart.

### 5.1 Combinations

The interesting cases are the mixed ones, and they compose because each half is
independent:

- **A person plus an image.** The Deployments roll on the new tag; the seeding
  Job runs because its name hash changed. Neither waits for the other, and the
  Job restarts nothing.
- **A person plus a product.** Two ConfigMaps change and a Job runs. No pod is
  replaced at all.
- **`config.yaml` plus a product.** The fleet rolls for `config.yaml`; the new
  product is in the ConfigMap the new pods mount and the old ones are already
  watching. Both are correct at every instant.
- **A fresh install.** Nothing is applied at once, and nothing crash-loops.
  Flux brings the database layer to Ready; only then does it apply the release.
  Helm runs the ZITADEL migration as a pre-install hook and waits for it, so the
  first ZITADEL pod is created against a schema that already exists. Every
  other workload waits in an init container for what it needs. The whole
  sequence is visible as pods moving from `Init:0/1` to `Running`, in order,
  with a restart count of zero. See §5.3.

### 5.2 The one thing that is not instant

A **removal**. The Coordinator verifies tokens offline and never asks whether
one is still good, so an already-issued token works until it expires. Removing
somebody stops the renewal; `identity.tokenLifetimes.accessToken` (15m) is the
window. That is a property of offline verification, not of this pipeline, and
the alternative - an introspection call per request - is a dependency on the
identity provider in the hot path of every API call.

### 5.3 Ordered startup, and why nothing crash-loops

Kubernetes has no `depends_on`, and the usual substitute is to let a pod start,
fail, and be restarted until its dependency appears. That is rejected here, for
a reason that is about people rather than machines: **a pod in CrashLoopBackOff
looks identical whether it is waiting for its database or is genuinely broken.**
Accept it as normal during a deployment and you have trained everybody to ignore
the one pod state that should never be ignored, and made a restart count mean
nothing.

Ordering is therefore explicit, at three levels, and each one covers what the
one below it cannot:

**Between layers - Flux `dependsOn`.** A three-link chain, each with
`wait: true`, so each holds entirely until the one before it reports **Ready**
rather than merely applied:

```
platform-operators                 CloudNativePG. Cluster-scoped, once per cluster.
  └─ software-gateway-<env>-database   the Cluster: initdb, instances joined, a primary elected
       └─ software-gateway-<env>-platform   the Helm release
```

This is `depends_on`, at the level where GitOps has it. The first link matters
for a reason that is easy to miss: the database layer submits a `Cluster`, a
kind the API server only knows once the operator has registered its CRD and
whose validating webhook must be answering to admit it. Without the dependency
the apply fails with `no matches for kind Cluster` and retries until it happens
to work - the flapping this whole arrangement exists to remove.

**Inside the release - Helm hooks.** The ZITADEL migration is a
`pre-install,pre-upgrade` hook at weight -10, with its RBAC at -20. Helm applies
hooks, waits for them to complete, and only then creates the release's own
objects. So the schema is migrated **before the first ZITADEL pod exists**,
rather than after it has failed a few times.

This is only possible because the database is in an earlier layer. A pre-install
hook in a chart that also deploys its own database deadlocks on a fresh install:
the hook waits for a Postgres that Helm has not created yet. That is the third
reason the database is not in the chart, alongside keeping `helm rollback` away
from it and letting it be upgraded on its own schedule.

**Inside a pod - `wait-for-*` init containers.** What is left is the ordering
Helm cannot express, because it is between objects in one release:

| workload | waits for | hard? |
|---|---|---|
| coordinator | the database | hard - it migrates a schema at startup |
| zitadel | the database | hard - covers a failover in progress |
| zitadel-login | ZITADEL, then the seeder's service-user token | hard - it reads the token from a file and **exits** if absent |
| zitadel-proxy | ZITADEL, then the sign-in screens | hard, then soft |
| web | the coordinator, then the SPA's OIDC client id | **soft** |
| worker | the coordinator | **soft** |
| seed Job | ZITADEL | hard |
| cerbos | nothing | it talks to nothing |

A waiting pod sits in `Init:0/1` and logs one line naming what it is waiting
for. Its application container has not run, so its restart count stays zero and
keeps meaning something.

**Soft where the container is useful without its dependency**, and that is not a
weakening. The web tier renders the page that explains an outage, so it must come
up during one; a worker is designed to start, report DEGRADED and lease nothing
([27](27-configuration-as-data.md) §6), because the data plane must not depend on
the control plane being up first. A soft wait orders a first install and then
gives up and starts, which is what those two rules ask for.

**`go test ./deploy/...` asserts the whole table.** A workload whose waits change
fails `TestEveryDependencyIsWaitedFor` by name, and a Postgres put back into the
chart fails `TestTheDatabaseIsNotInTheChart` - which would otherwise turn ordered
startup into a first-install deadlock silently.

## 6. Rolling out without dropping a request

`maxUnavailable: 0`, `maxSurge: 1`. A replica is removed **only after its
replacement reports ready**, and readiness on the Coordinator means the database
answers, its schema is the one this build expects, and the products loaded.

So a bad image cannot take capacity away. The new pod never becomes ready, the
rollout stalls with the old pods serving, `progressDeadlineSeconds` marks the
Deployment failed, the HelmRelease's `upgrade.timeout` fires, and
`remediation.strategy: rollback` puts the previous release back. **Nothing went
down at any point, and nobody had to be paged to make that true.**

Around that:

- **PodDisruptionBudgets** keep a quorum through a node drain.
- **`preStop: sleep 5` on the two nginx tiers.** Endpoint removal and the
  container's own shutdown propagate at different speeds; sleeping through the
  gap is what makes a rollout drop zero requests rather than nearly zero.
- **`terminationGracePeriodSeconds: 120` on the workers**, so a worker finishes
  the blob it is streaming. One that outlives it is killed and its lease
  expires - correct, merely slower.

> **Decision - a surge rolling update rather than Flagger.**
>
> *Alternative:* progressive traffic shifting with metric analysis, which needs
> Flagger plus either a service mesh or a supported ingress controller.
>
> *Rejected because* the property being bought is already held. What Flagger
> adds over the above is **automatic abort on a metric regression in code that
> is serving real traffic** - a bad release that is healthy by every probe and
> wrong in its behaviour. That is a real failure mode and it is not this
> system's most likely one: the Coordinator's readiness check is unusually
> strong (schema match, configuration loaded), so the failures that get past it
> are a narrow class. Flagger would also double the control plane's database
> connections during every analysis window and would need Prometheus queries
> maintained per component.
>
> *What would change our mind:* a release that was green on every probe and
> wrong in production. That is the evidence that would make the analysis window
> worth its cost, and the chart is arranged so adding it changes no workload -
> Flagger takes over a Deployment's Service, and every Service here is already
> separate from its Deployment.
>
> **Blue/green** was rejected on the same evidence plus a specific hazard: two
> full releases against one database means two schema versions live at once,
> which the expand-and-contract rule in §7 already requires, and doubles the
> stateful footprint to buy an instant cutover nobody had asked for.

## 7. The rule a rolling update imposes on migrations

During any rollout, **the old and the new binary are both running against one
database**. Migrations run at Coordinator startup, so the new schema arrives
while the old binary is still serving.

Every migration must therefore be **backwards compatible with the release before
it**: add a column, do not rename one; write to both while a read moves; drop
only in a later release. This is the ordinary expand-and-contract rule and it is
not new here - `maxUnavailable: 0` makes it load-bearing rather than merely
good practice, because the overlap is guaranteed rather than incidental.

### 7.1 The database, and the operator that runs it

**PostgreSQL runs in-cluster, in every environment. There is no managed-database
path anywhere in this repository**, and CloudNativePG is what makes that a
defensible position rather than a liability: three instances in production with
synchronous replication, automatic failover in seconds, and rolling minor
upgrades. Two in lab - the smallest number that can demonstrate a failover,
which is most of what a lab is for.

The operator generates the owner's password and publishes it as `swgw-db-app`.
So it exists in exactly one place, in no values file and no commit, and there is
nothing to rotate by hand. The chart reads two keys out of it and lets the
kubelet assemble the DSN with `$(VAR)` substitution - which keeps the password
out of every manifest without putting a shell into a distroless image to build a
URL.

**The operator is managed by Flux too**, in `deploy/flux/platform/operators`,
pinned and reviewed - because an operator that owns every database in the
cluster is exactly the thing whose version should be written down and the same
in both environments. It is deliberately not treated like the application:

| | why |
|---|---|
| `prune: false` | pruning removes CRDs, and deleting a CRD deletes every object of that kind - here, every database. Removing an operator is a maintenance window, not something a merge can do. |
| `upgrade.crds: CreateReplace` | `helm upgrade` does not touch CRDs at all. Without it the operator moves and its schema does not, and new fields are dropped silently. |
| `remediation.retries: 0` | no automatic rollback. Rolling a database operator back mid-upgrade, while it holds every Cluster and may already have migrated their CRDs, turns a bad ten minutes into a bad week. It stops and alerts. |

**Where the operator runs is a per-cluster choice, and one thing about it is
not a choice.** There is one CloudNativePG per cluster in either scope, because
two pieces of what it installs are cluster-scoped singletons: the CRDs, and the
admission webhook configurations it reconciles at startup to point at its own
Service. Two operators would each rewrite those to point at themselves.

So the switch is where the single operator LIVES and what it WATCHES:

```sh
kubectl apply -k deploy/flux/platform/bootstrap/cluster-scoped        # one cluster, both environments
kubectl apply -k deploy/flux/platform/bootstrap/namespace-scoped/lab  # one cluster, one environment
```

Both create a Kustomization named `platform-operators`, so
`deploy/environments/<env>/layers.yaml` depends on that one name and nothing in
the repository moves when the scope does. `TestEveryOperatorScopeBuildsAndKeepsOneName`
asserts both halves of that - every arrangement builds, and every one of them
keeps the name - because a scope that renamed it would leave every environment
waiting on a dependency that will never exist, quietly, since waiting is what
that arrangement is designed to do.

Namespace scope is the better answer where it applies: the operator holds a
Role rather than a ClusterRole, so a bug or a compromise reaches the namespace
it serves and nothing else. Cluster scope is **required** as soon as one cluster
carries both environments - and that topology has a consequence worth knowing
before choosing it: a shared operator cannot be upgraded in lab first, because
it is the same Deployment production is using.

### 7.2 Changing scope is a migration, not a toggle

Both bootstraps create a Kustomization with the **same name**, so applying the
other one updates `spec.path` in place. What that does not do is remove the
operator the previous scope installed - that is a HelmRelease in a different
namespace, and this layer has `prune: false` because pruning an operator can
take its CRDs, and deleting a CRD deletes every object of that kind.

Left alone, a scope change would therefore leave **two operators running**, each
reconciling the same cluster-scoped admission webhooks to point at itself, each
reporting perfectly healthy. Nothing would say so, because nothing is broken
about either one individually - only about there being two.

So every scope pins `releaseName: cloudnative-pg` and
`storageNamespace: cnpg-system`. Helm keys a release by that pair, so the second
scope cannot install: it fails, and the failure Alert carries it. A stuck switch
is a bad afternoon; two operators fighting over one webhook while both report
green is a bad quarter.

The migration itself has a runbook in
[`deploy/flux/platform/operators/README.md`](../../deploy/flux/platform/operators/README.md).
Its one irreversible step is annotating the CRDs `helm.sh/resource-policy: keep`
**before** removing the old operator; everything else can be re-run.

### 7.3 What happens when the operator is not there

Nothing is applied, nothing crash-loops, and it is not silent - which is three
separate claims and each one is a decision.

`software-gateway-<env>-database` declares `dependsOn: [platform-operators]`. If
that Kustomization does not exist, Flux reports `dependencies do not meet ready
condition`, applies nothing, and retries every minute. The platform layer waits
behind it. **No `Cluster` is submitted to an API server that has never been
taught the kind**, so the failure is one clear sentence rather than
`no matches for kind Cluster` repeating in a controller log.

Being told about it took a correction. The info Alert originally EXCLUDED
`dependencies do not meet ready condition` as noise - which made the one failure
that waits forever the one failure nobody hears about. It is no longer excluded:
the channel repeats "waiting for platform-operators" every minute, which is how
somebody works out they skipped a bootstrap step. Forty-five minutes in, the
root Kustomization's own timeout fires and the failure Alert repeats it as an
error.

The failure Alert also named the two layers by glob and missed the two objects
most likely to be the actual cause - the root Kustomization, which is what times
out, and `platform-operators` itself. Both are named explicitly now.

> **Backups are not configured.** A replicated cluster protects against losing an
> instance, not against losing the data: a `DROP TABLE` is replicated faithfully
> and immediately. `spec.backup` in `deploy/environments/<env>/database/cluster.yaml`
> is where continuous WAL archiving goes, with the shape of the answer in a
> comment and the target deliberately unchosen. **This is the one gap to close
> before the cluster holds data anybody would miss.**

## 8. Credentials

`config/secrets/secrets.yaml` lists what this deployment needs: a name, its
keys, and a **relative** path. No values, and there is nowhere in the schema to
put one. The environment supplies the root (`secrets.vault.pathPrefix`,
`secrets.azure.keyvaultName`); the inventory supplies the leaf. That is why lab
and production share one list.

From it, `templates/secrets.yaml` renders either a `VaultStaticSecret` or a
`SecretProviderClass`, and **the Secret that lands in the namespace is identical
either way** - same name, same keys - so everything above `secrets.backend` is
unaware of the choice.

| | Vault Secrets Operator | Azure Key Vault CSI |
|---|---|---|
| produces a Secret | always | only while a pod mounts the class |
| rotation | on `refreshAfter`, no restart | on pod mount refresh |
| this chart | the recommendation | supported; the coordinator mounts the class |

The CSI driver's pod-scoped behaviour is the one real difference, it is stated
in the template rather than hidden, and it is why Vault is the default
recommendation rather than a preference.

Locally the same inventory produces the directory a developer fills in by hand:

```sh
task secrets:scaffold      # one directory per secret, one EMPTY file per key
```

Empty rather than `CHANGEME`, because a placeholder is a credential the registry
rejects with a 401 that names nothing, and an empty file is reported at load, by
path, as the missing value it is.

**`go test ./deploy/...` fails when a product references a secret the inventory
does not declare** - the same shape of check as "every product has an owner"
([27](27-configuration-as-data.md) §3), and for the same reason.

## 9. Getting `config/` into a chart

Helm packages a **directory**, and `.Files` cannot reach outside it.
`.Files.Glob("../../config/**")` returns nothing - not an error, nothing - so a
chart that tried it would package cleanly, install cleanly, and mount empty
ConfigMaps.

`deploy/chartstage` copies `config/` and the deployment machinery into the
chart before packaging. The copy is **not committed**, so there is no second
tree for a reviewer to miss, and `TestStagedChartIsReproducible` re-runs the
stage into a temporary directory and compares fingerprints - a stale copy is a
failed test rather than a chart that shipped yesterday's products.

It is Go rather than a shell script because `cp -r` does not exist in PowerShell
and this repository asserts in CI that its tasks run with only `go`, `gofmt`,
`git` and `task` on PATH.

## 10. The seeder in a cluster

`deploy/zitadel/bootstrap.mjs` writes four things that outlive its run and are
read by other containers: the instance's machine token, the sign-in screens'
service-user token, the SPA's OIDC client id, and the worker fleet's
credentials. Under compose those are named volumes. A Job has no such thing, and
a ReadWriteOnce PVC shared with pods on other nodes is not a mechanism - it is a
scheduling constraint that eventually fails at 03:00.

So in a cluster the volume is a **Secret**, and `deploy/zitadel/k8s-state.mjs`
is the only thing that knows it:

```
node k8s-state.mjs pull    # Secret -> files
node bootstrap.mjs         # UNCHANGED - it reads and writes files, as on a laptop
node k8s-state.mjs push    # files -> Secret
```

The seeder itself is untouched, which is the point: one program, one behaviour,
two deployments. `k8s-state.mjs` calls the in-cluster API with `fetch` and the
service account token - no `kubectl`, for the same reason the seeder parses YAML
by hand, which is that `apk add` at deploy time is a network call an air-gapped
estate does not have. Its Role grants `get` and `patch` on **one Secret by
name**, and `create` on the resource, which RBAC cannot restrict to a name
before the object exists.

### 10.1 Provisioning runs when the people change

The seeding Job's **name carries a hash** of `users.yaml`, `roles.yaml`, the
product names and the identity settings. A release that only moved an image
renders the same name, Kubernetes finds an object that already exists and
completed, and nothing runs. A release that adds a person renders a new name, so
a new Job is created.

It is deliberately **not a Helm hook**: a hook runs on every upgrade including
the ones that change nothing about identity, and a hook failure fails the
release. As an ordinary resource, a provisioning problem is a failed Job
somebody reads rather than a rollback of unrelated code.

### 10.2 Being told what happened

The pipeline moves one line in Git and stops. That is the property that keeps
cluster credentials out of CI, and it has a corollary: **nothing in the pipeline
knows whether the deployment worked.** A green CD run means a chart was
published and a pointer moved, which is not the same claim.

So the notification-controller closes it, with two Alerts per environment and
the split is the point:

- **info** is the running commentary - the database layer reaching Ready, the
  platform layer starting because of it, the upgrade finishing. Health-check
  progress is excluded; it fires every few seconds during a rollout and says
  nothing anybody can act on.
- **error** is wider than the deploy: every source and release in the namespace,
  so a chart that cannot be pulled from the internal registry, or a credential
  the operator could not resolve, is heard about too - none of which a deploy
  would have touched.

One channel for both would put the failures in a scroll of successes, where
they are read on Monday.

## 11. Two environments

The **branch is the environment**: `lab` deploys to the lab namespace, `main` to
production. Promotion is therefore the pull request that merges one into the
other - the thing the team already reviews - rather than a second mechanism that
has to be kept honest.

Same cluster or two clusters: the manifests do not care. Two namespaces in one
cluster works because every name is namespaced; two clusters works because each
applies only its own directory.

The one constraint the chart imposes is **one release per namespace**. Service
names are not release-prefixed - `deploy/web/nginx.conf` proxies to
`controller:8080` and `deploy/zitadel/nginx.conf` to `zitadel:8080`, and those
files are baked into images and mounted by `docker-compose.yml`. Prefixing would
mean a second copy of both, differing in three words, and a class of bug where
the compose stack works and the cluster 502s.

## 12. Pipelines, and why there are two

**`ci.yml`** runs on every pull request: build, test, lint, the configuration
validators, and now the chart - staged, linted, rendered with **both**
environments' real values, validated against the Kubernetes schemas, and proved
to refuse five configurations that cannot work.

**`cd.yml`** runs on a merge to `lab` or `main`: decide the version, build the
images if code changed, package and publish the chart, move the pointer.

They are two because they answer different questions at different times. They
are not three because there is one artefact: a chart version carries a config
directory and names an image version, and the three are decided together.
Splitting the image build from the chart publish would mean two runs each
knowing half the answer and a window in which they disagree.

The pointer commit is excluded by `paths-ignore`, so a release does not trigger
the release after it - and a person editing the pointer to pin or roll back
publishes nothing, which is correct: the version being pinned to already exists.

## 13. Files

- [`deploy/charts/software-gateway/`](../../deploy/charts/software-gateway/) - the chart, and its README
- [`deploy/environments/`](../../deploy/environments/) - the two ordered layers, per environment
- [`deploy/flux/`](../../deploy/flux/) - bootstrapping a cluster, the operators, the alerts
- [`deploy/flux/platform/operators/`](../../deploy/flux/platform/operators/) - CloudNativePG, pinned and Flux-managed
- [`deploy/chartstage/`](../../deploy/chartstage/) - the copy, and the environment values
- [`deploy/secretsinv/`](../../deploy/secretsinv/) - the inventory, shared by the chart, the scaffold and the test
- [`deploy/zitadel/k8s-state.mjs`](../../deploy/zitadel/k8s-state.mjs) - a compose volume, as a Secret
- [`deploy/delivery_test.go`](../../deploy/delivery_test.go) - the three invariants
- [`.github/scripts/version.sh`](../../.github/scripts/version.sh) - the version
- [`.github/workflows/cd.yml`](../../.github/workflows/cd.yml) - the pipeline
