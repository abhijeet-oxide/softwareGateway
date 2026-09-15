# Deploying with Flux

```
deploy/flux/
├── clusters/<cluster>/       what is true of the cluster
│   ├── 0-sources/            the Git and chart sources, and the operators
│   ├── 1-instances/          which instances this cluster runs
│   ├── kustomization.yaml    applied once, by hand
│   └── sync.yaml             the cluster reconciling this directory
│
├── instances/<instance>/     one deployment, in one namespace
│   ├── 0-secrets/            what must exist before the release installs
│   ├── release/              which release of the software this instance runs
│   ├── values/values.yaml    THE ONE FILE
│   ├── kustomization.yaml
│   └── namespace.yaml
│
└── software/                 the product, with nothing instance-specific in it
    ├── base/                 the two HelmReleases
    ├── patch/<version>/      one directory per chart version an instance may run
    └── schema/               what a values file is allowed to say
```

## One file per instance

`instances/<instance>/values/values.yaml` is the whole of what a deployment
differs in. A `configMapGenerator` turns it into a ConfigMap, and **both**
HelmReleases read that one ConfigMap through `valuesFrom` — so the database and
the application cannot disagree about the database.

There is no second place to look. Anything not in that file is the chart's
default, listed by `helm show values software-gateway`, and constrained by
`software/schema/values.schema.json` — which Helm checks before the render, so a
misspelled key is a failed reconciliation naming the key rather than a setting
that silently did not apply.

Deploying somewhere new is therefore: copy an instance directory, edit one file,
add one line to a cluster's `1-instances/`.

A values edit is applied at the next HelmRelease reconciliation (5 minutes), or
at once with `flux -n <namespace> reconcile helmrelease software-gateway`.

### What is deliberately not in it

Three things are facts about a **cluster** rather than about a deployment, so
they live in `clusters/<cluster>/0-sources/` and are set once when the cluster is
bootstrapped: which Git repository it reconciles from, which registry it pulls
charts from, and which version of the database operator it runs. An instance
never restates them, and moving an instance between clusters changes none of its
own files.

## Two layers, and the order between them

`software/base` holds two HelmReleases of the same chart:

| release | renders | remediation |
|---|---|---|
| `software-gateway-db` | the CloudNativePG `Cluster` | none — a database is not rolled back automatically |
| `software-gateway` | everything else, `dependsOn` the first | rollback |

They are separate so that a rollback of the application cannot reach the
database, so `helm uninstall` cannot delete it, and so the chart's ZITADEL
migration can stay a Helm pre-install hook — which needs the database to already
exist.

Ordering continues below Helm: every workload has a `wait-for-<dependency>` init
container, so a pod whose dependency is not ready sits in `Init` saying what it
is waiting for, rather than crash-looping.

```
platform-operators                CloudNativePG           wait: true
  software-gateway-<instance>     the instance            wait: true
    software-gateway-db           the database            dependsOn ^
      software-gateway            the application         dependsOn ^
```

## Bootstrapping a cluster

Once per cluster, by somebody with admin on it. Everything after this is a pull
request.

```sh
# 1. Flux itself. --registry points it at an internal mirror.
flux install --namespace flux-system

# 2. Read access to this repository
flux create secret git software-gateway-git \
  --namespace flux-system \
  --url https://github.com/abhijeet-oxide/softwareGateway \
  --username git --password "$GIT_TOKEN"

# 3. The registry credential. Used twice: Flux pulls the chart with it, and the
#    kubelet pulls the images with it.
flux create secret oci chart-registry \
  --namespace flux-system \
  --url <registry host> \
  --username "$REGISTRY_USERNAME" --password "$REGISTRY_TOKEN"

# 4. The cluster
kubectl apply -k deploy/flux/clusters/lab
```

Step 4 creates the sources, the operator layer, and one reconciler per instance.
Nothing is silent if a step is skipped: the instance layer reports
`dependencies do not meet ready condition` naming `platform-operators`, applies
nothing, and retries every minute.

## Watching it

```sh
flux -n flux-system get kustomizations            # the chain, in order
flux -n swgw-lab get helmreleases                 # the two layers
kubectl -n swgw-lab get cluster.postgresql.cnpg.io
kubectl -n swgw-lab get pods

# A pod in Init is WAITING, not failing.
kubectl -n swgw-lab logs <pod> -c wait-for-database
```

A restart count above zero means something actually went wrong, which is the
whole reason the waits exist.

## When a release goes wrong

Nothing has to be done. `maxUnavailable: 0` means a replica is replaced only
after its successor reports ready, so a broken image never removes capacity — the
rollout stalls with the old pods serving, `spec.upgrade.timeout` fires, and
`remediation.strategy: rollback` puts the previous release back.

What is left is deciding whether to go forward or stay put:

```sh
# stay put: repoint instances/<instance>/release/kustomization.yaml at an
#           earlier software/patch/<version>, in a pull request

# stop deploying entirely, right now
flux -n swgw-lab suspend helmrelease software-gateway
```

`flux suspend` is the emergency brake and not a fix: Git and the cluster now
disagree and nothing will say so later. Follow it with the pull request that
makes Git say what the cluster is doing.

## Adding an instance

```sh
cp -r deploy/flux/instances/lab deploy/flux/instances/lab2
$EDITOR deploy/flux/instances/lab2/{namespace.yaml,kustomization.yaml,values/values.yaml}
cp deploy/flux/clusters/lab/1-instances/lab.yaml \
   deploy/flux/clusters/lab/1-instances/lab2.yaml   # edit name, path, namespace
$EDITOR deploy/flux/clusters/lab/1-instances/kustomization.yaml
```

Two instances in one cluster works because every name is namespaced and the two
never meet. Two clusters works because each applies only its own directory.
