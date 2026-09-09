# Secret manifests

One document per secret, referencing a value rather than carrying one.

Nothing here is applied by `docker compose`: locally the values are files in
`../local/`, which is the same layout without an operator to produce it. These
are for the cluster, applied by Flux alongside everything else in `data/`.

A VaultStaticSecret, for the Vault Secrets Operator:

```yaml
apiVersion: secrets.hashicorp.com/v1beta1
kind: VaultStaticSecret
metadata:
  name: internal-registry
spec:
  mount: kv
  type: kv-v2
  path: softwaregateway/internal-registry
  refreshAfter: 1h
  destination:
    name: internal-registry     # must match credentialsRef.secretName
    create: true
```

The Secret's keys become the file names under
`/etc/softwaregateway/secrets/internal-registry/`, so the KV entry needs
`username` and `password` and nothing else has to line up.
