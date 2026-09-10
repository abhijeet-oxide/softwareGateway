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

### The credential operator

The chart renders `VaultStaticSecret` or `SecretProviderClass` objects from
`config/secrets/secrets.yaml`; the operator that acts on them is a cluster
prerequisite and is not installed by this chart - a chart that installs its own
secrets operator is a chart that has to hold a Vault token.

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
flux -n flux-system get kustomizations software-gateway
flux -n swgw get helmreleases
flux -n swgw events --for HelmRelease/software-gateway
kubectl -n swgw rollout status deploy/swgw-software-gateway-coordinator
```

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
