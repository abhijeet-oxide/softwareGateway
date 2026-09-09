# Product configuration

Drop product YAML documents here. They are mounted read-only into the
controller, which watches the directory and hot-reloads on change.

This directory is the SOURCE OF TRUTH for both halves. It used to be one of two:
this said what the gateway replicates and `GATEWAY_PRODUCTS` in `.env` said what
people could be granted access to, in a different format, applied a different
way, with nothing checking that they named the same products - so a product
could exist that nobody could be granted access to, and nothing said so.

Now a document here creates the product's ZITADEL project and its roles as well,
and the seeder refuses to run when nobody in `../users/users.yaml` holds the
owner role on it. See [`../README.md`](../README.md).

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
