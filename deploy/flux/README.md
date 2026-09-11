# Flux - how this product reaches a cluster

Flux reconciles many applications in these clusters. This directory is the part
of it that is ours, and it is deliberately small: two objects per cluster, and
neither of them says anything about what the application IS.

```
clusters/
  lab/   source.yaml  GitRepository (branch `lab`)  + HelmRepository (JFrog)
         kustomization.yaml  -> ./deploy/environments/lab
  prod/  source.yaml  GitRepository (branch `main`) + HelmRepository (JFrog)
         kustomization.yaml  -> ./deploy/environments/prod
```

Each of those directories creates **two more** Kustomizations, and the second
`dependsOn` the first:

```
software-gateway-<env>-database    the CloudNativePG Cluster       wait: true
software-gateway-<env>-platform    the Helm release  dependsOn ^   wait: true
```

Flux will not apply the platform layer until the database reports Ready. That
is `depends_on`, and it is why nothing in this deployment ever starts against a
database that is not there.

**The branch is the environment.** `lab` deploys to the lab namespace, `main`
to production. Promotion is therefore the pull request that merges one into the
other - the thing the team already reviews - rather than a second mechanism
that has to be kept honest.

Same cluster or two clusters: the manifests do not care. Two namespaces in one
cluster works because every name in the chart is namespaced and the two
HelmReleases never meet. Two clusters works because each one applies only its
own directory. Start with one cluster and split later; nothing here changes.

## Bootstrapping a cluster

Once per cluster, by somebody with admin on it. Everything after this is a
pull request.

```sh
# 1. Flux itself
flux install --namespace flux-system

# 2. Read access to this repository
flux create secret git software-gateway-git \
  --namespace flux-system \
  --url https://github.com/abhijeet-oxide/softwareGateway \
  --username git --password "$GITHUB_TOKEN"

# 3. The JFrog credential - the SAME one the pipeline pushes with. It is used
#    twice: Flux pulls the chart with it, and the kubelet pulls the images with
#    it (the chart's secrets.registryPullSecret puts it in the namespace).
flux create secret oci jfrog \
  --namespace flux-system \
  --url artifactory.internal.example.com \
  --username "$JFROG_USERNAME" --password "$JFROG_TOKEN"

# 4. This product
kubectl apply -k deploy/flux/clusters/lab      # or clusters/prod
```

### Two operators, both cluster prerequisites

Neither is installed by this repository. An application chart that installs its
own secrets operator has to hold a Vault token; one that installs its own
database operator owns every other database in the cluster.

**CloudNativePG**, which runs the database:

```sh
helm install cnpg cloudnative-pg/cloudnative-pg \
  --namespace cnpg-system --create-namespace
```

There is no managed-database option anywhere in this repository. PostgreSQL runs
in-cluster in every environment, with replication and automatic failover, and
`deploy/environments/<env>/database/cluster.yaml` is the whole description of it.

> **Backups are not configured, and that is the one gap to close before this
> holds data anybody would miss.** A CloudNativePG cluster replicates, which
> protects against losing an instance and not against losing the data: a
> `DROP TABLE` is replicated faithfully and immediately. `spec.backup` in that
> file is where continuous WAL archiving goes, and it ships unset with the
> shape of the answer in a comment.

**The credential operator**, which turns `config/secrets/secrets.yaml` into
Secrets:

```sh
# HashiCorp Vault Secrets Operator (secrets.backend: vault)
helm install vault-secrets-operator hashicorp/vault-secrets-operator \
  --namespace vault-secrets-operator-system --create-namespace
# then a VaultConnection and a VaultAuth in each namespace, named by
# secrets.vault.authRef.

# or Azure Key Vault CSI (secrets.backend: azure)
helm install csi-secrets-store secrets-store-csi-driver/secrets-store-csi-driver \
  --namespace kube-system --set syncSecret.enabled=true
```

On AKS the CSI driver is an add-on: `az aks enable-addons --addons
azure-keyvault-secrets-provider`. Both paths produce the same Secrets with the
same names and keys, so the choice is invisible above `secrets.backend`.

## Watching a deployment

```sh
# The layers, in order. The platform one reads "dependency not ready" until the
# database one is Ready - which is the ordering working, not a fault.
flux -n flux-system get kustomizations

kubectl -n swgw get cluster.postgresql.cnpg.io    # instances, and which is primary
flux -n swgw get helmreleases
kubectl -n swgw get pods
```

**A pod in `Init:0/1` is waiting, not failing.** Every workload has a
`wait-for-<dependency>` init container, so ordered startup is visible as pods
that have not begun rather than pods that are crashing:

```sh
kubectl -n swgw logs <pod> -c wait-for-database
#   waiting for database at swgw-db-rw:5432
```

A restart count above zero in this deployment means something actually went
wrong, which is the whole reason the waits exist.

## When a release goes wrong

Nothing has to be done, and that is the design. `maxUnavailable: 0` means a
replica is replaced only after its successor reports ready, so a broken image
never removes capacity - the rollout stalls with the old pods serving.
`spec.upgrade.timeout` then fires and `remediation.strategy: rollback` puts the
previous release back.

What is left for a person is deciding whether to go forward or stay put:

```sh
# stay put: pin the version in Git, so Flux stops trying
#   deploy/environments/prod/helmrelease.yaml -> spec.chart.spec.version
# go back further: set it to any version in the registry, in a pull request

# stop deploying entirely, right now
flux -n swgw suspend helmrelease software-gateway
```

`flux suspend` is the emergency brake and it is not a fix: Git and the cluster
now disagree, and nothing will tell you so later. Follow it with the pull
request that makes Git say what the cluster is doing.
