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

## The list itself

**`secrets.yaml` is the inventory**: every credential this deployment needs, by
name, with its keys and a path relative to whatever root the environment uses.
No values, and there is nowhere in it to put one. Three things read it and none
of them restates it - the chart renders one `VaultStaticSecret` or
`SecretProviderClass` per entry, `task secrets:scaffold` writes the local
layout from it, and `go test ./deploy/...` fails when a product references
something it does not declare.

```sh
task secrets:scaffold      # one directory per secret, one EMPTY file per key
```

Empty rather than a placeholder: a file containing `CHANGEME` is a credential
the registry rejects with a 401 that names nothing, and an empty one is
reported at load, by path, as the missing value it is.

## The two directories here

**`manifests/` is committed.** Worked examples of the documents the chart
renders from `secrets.yaml` - a VaultStaticSecret, an ExternalSecret - kept so
a reader can see the shape without rendering a chart. They carry a REFERENCE to
a value, never a value. In a deployment the chart produces them from the
inventory rather than these files being applied directly, so there is one list
and not two.

**`local/` is not committed, and never will be.** It is the same shape, filled
in by hand, bind-mounted by `docker-compose.yml` at the same path. A developer
gets the same layout as production without a Vault:

```sh
mkdir -p config/secrets/local/internal-registry
printf '%s' 'svc-account'        > config/secrets/local/internal-registry/username
printf '%s' 'the-password'       > config/secrets/local/internal-registry/password
```

`printf` rather than `echo`, because `echo` appends a newline and a newline in a
password is a 401 nobody can see.

## What happens when one is missing

The loader reports it at load, naming the exact path it looked for, and the
product is marked invalid rather than the process failing. So a missing
credential takes one product out of service and says which file and which path;
it does not take the deployment down.
