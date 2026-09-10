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

## 2. The shape

```
config/                     CONTENT. products, users, roles, policies, and the
                            credential inventory. No values, ever.
deploy/charts/              the chart. config/ is copied in at package time.
deploy/environments/{lab,prod}/helmrelease.yaml
                            WHAT IS DEPLOYED. One line per environment.
deploy/flux/clusters/{lab,prod}/
                            how a cluster finds the two above.
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
- **A fresh install.** Everything is applied at once. Postgres comes up, the
  ZITADEL setup Job migrates, ZITADEL crash-loops until it has, the seeding Job
  waits for ZITADEL and then provisions, and the workers start immediately and
  lease nothing until they have credentials and products
  ([27](27-configuration-as-data.md) §6). Nothing here needs an ordering
  primitive Kubernetes does not have.

### 5.2 The one thing that is not instant

A **removal**. The Coordinator verifies tokens offline and never asks whether
one is still good, so an already-issued token works until it expires. Removing
somebody stops the renewal; `identity.tokenLifetimes.accessToken` (15m) is the
window. That is a property of offline verification, not of this pipeline, and
the alternative - an introspection call per request - is a dependency on the
identity provider in the hot path of every API call.

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
- [`deploy/environments/`](../../deploy/environments/) - what is deployed where
- [`deploy/flux/`](../../deploy/flux/) - bootstrapping a cluster
- [`deploy/chartstage/`](../../deploy/chartstage/) - the copy, and the environment values
- [`deploy/secretsinv/`](../../deploy/secretsinv/) - the inventory, shared by the chart, the scaffold and the test
- [`deploy/zitadel/k8s-state.mjs`](../../deploy/zitadel/k8s-state.mjs) - a compose volume, as a Secret
- [`deploy/delivery_test.go`](../../deploy/delivery_test.go) - the three invariants
- [`.github/scripts/version.sh`](../../.github/scripts/version.sh) - the version
- [`.github/workflows/cd.yml`](../../.github/workflows/cd.yml) - the pipeline
