/*
 * A stand-in for Microsoft Entra, faithful in the one way that matters.
 *
 * # Why this exists
 *
 * A sign-in through a corporate directory either links to a provisioned
 * account or is refused, and which one happens is decided by a single string:
 * the address ZITADEL believes the provider asserted. Nobody can read that
 * string. It is computed inside ZITADEL from a Microsoft Graph response, it is
 * never logged, and the only externally visible consequence is
 * `Errors.User.NotFound` - which is also what a genuinely unknown person gets.
 * So a real tenant gives one bit of information back, and a deployment that
 * fails here has no way forward except to guess and redeploy.
 *
 * This removes the guessing. ZITADEL's Microsoft connector does not take an
 * issuer: it has `login.microsoftonline.com` and `graph.microsoft.com`
 * compiled into it (internal/idp/providers/azuread). So this server ANSWERS TO
 * THOSE NAMES - a network alias on the compose network puts it there, and a
 * certificate for both names plus SSL_CERT_FILE makes ZITADEL believe it. The
 * stack under test is otherwise unmodified: the same seeder, the same
 * connector type, the same linking options, the same login screens.
 *
 * # What it is faithful to
 *
 * Exactly one thing, deliberately: the shape of the Graph `/v1.0/me` document,
 * because that is what the connector reads a person out of. In particular
 * `mail` may be null - a directory account with no mailbox has no `mail`
 * attribute - and ZITADEL then falls back to `userPrincipalName` and treats
 * THAT as the address to match on. Two different addresses, one of them
 * invisible, is the whole failure this reproduces.
 *
 * It is not faithful to anything else and does not try to be. There is no
 * password, no MFA, no consent, no token signing: choosing a person is a
 * button. This is a test double, and it is never built into an image or
 * reachable from a deployment that has not deliberately included
 * docker-compose.mock-entra.yml.
 */
import fs from 'node:fs';
import https from 'node:https';
import { URL, URLSearchParams } from 'node:url';
import { randomUUID } from 'node:crypto';

const DIRECTORY = JSON.parse(fs.readFileSync(process.env.MOCK_DIRECTORY || '/mock/directory.json', 'utf8'));
const PORT = Number(process.env.MOCK_PORT || 443);
const KEY = process.env.MOCK_TLS_KEY || '/certs/mock.key';
const CRT = process.env.MOCK_TLS_CERT || '/certs/mock.crt';

/* Authorization codes and access tokens, held for as long as the process runs.
 * A map rather than anything signed: nothing here is a security boundary, and
 * a token that can be read straight back is a token a test can assert on. */
const codes = new Map();
const tokens = new Map();

const personById = (id) => DIRECTORY.people.find((p) => p.id === id);

function log(...parts) {
  console.log(`[mock-entra] ${parts.join(' ')}`);
}

function json(res, status, doc) {
  const body = JSON.stringify(doc);
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) });
  res.end(body);
}

function html(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'text/html; charset=utf-8' });
  res.end(body);
}

function readBody(req) {
  return new Promise((resolve) => {
    let b = '';
    req.on('data', (c) => (b += c));
    req.on('end', () => resolve(b));
  });
}

/* The account picker.
 *
 * Every person in the directory is a button, and each one states the two
 * attributes that decide the outcome, because the point of this screen is to
 * choose WHICH assertion to send rather than to pretend to be a sign-in form.
 */
function picker(res, tenant, params) {
  const rows = DIRECTORY.people.map((p) => {
    const g = p.graph;
    return `<form method="POST" action="/${tenant}/oauth2/v2.0/pick">
      <input type="hidden" name="person" value="${p.id}">
      <input type="hidden" name="state" value="${params.get('state') || ''}">
      <input type="hidden" name="redirect_uri" value="${params.get('redirect_uri') || ''}">
      <button id="pick-${p.id}" type="submit">
        <strong>${g.displayName}</strong>
        <span>${p.label}</span>
        <code>mail: ${g.mail === null ? 'null' : g.mail}</code>
        <code>userPrincipalName: ${g.userPrincipalName}</code>
      </button>
    </form>`;
  }).join('\n');
  html(res, 200, `<!doctype html><meta charset="utf-8"><title>Mock Entra</title>
    <style>
      body{font:14px system-ui,sans-serif;margin:0;background:#f3f2f1;color:#201f1e}
      main{max-width:34rem;margin:3rem auto;background:#fff;padding:2rem;border-radius:2px;
           box-shadow:0 2px 6px rgba(0,0,0,.13)}
      h1{font-size:1.3rem;margin:0 0 .25rem}
      p.sub{margin:0 0 1.5rem;color:#605e5c}
      button{display:grid;gap:.15rem;width:100%;text-align:left;padding:.75rem;margin:0 0 .5rem;
             background:#fff;border:1px solid #8a8886;border-radius:2px;cursor:pointer;font:inherit}
      button:hover{background:#f3f2f1}
      span{color:#605e5c}
      code{font-size:12px;color:#605e5c}
    </style>
    <main>
      <h1>Mock Entra directory</h1>
      <p class="sub">Tenant ${tenant}. Choose the identity to assert.</p>
      ${rows}
    </main>`);
}

const server = https.createServer(
  { key: fs.readFileSync(KEY), cert: fs.readFileSync(CRT) },
  async (req, res) => {
    const host = (req.headers.host || '').split(':')[0];
    const url = new URL(req.url, `https://${host}`);
    const path = url.pathname;

    /* ---- Microsoft Graph: the document the connector reads a person out of */
    if (host === 'graph.microsoft.com') {
      if (path === '/v1.0/me') {
        const bearer = (req.headers.authorization || '').replace(/^Bearer\s+/i, '');
        const person = tokens.get(bearer);
        if (!person) return json(res, 401, { error: { code: 'InvalidAuthenticationToken' } });
        log('graph /v1.0/me ->', JSON.stringify(person.graph));
        return json(res, 200, person.graph);
      }
      return json(res, 404, { error: { code: 'ResourceNotFound', message: path } });
    }

    /* ---- login.microsoftonline.com --------------------------------------- */
    const seg = path.split('/').filter(Boolean);
    const tenant = seg[0] || 'common';

    if (path.endsWith('/.well-known/openid-configuration')) {
      const base = `https://login.microsoftonline.com/${tenant}`;
      return json(res, 200, {
        issuer: `${base}/v2.0`,
        authorization_endpoint: `${base}/oauth2/v2.0/authorize`,
        token_endpoint: `${base}/oauth2/v2.0/token`,
        jwks_uri: `${base}/discovery/v2.0/keys`,
        response_types_supported: ['code'],
        scopes_supported: ['openid', 'profile', 'email', 'User.Read'],
      });
    }

    if (path.endsWith('/discovery/v2.0/keys')) return json(res, 200, { keys: [] });

    if (path.endsWith('/oauth2/v2.0/authorize')) {
      log('authorize', 'redirect_uri=' + (url.searchParams.get('redirect_uri') || ''),
        'scope=' + (url.searchParams.get('scope') || ''));
      return picker(res, tenant, url.searchParams);
    }

    if (path.endsWith('/oauth2/v2.0/pick')) {
      const form = new URLSearchParams(await readBody(req));
      const person = personById(form.get('person'));
      if (!person) return json(res, 400, { error: 'unknown_person' });
      const code = randomUUID();
      codes.set(code, person);
      const back = new URL(form.get('redirect_uri'));
      back.searchParams.set('code', code);
      if (form.get('state')) back.searchParams.set('state', form.get('state'));
      log('chose', person.id, '->', back.toString());
      res.writeHead(302, { Location: back.toString() });
      return res.end();
    }

    if (path.endsWith('/oauth2/v2.0/token')) {
      const form = new URLSearchParams(await readBody(req));
      if (form.get('client_secret') !== DIRECTORY.clientSecret) {
        /* The wording Entra actually uses, because the seeder reads it and
         * turns it into advice about the Azure portal. */
        return json(res, 401, {
          error: 'invalid_client',
          error_description: 'AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value, not the client secret ID.',
        });
      }
      const token = randomUUID();
      if (form.get('grant_type') === 'client_credentials') {
        log('token client_credentials accepted');
        return json(res, 200, { token_type: 'Bearer', expires_in: 3600, access_token: token });
      }
      const person = codes.get(form.get('code'));
      if (!person) return json(res, 400, { error: 'invalid_grant' });
      codes.delete(form.get('code'));
      tokens.set(token, person);
      log('token authorization_code accepted for', person.id);
      return json(res, 200, {
        token_type: 'Bearer', expires_in: 3600, access_token: token, scope: 'openid profile email User.Read',
      });
    }

    log('unhandled', req.method, host + path);
    return json(res, 404, { error: 'not_found', path });
  },
);

server.listen(PORT, '0.0.0.0', () => {
  log(`listening on :${PORT} as login.microsoftonline.com and graph.microsoft.com`);
  log(`tenant ${DIRECTORY.tenantId}, ${DIRECTORY.people.length} people`);
});
