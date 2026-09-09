# Registry credentials

A product that is not `anonymous: true` names a secret:

```yaml
sources:
  - name: vendor
    registry: registry.internal.example.com:9443
    repository: vendor/software-01
    credentialsRef:
      secretName: internal-registry
```

and every component resolves it the same way, on every runtime:

```
<config dir>/secrets/<secretName>/<key>
```

`username` and `password` are the keys a registry credential needs. That is a
projected Kubernetes Secret volume, and it is deliberately not the Kubernetes
API: no client-go, no cluster-wide Secret read permission, no API-server load,
and the same code path works against a plain directory on a laptop.
See `internal/product/secrets.go`.

## The two directories here

**`manifests/` is committed.** VaultStaticSecret / ExternalSecret / SealedSecret
documents - whatever this organization uses to get a Secret into a cluster. They
carry a REFERENCE to a value, never a value, which is what makes them reviewable
in a pull request. Flux applies them; the operator writes the Secret; the
Deployment projects it at `/etc/softwaregateway/secrets/<name>/`.

**`local/` is not committed, and never will be.** It is the same shape, filled
in by hand, bind-mounted by `docker-compose.yml` at the same path. A developer
gets the same layout as production without a Vault:

```sh
mkdir -p data/secrets/local/internal-registry
printf '%s' 'svc-account'        > data/secrets/local/internal-registry/username
printf '%s' 'the-password'       > data/secrets/local/internal-registry/password
```

`printf` rather than `echo`, because `echo` appends a newline and a newline in a
password is a 401 nobody can see.

## What happens when one is missing

The loader reports it at load, naming the exact path it looked for, and the
product is marked invalid rather than the process failing. So a missing
credential takes one product out of service and says which file and which path;
it does not take the deployment down.
