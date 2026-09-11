# The database operator, and where it runs

```sh
# one cluster, more than one environment
kubectl apply -k deploy/flux/platform/bootstrap/cluster-scoped

# this cluster hosts one environment
kubectl apply -k deploy/flux/platform/bootstrap/namespace-scoped/lab      # or /prod
```

That is the whole switch. Both create a Kustomization named **`platform-operators`**,
so `deploy/environments/<env>/layers.yaml` says `dependsOn: [platform-operators]`
and never needs to know which was chosen. Changing scope is one command and
nothing else in the repository moves.

## The thing that is not configurable, and why

**There is one CloudNativePG per cluster, in either scope.** Not because the
operator insists, but because two pieces of what it installs are cluster-scoped
singletons and Kubernetes has nowhere else to put them:

- **The CRDs.** A `CustomResourceDefinition` is a cluster-scoped object. There
  is one `clusters.postgresql.cnpg.io` in a cluster and it has one schema.
- **The admission webhooks.** `ValidatingWebhookConfiguration` and
  `MutatingWebhookConfiguration` are cluster-scoped too, and the operator
  reconciles them at startup to point at its own Service and CA bundle. Two
  operators would each rewrite them to point at itself, and the loser's
  databases would be admitted - or rejected - by the winner's webhook.

So "namespace scope" does not mean "an operator per namespace". It means **the
single operator lives in the namespace it serves and watches only that
namespace**, which is a sensible thing to want when the cluster has one
environment in it and a wrong thing to attempt when it has two.

## Which to choose

| | cluster-scoped | namespace-scoped |
|---|---|---|
| operator lives in | `cnpg-system` | the environment's own namespace |
| watches | every namespace | one |
| RBAC | ClusterRole | Role, plus the irreducible cluster-scoped parts |
| **one cluster, two environments** | **required** | breaks: two operators, one webhook |
| one cluster, one environment | works | **preferred** |

**Namespace scope is the better answer when it applies**, for the ordinary
reason: the operator holds a Role rather than a ClusterRole, so a bug or a
compromise reaches the namespace it serves and nothing else in the cluster. It
also puts the operator and the database it runs in one place, where somebody
debugging at 03:00 is already looking.

**Cluster scope is required as soon as one cluster carries both environments**,
and that has a consequence worth knowing before choosing that topology: a
shared operator means **you cannot prove an operator upgrade in lab first.**
Upgrading it upgrades production's operator in the same action, because it is
the same Deployment. Separate clusters do not have that problem, and it is one
of the better arguments for separating them.

Either way the CRDs are shared per cluster, so where lab and production share a
cluster their databases share a schema version whatever the operator scope is.

## Changing scope after a deployment

**It is a migration, not a toggle, and the repository is arranged so that
getting it wrong fails rather than half-works.**

Applying the other scope's bootstrap updates the `platform-operators`
Kustomization in place - same name - and points it at a different directory.
What it does NOT do is remove the operator the old scope installed: that is a
HelmRelease in a different namespace, and this layer has `prune: false` because
pruning an operator can take its CRDs and therefore every database with it.

So without care you would end up with two operators, each reconciling the same
cluster-scoped admission webhooks to point at itself, **both reporting healthy**.
That is why every scope pins `releaseName: cloudnative-pg` and
`storageNamespace: cnpg-system`: Helm keys a release by that pair, so the second
one cannot install. The switch stops with an error the failure Alert carries,
instead of succeeding into the broken state.

To actually move it:

```sh
# 1. Stop the environments reconciling while the operator is absent. Running
#    databases are NOT affected - the operator is a control plane, and the
#    PostgreSQL instances keep serving without it. What stops is failover.
flux -n flux-system suspend kustomization software-gateway-lab-database
flux -n flux-system suspend kustomization software-gateway-lab-platform

# 2. PROTECT THE CRDs BEFORE REMOVING ANYTHING. Deleting a CRD deletes every
#    object of that kind - here, every Cluster, which is every database. This
#    annotation tells Helm to leave them behind on uninstall.
for crd in $(kubectl get crd -o name | grep cnpg.io); do
  kubectl annotate "$crd" helm.sh/resource-policy=keep --overwrite
done

# 3. Remove the old operator. Check the CRDs are still there before going on -
#    if this list is empty, STOP and restore them before step 4.
flux -n swgw-lab delete helmrelease cloudnative-pg      # or -n cnpg-system
kubectl get crd | grep cnpg.io

# 4. Apply the scope you want.
kubectl apply -k deploy/flux/platform/bootstrap/cluster-scoped

# 5. One operator, and the databases still there.
kubectl get deploy -A -l app.kubernetes.io/name=cloudnative-pg   # exactly one
kubectl get cluster.postgresql.cnpg.io -A

# 6. Resume.
flux -n flux-system resume kustomization software-gateway-lab-database
flux -n flux-system resume kustomization software-gateway-lab-platform
```

Step 2 is the one that matters. Everything else is recoverable by re-running it.

## What it does not do

It does not create the environment namespace. In namespace scope the operator
is placed INTO a namespace that `deploy/environments/<env>` owns - deliberately,
because an operator that created the namespace its database lives in would take
the database with it when it was removed.
