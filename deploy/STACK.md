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

## Podman: `archive/tar: write too long`

```
archive/tar: write too long
Error: Post "http://d/v5.5.2/libpod/build?...&secrets=["id=netrc,src=podman-build-secret311835887"]": io: read/write on closed pipe
```

This is an open podman bug, and the trigger is **a build secret whose file is
not empty**.

podman's remote client - podman machine, so every Windows and macOS host -
cannot pass a build secret to the server over the wire. It copies each secret
INTO THE BUILD CONTEXT, keeps the handle open, and tars the context. On Windows
the size in the tar header comes from the directory entry, which still reads
zero while podman's writes sit unflushed, so the copy delivers the real bytes
and `archive/tar` refuses them. A zero byte secret is immune, because zero is
what the header promised.

Upstream: [containers/podman#26914](https://github.com/containers/podman/issues/26914)
("empty secret files work, any non-empty file causes the build to fail"),
[#17899](https://github.com/containers/podman/issues/17899),
[#23815](https://github.com/containers/podman/issues/23815).

**With no credentials configured this now works**, because
`deploy/npm/npmrc.default` and `deploy/go/netrc.default` are zero bytes.
They used to carry a comment saying "Intentionally empty" while being 423 and
215 bytes, and on Windows those comments broke every build.
`deploy/deploy_test.go` fails if either file grows again.

**With credentials configured it does not**, because a credentialed npmrc or
netrc is not empty. Until podman fixes it, the choices are:

- Build that one image with Docker, which is unaffected: buildx streams secrets
  over its session rather than through the context.
- Build inside the podman machine (`podman machine ssh`, then `podman build` on
  a checkout there). A local build reads the secret from disk and never copies
  it into the context.
- Put the credential in the registry URL instead - `NPM_REGISTRY` for npm,
  `GOPROXY` for Go - and accept the exposure. **These are build arguments.**
  They are recorded in the build request, printed in full in any error the
  build reports, and kept in the build cache. Anyone who is sent a screenshot
  of a failed build is sent the token with it. They do not reach the shipped
  image, because both are used only in a discarded build stage, but treat a
  token used this way as published and rotate it when you stop.

Things that are not the fix: `--parallel 1` (the failure needs no concurrency;
a single build fails on its own), a smaller build context, and ignoring
`podman-build-secret*` in `.dockerignore` - podman ships the secret to the
server as part of the context, so excluding it means the build cannot find its
secret at all ([#25314](https://github.com/containers/podman/issues/25314)).

See [docs/design/24 - Identity and Access](../docs/design/24-identity-and-access.md)
for why it is built this way, including four environment behaviours that are
easy to get wrong and cost real debugging time.
