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
| `CONFIG_DIR` | `./config` | Where the products, people, roles and policies are. Point it at a checkout of the configuration repository to keep content apart from code. |
| `AUTH_ENABLED` | `true` | `false` makes every caller anonymous. Local debugging only. |

The products, the roles and the people are **not** environment variables. They
are files in `CONFIG_DIR`, because they are content an administrator manages
rather than settings a deployment is tuned with - and because the same
directory is what Flux reconciles in a cluster. See [`config/README.md`](config/README.md).

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

`config/users/users.yaml` is the provisioning mechanism. Edit it, commit it,
re-run the init container. Who has access is then reviewable in a pull request
rather than being clicks in a console nobody can audit later.

```yaml
users:
  - email: dana@example.com          # username is optional; it defaults to this
    firstName: Dana
    lastName: Okafor
    password: OnlyForLocalUse!23     # ignored when SSO is configured
    orgRoles: [org-security]         # tenant-wide
    products:
      software-01: [product-owner]

apiUsers:
  - username: ci-deployer
    description: pipeline that requests downloads
    orgRoles: []
    products:
      software-01: [product-operator]
```

**One person's name in two domains.** `test@domain1.com` and `test@domain2.com`
are two different people, and a username is unique across the whole instance -
so they cannot both be `test`. Leave `username` out of both rather than
inventing `test2` for the second: each then signs in as their own address,
which was unique to begin with. (Typing the address works either way - the
sign-in screen looks a typed value up as a login name first and as an address
second - but the default leaves one string to know instead of two.) Two entries
sharing one address are refused: the address is how a re-run finds an existing
account, so they would be one account with both sets of roles on it.

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
# 1. write config/products/software-04.yaml
# 2. give it an owner in config/users/users.yaml:
#        products:
#          software-04: [product-owner]
docker compose run --rm zitadel-init
```

Only the new project and its roles are created. Step 2 is not optional: the
seeder refuses a product nobody owns, and `go test ./deploy/...` fails the pull
request that adds one.

The **replication** side needs nothing at all. The controller and every worker
watch `config/products` and reload in place, so the new product is visible in the
interface within seconds and no worker is restarted.

### Adding a role

Add it to `config/access/roles.yaml`, re-run the init container, then say what it
may do in `config/access/policies/` beside it. The two halves
are deliberately separate: **ZITADEL holds roles, Cerbos holds permissions**,
so a new gated endpoint is one line of policy and one code change in one
commit, with no console work and no re-assigning anybody.

---

## 4. Permissions

`config/access/policies/` is plain YAML, versioned with the code it gates.

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

## 5.1 Cloning on Windows

Nothing to configure. `.gitattributes` pins every text file to LF, which beats
`core.autocrlf` because it is a per-path instruction a client setting cannot
override, so a Windows clone gets the same bytes as any other.

**In a clone made before that file existed**, the working tree is still whatever
it was checked out as:

```bash
git add --renormalize .
git status          # shows the files whose endings changed
```

This matters because the artifacts are Linux containers and a shell script with
CRLF does not run: the shebang becomes `#!/bin/sh\r`, the kernel looks for a
program of that name, and the container dies reporting that the script is
missing. It is not.

Three things now have to fail before that reaches a container: the checkout
(above), the image build (Dockerfiles strip carriage returns from any script
they copy), the container start (bind-mounted entrypoints are stripped in
place), and `go test ./deploy/...` fails on any script committed with CRLF.

## 6. Everyday commands

```bash
docker compose up -d                    # start, in dependency order
docker compose ps                       # health of every service
docker compose logs -f controller       # follow one service
docker compose run --rm zitadel-init    # apply users/products/roles changes
docker compose down                     # stop; data kept
docker compose down -v                  # stop and discard all data
```

### What `ps` actually tells you

```bash
docker compose ps      # podman ps
```

**A green worker means the Coordinator has accepted it**, not merely that the
process started: healthy here means the worker reached the Coordinator,
authenticated, and had a lease call served, so it is in the fleet and will be
given work. An idle queue is still green - being told there is nothing to do is
the Coordinator accepting the request, which is the whole test.

That is a deliberate reversal. It used to mean only that the process was up and
the control plane answered a probe, which reported a green fleet over a
deployment where every lease was being refused for want of a credential. If `ps`
is green now, the deployment works.

It takes up to 45 seconds after start for a worker to go green, and up to 50
seconds of refusal for it to go red, so a Coordinator being restarted does not
flap the fleet.

Two things `ps` does NOT tell you, on purpose:

- **The Coordinator is not unhealthy for having no workers.** It serves the API
  and the UI perfectly well with an empty fleet; that is a scale question, not a
  fault. Green controller plus red workers is the honest picture of exactly that
  situation.
- **Nothing here is a liveness verdict.** A worker the Coordinator refuses is
  reported red and is deliberately never restarted for it: restarting fixes none
  of the causes, and would turn one bad credential into a crash loop across the
  fleet.

### Upgrading a stack that is already running

Pulling new code needs **no `down`**, and specifically no `down -v`: that
discards the database and the whole identity provider, so every user, role and
grant would have to be seeded again. Nothing here needs it.

```bash
git pull
docker compose build controller worker web   # only the images whose code moved
docker compose run --rm zitadel-init         # apply anything new in the seeder
docker compose up -d                         # recreate what changed
```

Read it as three separate facts:

- **Build only what moved.** Go changes are `controller` and `worker`; anything
  under `web/src` is `web`. The seeder is a bind-mounted script, not an image,
  so a change to `deploy/zitadel/bootstrap.mjs` needs no build at all.
- **Run the seeder before `up`, not after.** It is idempotent, safe on a running
  stack, and it is what creates anything new in the identity provider - a role,
  a machine account, a set of credentials. Running it first means the services
  come up to a directory that already has what they are about to ask for.
  Nothing is lost if you get the order wrong: see the next point.
- **`up -d` recreates only containers whose image or definition changed.**
  Postgres and ZITADEL keep their volumes and are not restarted unless you
  changed them.

> **Under podman, run the seeder explicitly.** `depends_on` conditions are not
> implemented by podman-compose, so `podman compose up -d` will not run
> `zitadel-init` for you and will not wait for it. The command above does it by
> hand, which is correct on both runtimes.
>
> It is not fragile either way: a worker reads its credentials when it needs a
> token rather than once at startup, so one that comes up before the seeder has
> written them retries every five seconds, logs
> `no workload credentials yet`, and starts leasing on its own within seconds of
> the file appearing. No restart, no ordering to get right.

Build **sequentially**, not with `--parallel`, if any of your build secrets are
non-empty: podman copies each secret into the build context, and concurrent
builds sharing that context corrupt each other (§8).

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
| Worker up but doing nothing, logging `not leasing` | No valid product YAML in `config/products/`. A worker will not lease work it cannot execute, because attempts are counted when a job is handed out. It starts anyway, says so once, and begins working the moment a product is loaded - no restart. |
| Sign-in ends on "Account Not Found", or "This account is not recognised" | The address the identity provider asserted matches no account here. The seeder prints every account that can sign in, username and address together - compare that list against what the directory actually sends. If the address is right and it still fails, the directory is asserting something else (commonly a user principal name where there is no mail attribute): set `SSO_LINK_ON=username` and re-run the seeder. |
| Two people share a local part in different domains, and only one of them can be `test` | Do not invent `test2`. Leave `username` out of both entries in `config/users/users.yaml` and each signs in as their own address. On a stack that already carries the invented names, removing the `username` lines renames those accounts on the next seeding run - the seeder prints the rename, and it ends any session those people are holding. |
| Sign-in ends on "This account is not recognised" | Correct, and the point: accounts are provisioned here, never created by signing in. Add that person's address to `config/users/users.yaml` (or `BOOTSTRAP_ADMIN_EMAIL` for the administrator) and re-run the seeder. The seeder lists which addresses can sign in at the end of every run, and refuses to seed a stack where that list would be empty. |
| Signed in and every page says "This account has no access yet" | Correct, and the point: routes are refused to an account holding no roles. Grant one - the row below - and sign in again. |
| Signed in, but the profile says "Tenant roles: none" | The roles are on a different account. A sign-in through the identity provider creates its own account when nothing already holds that address, so the seeded one keeps the roles and the one you actually sign in as holds none. Set `BOOTSTRAP_ADMIN_EMAIL` to the address you sign in with, or add yourself to `config/users/users.yaml` with that address, and re-run the seeder - it matches on the address, so the roles land on the account you use. Roles arrive in the token, so sign out and back in. |
| Two accounts for you in ZITADEL's account switcher, one you cannot sign in to | Same cause. The seeder now names both at the end of its run. The leftover has no identity at the provider and password sign-in is off, which is exactly why it cannot be signed in to; delete it in the console under Users. |
| Accounts still there after `docker compose down` | `down` keeps volumes - only `down -v` discards them. ZITADEL's whole directory lives in the `pgdata` volume, so every user, role and grant survives a rebuild. That is what you want almost always, and it is why a duplicate made once stays until somebody removes it. |
| Worker `unhealthy` | The Coordinator is not accepting it, and the worker's own log says why: `docker compose logs worker`. `UNAUTHENTICATED: no bearer token` means its credentials have not been published - run `docker compose run --rm zitadel-init` and it recovers by itself within seconds, no restart. The full report is at `:8081/readyz` on the worker, which names the failing check. |
| `go mod download` TLS handshake timeout, or `UND_ERR_CONNECT_TIMEOUT` to registry.npmjs.org | The build container has no proxy. Section 9. |
| npm `E401`/`E403` against an internal registry | Credentials are missing. Section 9, `NPM_CONFIG_FILE`. |
| `EROFS` / "rofs that don't support symlinks" during install | Something ran `npm config set` while `/root/.npmrc` was a read-only secret mount. Configure via `NPM_CONFIG_*` env instead. |
| `proxyconnect tcp: dial tcp 127.0.0.1:PORT: connection refused` | The proxy is set to localhost, which inside a container is the container. Section 9. |
| `masterkey must be 32 bytes, but is 33` | `ZITADEL_MASTERKEY` is the wrong length. Count it. |
| `exec /docker-entrypoint.sh: no such file or directory`, on a file that is plainly there | CRLF line endings. The shebang reads `#!/bin/sh\r`, so the kernel looks for an interpreter named `/bin/sh\r` and the message names the script instead of the thing it could not find. Fixed at the root by `.gitattributes`; in a clone made before it, run `git add --renormalize .`. Images built from this repository strip carriage returns anyway, so rebuilding also clears it. |
| A bind-mounted script fails with `set: illegal option` or `nginx: not found` | The same CRLF, one layer along: the script is read by `sh` rather than exec'd, so every line ends in a carriage return instead. Same fix. |
| A wall of Python traceback from `podman compose` | Usually Podman itself. Check `podman machine start` first. |

Design and rationale: [docs/design/24 - Identity and Access](docs/design/24-identity-and-access.md).
