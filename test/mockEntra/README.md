# A Microsoft directory whose answers can be read

`test/mockEntra/docker-compose.mock-entra.yml` replaces Microsoft Entra with a mock that
ZITADEL's own Microsoft connector talks to unmodified, so a sign-in failure can
be reproduced on a laptop instead of diagnosed by redeploying against a live
tenant.

## Why it exists

A federated sign-in either links to a provisioned account or is refused, and
which one happens is decided by a single string: the address ZITADEL believes
the directory asserted. That string is never shown. It is computed inside
ZITADEL from a Microsoft Graph response, and the only externally visible
consequence is `Errors.User.NotFound` - which is also the answer for a person
nobody has ever heard of.

So a real tenant returns one bit of information, and a deployment that fails
here has nothing to go on. This makes the string controllable.

## Running it

```sh
./test/mockEntra/mkcerts.sh
docker compose -f docker-compose.yml -f test/mockEntra/docker-compose.mock-entra.yml up -d --force-recreate
```

`mkcerts.sh` needs `openssl` and nothing else. It writes an authority and a
certificate into `test/mockEntra/certs/`, which is not tracked: a private key
that ships with a project is a private key that ends up trusted somewhere it was
never meant to be.

Then open <http://localhost:8000>, press the Microsoft button, and choose an
identity from the mock's picker.

## What it is faithful to

Exactly one thing, deliberately: the shape of the Graph `/v1.0/me` document,
because that is what the connector reads a person out of.

| Graph attribute | What ZITADEL does with it |
|---|---|
| `mail` | becomes the address auto-linking matches on |
| `userPrincipalName` | becomes that address instead, when `mail` is null |
| `id` | the external identity's own id, stored on the link |

`mail` being null is the case worth having: a directory account with no mailbox
has no `mail` attribute, ZITADEL silently falls back to the user principal name,
and an administrator who provisioned the person under their mailbox address gets
a refusal that names nothing.

`test/mockEntra/directory.json` holds the people. Four of them cover the
outcomes that differ:

| id | asserts | expected |
|---|---|---|
| `mailbox` | `alex.hart@contoso.com` | links, when an account holds that address |
| `no-mailbox` | `bn5678@contoso.com`, the UPN | refused unless provisioned under the UPN |
| `upn-elsewhere` | `casey.doyle@contoso.com` | mailbox and UPN on different domains |
| `stranger` | `dee.stranger@contoso.com` | refused: not provisioned |

There is no password, no MFA, no consent and no token signing. Choosing a person
is a button. This is a test double.

## How it gets to be Microsoft

ZITADEL's Microsoft connector takes no issuer: `login.microsoftonline.com` and
`graph.microsoft.com` are compiled into it
(`internal/idp/providers/azuread`). Three things put the mock behind those
names, and all three are in the overlay:

- a **network alias** on the compose network, so Docker's embedded DNS hands
  every container the mock's address for both hostnames;
- a **certificate** for both names, trusted through `SSL_CERT_FILE` (Go reads
  it) and `NODE_EXTRA_CA_CERTS` (Node does not read the former);
- `ZITADEL_HTTPCLIENT_DENYLIST` set to an inert range, because ZITADEL refuses
  outbound calls to private address space and a compose network is exactly that.
  An **empty** value reads as unset and leaves the default in place.

Nothing about the connector itself is reconfigured, which is the point: the code
path under test is the one a real tenant uses.

## Driving it from a browser

The browser is redirected to `login.microsoftonline.com` too, so a test needs
its own route to the mock. The overlay publishes it on 8443:

```js
chromium.launch({ args: [
  '--no-proxy-server',
  '--host-resolver-rules=MAP login.microsoftonline.com 127.0.0.1:8443, ' +
                        'MAP graph.microsoft.com 127.0.0.1:8443',
]})
// and a context with ignoreHTTPSErrors: true
```

## Reading the result

After an attempt, the seeder reports what was asserted and whether anything
matched:

```
docker compose run --rm zitadel-init
...
Sign-in attempts through Microsoft
    recorded                      3

    WHEN                 ASSERTED ADDRESS                    MATCHED ACCOUNT
    2026-09-08 19:37:40  dee.stranger@contoso.com            none
    2026-09-08 19:31:57  alex.hart@contoso.com               ap999e
    2026-09-08 19:29:14  bn5678@contoso.com                  none
```

That section is not part of the mock. It reads ZITADEL's `idpintent.succeeded`
events and works the same way against a real tenant, which is where it is
actually needed.
