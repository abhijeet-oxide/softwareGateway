# 25. Signing in: ZITADEL Login V2, the SPA's OIDC flow, and what a 401 means

Status: **implemented**. This document was originally a handoff naming two
confirmed gaps; it now records what was built for each, the three faults that
turned up while building it, and how the whole path was verified. Section 6
lists what is still open.

## 1. The symptom

On a fresh stack (`docker-compose.yml` at the repo root, `.env` with
`SSO_ISSUER`/`SSO_CLIENT_ID`/`SSO_CLIENT_SECRET` set so Microsoft SSO is
configured), opening `http://localhost:${WEB_PORT}` showed the SPA's
"Service unavailable / The Coordinator did not respond" screen forever, while
`controller`, `zitadel`, `postgres` and `cerbos` were all healthy. The network
tab showed `GET /api/v1/system/version` and `GET /api/v1/whoami` both answering
`401`. Following a link ZITADEL itself generated -
`http://localhost:${ZITADEL_PORT}/ui/v2/login/login?authRequest=...` - showed a
bare `{"code":5,"message":"Not Found"}` instead of a login page.

Nothing in the stack could get a person from "not signed in" to "holds a valid
token", and the one screen that could have said so said the opposite.

## 2. Gap 1 - the sign-in screens are a service this stack never deployed

### What was wrong

Since v3, ZITADEL's login screens are **not served by the `zitadel` binary**.
They are a standalone Next.js service published as
`ghcr.io/zitadel/zitadel-login`; the core container serves `/ui/console` and
the APIs, and something in front of both routes `/ui/v2/login/*` to the login
service. This repository deployed only the core.

Verified against the running v4.17.3 image rather than assumed. Hitting
`/oauth/v2/authorize` on the deployed instance answers:

```
HTTP/1.1 302 Found
Location: /ui/v2/login/login?authRequest=V2_389806231822270470
```

so Login V2 is the default on v4 whether or not `LOGINV2_REQUIRED` is set, and
- decisively for the shape of the fix - **that redirect is RELATIVE**. The
login screens have to answer on ZITADEL's own origin.

### What was built

Three containers, one address.

- **`zitadel-login`** runs the sign-in screens. It talks to the core
  server-to-server at `http://zitadel:8080` and carries `CUSTOM_REQUEST_HEADERS:
  Host:<external domain>:<port>`, because ZITADEL identifies its instance by
  the Host header and answers 404 to a name it does not know. That value
  contains a colon of its own and is safe: upstream splits the pair on the
  FIRST colon (`apps/login/src/lib/custom-headers.ts`).
- **`zitadel-proxy`** (nginx, `deploy/zitadel/nginx.conf`) publishes
  `${ZITADEL_PORT}` and splits it: `/ui/v2/login` to the login service,
  everything else to the core. `zitadel` no longer publishes a port itself.
- The core gets the four `LOGINV2_*` / `OIDC_DEFAULT*URLV2` variables from
  ZITADEL's own compose reference, pointed at that one address.

**Why one origin rather than a second published port**, which would have been a
smaller diff: the core's redirect to the login screens is relative, the login
screens' redirect back into the core is absolute from ZITADEL's external URL,
and `SWGW_AUTH_ISSUER`, `SWGW_AUTH_HOSTHEADER`, the seeder's `ZHOST` and the
SPA's issuer all already agree on a single ZITADEL address. A second port makes
that four values to keep in step instead of one, for the sake of not running
nginx twice.

### The login service's own credential

The login service drives the flow as a service user holding `IAM_LOGIN_CLIENT`
(confirmed present in v4.17.3 via `POST /admin/v1/members/roles/_search`).

ZITADEL can create that account at first boot with
`ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_*`, and this stack deliberately does
**not** use it. First-instance settings are ignored on an instance that already
exists, so that mechanism fixes a fresh stack and leaves every stack that has
ever been started before unable to show a login page - which is every stack
that would be upgrading into this fix. `deploy/zitadel/bootstrap.mjs` creates
the machine user, grants the instance-level role and issues the PAT instead, on
the same idempotent path as everything else it does. The PAT is a separate,
narrower credential from the seeder's own on purpose: the login container is
the one service in this stack that a signed-out browser talks to.

## 3. Gap 2 - the SPA never asked anyone to sign in

### What was wrong

`OIDC_ISSUER` was declared on the `web` service with a comment saying it was
"baked into the page at request time", and nothing baked it into anything.
`web/src` had no OIDC code of any kind, no `Authorization` header on any
request, and no way for the client id - which ZITADEL GENERATES, and which the
seeder printed to its own container log and nowhere else - to reach a browser.

### What was built

**Runtime configuration.** The seeder writes `{issuer, clientId, redirectUri}`
to a shared `oidcshare` volume; `deploy/web/docker-entrypoint.sh` renders it
into `/runtime-config.json` at container start, with `OIDC_ISSUER` /
`OIDC_CLIENT_ID` / `OIDC_REDIRECT_URI` as overrides for a deployment that
provisions its identity provider some other way. Public values only: a public
OIDC client has no secret, which is why this is a file served to every browser
rather than a mounted secret. The entrypoint says on stdout whether sign-in is
configured, so "why can nobody sign in" has an answer in `docker logs web`.

The seeder also RECONCILES the registered redirect URI rather than only
creating it. It is derived from `WEB_PORT`; change that port on a stack that
has already been seeded and every login used to end on ZITADEL's own error page
about an invalid `redirect_uri`, a week after the `.env` edit that caused it.

**The flow itself** is in `web/src/auth/session.ts`: authorization code with
PKCE, hand-rolled, no new dependency. Tokens live in `sessionStorage` - scoped
to the tab, gone when the browser closes - and the refresh token with them,
which is a deliberate trade recorded in that file's own header.

**The server decides whether authentication is required.** Nothing in the
browser guesses. The application loads, makes its first read, and a `401` is
what starts a sign-in; a Coordinator running with `SWGW_AUTH_ENABLED=false`
needs no flag in the SPA to keep working. That decision lives in ONE place,
`web/src/api/client.ts`, because it is the only code that sees every
unauthenticated answer - a page handling its own 401 would send the browser to
the identity provider once per read on screen.

Two guards matter more than the happy path:

- **One renewal, then stop.** A 401 buys exactly one refresh-and-retry. The
  refresh is single-flight, because an identity provider that rotates refresh
  tokens invalidates the previous one and a second concurrent attempt would
  end the session.
- **A token the Coordinator refuses is not a reason to sign in again.** A
  mismatched issuer, audience or clock produces a valid token that is rejected,
  and redirecting would obtain the same token again. Within a minute of a
  completed sign-in, a 401 flips to a screen naming that as the fault instead
  of bouncing the reader between two services forever.

**`SessionGate`** (`web/src/auth/SessionGate.tsx`) sits above every read in
`main.tsx`, and that order is load-bearing: the callback address carries a
single-use code, and anything that fires a request while it is being exchanged
gets a 401, starts a second sign-in, and navigates away before the first
finished.

**Decision recorded, not implied:** the SPA holds the token; there is no
backend session and no `/api/v1/auth/callback` on the Coordinator. What the
seeder already provisions is a public `OIDC_APP_TYPE_USER_AGENT` client with
PKCE and no secret; `pkg/authz` already validates bearer tokens and the
Coordinator is stateless. A backend-mediated flow would have meant new routes,
cookies, CSRF and a token store, against the grain of both.

## 4. Three faults found while building it

### 4.1 The SSO connector was never offered on the sign-in screen

The seeder created the Microsoft IdP and stopped there. An identity provider is
offered to a person because it is attached to the organization's **login
policy**, and an organization that has never been given one of its own inherits
the instance's, which cannot name an org-owned connector. Confirmed live:
`POST /management/v1/policies/login/idps` answered `404 Login Policy not found
(Org-Ffgw2)`, and the sign-in screen showed a username box and nothing else.

So the connector existed, the console read as correctly configured, and SSO
did not appear. That is the second half of "the UI was not redirecting to SSO
login", and it would have survived the Login V2 fix untouched.

The seeder now gives the tenant its own login policy - **copied from whatever
it was inheriting**, not written from a fresh set of opinions, because
deciding this deployment's registration, MFA and password rules is not that
step's business - and attaches the connector to it. After the fix the login
screen reads "or sign in with / Microsoft".

### 4.2 The login image cannot start on a host without IPv6

`ghcr.io/zitadel/zitadel-login` ships `HOSTNAME=::` in the image, and its
Next.js server listens on exactly what that says. On an engine or host without
IPv6 the container restart-loops on `listen EAFNOSUPPORT ... :::3000`, a
message that names neither ZITADEL nor IPv6 as the problem. The compose file
sets `HOSTNAME: 0.0.0.0`; nothing in this stack talks IPv6 to it.

### 4.3 Every role was held twice

ZITADEL emits the same grant under more than one claim: one per project
(`urn:zitadel:iam:org:project:<projectID>:roles`) and one flattened across all
of them (`urn:zitadel:iam:org:project:roles`). Both match the shape
`pkg/authz` reads, so each role was counted twice - reaching the policy engine
twice and the Settings page as `org-admin, org-admin`, which reads as a
misconfigured grant rather than as one role named twice. `readRoles` now
deduplicates, with `TestRoleClaimedTwiceIsHeldOnce` over the real claim shape.

## 5. The nitpick that was not one: what a boot failure screen may say

The original complaint was that "Service unavailable" appeared for a `401`.
That is worth stating as a rule rather than as a patch, because the screen was
wrong in every word: the Coordinator responded, promptly, and said exactly what
was missing. Somebody reading that screen has no reason to suspect a sign-in is
needed and every reason to go and look at a healthy service.

`BootGate` now branches on the problem code and nothing else:

| what came back | screen |
|---|---|
| `UNAUTHENTICATED` | signing in, or the two states where signing in cannot help |
| `PERMISSION_DENIED` | no access: signed in, and not for this. No retry - only a grant fixes it |
| `UNAVAILABLE` (503) | maintenance, unchanged |
| 500 | service unavailable: "The Coordinator answered 500. It received the request and failed on it." |
| 502 / 504 | service unavailable: "The web tier answered 502: it could not reach the Coordinator." |
| no answer at all | service unavailable: "The Coordinator did not respond." |
| any other 4xx | unexpected answer, naming the status - almost always an origin whose `/api/v1` is proxied somewhere else |

The outage screen is for an outage. The three 5xx wordings are separated
because the sentence a reader quotes into a ticket is what decides who picks it
up: silence is a stopped container or a network, a 502 is the web tier saying
it could not reach the Coordinator, and a 500 is the Coordinator failing a
request it did receive - which is a bug, and a different team's.

Signing out lives beside the identity it ends, on Settings, and is absent when
there is no session to end. It ends the session at the issuer too: clearing
only the tab's tokens would put the reader straight back in at the next
redirect, which reads as a button that does nothing.

## 6. How this was verified, and what is still open

Verified on a stack brought up from empty volumes (`docker compose down -v`
then `docker compose up -d`), driven through a real browser:

- the first read answers 401 and the browser lands on
  `http://localhost:8090/ui/v2/login/loginname?requestId=oidc_V2_...`, a
  rendered ZITADEL login form titled "Welcome back!";
- with SSO configured, that form offers "or sign in with / Microsoft";
- signing in returns to `http://localhost:8000/auth/callback?code=...&state=...`,
  the code is exchanged, and the address is replaced with where the reader was;
- `GET /api/v1/whoami` answers `200` with
  `{"method":"oidc","authenticated":true,"tenant":"default","roles":["org-admin"]}`;
- a reload holds the session with no further trip to the issuer and no failed
  request;
- the refresh grant returns a new access token;
- signing out clears the tab and ends the ZITADEL session;
- stopping the controller renders "The web tier answered 502", not a 401 story.

**Still open, and not addressed here:**

- **A worker cannot authenticate to the Coordinator.** With
  `SWGW_AUTH_ENABLED=true` - the default - every lease is refused with
  `UNAUTHENTICATED: no bearer token` and the data plane does nothing. Workers
  need a machine identity (ZITADEL client credentials, which the seeder
  already issues for its `apiUsers`) the way a person needs a browser flow.
  This is the next piece of the same work.
- ~~`/worker --health-check` does not exist.~~ **Wrong, and corrected here:**
  the flag has been on the binary since `f9ee873`. The workers seen failing
  were running an image built before that commit, so this was a stale image
  and not a missing flag. The probe work in [09](09-api.md) §9.1 went over the
  whole probe surface afterwards.
- ~~**The seeder logs "tenant 'default' created" on a fresh stack when it did
  not create one.**~~ **Fixed**, and it was worse than a wrong log line. The
  org projection is behind the write that fills it, so the search misses, the
  create is refused on the duplicate name, and `ORG_ID` was left `undefined`.
  Everything then landed in the PAT's own organization, which happens to be the
  right one - so the outcome was correct and the log was not. On a loaded
  machine the same lag reaches one line further, into the project created next,
  and the run then comes up with NO OIDC CLIENT: the web tier loads and answers
  "Sign-in is not configured", which was reproduced here. Both the tenant and
  the web application now wait for the projection rather than assuming it, and
  a tenant that cannot be resolved at all is fatal instead of `undefined`.
- The login screen's Content-Security-Policy carries `http://zitadel:8080` for
  `font-src`/`img-src`, taken from `ZITADEL_API_URL`. Custom branding assets
  will not load in a browser, which cannot reach that name. Upstream's own
  compose has the same shape; default styling is unaffected.

## 7. Files

- [`docker-compose.yml`](../../docker-compose.yml) - `zitadel`, `zitadel-login`, `zitadel-proxy`, `zitadel-init`, `web`
- [`deploy/zitadel/nginx.conf`](../../deploy/zitadel/nginx.conf), [`docker-entrypoint.sh`](../../deploy/zitadel/docker-entrypoint.sh) - ZITADEL's front door
- [`deploy/zitadel/bootstrap.mjs`](../../deploy/zitadel/bootstrap.mjs) - login-client PAT, published client id, redirect reconciliation, login policy
- [`deploy/web/docker-entrypoint.sh`](../../deploy/web/docker-entrypoint.sh), [`nginx.conf`](../../deploy/web/nginx.conf) - `/runtime-config.json`
- [`web/src/auth/session.ts`](../../web/src/auth/session.ts), [`SessionGate.tsx`](../../web/src/auth/SessionGate.tsx) - the flow
- [`web/src/api/client.ts`](../../web/src/api/client.ts) - the bearer header, the one renewal, the one place a 401 is acted on
- [`web/src/BootGate.tsx`](../../web/src/BootGate.tsx) - the six screens
- [`pkg/authz/verifier.go`](../../pkg/authz/verifier.go) - `readRoles`
- [`docs/design/24-identity-and-access.md`](24-identity-and-access.md) - the identity model this implements
