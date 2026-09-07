# Product configuration

Drop product YAML documents here. They are mounted read-only into the
controller, which watches the directory and hot-reloads on change.

This directory is separate from `GATEWAY_PRODUCTS` in `.env`, and the split is
deliberate: this is what the gateway REPLICATES, that is what people can be
granted ACCESS to. They are expected to name the same products, and
`transferctl config check` reports a product configured here with no project in
ZITADEL.

See docs/design/02-configuration.md for the document schema.

## The shipped samples

`software-01/02/03.yaml` are credential-free on purpose: they use
`anonymous: true` so `docker compose up` produces a green stack on a machine
with no registry credentials at all. They point at `*.example.com`, so
discovery finds nothing - which is correct for a first run and wrong for a real
one.

For a real product, replace `anonymous: true` with:

```yaml
credentialsRef:
  secretName: internal-registry
```

and mount the secret at `/etc/softwaregateway/secrets/internal-registry/` with
`username` and `password` files. The controller validates this at load and
names the exact missing path, so a mistake here is reported at startup rather
than at the first transfer.
