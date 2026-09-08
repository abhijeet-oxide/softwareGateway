# 25. ZITADEL Login V2 service and the SPA's missing login redirect

Status: **not implemented**. This document is a handoff: it records two
confirmed, related gaps found while debugging a fresh `podman-compose up` of
this stack, with enough detail for another agent to implement both without
re-diagnosing from scratch. Neither fix has been started; no code below has
been written into the repository yet.

## Symptom this was debugging

On a fresh stack (`docker-compose.yml` at the repo root, `.env` with
`SSO_ISSUER`/`SSO_CLIENT_ID`/`SSO_CLIENT_SECRET` set so Microsoft SSO is
configured), opening `http://localhost:${WEB_PORT}` shows the SPA's
"Service unavailable / The Coordinator did not respond" screen forever, even
though `controller`, `zitadel`, `postgres`, `cerbos` are all healthy. Network
tab shows `GET /api/v1/system/version` and `GET /api/v1/whoami` both
returning `401`. Separately, following a link ZITADEL itself generated —
`http://localhost:${ZITADEL_PORT}/ui/v2/login/login?authRequest=...` — shows
a bare JSON body `{"code":5,"message":"Not Found"}` instead of a login page.

These are two different bugs with one connection: **nothing in this stack
can currently get a user from "not logged in" to "holds a valid token"**,
because (1) the SPA never asks ZITADEL to authenticate anyone and (2) even if
it asked, ZITADEL has nowhere to send the browser for the login page it
wants to use.

---

## Gap 1 — ZITADEL's Login V2 UI is a separate service we never deploy

### What's happening

Since ZITADEL v3/v4, the login screen is **not served by the core `zitadel`
binary**. It was split out into a standalone Next.js microservice,
published as `ghcr.io/zitadel/zitadel-login`. The core API container only
serves `/ui/console` (the admin console) and the gRPC/REST APIs; a reverse
proxy in front of both containers is expected to route:

- `/ui/v2/login/*` → the `zitadel-login` container (port `3000` by default)
- everything else → the core `zitadel` API container (port `8080`)

Confirmed against ZITADEL's own documentation and its official Docker
Compose reference (`https://zitadel.com/docs/self-hosting/deploy/compose`,
which states: *"The base stack runs: Traefik (reverse proxy) → ZITADEL API
(Go) + ZITADEL Login (Next.js) → PostgreSQL"*) and the upstream compose file
at `github.com/zitadel/zitadel/blob/main/deploy/compose/docker-compose.yml`.

This repository's `docker-compose.yml` only defines a `zitadel` service
(the core API/Go binary). There is no `zitadel-login` service and no proxy
rule for `/ui/v2/login/*`. So when ZITADEL generates a login URL under
`/ui/v2/login/login?authRequest=...` (which it does by default — see below),
that path is requested against the core API container, which does not own
it, and its gRPC-gateway answers the generic `{"code":5,"message":"Not
Found"}` (gRPC code 5 = `NOT_FOUND`) instead of a page.

### Why ZITADEL is generating `/ui/v2/login` URLs at all

The upstream compose file sets, on the core `zitadel-api` container:

```
ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_REQUIRED: true
ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_BASEURI: ${scheme}://${domain}:${port}/ui/v2/login/
ZITADEL_OIDC_DEFAULTLOGINURLV2: ${scheme}://${domain}:${port}/ui/v2/login/login?authRequest=
ZITADEL_OIDC_DEFAULTLOGOUTURLV2: ${scheme}://${domain}:${port}/ui/v2/login/logout?post_logout_redirect=
ZITADEL_SAML_DEFAULTLOGINURLV2: ${scheme}://${domain}:${port}/ui/v2/login/login?samlRequest=
```

This repository's `docker-compose.yml` `zitadel` service sets none of these,
yet a v4.17.3 instance is still handing back `/ui/v2/login/...` URLs — Login
V2 is evidently the default UI for a v4.x first-instance even without
`LOGINV2_REQUIRED` explicitly set. Whoever implements this should verify the
exact behavior on the pinned version (`ghcr.io/zitadel/zitadel:v4.17.3`,
see `ZITADEL_IMAGE` in `docker-compose.yml`) before deciding whether to
suppress Login V2 (see Option B) or embrace it (Option A).

### Fix option A (recommended) — deploy `zitadel-login` and route to it

This is what ZITADEL's own compose reference does, minus Traefik (this
stack's web tier is already nginx and already does path-based proxying for
`/api/`, see `deploy/web/nginx.conf`, so extending it is more consistent
with this codebase than adding Traefik just for ZITADEL).

1. **New service** in `docker-compose.yml`, alongside `zitadel`:

   ```yaml
   zitadel-login:
     image: ${ZITADEL_LOGIN_IMAGE:-ghcr.io/zitadel/zitadel-login:v4.17.3}
     restart: unless-stopped
     depends_on:
       zitadel:
         condition: service_healthy
     environment:
       ZITADEL_API_URL: http://zitadel:8080
       NEXT_PUBLIC_BASE_PATH: /ui/v2/login
       # A service-user PAT with the rights to drive the login flow.
       # The upstream compose mounts one written during first-instance setup
       # (ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_MACHINE_USERNAME etc. — see
       # below). This stack's zitadel-init seeder currently creates only the
       # `seeder` machine user (deploy/zitadel/bootstrap.mjs); a second
       # machine user + PAT for the login client needs the same treatment,
       # OR the `seeder` PAT can be reused if its grants are broad enough
       # (verify — the upstream compose uses a DEDICATED login-client user,
       # which is the safer default: this PAT is exposed to a
       # public-facing container, unlike the seeder's).
       ZITADEL_SERVICE_USER_TOKEN_FILE: /pat/login-client.pat
       CUSTOM_REQUEST_HEADERS: "Host:${ZITADEL_EXTERNAL_DOMAIN:-localhost}:${ZITADEL_PORT:-8090},X-Forwarded-Proto:http"
     volumes:
       - patshare:/pat:ro
     healthcheck:
       test: ["CMD", "/bin/sh", "-c",
              "node /app/healthcheck.mjs http://localhost:3000/ui/v2/login/healthy"]
       interval: 10s
       timeout: 5s
       retries: 12
       start_period: 20s
   ```

   Exact env var names (`ZITADEL_API_URL`, `NEXT_PUBLIC_BASE_PATH`,
   `ZITADEL_SERVICE_USER_TOKEN_FILE`, `CUSTOM_REQUEST_HEADERS`) were read
   from the upstream compose file via a truncated fetch and should be
   **re-verified against `ghcr.io/zitadel/zitadel-login`'s own
   documentation/image tag** before writing code — the version fetched
   during this investigation did not show the complete environment block.

2. **PAT for the login client.** `zitadel`'s `ZITADEL_FIRSTINSTANCE_*` env
   vars support declaring a second machine user at first boot, e.g.:

   ```
   ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_MACHINE_USERNAME: login-client
   ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_MACHINE_NAME: login-client
   ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_PAT_EXPIRATIONDATE: "2100-01-01T00:00:00Z"
   ```

   (mirroring the existing `ZITADEL_FIRSTINSTANCE_ORG_MACHINE_*` block for
   `seeder` in `docker-compose.yml`'s `zitadel` service). Confirm ZITADEL
   writes this PAT to a configurable path analogous to
   `ZITADEL_FIRSTINSTANCE_PATPATH` (currently `/pat/pat.txt` for the
   seeder) — if it's the same fixed mechanism, both users' PATs may need
   distinct `PATPATH` values or ZITADEL's docs may show a different
   mechanism entirely (e.g. one shared bootstrap directory with multiple
   files, as the upstream compose's
   `ZITADEL_SERVICE_USER_TOKEN_FILE: /zitadel/bootstrap/login-client.pat`
   suggests — note the different volume path, `/zitadel/bootstrap`, versus
   this repo's `/pat`). **This needs verifying against the actual image
   behavior, not assumed.**

3. **Route `/ui/v2/login/*` through the web tier's nginx**, OR directly
   expose `zitadel-login` on its own port and have ZITADEL's
   `LOGINV2_BASEURI` point at it directly. The simpler change for this
   codebase: publish `zitadel-login` on its own host port (e.g.
   `ZITADEL_LOGIN_PORT:-8091`) and set
   `ZITADEL_DEFAULTINSTANCE_FEATURES_LOGINV2_BASEURI` /
   `ZITADEL_OIDC_DEFAULTLOGINURLV2` etc. on the `zitadel` service to point
   at `http://${ZITADEL_EXTERNAL_DOMAIN:-localhost}:${ZITADEL_LOGIN_PORT:-8091}/ui/v2/login/`
   instead of trying to share `ZITADEL_PORT`. This avoids nginx-side
   path-splitting entirely and keeps this change scoped to the `zitadel`
   and `zitadel-login` services only. Whoever implements this should weigh
   this against the "one origin for ZITADEL" approach the upstream compose
   uses (single port, Traefik path-splits) — either is defensible, but the
   choice affects `SWGW_AUTH_ISSUER`/`SWGW_AUTH_HOSTHEADER` on `controller`
   and the browser-facing URLs the seeder and SPA construct, so pick one
   and apply it consistently everywhere ZITADEL's external URL is
   referenced (`docker-compose.yml`'s `zitadel`, `zitadel-init`, and
   `controller` services all reference `ZITADEL_EXTERNAL_DOMAIN`/
   `ZITADEL_PORT` today).

4. Set the `LOGINV2_*` and `OIDC_DEFAULTLOGIN*URLV2` env vars on the
   `zitadel` service in `docker-compose.yml` explicitly (currently unset),
   using whichever URL scheme was decided in step 3.

### Fix option B — fall back to Login V1 (smaller change, but going against upstream direction)

ZITADEL's docs reference an "Adopt Login V2" guide implying V1 (the
Angular UI baked into the core binary, served at `/ui/login`) still exists
as of v4.x and V2 is opt-in via `LOGINV2_REQUIRED`. If v4.17.3 turns out to
default to V2 regardless, there may be an instance-level feature flag to
force V1 (verify via ZITADEL's Admin/System API — an instance feature
setting, not an env var, based on the docs' phrasing "Deploy and enable
Hosted Login V2 for self-hosted ZITADEL v4 instances that currently use
Login V1" implying V1 is the default absent the flag). If confirmed, no new
service is needed, but Login V1's UI is on ZITADEL's own deprecation path,
so this is the short-term option, not the recommended one.

**Whoever picks up this doc must decide A vs B** — this write-up stops
short of that decision because it depends on live verification against
`ghcr.io/zitadel/zitadel:v4.17.3`'s actual default behavior, which was not
performed.

---

## Gap 2 — the SPA never redirects to ZITADEL for login

### What's happening

`docker-compose.yml`'s `web` service sets:

```yaml
environment:
  OIDC_ISSUER: http://${ZITADEL_EXTERNAL_DOMAIN:-localhost}:${ZITADEL_PORT:-8090}
```

with the comment *"Baked into the page at request time so the SPA knows
where to send the browser for login."* But nothing bakes it into anything,
and nothing sends the browser anywhere:

- `web/src` has **zero** references to `OIDC_ISSUER`, `oidc`, `authorize`,
  `signinRedirect`, or any `window.location` assignment aimed at a login
  page (checked with a full-repo search across `web/src/**`).
- [`web/src/BootGate.tsx`](../../web/src/BootGate.tsx) probes
  `GET /system/version` once at boot and gates the entire app behind it
  succeeding. It branches only on `ApiError.code === 'UNAVAILABLE'`
  (maintenance) vs. everything else (generic "Service unavailable"/
  "Coordinator did not respond" screen). A `401` from an unauthenticated
  browser falls into the generic branch and is indistinguishable, to the
  user, from the Coordinator being down.
- [`internal/api/middleware/auth.go`](../../internal/api/middleware/auth.go)
  and
  [`internal/api/middleware/oidc.go`](../../internal/api/middleware/oidc.go)
  only **validate** a bearer token already present on the request; there is
  no server-side redirect for browser navigations either.
- No `web/index.html` placeholder or nginx `envsubst`/entrypoint mechanism
  exists to inject `OIDC_ISSUER` (or any runtime config) into the built
  SPA at container start. Contrast with `deploy/web/docker-entrypoint.sh`,
  added separately to fix DNS-resolver portability — that script does NOT
  touch `OIDC_ISSUER` and was not intended to.

So: `AUTH_ENABLED=true` on the `controller` correctly requires a valid
token, but nothing in this codebase currently gets a user from zero to a
token. `OIDC_ISSUER` is declared but dead configuration.

### What needs to be built

1. **Runtime config injection.** The SPA is a static Vite build
   (`web/vite.config.ts`, `build/Dockerfile.web`); `OIDC_ISSUER` is only
   known at container start (it can vary per deployment), so it cannot be
   a Vite build-time env var. Follow the same pattern as the nginx
   DNS-resolver fix: either
   - extend `deploy/web/docker-entrypoint.sh` to render a small
     `window.__RUNTIME_CONFIG__ = { oidcIssuer: "...", ... }` script tag (or
     a `/config.json` static file fetched at app start) from the
     `OIDC_ISSUER` env var before starting nginx, or
   - add an nginx location (e.g. `/runtime-config.json`) served by a tiny
     substitution at container start, fetched once by the SPA before
     rendering `BootGate`.

2. **An OIDC Authorization Code + PKCE flow in the SPA.** This is not
   present in any form today; there is no `oidc-client-ts` (or similar)
   dependency in `web/package.json` — confirm and add one, or hand-roll
   the redirect using ZITADEL's `/oauth/v2/authorize` endpoint directly
   (simpler, since this stack already has a public client — see
   `deploy/zitadel/bootstrap.mjs`'s `web client created` step, which
   creates a public OIDC client with `accessTokenRoleAssertion`/
   `idTokenRoleAssertion` — its `client_id` needs to reach the SPA the same
   way `OIDC_ISSUER` does, and today it does not: nothing captures or
   surfaces that client ID at deploy time either. It is printed to the
   `zitadel-init` container's logs and nowhere else).

3. **Distinguish "unauthenticated" from "down" in `BootGate.tsx`.** A
   `401`/`403` on the boot probe should redirect to the login flow built in
   step 2, not render the generic outage screen. `q.error instanceof
   ApiError` already exists as a pattern
   ([`web/src/BootGate.tsx`](../../web/src/BootGate.tsx) L70-L73) — extend
   it with an `UNAUTHENTICATED`/`PERMISSION_DENIED` branch (both codes
   already exist in
   [`web/src/api/types.ts`](../../web/src/api/types.ts) L2248) that
   triggers the redirect instead of showing `ServiceUnavailable`.

4. **Handle the callback.** After ZITADEL authenticates the user it
   redirects back to the SPA's origin with either a code (PKCE — exchange
   it for tokens client-side) or, if this codebase prefers a
   confidential/backend-mediated flow, the `controller` would need a new
   `/api/v1/auth/callback`-style route to exchange the code and set a
   session cookie instead of the SPA holding a bearer token in memory.
   **This is a real architectural decision** (SPA-held token vs.
   backend session) that affects `internal/api/middleware/oidc.go` and
   should be made deliberately, not implied by this document — flagging
   it here so the implementing agent makes it explicitly rather than by
   accident.

5. Store/attach whatever token or session results to every
   `web/src/api/client.ts` request (currently that client has no auth
   header logic at all — confirm before assuming, since this was not
   fully audited).

### Dependency between Gap 1 and Gap 2

Gap 2's redirect target IS gap 1's fix. Building the SPA's redirect flow
against `/ui/v2/login/...` before gap 1 is fixed will redirect users into
the same `{"code":5,"message":"Not Found"}` dead end described above.
**Fix gap 1 first, or build gap 2 against whatever login URL gap 1's
chosen option (A or B) actually produces**, and verify end-to-end (click
through from the SPA's login redirect to a real ZITADEL login form and
back) before considering either done.

---

## Suggested order of work

1. Decide and implement Gap 1, Option A or B (needs live verification
   against `ghcr.io/zitadel/zitadel:v4.17.3` first — do not assume the env
   var names or default behavior documented above are exact; they were
   read from a partially-truncated fetch of ZITADEL's own docs/compose
   file during this investigation).
2. Confirm, by hand, that visiting ZITADEL's login URL directly in a
   browser (no SPA involved) now renders a working login form and
   completes a login.
3. Only then build Gap 2 (runtime config injection, OIDC/PKCE flow,
   `BootGate.tsx` branching, callback handling, token attachment).
4. Verify end-to-end: fresh `podman-compose up -d --wait`, open the web
   UI, get redirected to ZITADEL, log in (via the seeded
   `BOOTSTRAP_ADMIN_*` user or SSO if `SSO_ISSUER` is set), land back on
   the SPA authenticated, and confirm `/api/v1/whoami` returns `200`.

## Related files touched or read while investigating this

- [`docker-compose.yml`](../../docker-compose.yml) — `zitadel`, `zitadel-init`, `controller`, `web` services
- [`deploy/zitadel/bootstrap.mjs`](../../deploy/zitadel/bootstrap.mjs) — seeder; creates the `seeder` machine user and the SPA's public OIDC client
- [`deploy/web/nginx.conf`](../../deploy/web/nginx.conf) — existing `/api/` proxy pattern to follow for any new routing
- [`deploy/web/docker-entrypoint.sh`](../../deploy/web/docker-entrypoint.sh) — existing runtime-templating pattern to follow for injecting `OIDC_ISSUER`
- [`web/src/BootGate.tsx`](../../web/src/BootGate.tsx) — boot probe and error branching
- [`web/src/api/client.ts`](../../web/src/api/client.ts) — `ApiError`, request plumbing
- [`web/src/api/types.ts`](../../web/src/api/types.ts) — `UNAUTHENTICATED`/`PERMISSION_DENIED` error codes
- [`internal/api/middleware/auth.go`](../../internal/api/middleware/auth.go), [`oidc.go`](../../internal/api/middleware/oidc.go) — server-side token validation only, no redirect
- [`docs/design/24-identity-and-access.md`](24-identity-and-access.md) — existing identity/SSO design doc; this document supersedes nothing in it but should be cross-referenced from there once Gap 1/2 are implemented
