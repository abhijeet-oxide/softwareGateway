# 24 - Identity and Access

> **Prerequisites:** [09 - API](09-api.md) §10, [02 - Configuration](02-configuration.md), [19 - User Interface](19-user-interface.md) §2
> **Status: DESIGN. Nothing here is implemented.** It replaces the `AnonymousAuthenticator` that [09](09-api.md) §10 documents as an accepted risk, and it is the gate [17](17-delivery-plan.md) Q6 puts in front of any Ingress.

---

## 1. The statement

**One `docker compose up` brings up the whole system with working authentication and authorization, configured, seeded and ready to use. Nothing is set up by hand.**

Three parts, and the split is the whole design:

| Concern | Owner | We write |
|---|---|---|
| Who is this person | Microsoft Entra, brokered by ZITADEL | nothing |
| Tokens, sessions, refresh, user lifecycle | ZITADEL | nothing |
| May they do this, here | Cerbos, from policy in this repo | policy files |
| Wiring it together on day 0 | a one-shot bootstrap container | one script |

The Coordinator itself gains **no write access to anything**. It fetches a public key and asks a local policy engine a question. That is the entire runtime surface.

## 2. Why not build it

Token issuance, refresh, rotation and session revocation are the parts of an auth system where a subtle bug is a breach rather than a defect. They are also completely commoditized. ZITADEL is Go, single-binary, event-sourced, and its data model happens to match ours exactly (§5). Cerbos is a policy engine that answers one question and holds no state.

> **Decision - ZITADEL for identity, Cerbos for authorization, neither forked nor wrapped.**
>
> *Alternatives considered:* Keycloak (Apache-2.0, mature, but a JVM and ~1.25 GB base RAM for a component that must come up inside a `docker compose` on a laptop); Dex (smaller, but no user store, no admin UI, and its SAML connector is marked unmaintained and auth-bypass-prone by its own maintainers); Casdoor (an exact feature match on paper - Go, React, three databases - **disqualified on security**: CERT/CC VU#780781 covers multiple authentication bypasses including SAML signature validation that trusts the certificate inside the assertion); building an issuer on a Go OIDC library (rejected: this is the one component where our bugs are breaches).
>
> *Cost accepted:* ZITADEL requires PostgreSQL and is AGPL-3.0. We do not ship it to customers, so the licence governs a service we operate rather than a binary we distribute, and PostgreSQL is already this system's only stateful component ([14](14-deployment-and-development.md) §4).

## 3. The stack

```
docker compose up
│
├── postgres        one instance, two DATABASES: swgw + zitadel
├── zitadel         identity: SSO brokering, users, tokens, roles
├── zitadel-init    ONE-SHOT. Seeds orgs/projects/roles/admin, then exits.
├── cerbos          authorization: reads policy from ./deploy/auth/policies
└── coordinator     the product. No write access to ZITADEL.
```

`zitadel-init` is the whole day-0 story. It waits for ZITADEL to answer its
discovery endpoint, reads the product list from the environment, and makes the
world match it. It is **idempotent**: run it again after adding a product and it
creates only what is missing.

> **Decision - seeding is a one-shot container, not Coordinator startup code.**
>
> *Alternative considered:* have the Coordinator reconcile ZITADEL on config load, hooked to `product.Watcher`'s `OnReload`.
>
> *Rejected because* it would require the Coordinator to hold a ZITADEL management credential. Every replica could then create projects, mint roles and read the whole IAM estate - a large blast radius bolted onto a service whose job is moving bytes. The seeder holds that credential for the seconds it runs and never again.
>
> *Consequence, stated plainly:* a product added to Git is NOT authorizable until someone re-runs the seeder. That is deliberate. Granting access is an administrative act, and coupling it to a config push is how a product becomes grantable before anyone decided who should have it.

### 3.1 Sharing PostgreSQL

One instance, two databases. **Not one database.** ZITADEL creates nine schemas
(`adminapi`, `auth`, `cache`, `eventstore`, `logstore`, `projections`, `public`,
`queue`, `system`) and about 150 tables, and it claims `public`. Sharing a
database puts two migration systems in one namespace and lets a ZITADEL upgrade
lock tables the Coordinator is reading. Sharing an instance costs nothing: the
whole ZITADEL database is 15 MB at this scale.

## 4. Tenants: there is one, and it is called `default`

We are one company using our own tool. Multi-tenancy is a shape we keep open,
not a feature we ship.

- ZITADEL organization `default` is created at bootstrap.
- **Anything that does not name a tenant is in `default`.**
- No product YAML mentions a tenant. `metadata.tenant` is not added to the schema
  and MUST NOT be until a second tenant genuinely exists.
- `middleware.Scope.Tenant` ([09](09-api.md) §10, `internal/api/middleware/scope.go`)
  already exists and already carries the empty-means-everything convention, so
  the day a second tenant appears no signature changes.

> **Decision - the default tenant is a ZITADEL org, not a Coordinator concept.**
>
> *Alternative considered:* a `GATEWAY_DEFAULT_TENANT` setting the Coordinator reads and stamps onto things.
>
> *Rejected because* the Coordinator would then have an opinion about tenancy while having no way to enforce it, and every product document would grow a field that says `default` forty times. Tenancy lives entirely in ZITADEL and Cerbos. The Coordinator reads a tenant off the token and passes it to a policy check; it never decides one.

## 5. The model, and why it fits without bending

ZITADEL's native shape is our shape. This is the reason it was chosen over
tools that would need a mapping layer:

```
Organization  = TENANT      (default)
│
├── Project "platform"      = ORG-WIDE roles. Name no product.
│      org-admin, org-operator, org-security, org-reader
│
├── Project "software-01"   = one PRODUCT
├── Project "software-02"     product-owner, product-operator, product-reader
└── ...                       (one project per document in config/products)
```

A **role assignment** binds (user × project × roles) inside an org. That is
exactly "this person is product-owner of software-01 in the default tenant",
expressed once, natively.

### 5.1 The two tiers, and why global roles name no product

| Tier | Lives on | Example | Covers |
|---|---|---|---|
| **`org-`** | `platform` project | `org-admin`, `org-operator`, `org-security`, `org-reader` | every product in the tenant, **including ones created later** |
| **`product-`** | that product's project | `product-owner`, `product-operator`, `product-reader` | that product only |

> **Decision - the prefix names the SCOPE, the suffix names the LEVEL.**
>
> `org-security` and `product-operator` each say what they cover and how much they allow, in that order, with no glossary. The earlier draft called the top role `manager`, which named a job title rather than a permission: it answered neither "over what" nor "how much", and every reader had to be told. A name that has to be explained is a name that will be assigned wrongly.
>
> The suffixes track the `Action` ladder already in `internal/api/middleware/scope.go` (`read` < `operate` < `apply` < `admin`), so `-reader`, `-operator` and `-owner`/`-admin` map onto verbs the code already has rather than inventing a second vocabulary beside it.
>
> *Watch for one collision:* ZITADEL ships its own `ORG_OWNER` and `ORG_USER_MANAGER` for administering the directory. Ours are lowercase project roles in a different namespace and grant nothing in ZITADEL. §5.2 keeps them apart deliberately.

> **Decision - an org-wide role is ONE assignment on `platform`, never N assignments across N products.**
>
> *Alternative considered:* grant `security-admin` on each of the forty product projects.
>
> *Rejected for two reasons, and the second is the serious one.*
>
> **Size.** Measured against a live instance: a security admin holding forty enumerated grants produces an **8,358-byte** access token. Nginx's default `large_client_header_buffers` is 4k/8k, so that token is rejected by a default-configured proxy. The failure appears only for the most privileged users, only in production, and gets worse with every product added. The same person as one global role: **883 bytes.**
>
> **Correctness.** If global access is forty grants, then `software-41` requires someone to remember to extend it. When they forget, the security team's screen quietly omits a product and looks entirely normal. A role that names no product has no such failure mode.
>
> *Verified:* a token minted **before** `software-41` existed authorized a request against `software-41` immediately after creation, with no new token and no re-login. That is the requirement, and it is a property of the role naming no product.

### 5.1a The data plane is a WORKLOAD, not a person

A worker leases jobs over the same authenticated API a person uses. With
authentication on and nothing of its own to present it received
`UNAUTHENTICATED: no bearer token` every five seconds, forever: a container
that is up, whose probes are green, and that never moves a byte.

| Tier | Lives on | Role | Covers |
|---|---|---|---|
| **workload** | `platform` project | `org-worker` | leasing jobs and reporting on them, and nothing else |

It is a machine account in ZITADEL (`swgw-worker`), authenticating with the
**client credentials grant** (RFC 6749 §4.4) and receiving a short-lived JWT the
Coordinator verifies with the same keys, the same package and the same code path
as a person's. Provisioning is `deploy/zitadel/bootstrap.mjs`; the credentials
are written to their own volume and mounted read-only into every worker.

> **Decision - a machine account at the identity provider, not a token in the environment.**
>
> *Alternatives considered.*
>
> **A shared static token in `.env`.** A password that never expires, copied into
> every manifest that mentions the service, revocable only by redeploying
> everything holding it. It also has no roles, so nothing downstream can bound
> what it reaches.
>
> **Exempting the worker plane from authentication.** Worse than it sounds. Those
> routes hand out work and accept its results, so an unauthenticated worker plane
> lets anything with network reach claim every job in the queue and report each
> one finished - a denial of service on the only thing this system does, launched
> from a curl.
>
> *Chosen because* it adds no second trust root. The Coordinator already
> verifies OIDC tokens; this is the same token, from the same issuer, carrying
> real roles, expiring on its own, and revoked by disabling one account in a
> console that keeps an audit trail of the disabling.

> **Decision - ONE identity for the whole fleet.**
>
> A worker holds no data, decides nothing, and is interchangeable with every
> other worker by design. Its `workerId` names it in the QUEUE, which is
> scheduling and not authorization. Per-worker credentials would all carry the
> same grant, so they would contain nothing, and would cost the property the
> fleet exists for: that `replicas: 20` is a number rather than a conversation
> with whoever issues credentials.

> **Decision - the credential is a mounted FILE, never an environment variable.**
>
> The environment of a process is readable through the container runtime by
> anyone who can inspect it, is inherited by every child process, and turns up
> whole in a crash report. A file is mounted with a mode and an owner, read
> once, and can be rotated under a running fleet. The seeder writes it `0600`
> and chowns it to the runtime image's non-root uid, because a `0600` file
> written by root is otherwise perfectly delivered and unreadable by the one
> process that wants it.

> **Decision - the workload is CONFINED, in both directions.**
>
> `org-worker` maps to one action, `work`, and to nothing else - not even
> `read`. `internal/api/middleware/workload.go` then enforces two rules in one
> place: a request to the worker plane must hold `work`, and a request to
> anything else must not come from an identity that holds ONLY `work`.
>
> So the worst a stolen worker credential can do is take jobs and lie about
> their results. It cannot read the audit trail, request a transfer, or
> enumerate products it was never handed work for. *Verified against the running
> stack:* that credential gets `PERMISSION_DENIED` on `/products`,
> `/auditEvents`, `/workers` and `POST /transfers`, and is served on
> `jobs:lease` and `workers/{id}:heartbeat`.
>
> This does NOT make the human routes authorized - they remain open to any
> authenticated caller, exactly as they were. That gap is older than this
> change and is item 7 in §11; closing the half that arrived with this
> credential is not a claim to have closed the other half.

> **Decision - credentials are read when a token is needed, never once at startup.**
>
> Read at boot, a worker that came up before the seeder had written the file
> could never authenticate again however long it ran, and picking up a rotated
> secret meant restarting the fleet. Docker Compose declares that ordering as a
> dependency; **podman-compose does not implement the key**, so on that runtime
> it is not declared at all.
>
> Reading on demand makes the question moot on both. A worker with no
> credentials sends no token, is refused by the Coordinator in its own words,
> and retries on its ordinary five second cadence; the file appears and the next
> attempt succeeds. *Verified:* credentials deleted and the worker restarted
> against them, then written by the seeder - the fleet resumed leasing with no
> restart and no intervention.
>
> A missing file is therefore **not an error**. That is what an installation
> with authentication switched off looks like, and treating it as fatal would
> make the data plane the one component that cannot run in a supported
> deployment. A file that is present and half filled in IS an error, because
> that is a mistake somebody made rather than a deployment without an identity
> provider.

> **Decision - a worker is HEALTHY only once the Coordinator has accepted it.**
>
> This one was found the hard way and it is the reason the section above exists.
> With authentication on and no credential to present, two workers were refused
> on every lease, five seconds apart, indefinitely, and reported themselves
> healthy the whole time: the process was up, the control plane was reachable,
> the products had loaded. `podman ps` showed a green fleet over a deployment
> that had never once worked.
>
> Readiness now means REGISTERED: reached, authenticated, lease call served. An
> idle queue still passes, because being told there is no work is the
> Coordinator accepting the request. A staleness bound
> (`worker.RegistrationStale`) closes the other half, where a lease loop that
> stopped calling would leave the last success recorded as good forever.
>
> The earlier reasoning was that a worker which cannot lease is still running
> the jobs it holds, and that neither usual cause - a Coordinator restarting, a
> Coordinator refusing the credentials - is fixed by anything happening to that
> container. Every clause is true and the conclusion was still wrong, because it
> answered a question nobody was asking. A probe whose green means "this
> container started" rather than "this container is doing its job" is worse than
> no probe: it is a confident wrong answer in the first place anybody looks.
>
> It is safe here specifically because a worker **serves no traffic**: readiness
> removes a pod from a Service's endpoints, and this component is behind no
> Service, so unready costs nothing and is purely a statement of fact. It is
> emphatically NOT liveness, which stays "is this process's own loop still going
> round" - a refused worker must never be restarted, or one bad credential
> becomes a fleet-wide crash loop.

> **Where this goes next.** In Kubernetes the same fence should be reached by a
> projected ServiceAccount token and a TokenReview: no credential to provision
> at all, rotation handled by the kubelet. The seam is `v1.TokenSource`, which
> is an interface for that reason. It is not built, because this deployment is
> compose and a mechanism with no deployment to run in is a mechanism nobody
> tests.

### 5.1c Nobody is created by signing in

The connector is configured with `isCreationAllowed: false` and
`isAutoCreation: false`, so an account at the identity provider that this
gateway has never been told about **cannot sign in and is never issued a
token**. Linking stays on: that is how a person the seeder already provisioned
attaches their Microsoft identity to the account holding their roles, matched on
their address.

> **Decision - the line is drawn at the front door, not behind it.**
>
> With creation allowed, which is what shipped, an unprovisioned person signing
> in got a brand new ZITADEL account with no roles - and a valid token. Every
> defence after that is then working to contain a caller who should never have
> held a credential. Authorization has to hold that line anyway and does (§8.2),
> but "we have never heard of you" is a better answer than "you may do nothing",
> and it is the only one that produces no token at all.
>
> Who may use this gateway is decided by an administrator in
> `config/users/users.yaml`, reviewable in a pull request. It is not decided
> by who happens to hold an account in the corporate directory, which is
> everybody.

> **Every attached connector is checked, not only the managed one.** The seeder
> reconciles the connector whose display name matches `SSO_DISPLAY_NAME` and
> leaves any other alone - including, on a stack seeded before this rule, one
> with creation switched on. So it inspects them all and names any that can
> still mint accounts. Reported rather than corrected: updating a connector
> needs the endpoint for its own type, and guessing that for one this file did
> not create is how a seeder deletes somebody's working SSO.

> **The seeder refuses to produce a stack nobody can sign in to.** Creation off,
> password sign-in off, and no user carrying an address the provider will assert
> is a deployment that authenticates people correctly and then turns every one
> of them away with `Errors.User.NotFound`. That happened: `BOOTSTRAP_ADMIN_EMAIL`
> was left at its example default, so the administrator this file creates
> carried an address nobody can sign in with. The run now FATALs with the one
> line of configuration that fixes it, and on a successful run it LISTS the
> addresses that can sign in - a closed system should be able to say who it is
> closed to.
>
> The administrator's address is also RECONCILED now rather than written only at
> creation. It is not a cosmetic field: with SSO it is the sign-in identity that
> auto-linking matches on, and an address that could not be corrected made the
> advice "set BOOTSTRAP_ADMIN_EMAIL and re-run" quietly untrue on any stack that
> had ever been seeded.

> **Verified end to end, against a mock identity provider.** A Keycloak realm
> stood in for the corporate directory, with two users: one whose address
> matched a provisioned ZITADEL account, one nobody had heard of.
>
> - the provisioned one **signed straight in**, silently linked, and landed on
>   the application holding `org-admin`. Auto-linking works with creation off;
>   turning creation off does not lock out people who were provisioned.
> - the unknown one was **stopped at ZITADEL's own screen**: "Account Not Found
>   - We couldn't find an account associated with your identity provider
>   credentials." No account was created and no token was issued.
>
> Getting there needed one thing worth recording: ZITADEL refuses to dial
> private address ranges when reaching an identity provider, which is every
> address a local mock can have. The knob is `ZITADEL_HTTPCLIENT_DENYLIST`, and
> `ZITADEL_ACTIONS_HTTP_DENYLIST` is deprecated and MERGED into it - so clearing
> only the second changes nothing. An empty value reads as unset and the
> shipped defaults apply; it has to be replaced with an inert entry. That is a
> test-rig setting and appears in no committed file.

> **When a provisioned person still cannot sign in, the address is the suspect.**
> Linking compares one string on each side, and a directory that does not assert
> the one this side holds can never match however correct both look. The seeder
> prints the username AND the address of every account that can sign in, because
> printing only the matched field hides exactly the mismatch worth seeing. Where
> a directory asserts a user principal name and no `mail` attribute,
> `SSO_LINK_ON=username` matches on the username instead.

### 5.1b The address is the identity, not the username

A person who signs in through Microsoft is created by **ZITADEL**, not by the
seeder, and named by whatever the connector hands over - usually their address.
The account the seeder made for them carries a different name, and often a
different address, so one human ends up with two accounts.

> **Decision - lookups match on EMAIL first, username second.**
>
> The address is the durable identity: it is what the identity provider
> asserts, it is what ZITADEL's auto-linking keys on, and it is the same string
> on both systems. A username is a local artifact of whichever side created the
> account first.
>
> Matching on username first looks equivalent and is not. It finds the seeder's
> own account every time and grants it the roles; the person then signs in
> through Microsoft, lands on the other account, and reads "Tenant roles: none"
> on their own profile - with the roles sitting on an account they cannot sign
> in to, because it has no identity at the provider and password sign-in is off
> once SSO is configured. *This is what shipped*, and username-first is why the
> first attempt at this fixed nothing.

> **Decision - the seeder reports the mess, and does not clean it up.**
>
> Two checks, both at the end of the run where they cannot be lost in
> scrollback:
>
> - **people who can sign in and hold no roles** - a direct check for the
>   symptom rather than an inference from it, because the duplicate check below
>   cannot see the commonest shape of it: when the seeded administrator carries
>   the DEFAULT address, both lookups land on that same account, so there is no
>   duplicate to notice while the person's real account sits beside it
>   ungranted. Directory administrators are excluded - ZITADEL's own break-glass
>   account holds an instance membership and no project grant by design, and a
>   warning whose first line is always wrong is one people learn to skip.
> - **two accounts for one person** - named with both ids and which one now
>   holds the roles.
>
> Neither deletes anything. Removing somebody's account is not a decision a
> re-runnable seeding script should take on its own, and the leftover is
> harmless: it cannot sign in, which is the whole reason it is confusing rather
> than dangerous.

> **Operationally: `down` keeps the directory.** ZITADEL's users live in the
> `pgdata` volume, and only `docker compose down -v` discards it. A duplicate
> created once survives every rebuild until somebody removes it, which is why
> the reports above matter more than they would in a stack that started empty
> each time.

### 5.2 The four personas

| Persona | How it is expressed | Scope |
|---|---|---|
| **Oversees every product** | `org-admin` (full) or `org-operator` (no promote) on `platform` | tenant |
| **Security admin** - security detail across products | `org-security` on `platform` | tenant |
| **Admin** - adds users, assigns roles | ZITADEL's own `ORG_USER_MANAGER` | that org |
| **Superadmin** - break glass | ZITADEL `IAM_OWNER`, disabled after bootstrap (§7) | instance |

> **Decision - "admin who manages users" is a ZITADEL manager role, not an application role.**
>
> *Rejected alternative:* an `admin` role on the `platform` project.
>
> *Rejected because* it would be an application role that grants nothing in the application. Managing users is administering ZITADEL, and ZITADEL already has per-org roles for it with its own console, audit trail and delegation. Inventing a parallel one means two answers to "who can add users" that will disagree.

## 6. Configuration contract

Everything is environment. No file is edited by hand.

```bash
# --- identity ---------------------------------------------------------------
ZITADEL_MASTERKEY=<32 chars>
ZITADEL_DEFAULT_ORG=default

# --- Microsoft SSO: mounted, never committed --------------------------------
SSO_ISSUER=https://login.microsoftonline.com/<tenant-id>/v2.0
SSO_CLIENT_ID=<from Entra app registration>
SSO_CLIENT_SECRET=<from Entra app registration>
SSO_AUTO_REDIRECT=true          # single IdP: skip ZITADEL's own login form

# --- what exists: NOT here. See config/README.md ------------------------------
#   config/products/*.yaml      one project per product, with its roles
#   config/access/roles.yaml    the roles themselves, both tiers
#   config/users/users.yaml     the people, and their role on each product

# --- break glass: set EXACTLY ONE (see §7) ----------------------------------
BOOTSTRAP_ADMIN_EMAIL=platform-admin@example.com    # production
# BOOTSTRAP_ADMIN_PASSWORD=...                      # local development only
```

**The product directory is the seeder's input, and that is a reversal.** It was
`GATEWAY_PRODUCTS`, a comma-separated list in `.env`, deliberately separate from
the product documents on the reasoning that replication and access are different
concerns. They are - but they are not different SUBJECTS, and keeping them in two
formats meant nothing could check one against the other: a product could be
replicated with no project to grant access to, or a project could outlive the
product it was made for, and the only symptom either way was somebody unable to
open something.

One list now, in `config/products`, with the access rule enforced where the two
files first meet: a product nobody holds `product.ownerRole` on in
`config/users/users.yaml` refuses to seed. `go test ./deploy/...` makes the same
check on the pull request.

## 7. The bootstrap admin exists to appoint a real one

```
BOOTSTRAP_ADMIN_EMAIL set    -> user created with NO password. Signs in by SSO
                                or e-mail verification. Nothing to leak, nothing
                                to rotate, nothing to leave enabled by accident.
BOOTSTRAP_ADMIN_PASSWORD set -> user created with that password. The seeder
                                refuses this when SSO_ISSUER is set, so the
                                development shortcut cannot reach production.
```

Its only job is to grant `ORG_USER_MANAGER` to the first human administrator.
The seeder reports the account as pending removal on every subsequent run, and
`transferctl` surfaces it, because a break-glass account nobody disabled is the
credential an attacker finds first.

## 8. What the Coordinator actually does

Two things, and neither needs a credential.

**Validate the token.** `GET /oauth/v2/keys` is a public JWKS endpoint - verified
returning HTTP 200 with no authorization header. Fetch once, cache, rotate on
`kid` miss. Validation is then **offline**: no network call per request, no
secret to rotate, and ZITADEL being briefly unreachable cannot stop an
already-authenticated request.

**Ask Cerbos.** One local call, over a unix socket or localhost.

```
Request -> [existing middleware chain] -> Auth -> Handler
                                           │
                    validate JWT (cached JWKS, offline)
                                           │
                    Identity{Subject, Tenant, Roles, Grants}
                                           │
                             id.Can(action, Scope{...})  -> Cerbos
```

`Identity` and `Can(Action, Scope)` already exist in
`internal/api/middleware/`. **No handler changes, no route changes, no schema
changes.** `Can`'s resolver becomes a Cerbos call; its callers do not move. This
is what [09](09-api.md) §10.1 was holding the seam open for.

### 8.1 Where product grants come from

The token carries identity, tenant and **global** roles only. Per-product grants
are read from ZITADEL's API and cached for 60 seconds.

> **Decision - product grants are resolved and cached, not carried in the token.**
>
> *Alternative considered:* request every product's audience at login so the token carries all grants.
>
> *Rejected because* it requires the login scope string to enumerate every product ID - 2,160 characters at forty products. A product created on Tuesday is invisible to everyone until the OIDC client configuration is changed and redeployed, and the symptom is a permissions bug rather than a missing config. The cached lookup has no such coupling: a grant made at 10:00 works at 10:01.
>
> *Cost, measured:* the ZITADEL read is an index scan on a precomputed projection - `Execution Time: 0.063 ms`, two shared-buffer hits, no disk. ZITADEL is event-sourced, so this read never touches the event store. At 1,000 active users and a 60-second cache that is 63 ms of database time per minute.
>
> *Failure behaviour:* on a ZITADEL error, serve the stale cache entry. Refuse only when there is no cached entry at all. **Never fail open.**

### 8.2 Authorization is enforced, and where

Authentication and authorization are two questions, and for several releases
only the first was asked. Every route was reachable by anybody holding a valid
token, whatever roles they held. Federating a corporate directory made that
much larger than it sounds: **any account in the directory** could sign in and
read every product, every transfer and the audit trail. It was found exactly
that way - somebody signed in through Microsoft, was provisioned nothing, and
saw everything.

The machinery had been there the whole time and nothing called it:
`Identity.Can`, scoped grants, a policy engine constructed at startup and never
consulted. **A permission model no handler asks is documentation.**

`internal/api/middleware/authorize.go` is the gate.

> **Decision - one middleware, not a check in every handler.**
>
> A check inside each handler is the same decision written eighty times, and
> its failure mode is a route added later with the check left out: silent, and
> exactly how this gap would come back. Here the DEFAULT decides, so a new
> route is governed the day it is registered and the only way to weaken one is
> to edit a file whose whole subject is permissions.

> **Decision - it fails closed, by construction.**
>
> There is no "route I do not recognise" branch that permits. An unknown path
> gets the rule for its method: `read` for a GET, `operate` for anything that
> writes. A caller holding neither is refused.

| | requires |
|---|---|
| `GET`, `HEAD` | `read` |
| any write | `operate` |
| a path ending `:apply` | `apply` |
| the worker plane | `work` (§5.1a) |
| `/whoami`, `/system/version` | nothing beyond a valid token |

> **Decision - two routes answer whatever the caller holds.**
>
> The SPA probes `/system/version` before it renders anything and reads
> `/whoami` to learn what it may offer. Gate either and a person with no roles
> gets a service-unavailable screen or a blank one instead of a page naming the
> problem. A security control whose effect is that nobody can be told why they
> were refused produces a support ticket, not a fix. Neither route carries
> anything worth withholding: build metadata, and a description of the caller's
> own permissions.

> **Decision - a listing may only widen its door if it narrows its answer.**
>
> "May you list products" is not "may you act on the estate": a caller granted
> one product cannot answer the second and must still see the first. Those
> routes are marked `AnyScope` and admit anybody holding the action on ANY
> product - and each one filters its own results through
> `Identity.VisibleProducts`. The filtering is what makes the wider door safe,
> so the two are one change. Marking a route `AnyScope` without filtering it
> hands a caller scoped to one product the contents of all of them.
>
> Today that is `GET /products`, `GET /auditEvents` and `POST /transfers` -
> which cannot be scoped in a middleware at all, because its product is in the
> BODY, so `handleCreateTransfer` re-asks with the product it decoded.
> Everything else that cannot narrow its answer stays tenant-wide and fails
> closed, which is why a product-scoped caller cannot list every transfer.

> **Corrected while doing this:** the role ladder gave `operator` `ActionApply`.
> `config/access/policies/download.yaml` grants `apply` to `org_wide_admin` and
> `product_owner` only, and §5.2 describes org-operator in the same terms. The
> disagreement cost nothing while nothing consulted the ladder, and would have
> handed every operator the one action that writes into somebody else's
> registry the moment something did.

### 8.3 Cerbos decides, and it decides everything

The policy engine was constructed at startup and never consulted; the decision
came from a role ladder compiled into the binary. That is the opposite of why a
PDP is in this stack. `config/access/policies` is now the only answer whenever
an engine is configured - not a second opinion layered over the ladder, which
would be two answers that can disagree and a shipped behaviour decided by
whichever was checked last.

`internal/api/middleware/resource.go` maps each route to a resource kind and an
action in the policies' own vocabulary. Thirteen policy files cover the whole
API surface: `product`, `package`, `software_download`, `security_report`,
`compliance_report`, `replication`, `download_rule`, and the estate resources
`audit_event`, `report`, `worker`, `policy_catalogue`, `system`.

> **Decision - the ladder survives only where there is no engine.**
>
> Authentication without a PDP is a real deployment and the half-step this
> document already describes. With `SWGW_AUTH_CERBOSADDR` set, the ladder is not
> consulted at all.

> **Decision - an engine that cannot answer refuses.**
>
> Cannot know is not yes. This is a deliberate availability trade: a Cerbos
> outage refuses the API rather than opening it, for administrators too.
> *Verified:* with the PDP stopped, an administrator's `GET /products` answers
> 403 naming the policy engine as the cause.

> **Decision - the estate resources have no product tier.**
>
> A fleet is a fleet; the rulebook is what WILL be checked, which a vendor
> asking before they ship has no release to point at. A caller holding only
> product roles is refused, because the alternative is handing them the whole
> estate so they can see their corner of it. The audit trail is the exception
> that proves it: it CAN be narrowed, the handler narrows it by product, so a
> product reader may view it.

> **One PDP call in the ordinary case.** The tenant-wide question is asked
> first and every org-tier caller stops there. Only a product-tier caller on a
> listing route pays more - one call per product they hold - because "may you
> list products" is not "may you act on the estate", and that is the one
> question a single check cannot express.

*Verified against the running PDP*, not against a table: the matrix in
`policy_test.go` runs with `CERBOS_ADDR` set, and covers the boundaries that
matter - operator may `sync` and may not `apply`, a product owner reaches their
own product and not another, an account with no roles reaches nothing.

**The refusal is written for the person reading it.** "This account holds no
roles" is a different problem from "you hold the wrong ones", and only the first
has an answer that can be acted on - it is also by far the likelier one, because
an account the identity provider created at a first sign-in is the shape this
gate was written for.

### 8.4 What a refused person sees

A FULL SCREEN, with no navigation, saying one thing.

> **Decision - no application frame.**
>
> The first version rendered inside it, so somebody who may open nothing was
> shown nine things to open, none of which would answer. Chrome that leads
> nowhere is not reassurance, it is a maze.

> **Decision - the fact, not the mechanism.**
>
> The first version explained roles, identity providers and sign-in tokens.
> Those are this system's internals and none of the reader's business: they are
> a professional who has been told no, and what they need is the fact, the
> account it applies to, and who to ask. The screen carries exactly those three
> and a way out:
>
> > **This account is not enabled**
> > Access is granted by an administrator, and has not been granted for this
> > account.
> > `somebody@example.com`
> > [Sign out]   Request access from platform-team@example.com.
>
> Everything else belongs in a log. The Coordinator's own refusal still names
> the cause precisely, because that one is read by whoever is diagnosing it.

> **Decision - the contact is per deployment, and absent is a real answer.**
>
> `SUPPORT_CONTACT` is published in the SPA's runtime configuration and rendered
> as a link, because an address somebody has to retype is an address somebody
> mistypes. Unset, the sentence still completes and names no route: a refusal
> that invents one sends people to a mailbox nobody reads.

## 9. Policy lives in this repository

Cerbos policies are YAML under `deploy/auth/policies/`, versioned with the code
they gate and reviewed in the same pull request.

```yaml
resourcePolicy:
  resource: "security_report"
  rules:
    # Global role: no product condition, so future products are covered.
    - actions: ["view", "export"]
      roles: ["security-admin"]
      effect: EFFECT_ALLOW
      condition: {match: {expr: R.attr.tenant == P.attr.tenant}}
    # Product role: must match this product.
    - actions: ["view"]
      roles: ["product-owner"]
      effect: EFFECT_ALLOW
      condition:
        match:
          expr: R.attr.tenant == P.attr.tenant && R.attr.product in P.attr.products
```

> **Decision - roles in ZITADEL, permissions in Cerbos.**
>
> This is the rule that stops the model sprawling. ZITADEL holds a handful of stable, business-named roles that change when a person's job changes. Cerbos holds every action those roles may take, in files that change when the code changes.
>
> *Therefore adding a gated endpoint is: one line in a policy file and one `Can` call, in one commit.* No new ZITADEL role, no re-assignment, no console work. The alternative - a role per permission - is what makes RBAC unmaintainable, and it is the failure this decision exists to prevent.
>
> *And it is testable without ZITADEL running at all*, because a policy decision is a pure function of roles, resource and action.

## 10. Configer inherits this

[Configer](../../../configer) becomes a second client of the same ZITADEL, the
same product projects and the same policy files. It needs:

- an OIDC client in the `platform` project;
- the same middleware, lifted to a shared module alongside `Action`/`Scope`/`Grant`;
- its resources added to the existing policies.

Two consequences for Configer, recorded here because they are security-relevant
and easy to miss:

1. Its `defaultRole()` returns `RoleApprover` today, justified by "Configer
   cannot yet tell one person from another". Once identities are real that
   justification is void and the default **must invert to deny**.
2. With one shared Git credential, every commit is authored by the service
   account, so the `Changed-by:` trailer becomes the only record of authorship.
   It must be mandatory and carry the stable subject claim, never a display name.

## 11. What must be true before this ships

1. `docker compose up` on a clean machine yields a working login and an
   authorized request, with no manual step.
2. Re-running the seeder after adding a product document adds only the new
   project and its roles.
3. A global role authorizes a product created after the token was issued.
4. A user with no grant on a product is refused it.
5. The break-glass account is disabled, and its still being enabled is reported.
6. `docs/design/09` §10's risk note is deleted, because it is no longer true.
7. The human routes authorize, not only authenticate. **Closed** - see §8.2.
   It was open long enough to be found in production: a person signed in
   through Microsoft, provisioned nothing, and could read the whole estate.
8. A worker authenticates with no configuration by hand and no credential in
   the repository, and `WORKER_REPLICAS=20` needs no extra step.

## 12. Naming: why the binary is still `coordinator`

The service is called **controller** in `deploy/docker-compose.yml`, because
that is the better name and the deployment layer is free.

The Go binary is not renamed. `internal/platform/config/config.go` carries
`koanf:"coordinator"` and `koanf:"coordinatorEndpoint"`, which are **user-facing
configuration keys**: renaming them does not fail loudly, it makes koanf miss
the key and fall back to a default, so an existing deployment silently starts
talking to `http://localhost:8080` instead of its real controller. That is a
deprecation cycle with dual-key support, not a rename. The rest is 486 mentions
across 129 Go files, 291 across 31 documents, `cmd/coordinator`,
`deploy/build/Dockerfile.coordinator` and the worker's wire config.

> **Decision - rename at the deployment layer now, in the code never (or behind a deprecation).**
>
> The compose service, its DNS name and the nginx upstream all say `controller`. Anyone operating the stack sees the right word. Anyone reading the Go sees `coordinator` and one paragraph saying why.

## 13. Operating it

```bash
docker compose up -d          # start, in dependency order
docker compose down           # stop, reverse order, data kept
docker compose down -v        # stop and discard the databases

# after adding a document to config/products and an owner to config/users/users.yaml:
docker compose run --rm zitadel-init
```

Startup order is declared as conditions, never raced:

```
postgres --healthy--> zitadel --healthy--> zitadel-init --completed--+
     \                                                               |
      `--healthy-------------------------> controller <--healthy-----+
                              cerbos ----'      |
                                                `--started--> web
```

`controller` waits on `service_completed_successfully` for the seeder, so it
cannot start against a ZITADEL that has no projects in it. `docker compose
down` walks the same graph backwards, so nothing writes to a database that has
already stopped.

### 13.1 Eight things that are easy to get wrong

Each of these was found by running the stack, not by reading documentation.

1. **ZITADEL answers 404 to a valid request with the wrong `Host`.** It
   validates `Host` against `ZITADEL_EXTERNALDOMAIN`. Internally the seeder
   reaches it as `zitadel:8080` but it only answers to `localhost:8090`, so
   every call must carry that `Host` explicitly.
2. **Node's `fetch` silently drops the `Host` header.** The Fetch standard
   lists it as forbidden, so undici removes it and the seeder gets a 404 from a
   healthy service with nothing in any log to explain it. `deploy/zitadel/bootstrap.mjs`
   uses `node:http`, which permits it, and says so where it does.
3. **nginx resolves proxy upstreams once, at startup.** A literal
   `proxy_pass http://controller:8080` makes the web container refuse to start
   when the controller is absent, and pin a stale IP if it is later replaced.
   The config assigns the upstream to a variable with a `resolver`, forcing
   per-request resolution through Docker's DNS.
4. **The seeder must install nothing at runtime.** The obvious shell version
   needs `curl` and `jq`, so it runs `apk add` on every boot - a call to a
   distro CDN that fails in exactly the air-gapped estates this product targets
   ([19](19-user-interface.md) §1). Writing it in dependency-free Node removes
   the network call entirely.

5. **Creating the SSO connector does not put it on the sign-in screen.** An
   identity provider is offered because it is attached to the organization's
   LOGIN POLICY, and an organization that has never been given one of its own
   inherits the instance's, which cannot name an org-owned connector. Adding it
   answers `404 Login Policy not found (Org-Ffgw2)`. So the connector exists,
   the console reads as correctly configured, and the sign-in screen shows a
   username box and nothing else. The seeder gives the tenant its own policy,
   copied from what it was inheriting, and attaches the connector to it.
6. **The redirect URI is registered under the wrong Entra platform.** ZITADEL's
   callback is `${ZITADEL_PUBLIC_URL}/idps/callback` - not any URL a browser
   shows during a failed sign-in - and in Microsoft Entra it must be registered
   under the **Web** platform. Entra treats an SPA-registered redirect URI as a
   public client whose token endpoint requires PKCE; ZITADEL redeems the code
   from its own backend with the client secret, which is a confidential client.
   Register it as an SPA and the flow fails AFTER the password has been typed,
   with `AADSTS9002325: Proof Key for Code Exchange is required for
   cross-origin authorization code redemption` - a message that names neither
   the redirect URI nor the platform setting that caused it. Nothing on this
   side can be configured around it. The seeder prints the exact URI on every
   run, because a value this product cannot set for itself and cannot validate
   is a value it should at least say out loud.

7. **The SSO connector was created once and never reconciled.** A corrected
   `SSO_CLIENT_SECRET` in `.env` reached nothing: the seeder found the
   connector by name, said "exists", and left the old credentials in place, so
   sign-in kept failing with `AADSTS7000215: Invalid client secret provided`
   about a secret that had already been fixed. `.env` is the source of truth
   for the connector's issuer, client id and secret, and re-running the seeder
   is how they are applied - so it now PUTs them every run and names the ones
   that moved. Naming the SECRET takes a fingerprint: ZITADEL returns a
   connector's client id and issuer and never its secret, so the seeder keeps a
   truncated SHA-256 of what it last wrote beside the machine tokens and
   compares against that. Without it the only honest line was "written", which
   on every run is a line nobody reads by the time it matters. The write stays
   unconditional - comparing decides what to print, not whether to push - so a
   secret changed by hand in the console is still put back to what `.env` says. The same rule already applies to the web client's redirect URI
   (§6): anything derived from `.env` has to be reconciled, not merely created,
   or the second run of a config file is a no-op that looks like success.

   The seeder also refuses a `SSO_CLIENT_SECRET` shaped like a GUID. Azure
   shows a secret's ID and its value side by side; the ID stays on screen and
   the value is shown once, so the column still available to copy is the wrong
   one. A secret ID is a GUID and a secret value is not, which makes it
   checkable in one line at seed time instead of an opaque code after somebody
   has typed their password.

8. **A variable exported in the shell beats `.env`, for the whole session.**
   Compose applies the real environment last, so `SSO_CLIENT_SECRET=x` set once
   while testing wins over the file for every subsequent run, and correcting
   the file changes nothing. Reproduced through podman-compose's own
   substitution: a 39 character value in `.env` arrives as one character. This
   is indistinguishable from every other cause of a rejected secret, so the
   seeder now shows the MASKED value it was handed rather than only its length
   - three characters at each end identify a secret at a glance and leave it
   unusable - and it asks the identity provider, with a `client_credentials`
   request, whether the credentials it just wrote actually work. Only
   `invalid_client` is treated as failure: that is the provider rejecting the
   client authentication, which is the question. Anything else happened after
   the credentials were accepted. A provider that cannot be reached is reported
   and is not fatal, and is worth reading anyway, because ZITADEL needs the
   same network path from the same network to sign anybody in.

Cerbos telemetry is disabled in `deploy/cerbos/config.yaml` for the same
air-gap reason: by default it posts to `telemetry.cerbos.dev`, which is a
failing TLS handshake every few seconds where there is no egress.

## 14. Evidence

Measured against ZITADEL and Cerbos running in a container, not taken from
documentation:

**Measured against the stack in `deploy/`, brought up from an empty machine:**

| Question | Answer |
|---|---|
| `docker compose up` to fully seeded, from clean volumes | **18 s**, seeder exit 0 |
| Re-run seeder, nothing changed | creates nothing, reports `exists` |
| Re-run seeder after adding `software-04` | creates only that project and its 3 roles |
| ZITADEL cold start | 8 s (image 234 MB) |
| Token: 6-product user, 40 audiences requested | 2,772 B, 6 role claims |
| Token: security admin, 40 enumerated grants | **8,358 B** |
| Token: security admin, one global role | **883 B** |
| Grant lookup, ZITADEL API | ~12 ms wall (incl. process spawn) |
| Grant lookup, PostgreSQL alone | **0.063 ms**, index scan, 0 disk reads |
| JWKS without credentials | HTTP 200 |
| Token issued before `software-41`, used against it | **ALLOW** |
| User without a grant, same product | **DENY** |
| `org-security` on a product created after seeding | view, export **ALLOW** |
| `org-security` attempting a download promote | **DENY** (least privilege holds) |
| `org-operator` request / promote | ALLOW / **DENY** |
| `org-reader` view / request | ALLOW / **DENY** |
| `product-owner` of software-01, promote on software-02 | **DENY** |

**Not verified, and each must be before this is called done:**

- The Microsoft Entra federation leg. No Entra tenant is reachable from the
  environment this was built in, so `SSO_ISSUER` was left empty and
  username/password login was exercised instead. The seeder's IdP-creation path
  is therefore written but unproven.
- The size of a human token once Entra adds name, e-mail and profile claims.
  Every measurement above used machine users.
- The `web` and `controller` images were not built here: `npm ci` fails against
  this environment's TLS-intercepting proxy with `SELF_SIGNED_CERT_IN_CHAIN`.
  That is an environment limitation rather than a defect in the Dockerfiles,
  but it means neither image has been built end to end. `deploy/web/nginx.conf`
  was validated with `nginx -t` against the real image.
- The Go middleware described in §8 **does not exist yet**. The stack seeds
  ZITADEL correctly and Cerbos decides correctly, but the Coordinator still
  installs `AnonymousAuthenticator`. Wiring it is the remaining code change,
  and until it lands nothing in the API is actually enforced.
