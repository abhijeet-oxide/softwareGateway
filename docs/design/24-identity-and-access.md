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
├── Project "platform"      = GLOBAL roles. Names no product.
│      manager, security-admin
│
├── Project "software-01"   = one PRODUCT
├── Project "software-02"     roles: product-owner, product-viewer
└── ...                       (one project per entry in GATEWAY_PRODUCTS)
```

A **role assignment** binds (user × project × roles) inside an org. That is
exactly "this person is product-owner of software-01 in the default tenant",
expressed once, natively.

### 5.1 The two tiers, and why global roles name no product

| Tier | Lives on | Example | Covers |
|---|---|---|---|
| **Global** | `platform` project | `manager`, `security-admin` | every product in the tenant, **including ones created later** |
| **Product** | that product's project | `product-owner`, `product-viewer` | that product only |

> **Decision - a global role is ONE assignment on `platform`, never N assignments across N products.**
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

### 5.2 The four personas

| Persona | How it is expressed | Scope |
|---|---|---|
| **Manager** - full access to every product | `manager` on `platform` | tenant |
| **Security admin** - security detail across products | `security-admin` on `platform` | tenant |
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
GATEWAY_PRODUCT_ROLES=product-owner,product-viewer
GATEWAY_GLOBAL_ROLES=manager,security-admin

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

## 12. Evidence

Measured against ZITADEL and Cerbos running in a container, not taken from
documentation:

| Question | Answer |
|---|---|
| ZITADEL cold start | 8 s (image 234 MB) |
| Token: 6-product user, 40 audiences requested | 2,772 B, 6 role claims |
| Token: security admin, 40 enumerated grants | **8,358 B** |
| Token: security admin, one global role | **883 B** |
| Grant lookup, ZITADEL API | ~12 ms wall (incl. process spawn) |
| Grant lookup, PostgreSQL alone | **0.063 ms**, index scan, 0 disk reads |
| JWKS without credentials | HTTP 200 |
| Token issued before `software-41`, used against it | **ALLOW** |
| User without a grant, same product | **DENY** |

Two things are NOT verified and must be before this is called done: the
Microsoft Entra federation leg itself, and the size of a human token once Entra
adds name, e-mail and profile claims.
