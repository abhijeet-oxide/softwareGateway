# Example values files

One file is the whole of what a deployment differs in. Copy the closest of these
to `deploy/flux/instances/<name>/values/values.yaml` and edit it, or pass it to
`helm install --values`.

| file | the situation it is for |
|---|---|
| [`minimal.yaml`](minimal.yaml) | an evaluation on a cluster with egress, reached by `kubectl port-forward` |
| [`aks-lab-no-dns.yaml`](aks-lab-no-dns.yaml) | an internal AKS lab: private registry, private load balancer addresses, no DNS and no certificates |
| [`reverse-proxy-no-dns.yaml`](reverse-proxy-no-dns.yaml) | your own nginx in front on one private IP with a self-signed certificate — includes the nginx config |
| [`aks-production.yaml`](aks-production.yaml) | AKS with DNS, TLS, Entra single sign-on, Key Vault credentials and database backups |
| [`openshift.yaml`](openshift.yaml) | OpenShift: `Route` instead of `Ingress`, and the SCC note that matters |
| [`existing-database.yaml`](existing-database.yaml) | a managed PostgreSQL service, or a cluster somebody else runs |

Every key not in a file is a chart default. Two ways to read them:

```sh
helm show values oci://<registry>/charts/software-gateway
```

or [`../charts/software-gateway/values.yaml`](../charts/software-gateway/values.yaml),
which is the same file with the reasoning attached.

## Checking one before deploying it

```sh
task chart:stage
helm template swgw deploy/charts/software-gateway --values deploy/examples/aks-lab-no-dns.yaml
```

The chart refuses to render a configuration that would come up green and not
work: an issuer no entry point serves, a half-mirrored registry, a pull secret
nothing creates, an `Ingress` routing on an IP literal. `values.schema.json`
catches the misspelled keys before any of that.

## The three settings that decide everything else

**`access.webUrl` and `access.identityUrl`** are the two addresses a browser
uses, written as whole URLs. Scheme, host and port are read out of them, so
there is nothing to keep in step. `access.expose.type` says how they are
published — `none` when a proxy you already run points at the `web` and
`zitadel-proxy` Services, or `ingress`, `route`, `loadBalancer`, `nodePort`.

**`images.registry` and `images.mirror`** decide whether the deployment reaches
the internet. `registry` is where this product's own three images live; `mirror`
is a repository proxying Docker Hub and ghcr.io, and every third-party image is
rewritten through it. Setting the first without the second gives a cluster that
pulls three images from you and six from the internet — not an air-gapped
deployment, and indistinguishable from one until the first node without egress,
so the chart refuses it.

The image **versions** are chart defaults, pinned and tested together. A values
file names a registry, never a tag.

**`database.cluster.name`** decides who owns the database. Named, and the
`database` layer provisions a CloudNativePG cluster and derives `database.host`
and `database.existingSecret` from it. Empty, and you state those two yourself.
