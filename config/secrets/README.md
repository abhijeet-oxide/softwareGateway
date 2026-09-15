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

## Adding one, end to end

A product that pushes to a registry needing credentials, from nothing to
working. Three edits and one command.

**1. Declare it in the inventory.** Name, keys, and the path the backend looks
under. No value.

```yaml
# config/secrets/secrets.yaml
  - name: acme-registry
    description: >-
      Service account for the ACME vendor registry that software-04 pulls from.
    keys: [username, password]
    path: acme-registry
```

**2. Name it on the product.**

```yaml
# config/products/software-04.yaml
spec:
  sources:
    - name: vendor
      registry: registry.acme.example.com:443
      repository: acme/software-04
      credentialsRef:
        secretName: acme-registry      # must match `name` above
```

Getting these two out of step is a test failure on the pull request, not a
deployment that comes up healthy and marks one product invalid.

**3. Create the Secret** where the deployment runs.

```sh
# A cluster, with no credentials operator (secrets.backend: none)
kubectl -n swgw-lab create secret generic acme-registry \
  --from-literal=username="$ACME_USERNAME" \
  --from-literal=password="$ACME_PASSWORD"

# Locally
mkdir -p config/secrets/local/acme-registry
printf '%s' "$ACME_USERNAME" > config/secrets/local/acme-registry/username
printf '%s' "$ACME_PASSWORD" > config/secrets/local/acme-registry/password
```

With an operator instead, create the entry in the vault at
`<secrets.vault.pathPrefix>/acme-registry` with those two keys and the chart
renders the rest. Nothing above this line changes.

**Check it.** `task flux:secrets -- <instance>` lists it as required from the
moment a product names it, and as optional before that.

```sh
kubectl -n swgw-lab logs -l app.kubernetes.io/component=coordinator | grep acme
#   product software-04 invalid: credential not found at
#   /etc/softwaregateway/secrets/acme-registry/password
```

## `local/`

**Not committed, and never will be.** It is the same shape, filled
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
