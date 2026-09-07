# Software Gateway - full stack

One command brings up the product, its database, an identity provider and a
policy engine, seeded and ready to use.

```bash
curl -O    https://raw.githubusercontent.com/abhijeet-oxide/softwareGateway/main/deploy/docker-compose.yml
curl -o .env https://raw.githubusercontent.com/abhijeet-oxide/softwareGateway/main/deploy/.env.example
# edit .env: at minimum POSTGRES_PASSWORD and ZITADEL_MASTERKEY
docker compose up -d
```

| What | Where | Credentials |
|---|---|---|
| Software Gateway | http://localhost:8000 | `admin` / `BOOTSTRAP_ADMIN_PASSWORD` |
| ZITADEL console | http://localhost:8090/ui/console | the same |
| Controller API | http://localhost:8080 | bearer token |

## What gets created for you

The `zitadel-init` container runs once and exits. It creates the `default`
tenant, one project per entry in `GATEWAY_PRODUCTS`, every role, the web OIDC
client, the Microsoft SSO connector when configured, and the first
administrator. Nothing is set up by hand.

It is **idempotent**. Add a product and re-run it; only the new one is created.

```bash
# add software-04 to GATEWAY_PRODUCTS in .env, then:
docker compose run --rm zitadel-init
```

## Roles

The prefix is the scope, the suffix is the level.

| Role | Covers |
|---|---|
| `org-admin` | everything, every product, **including products added later** |
| `org-operator` | request and retry on every product; not promote |
| `org-security` | security detail on every product |
| `org-reader` | read every product |
| `product-owner` | everything on one product |
| `product-operator` | request and retry on one product |
| `product-reader` | read one product |

An `org-` role names no product, so a product created next month is covered
with no re-login and no new grant.

Assign them in the ZITADEL console: **Organization → Projects → platform (or a
product) → Authorizations**.

## Turning on Microsoft SSO

Fill `SSO_ISSUER`, `SSO_CLIENT_ID` and `SSO_CLIENT_SECRET` in `.env`, then
re-run `docker compose run --rm zitadel-init`. Login then redirects straight to
Microsoft and ZITADEL's own form is never shown.

With SSO configured the seeder **refuses** to start if
`BOOTSTRAP_ADMIN_PASSWORD` is still set, so the local shortcut cannot reach
production.

## Everyday commands

```bash
docker compose up -d           # start, in dependency order
docker compose logs -f web     # follow one service
docker compose ps              # health of everything
docker compose down            # stop; databases kept
docker compose down -v         # stop and discard all data
```

See [docs/design/24 - Identity and Access](../docs/design/24-identity-and-access.md)
for why it is built this way, including four environment behaviours that are
easy to get wrong and cost real debugging time.
