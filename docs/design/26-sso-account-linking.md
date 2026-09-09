# 26. Why a provisioned administrator could not sign in

## 1. The symptom

A stack seeded from empty volumes, with Microsoft Entra configured and the
administrator's address set correctly, refused that administrator:

```
Account Not Found
We couldn't find an account associated with your identity provider credentials.
```

The seeder, run again immediately afterwards, reported the opposite:

```
1 account(s) can sign in through Microsoft, matched on address:
    ap999e                       ap999e@att.com
```

Both statements were true. Neither pointed at the fault, and the two obvious
explanations - the wrong address, or a connector misconfiguration - were both
wrong. `docker compose down -v` and a rebuild changed nothing, because the
failure was deterministic: **nobody could ever have signed in to that stack.**

## 2. The fault

`POST /management/v1/users/human/_import` accepts an `isEmailVerified: true`
flag. When the request carries **no password**, it ignores it: the address is
stored unverified and the account is left in `USER_STATE_INITIAL`. The endpoint
answers `200`. Nothing warns.

The seeder deliberately sets no password when SSO is configured - the person
signs in through the identity provider and never needs one - so every human it
created was uninitialised, with an unverified address.

ZITADEL auto-links an arriving external identity to an existing user by a
**verified** address on an **active** account. None of those accounts qualified.
Every sign-in was refused as `Errors.User.NotFound`, which is also the answer for
a person nobody has ever heard of, so the refusal named nothing.

`USER_STATE_INITIAL` is also a dead end. All three ways out are refused:

| attempted on an INITIAL user | answer |
|---|---|
| `PUT /management/v1/users/{id}/email` | `User is not yet initialized (COMMAND-J8dsk)` |
| `POST /v2/users/{id}/email` | `User is not yet initialized (COMMAND-uz0Uu)` |
| `POST /v2/users/{id}/password` | `User is not yet initialized (COMMAND-M9dse)` |

The account cannot be corrected, cannot be verified and cannot be given a
password. The only way out is the initialisation mail, which this stack has no
mailer for.

Measured, rather than reasoned about:

| create call | resulting state | address |
|---|---|---|
| `_import`, no password | `USER_STATE_INITIAL` | unverified |
| `_import`, with password | `USER_STATE_ACTIVE` | verified |
| `/v2/users/human`, no password | `USER_STATE_ACTIVE` | verified |
| `/v2/users/human`, with password | `USER_STATE_ACTIVE` | verified |

## 3. What was changed

- **People are created through `POST /v2/users/human`.** It honours the flag with
  or without a password. `deploy/deploy_test.go` fails the build if the import
  endpoint comes back, because the two calls look interchangeable and differ in
  nothing a reviewer can see.
- **Accounts already stuck in `USER_STATE_INITIAL` are replaced.** Nothing is
  lost: nobody has ever signed in to one - they could not - and the run that
  replaces the account writes its grants again. The alternative is a stack that
  reports success forever and lets nobody in. The replacement is named in the
  seeder's summary.
- **The eligibility listing tells the truth.** "Who can sign in" now requires a
  verified address on an active account, not merely an address. It was that
  listing, saying yes while the door said no, that made this take as long as it
  did.

## 4. The other half: what the directory actually asserted

Fixing the fault does not fix the class of problem, because the same refusal
still happens for an ordinary reason - the directory asserts an address nobody
provisioned - and it is still invisible. ZITADEL computes that address inside
the connector and never shows it.

It does record it. Every attempt writes an `idpintent.succeeded` event
(succeeded meaning the provider authenticated the person, not that the sign-in
worked) carrying the provider's own document about them. The seeder now reads
the last few and prints them beside the addresses the stack holds:

```
Sign-in attempts through Microsoft
    recorded                      3

    WHEN                 ASSERTED ADDRESS                    MATCHED ACCOUNT
    2026-09-08 19:37:40  dee.stranger@contoso.com            none
    2026-09-08 19:31:57  alex.hart@contoso.com               ap999e
    2026-09-08 19:29:14  bn5678@contoso.com                  none
```

Which value becomes the asserted address is the connector's rule, and for
Microsoft it is worth knowing:
`internal/idp/providers/azuread` reads Graph `/v1.0/me` and returns the `mail`
attribute, **or the `userPrincipalName` when `mail` is null**. A directory
account with no mailbox therefore asserts its UPN, which is usually not the
address an administrator typed into `users.json`.

## 5. Reproducing it without a tenant

`test/mockEntra/docker-compose.mock-entra.yml` and `deploy/mock-entra/` stand a mock directory
behind `login.microsoftonline.com` and `graph.microsoft.com` - those two names
are compiled into ZITADEL's Microsoft connector, so a network alias, a
certificate for both names and `SSL_CERT_FILE` are what it takes to be them. The
connector itself is not reconfigured, so the code path under test is the real
one. See [`deploy/mock-entra/README.md`](../../deploy/mock-entra/README.md).

Verified through a real browser against a stack brought up from empty volumes:

- a provisioned person whose Graph `mail` matches the seeded address signs
  straight into the application as `org-admin`, and into ZITADEL's own console;
- a person whose Graph `mail` is null asserts their UPN and is refused;
- a person nobody provisioned is refused;
- an account deliberately re-created through the old `_import` path is detected
  as uninitialised, replaced, and signs in on the next attempt.

## 6. What the refusal says

A person refused this way never reaches the application: they are stopped inside
ZITADEL, on its own Account Not Found page. What that page says by default is
written for whoever built the integration:

> We couldn't find an account associated with your identity provider
> credentials.
>
> No existing account was found. Please sign in with an existing account or
> contact your administrator for assistance.

Two sentences, one of them about identity providers and credentials, and neither
naming anybody to ask. The reader is a colleague who has been told no. It now
says one thing:

> **Account Not Found**
>
> This account is not registered. Kindly reach out to platform-team@example.com
> for access.

The address is `SUPPORT_CONTACT`, defaulting to the bootstrap administrator - the
person who provisions accounts here. With neither set the sentence still
completes and names a role instead, because an invented address sends people to a
mailbox nobody reads.

`zitadel-proxy` does the substitution, being already the only thing between that
page and a browser, and it does it in CSS rather than by rewriting the HTML. The
direct version was written and watched: the server sends the new sentence, React
hydrates over it, and the original is back before anybody reads it. A stylesheet
is not hydrated. The cost is that the address is text rather than a link.

The **asserted** address cannot be shown there. It is not in the page, not in
the URL, and not in any redirect the browser sees - §4 is where it lives.

## 7. What a person sees on their own profile

Two things were wrong with it, and both come from the same place: the seeder
writes a profile for somebody it has never met.

**It invented a name.** ZITADEL requires both a given and a family name, and
what was written was `Platform` / `Administrator` for the first administrator
and `User` as a surname for everybody in `users.json`. A fabricated person's
name, on a real person's account, under their own initials.

**And the invention outlived the sign-in.** The connector carries
`isAutoUpdate`, so the directory's own name does replace it - but not on the
sign-in that LINKS the account, only on the next one. Measured against the
mock, three consecutive sign-ins on a fresh stack:

| sign-in | `name` claim |
|---|---|
| 1 | `Platform Administrator` |
| 2 | `Alex Hart` |
| 3 | `Alex Hart` |

So there is a real window - usually somebody's first impression of the product
- where whatever the seeder chose is what they read.

`nameFor` replaced the invention with three sources, in order of how likely
each is to be true:

1. what the operator supplied: `firstName`/`lastName` in `users.json`, or
   `BOOTSTRAP_ADMIN_FIRST_NAME`/`BOOTSTRAP_ADMIN_LAST_NAME`;
2. what the ADDRESS spells, when it spells a name: `alex.hart@example.com` is
   Alex Hart in every directory that issues addresses that way. Only when every
   part is letters - `ap999e@` spells nothing, and a login id capitalised into
   a surname is worse than no name at all;
3. the username, in the DISPLAY name, so the page reads `ap999e` rather than
   `ap999e ap999e` - which is what setting only the two halves gives, because
   ZITADEL's `name` claim is the display name.

The first sign-in on a fresh stack now reads `Alex Hart`.

**The page says where the details came from.** A profile showing a name and an
address and nothing else cannot answer the question it provokes, and the answer
was never this product's to give: it is what the directory asserted. So the
fields are listed, the ones that did not arrive are named as not arriving
("Not provided by Microsoft"), and the source is stated:

```
DIRECTORY
  Source            Microsoft
  Full name         Alex Hart
  Given name        Alex
  Family name       Hart
  Language          Not provided by Microsoft
  Details updated   09 Sept 2026, 07:55 am

  Read from Microsoft at each sign-in. A field Microsoft does not hold is not
  held here. The first sign-in links the account only; details recorded at
  provisioning are replaced from the second.
```

There is no more to show than this. ZITADEL's Microsoft connector reads Graph
`/v1.0/me` and maps `id`, `givenName`, `surname`, `displayName`, `mail` or
`userPrincipalName`, and `preferredLanguage` onto its user; job title,
department and the rest of the Graph document are not carried into ZITADEL at
all, so they are not in the token and cannot be shown without a ZITADEL Action.

`SSO_DISPLAY_NAME` is published into `/runtime-config.json` so the page can name
the provider. It is one string across three surfaces - the sign-in button, the
refusal page and this - which is what stops them disagreeing about what the
organization's identity provider is called.

## 8. A user made in ZITADEL's console

Reported as: "I go to users, I create users, but I am not able to make them use
Microsoft to log in. It says no external IDP found."

There is nothing to assign there, and the message is not a fault. A user's
**Identity Providers** tab lists the external identities ALREADY LINKED to that
account. It is a report, not a control. Verified against the mock: an account
that has signed in once shows

| IDP CONFIG ID | IDP NAME | EXTERNAL USER ID | EXTERNAL NAME |
|---|---|---|---|
| 389979434733535237 | Microsoft | 00000000-...-0000000000a4 | ds3456@contoso.com |

and an account that never has shows "No external IdP found". An administrator
holds none of those values before the fact - the directory object id is minted
by the sign-in - which is why there is nothing to pick from.

**The address is the assignment**, and the console has one trap in it: the
create-user form's **Email Verified** box is off by default. Measured:

| created in the console | state | address | signs in through Microsoft |
|---|---|---|---|
| Email Verified ticked | ACTIVE | verified | yes, and links itself |
| Email Verified unticked | ACTIVE | unverified | no: `Errors.User.NotFound` |

Both accounts look finished in the console. Only one can ever be matched, and
nothing anywhere connects the tick to the refusal - which is the same shape as
§2, in a different place.

So the seeder now reports the complement of "who can sign in". The list of who
can was the answer to "can this person get in"; an account that cannot be
matched was simply absent from it, which looks exactly like an account nobody
has added:

```
Accounts that cannot sign in through Microsoft
    count                         2

    USERNAME     ADDRESS                  REASON
    nobodyhere   nobody.here@contoso.com  the address is not verified
    test-reader  test-reader@example.com  the address is a placeholder that no directory asserts
```

The console steps that produce a linkable account are in
[`deploy/STACK.md`](../../deploy/STACK.md).

## 9. Files

- [`deploy/zitadel/bootstrap.mjs`](../../deploy/zitadel/bootstrap.mjs) - `createHuman`, `replaceIfUninitialised`, the attempt report
- [`deploy/deploy_test.go`](../../deploy/deploy_test.go) - `TestSeederDoesNotImportHumans`
- [`deploy/mock-entra/`](../../deploy/mock-entra/README.md) - the mock directory
- [`test/mockEntra/docker-compose.mock-entra.yml`](../../test/mockEntra/docker-compose.mock-entra.yml) - the overlay
- [`deploy/zitadel/docker-entrypoint.sh`](../../deploy/zitadel/docker-entrypoint.sh) - the contact on the refusal page
- [`web/src/pages/Profile.tsx`](../../web/src/pages/Profile.tsx), [`web/src/auth/session.ts`](../../web/src/auth/session.ts) - the Directory section and the claims behind it
- [`docs/design/25-zitadel-login-v2-and-sso-redirect.md`](25-zitadel-login-v2-and-sso-redirect.md) - the sign-in flow this sits on
