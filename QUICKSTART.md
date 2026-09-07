# Quick start

Everything below is one machine, one command, no manual setup.

```bash
git clone https://github.com/abhijeet-oxide/softwareGateway
cd softwareGateway
docker compose up -d          # podman compose up -d works too
```

**That is the whole first run. There is no `.env` to create.** Every variable
has a working default, so a fresh clone comes up seeded: the `default` tenant,
three sample products, every role, three test users, two API users and an
administrator you can sign in as.

Then open **http://localhost:8000** and sign in as `admin` /
`INSECURE-local-admin-Passw0rd!`.

The defaults are insecure and say so in their own values. The seeder prints a
banner at the end of every run naming exactly which ones are still at their
default. For anything reachable by another person:

```bash
cp .env.example .env          # then edit it
docker compose up -d
docker compose run --rm zitadel-init
```

> **Why not require the variables?** An earlier version did, using compose's
> `${VAR:?message}` syntax. It is correct and unusable: `podman compose` turns
> a missing variable into a thirty-line Python traceback with the message
> buried in the last line, so the first thing a new user sees is a stack dump
> rather than "set POSTGRES_PASSWORD". Working defaults plus a loud banner
> fails better.

| What | URL | Sign in with |
|---|---|---|
| Software Gateway | http://localhost:8000 | `BOOTSTRAP_ADMIN_USERNAME` / `BOOTSTRAP_ADMIN_PASSWORD` |
| ZITADEL console (manage users) | http://localhost:8090/ui/console | the same account |
| Controller API | http://localhost:8000/api/v1 | `Authorization: Bearer <token>` |

The API is deliberately not published on its own port. It is reached through
the web tier, which keeps the browser same-origin and lets the controller run
more than one replica.

---

## 1. Every environment variable

### The ones that matter

Nothing is required to start. These are the ones to change before anyone else
can reach the system.

| Variable | Default | What it is |
|---|---|---|
| `POSTGRES_PASSWORD` | `INSECURE-local-dev-password` | Database password, shared by the gateway and ZITADEL. |
| `ZITADEL_MASTERKEY` | `INSECURE-local-dev-masterkey-32c` | **Exactly 32 characters** - ZITADEL refuses to start otherwise, and says so from inside a migration failure. `openssl rand -hex 16`. Encrypts every stored secret; lose it and you lose them all. |
| `BOOTSTRAP_ADMIN_PASSWORD` | `INSECURE-local-admin-Passw0rd!` | The first administrator. The seeder **refuses to run** if this is set while SSO is configured. |

### Ports and scale

| Variable | Default | What it is |
|---|---|---|
| `WEB_PORT` | `8000` | The UI, and the API beneath it at `/api/v1`. |
| `ZITADEL_PORT` | `8090` | The identity console. |
| `ZITADEL_EXTERNAL_DOMAIN` | `localhost` | The host people type. **Must match how the browser reaches ZITADEL**, or every token is rejected: it is what lands in the token's `iss`. |
| `CONTROLLER_REPLICAS` | `1` | Control plane. Leader election keeps one active; the rest are warm. |
| `WORKER_REPLICAS` | `2` | Data plane. Stateless, so raise it for throughput. |

### The first administrator

Set **exactly one** of these.

| Variable | Use |
|---|---|
| `BOOTSTRAP_ADMIN_PASSWORD` | Local and demo. **The seeder refuses to run if this is set while SSO is configured**, so the shortcut cannot reach production. |
| `BOOTSTRAP_ADMIN_EMAIL` alone | Production. The account is created with **no password at all** - nothing to leak, nothing to rotate. |

`BOOTSTRAP_ADMIN_USERNAME` (default `admin`) names it either way. Its only job
is to appoint a real administrator; disable it once you have one.

### Microsoft SSO

Leave `SSO_ISSUER` empty for username and password login. Fill all three and
login redirects straight to Microsoft; ZITADEL's own form is never shown.

| Variable | Where it comes from |
|---|---|
| `SSO_ISSUER` | `https://login.microsoftonline.com/<directory-tenant-id>/v2.0` |
| `SSO_CLIENT_ID` | Entra → App registrations → your app → **Application (client) ID** |
| `SSO_CLIENT_SECRET` | Entra → your app → Certificates & secrets → **New client secret** |
| `SSO_DISPLAY_NAME` | The button label. Default `Microsoft`. |
| `SSO_AUTO_REDIRECT` | `true` skips the ZITADEL form entirely. |

In Entra, set the app's **redirect URI** to
`http://localhost:8090/ui/login/login/externalidp/callback`
(swap `localhost:8090` for your real `ZITADEL_EXTERNAL_DOMAIN` and port).

After filling these in, apply them:

```bash
docker compose run --rm zitadel-init
```

### Tenant, products, roles

| Variable | Default | What it is |
|---|---|---|
| `GATEWAY_TENANT` | `default` | The ZITADEL organization. Anything unspecified belongs to it. Leave it alone until you genuinely have a second tenant. |
| `GATEWAY_PRODUCTS` | `software-01,...` | One ZITADEL project per entry. This is what people are granted access **to**. |
| `GATEWAY_ORG_ROLES` | `org-admin,org-operator,org-security,org-reader` | Tenant-wide. Name no product. |
| `GATEWAY_PRODUCT_ROLES` | `product-owner,product-operator,product-reader` | Per product. Stored as `<product>:<role>`. |
| `AUTH_ENABLED` | `true` | `false` makes every caller anonymous. Local debugging only. |

---

## 2. Roles

The prefix is the scope. The suffix is the level.

| Role | May do | Over |
|---|---|---|
| `org-admin` | everything | every product, **including ones added later** |
| `org-operator` | read, request, retry | every product |
| `org-security` | read security detail | every product |
| `org-reader` | read | every product |
| `product-owner` | everything | one product |
| `product-operator` | read, request, retry | one product |
| `product-reader` | read | one product |

An `org-` role names no product, so a product created next month is covered
with **no re-login and no new grant**. That is the whole reason the tier exists.

---

## 3. Adding people: the file IS the deployment

`deploy/zitadel/users.json` is the provisioning mechanism. Edit it, commit it,
re-run the init container. Who has access is then reviewable in a pull request
rather than being clicks in a console nobody can audit later.

```jsonc
{
  "users": [
    {
      "username": "dana",
      "email": "dana@example.com",
      "firstName": "Dana", "lastName": "Okafor",
      "password": "OnlyForLocalUse!23",       // ignored when SSO is configured
      "orgRoles": ["org-security"],            // tenant-wide
      "products": { "software-01": ["product-owner"] }
    }
  ],
  "apiUsers": [
    {
      "username": "ci-deployer",
      "description": "pipeline that requests downloads",
      "orgRoles": [],
      "products": { "software-01": ["product-operator"] }
    }
  ]
}
```

Apply it:

```bash
docker compose run --rm zitadel-init
```

**It is idempotent.** Existing users are left alone, missing grants are added,
nothing is ever removed. Run it as often as you like - this is the intended
continuous-deployment path for access changes.

API user secrets are printed **once**, in that command's output, because
ZITADEL does not store them retrievably. Capture them then. To reissue one, add
`"rotateSecret": true` to that entry, run the command, then take the flag back
out - left in, it rotates on every `docker compose up` and invalidates whatever
is using it.

### Adding a product

```bash
# 1. add it to GATEWAY_PRODUCTS in .env
# 2. put its product YAML in deploy/products/
docker compose run --rm zitadel-init
```

Only the new project and its roles are created.

### Adding a role

Add it to `GATEWAY_ORG_ROLES` or `GATEWAY_PRODUCT_ROLES`, re-run the init
container, then say what it may do in `deploy/cerbos/policies/`. The two halves
are deliberately separate: **ZITADEL holds roles, Cerbos holds permissions**,
so a new gated endpoint is one line of policy and one code change in one
commit, with no console work and no re-assigning anybody.

---

## 4. Permissions

`deploy/cerbos/policies/` is plain YAML, versioned with the code it gates.

```yaml
# security.yaml
rules:
  # An org role names no product, so this covers future products too.
  - actions: ["view", "export"]
    effect: EFFECT_ALLOW
    derivedRoles: [org_wide_security]
  # A product role must match the product being touched.
  - actions: ["view"]
    effect: EFFECT_ALLOW
    derivedRoles: [product_reader]
```

Cerbos watches the directory, so an edit is live without a restart. Because a
decision is a pure function of roles, resource and action, the policies are
testable in CI with nothing else running.

---

## 5. Running under Podman

`podman compose up -d` works. Two differences worth knowing:

- **`deploy.replicas` is ignored.** `CONTROLLER_REPLICAS` and `WORKER_REPLICAS`
  are honoured by Docker Compose; podman-compose does not implement that key,
  so you get one of each. Nothing breaks, you just do not get the scale-out.
- **`podman ps ... exit status 125`** during `up` is Podman itself failing, not
  this stack. On Windows and macOS it almost always means the Podman machine is
  not running: `podman machine start`.

## 6. Everyday commands

```bash
docker compose up -d                    # start, in dependency order
docker compose ps                       # health of every service
docker compose logs -f controller       # follow one service
docker compose run --rm zitadel-init    # apply users/products/roles changes
docker compose down                     # stop; data kept
docker compose down -v                  # stop and discard all data
```

## 7. Behind an internal registry or proxy

Every image is a variable with a pinned default, and nothing is tagged
`latest`. Versions this stack is verified against:

| Image | Pinned to | Registry |
|---|---|---|
| PostgreSQL | `16.15-alpine` | Docker Hub |
| **ZITADEL** | `v4.17.3` | **GHCR only, not on Docker Hub** |
| Cerbos | `0.55.0` | Docker Hub |
| Node (seeder, web build) | `22.23.2-alpine` | Docker Hub |
| nginx (web runtime) | `1.27.5-alpine` | Docker Hub |
| Go (build) | `1.25.14` | Docker Hub |
| distroless (Go runtime) | `static-debian12:nonroot` | gcr.io |

If your daemon has a pull-through cache configured, change nothing. If your
internal registry re-hosts images under its own path, set the overrides in
`.env`:

```bash
ZITADEL_IMAGE=artifactory.corp/ghcr/zitadel/zitadel:v4.17.3
CERBOS_IMAGE=artifactory.corp/dockerhub/cerbos/cerbos:0.55.0
POSTGRES_IMAGE=artifactory.corp/dockerhub/postgres:16.15-alpine
SEEDER_IMAGE=artifactory.corp/dockerhub/node:22.23.2-alpine
# build-time bases
GO_IMAGE=artifactory.corp/dockerhub/golang:1.25.14
NODE_IMAGE=artifactory.corp/dockerhub/node:22.23.2-alpine
NGINX_IMAGE=artifactory.corp/dockerhub/nginx:1.27.5-alpine
RUNTIME_IMAGE=artifactory.corp/gcr/distroless/static-debian12:nonroot
```

**ZITADEL is the one to mirror first** if your proxy reaches only one upstream:
it is published to GHCR and nowhere else.

## 8. Building behind a corporate proxy

A build container inherits nothing from the host. Without these the build fails
with `go mod download` TLS handshake timeouts and `UND_ERR_CONNECT_TIMEOUT` to
registry.npmjs.org, even though the host has working internet.

```bash
HTTP_PROXY=http://proxy.corp:8080
HTTPS_PROXY=http://proxy.corp:8080
NO_PROXY=localhost,127.0.0.1,.corp
```

> **The proxy must not be `localhost` or `127.0.0.1`.** Inside the container
> that address is the container itself, and the build fails with
> `proxyconnect tcp: dial tcp 127.0.0.1:PORT: connect: connection refused`.
> Use the proxy's real hostname, or `host.containers.internal` (Podman) /
> `host.docker.internal` (Docker) when it really does run on your machine.

Better still, point at internal mirrors and skip the proxy for these fetches:

```bash
GOPROXY=https://artifactory.corp/api/go/go,direct
NPM_REGISTRY=https://artifactory.corp/api/npm/npm/
```

## 9. Authenticated internal registries

A registry URL is not secret; a token is. Credentials are passed as **mounted
build secrets**, so they exist only for the one build step that needs them and
never reach an image layer.

```bash
cp deploy/npm/npmrc.example deploy/npm/npmrc     # npm / pnpm
cp deploy/go/netrc.example  deploy/go/netrc      # Go module proxy
```

Fill them in, then in `.env`:

```bash
NPM_REGISTRY=https://artifactory.corp/api/npm/npm-remote/
NPM_CONFIG_FILE=./deploy/npm/npmrc

GOPROXY=https://artifactory.corp/api/go/go,direct
GOSUMDB=off                     # a private proxy cannot serve public checksums
GO_NETRC_FILE=./deploy/go/netrc
```

Both files are gitignored. `docker compose build` and `podman compose build`
both support this; podman passes it through as `--secret`.

> **Why not a build argument?** `--build-arg NPM_TOKEN=...` is baked into the
> image's history and `docker history --no-trunc` prints it to anyone who can
> pull the image. Verified on this repository: with a token in the npmrc, it
> appears **zero** times in `docker history`, zero times in the saved image
> tarball, and `/root/.npmrc` does not exist in the final image.

> **Do not set `strict-ssl=false`.** If the registry uses your own CA, put the
> CA in `deploy/certs/*.crt` (section 10) and verification keeps working.

## 10. Behind a TLS-intercepting proxy

If `docker compose build` fails with `SELF_SIGNED_CERT_IN_CHAIN` or
`x509: certificate signed by unknown authority`, drop your proxy's CA into
`deploy/certs/*.crt` and rebuild. The images trust anything there. Do not
disable certificate verification.

## 11. If something is wrong

| Symptom | Cause |
|---|---|
| Every token rejected, `iss` mismatch | `ZITADEL_EXTERNAL_DOMAIN` is not how the browser reaches ZITADEL. |
| ZITADEL answers 404 to a healthy service | The `Host` header does not match `ZITADEL_EXTERNAL_DOMAIN`. |
| `controller` unhealthy at boot | It fails fast on broken auth config rather than starting and refusing everyone. Read its logs. |
| Worker restarting | No valid product YAML in `deploy/products/`. A worker says so rather than leasing jobs it cannot run. |
| `go mod download` TLS handshake timeout, or `UND_ERR_CONNECT_TIMEOUT` to registry.npmjs.org | The build container has no proxy. Section 9. |
| npm `E401`/`E403` against an internal registry | Credentials are missing. Section 9, `NPM_CONFIG_FILE`. |
| `EROFS` / "rofs that don't support symlinks" during install | Something ran `npm config set` while `/root/.npmrc` was a read-only secret mount. Configure via `NPM_CONFIG_*` env instead. |
| `proxyconnect tcp: dial tcp 127.0.0.1:PORT: connection refused` | The proxy is set to localhost, which inside a container is the container. Section 9. |
| `masterkey must be 32 bytes, but is 33` | `ZITADEL_MASTERKEY` is the wrong length. Count it. |
| A wall of Python traceback from `podman compose` | Usually Podman itself. Check `podman machine start` first. |

Design and rationale: [docs/design/24 - Identity and Access](../docs/design/24-identity-and-access.md).
