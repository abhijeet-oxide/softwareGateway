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

`docker-compose.mock-entra.yml` and `deploy/mock-entra/` stand a mock directory
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

## 7. Files

- [`deploy/zitadel/bootstrap.mjs`](../../deploy/zitadel/bootstrap.mjs) - `createHuman`, `replaceIfUninitialised`, the attempt report
- [`deploy/deploy_test.go`](../../deploy/deploy_test.go) - `TestSeederDoesNotImportHumans`
- [`deploy/mock-entra/`](../../deploy/mock-entra/README.md) - the mock directory
- [`docker-compose.mock-entra.yml`](../../docker-compose.mock-entra.yml) - the overlay
- [`deploy/zitadel/docker-entrypoint.sh`](../../deploy/zitadel/docker-entrypoint.sh) - the contact on the refusal page
- [`docs/design/25-zitadel-login-v2-and-sso-redirect.md`](25-zitadel-login-v2-and-sso-redirect.md) - the sign-in flow this sits on
