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
└── ...                       (one project per entry in GATEWAY_PRODUCTS)
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

# --- what exists ------------------------------------------------------------
GATEWAY_PRODUCTS=software-01,software-02,software-03
GATEWAY_ORG_ROLES=org-admin,org-operator,org-security,org-reader
GATEWAY_PRODUCT_ROLES=product-owner,product-operator,product-reader

# --- break glass: set EXACTLY ONE (see §7) ----------------------------------
BOOTSTRAP_ADMIN_EMAIL=platform-admin@example.com    # production
# BOOTSTRAP_ADMIN_PASSWORD=...                      # local development only
```

`GATEWAY_PRODUCTS` is the seeder's input. It is deliberately NOT read from the
product YAML directory: see §3's decision. The two lists are expected to match,
and `transferctl config check` should report a product with no project.

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
2. Re-running the seeder after adding to `GATEWAY_PRODUCTS` adds only the new
   project and its roles.
3. A global role authorizes a product created after the token was issued.
4. A user with no grant on a product is refused it.
5. The break-glass account is disabled, and its still being enabled is reported.
6. `docs/design/09` §10's risk note is deleted, because it is no longer true.
7. **Still open.** The human routes authenticate and do not authorize: any
   caller with a valid token reaches every one of them whatever roles they
   hold. The machinery is all present - `Identity.Can`, scoped grants, Cerbos
   wired - and no handler asks it. The data plane is fenced (§5.1a) because its
   credential is new and its blast radius is bounded and known; a person with
   no roles is currently refused nothing.
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
`build/Dockerfile.coordinator` and the worker's wire config.

> **Decision - rename at the deployment layer now, in the code never (or behind a deprecation).**
>
> The compose service, its DNS name and the nginx upstream all say `controller`. Anyone operating the stack sees the right word. Anyone reading the Go sees `coordinator` and one paragraph saying why.

## 13. Operating it

```bash
docker compose up -d          # start, in dependency order
docker compose down           # stop, reverse order, data kept
docker compose down -v        # stop and discard the databases

# after editing GATEWAY_PRODUCTS in .env:
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
