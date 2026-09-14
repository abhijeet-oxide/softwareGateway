# Registering the application in Microsoft Entra

Single sign-on against Entra needs one app registration, and three of its
settings are easy to get wrong in ways that fail only after somebody has typed
their password. This is that registration, once.

It is the same registration for every way of running this: the only thing that
differs is the redirect URI, which is `<identity url>/idps/callback` — so
`http://localhost:8090/idps/callback` under compose, and whatever
`access.identity` says in a cluster. The seeder prints the exact URI on every
run.

Where the values go:

| | compose | a cluster |
|---|---|---|
| issuer | `SSO_ISSUER` in `.env` | `identity.sso.issuer` in the instance's values |
| client id | `SSO_CLIENT_ID` | `identity.sso.clientId` |
| client secret | `SSO_CLIENT_SECRET` | a Secret named by `identity.sso.existingSecret` — never a value in Git |

---

## Microsoft Entra: registering the app

ZITADEL needs one redirect URI registered in the Entra app registration, and it
is not any of the URLs a browser shows during a failed sign-in:

```
http://localhost:8090/idps/callback
```

That is `${ZITADEL_PUBLIC_URL}/idps/callback`. The seeder prints it on every
run. It must match exactly, including scheme, host and port. Entra requires
`https` for a redirect URI on anything other than `localhost`, so a deployment
reachable by name needs TLS in front of ZITADEL before SSO will work at all.

**Register it under the "Web" platform, not "Single-page application".** In the
Azure portal: App registrations, the app, Authentication, "Add a platform",
"Web".

Getting that wrong is the most likely way for sign-in to fail after the
password has already been typed:

```
http://localhost:8090/ui/v2/login/idp/oidc/failure?error=invalid_request
  &error_description=AADSTS9002325: Proof Key for Code Exchange is required
  for cross-origin authorization code redemption.
```

Entra treats a redirect URI registered as a single-page application as
belonging to a public client, whose token endpoint is CORS-enabled and requires
PKCE. ZITADEL is not that: it redeems the authorization code from its own
backend, using the client secret, which in Entra's model is a confidential
client and therefore the Web platform. Nothing on this side can be configured
around it - the registration is what has to change.

The rest of the app registration:

- a client secret (Certificates and secrets), copied into `SSO_CLIENT_SECRET`
- `SSO_CLIENT_ID` is the Application (client) ID
- `SSO_ISSUER` is `https://login.microsoftonline.com/<tenant-id>/v2.0`
- API permissions: `openid`, `profile`, `email` (delegated) are enough

### What the sign-in screen shows, and who arrives

The seeder configures the screen as well as the connector:

- **Self-registration is off, always.** Everyone who may use this gateway is
  provisioned - by the seeder, or by `config/users/users.yaml`.
- **Password sign-in is off once SSO is configured.** `SSO_ALLOW_PASSWORD_LOGIN=true`
  keeps it. Note that switching it off locks out `zitadel-admin` as well: it is
  in the same organization and has no Microsoft account. That is recoverable at
  any time, because the seeder authenticates with a machine token rather than
  through the sign-in screen - set the variable and re-run it.
- **Microsoft gets ZITADEL's Microsoft connector**, not a generic OIDC one, so
  the button carries Microsoft's mark. Anything else stays generic OIDC.
- **People are matched to their existing account by e-mail address**
  (`AUTO_LINKING_OPTION_EMAIL`). Without it, signing in through Microsoft
  creates a SECOND, brand new user with no roles and asks them to invent a
  username, while the account seeded for them sits beside it holding the roles.
  If a stack already has both, delete the empty duplicate in the ZITADEL
  console; from then on the linking is automatic.
- **The screen is branded** with `deploy/zitadel/branding/logo.svg` and this
  product's colours, and ZITADEL's watermark is off. See
  [zitadel/branding/README.md](../deploy/zitadel/branding/README.md).
- **One language.** ZITADEL's login still draws the picker - there is no
  setting that removes it - but with a single allowed language it has nothing
  to offer.

### Adding a person from the ZITADEL console

`config/users/users.yaml` is the reviewable way, and the console is the quick
one. Both work; the console has three traps, in steps 2 and 4.

1. Users, New. Fill in the address, the username and the name.
2. **Tick "Email Verified".** It is off by default, and it is the whole thing.
3. Choose "Setup authentication later for this User". They have no password
   here and do not need one.
4. Create, then grant them roles **on the `platform` project** - its keys are
   named `<product>:<role>` for product roles. Two traps here:
   - **Grant `org-member` as well as whatever else they get.** It carries no
     permission; it is what says this account was provisioned rather than
     merely able to sign in, and without it they meet a closed door telling
     them their account is not enabled.
   - **Do not grant on the product's own project.** It is the obvious place and
     the wrong one: no token carries roles for those projects, so the console
     shows the grant and the person signs in holding nothing.

   Or add them to `config/users/users.yaml` and re-run the seeder, which is the
   same grant in a file somebody can review - and which gets the baseline role
   right on its own.

**There is nothing to assign under a user's "Identity Providers" tab, and the
"No external IdP found" it shows is not a fault.** That table lists external
identities ALREADY LINKED to the account. It is a report, not a control: the
link is made by the person signing in, matched on their verified address, and
appears there by itself afterwards - the connector's id, `Microsoft`, their
directory object id and the name it asserted. An administrator has none of
those values before the fact, which is why there is nothing to pick from.

So the address is the assignment. An unverified one is matched by nothing, the
sign-in comes back `Errors.User.NotFound`, and the account looks completely
finished in the console while it happens. The seeder names every account in
that state on every run:

```
Accounts that cannot sign in through Microsoft
    count                         1

    USERNAME    ADDRESS                  REASON
    nobodyhere  nobody.here@contoso.com  the address is not verified
```

On an account that already exists, the box is under Contact Information.

**Re-running the seeder against a stack that is already up needs a restart:**

```bash
docker compose run --rm zitadel-init
docker compose restart zitadel-login      # it caches all of the above
```

Without the restart every write succeeds, the ZITADEL console shows the new
values, and the sign-in screen keeps the old ones. On a first run the ordering
already handles it: `zitadel-login` does not start until seeding has finished.

### `AADSTS7000215: Invalid client secret provided`

Two causes, and the first is the common one.

**The Value was not what got copied.** Azure's Certificates and secrets page
shows a Secret ID and a Secret Value side by side. The ID stays on screen
forever; the Value is shown once, when the secret is created, and is hidden
from then on. So the column that is still there to copy is the wrong one. A
secret ID is a GUID and a secret value is not, which is the check the seeder
now makes: it refuses to run rather than letting the mistake surface as a
Microsoft error code after somebody has typed their password. If the Value has
been lost, add a new client secret; it cannot be recovered.

**Or the secret in `.env` never reached ZITADEL.** The connector used to be
created on the first run and never touched again, so correcting
`SSO_CLIENT_SECRET` and re-running changed nothing: the seeder said
`SSO connector 'Microsoft' exists` and moved on while the old credentials
stayed in place.

It now writes the issuer, client id and secret on every run, and says which of
them moved:

```
SSO connector 'Microsoft' reconciled from .env
  client id CHANGED: 0000... -> bc69a09a-6358-414b-b52e-1a562a38cba7
  client secret CHANGED (38 characters)
  client id     : bc69a09a-6358-414b-b52e-1a562a38cba7
  client secret : 38 characters
```

The secret line reads `RECORDED` the first time, then `unchanged` or `CHANGED`.
That last one is the line to look for after correcting `.env`: **if a re-run
does not say `CHANGED`, the file was not what was read.**

Knowing that at all takes a little work, because ZITADEL returns a connector's
client id and issuer and never its secret. The seeder keeps a truncated
SHA-256 of what it last wrote in `/pat/sso-fingerprint.json` and compares
against that - which is why it can tell two different secrets of the same
length apart, and why the file contains a sixteen character digest rather than
anything usable.

The write itself is unconditional. Comparing only decides what to print: the
secret is pushed every run regardless, so a value changed by hand in the
console is put back to what `.env` says.

So after any change to `SSO_*`:

```bash
docker compose run --rm zitadel-init      # podman-compose run --rm zitadel-init
```

### A variable in the shell beats `.env`, silently and permanently

If `SSO_CLIENT_SECRET` exists in the shell, `.env` is ignored. Compose applies
the real environment last, so a value exported once - while testing, or in a
profile - wins over the file for every run in that session, and editing the
file changes nothing:

```
.env:                     SSO_CLIENT_SECRET=aBc8Q~the.long.correct.value-1234567890
shell:                    SSO_CLIENT_SECRET=x
what the container gets:  x        (1 character)
```

This is the explanation for a secret that stays what it was however many times
`.env` is corrected. Check it before anything else:

```powershell
$env:SSO_CLIENT_SECRET            # anything printed here is beating .env
Remove-Item Env:SSO_CLIENT_SECRET
```

```bash
echo "$SSO_CLIENT_SECRET"         # same question on a shell
unset SSO_CLIENT_SECRET
```

The seeder shows the masked value it was handed, so the count and the ends can
be compared against the portal without pasting the secret anywhere:

```
  client secret : aBc...890 (37 characters)
```

### `.env` can silently shorten a secret

Compose expands variables inside `.env` values, so a `$` in a secret is read as
the start of a variable name and the rest of the word disappears:

```
.env:          SSO_CLIENT_SECRET=aBc8Q~with$dollar.and_more
container gets: aBc8Q~with.and_more
```

Nothing warns about it. Write `$$` for a literal `$`, and avoid a ` #` inside
the value, which starts a comment. This is what the seeder's character count is
for: compare it with the length of the secret in the Azure portal, and if they
differ, `.env` ate part of it.

### The seeder asks the identity provider whether the credentials work

A wrong secret does not fail at seeding time. It fails at Microsoft, in
Microsoft's vocabulary, after somebody has typed their password. So the seeder
now makes a `client_credentials` request with the client id and secret it just
wrote, and refuses to finish if the provider rejects them:

```
FATAL: https://login.microsoftonline.com/<tenant>/v2.0 rejected these credentials.
       AADSTS7000215: Invalid client secret provided. Ensure the secret being
       sent in the request is the client secret value, not the client secret ID.
```

and says so plainly when they are good:

```
  credentials verified: a token was issued
```

Only `invalid_client` counts as failure: that is the provider rejecting the
client authentication, which is the question being asked. Any other error
happened after the credentials were accepted, so it proves the secret is right
and says nothing about the rest.

Being unable to reach the provider is reported and is not fatal, and it is
worth reading rather than skipping:

```
  ! could not verify the credentials: https://login.microsoftonline.com/...: fetch failed
    ZITADEL needs this same network path to sign anybody in, so this is
    worth fixing even though the seeding itself succeeded.
```

Behind a corporate proxy the seeder has no proxy configured, so this is
expected there. It still matters: ZITADEL reaches the provider from the same
network.

### Checking what ZITADEL actually holds

The seeder's output above is the quickest answer, and the character count is
the useful part: an Entra secret **ID** is 36 characters, a secret **value** is
not.

To read it back independently, open the ZITADEL console at
`http://localhost:8090/ui/console`, sign in as `zitadel-admin` with
`ZITADEL_ROOT_PASSWORD`, and go to the organization's Identity Providers. The
client ID and issuer are shown there.

**The client secret is not readable, by design** - ZITADEL's API returns the
client ID and issuer for a connector and never the secret. There is no way to
confirm a stored secret is correct except by using it, which is why the seeder
pushes `.env` into the connector on every run rather than trying to compare.
