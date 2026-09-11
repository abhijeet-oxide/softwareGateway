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

## What it does not do

It does not create the environment namespace. In namespace scope the operator
is placed INTO a namespace that `deploy/environments/<env>` owns - deliberately,
because an operator that created the namespace its database lives in would take
the database with it when it was removed.
