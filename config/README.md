# config/ - what an administrator manages

Everything in here is CONTENT: the products this gateway replicates, the people
who may use it, the roles they hold, the policies that decide what a role may
do, and the credentials the products need. It changes without a release.

Everything in `deploy/` is MACHINERY: Dockerfiles, nginx configuration, the
seeder, the database's init script. It changes with the code.

That line is the whole point of the split, and it is what makes one directory
serve both deployment paths:

| | local | cluster |
|---|---|---|
| how it arrives | `docker-compose.yml` bind-mounts `${CONFIG_DIR:-./config}` | Flux reconciles this directory |
| how it is applied | `docker compose run --rm zitadel-init` | the seeding Job, or a CI step |
| what reads it | the same binaries, at the same paths | the same binaries, at the same paths |

There is no second format, no rendering step and no per-environment fork. A
change here is reviewed once and applied the same way in both.

```
config/
  access/
    roles.yaml          the roles that exist: tenant-wide, and per product
    policies/           Cerbos policies - what each role may actually do
  users/
    users.yaml          who may sign in, and their role on each product
  products/
    *.yaml              what this gateway replicates, one file per product
  secrets/
    manifests/          VSO / ExternalSecret documents. Committed, no values.
    local/              the same layout filled in by hand. Never committed.
```

## The one rule that connects them

**Every product in `products/` must have an owner in `users/users.yaml`.**

Seeding creates a project per product with the roles from `access/roles.yaml`,
and refuses to finish if a product has nobody holding `ownerRole`. The grants
themselves are written on the `platform` project, where the role keys are
namespaced `<product>:<role>` - that is the only project whose roles reach a
token, so a grant anywhere else grants nothing.

Everybody in `users/users.yaml` is also granted `tenant.baselineRole`, which
carries no permission and marks the account as provisioned here. It is what
separates a colleague waiting on a product grant from somebody the identity
provider let in whom nobody has heard of. A product
nobody owns is a product whose downloads nobody can approve, and the right time
to find that out is the pull request rather than the incident.

`go test ./deploy/...` enforces the same invariant in CI, so the pull request
that adds a product without an owner fails before anything is deployed.

## Applying a change

```sh
docker compose run --rm zitadel-init      # roles, products, people
docker compose restart zitadel-login      # only if the sign-in screen changed
```

Products and policies need neither: the controller and every worker watch their
directories and reload in place, and Cerbos watches its own.
