#!/usr/bin/env node
/**
 * ZITADEL day-0 seeder. One shot, idempotent, safe to re-run.
 *
 * Creates the org (tenant), the `platform` project holding org-wide roles, one
 * project per product with its roles, the web OIDC client, the Microsoft SSO
 * connector when configured, and the first administrator.
 *
 * Node with no dependencies, deliberately. The obvious shell version needs
 * curl and jq, which means `apk add` at container start - a network call to a
 * distro CDN on every boot. This product ships into air-gapped estates
 * (docs/design/19 section 1), so a seeder that cannot run without internet
 * access is a seeder that cannot run where it matters. node:22-alpine has an
 * HTTP client and JSON built in, and is already required for the web build.
 */
const Z        = process.env.ZITADEL_INTERNAL_URL || 'http://zitadel:8080';
const PAT_FILE = process.env.PAT_FILE || '/pat/pat.txt';
/* ZITADEL validates Host against ZITADEL_EXTERNALDOMAIN and answers 404 to
 * anything else. Internally we reach it as `zitadel:8080` but it only answers
 * to its external name, so every request carries that Host explicitly. */
const ZHOST    = process.env.ZITADEL_HOST_HEADER || 'localhost:8090';

/* Validated before anything else touches the network: this is a config
 * error, not a transient one, so failing here prints one clear message
 * instead of the same FATAL buried under a wait-and-retry loop on every one
 * of the container's `restart: on-failure` attempts. */
if (process.env.SSO_ISSUER && process.env.BOOTSTRAP_ADMIN_PASSWORD) {
  console.error('FATAL: BOOTSTRAP_ADMIN_PASSWORD is set while SSO is configured.');
  console.error('       The password shortcut is for local use only. Unset it.');
  process.exit(1);
}

const fs   = await import('node:fs/promises');
const http = await import('node:http');
const { createHash } = await import('node:crypto');
const say = (...a) => console.log('  ' + a.join(' '));

/* Two accounts found for one person, collected while looking them up and
 * reported together at the end.
 *
 * Declared HERE rather than beside findUserId, which is where it belongs and
 * where it does not work: a function declaration is hoisted and this file
 * calls findUserId from a section that runs well before that point, while a
 * const is not hoisted with it. Left there, the first duplicate this ever
 * found threw a ReferenceError instead of reporting itself - a crash in the
 * one code path written to explain a confusing situation. */
const duplicates = [];
const list = (v, d) => (v ?? d).split(',').map(s => s.trim()).filter(Boolean);

/* A credential, shown well enough to recognise and not well enough to use. */
function mask(v) {
  if (v.length < 12) return `${v.length} characters (too short to show safely)`;
  return `${v.slice(0, 3)}...${v.slice(-3)} (${v.length} characters)`;
}

/* Writes a file another container reads, and says so when it cannot.
 *
 * The volume is mounted read-only into some of this stack's containers and
 * read-write into this one; a mode mistake shows up here as EROFS, at seeding
 * time, rather than as a service that starts and then cannot sign anyone in. */
async function writeShared(path, contents, mode = 0o644) {
  try {
    await fs.mkdir(path.slice(0, path.lastIndexOf('/')) || '/', { recursive: true });
    await fs.writeFile(path, contents, { mode });
  } catch (e) {
    console.error(`FATAL: could not write ${path}: ${e.message}`);
    process.exit(1);
  }
}

/* Ask the identity provider whether these credentials are real.
 *
 * # Why the seeder does this rather than leaving it to the first sign-in
 *
 * A wrong client secret does not fail here. It fails at the identity provider,
 * in the identity provider's vocabulary, AFTER somebody has typed their
 * password - `AADSTS7000215: Invalid client secret provided` - and it looks
 * identical whether the secret is wrong, stale, or was never written. Nothing
 * in this stack can tell those apart afterwards, because ZITADEL returns a
 * connector's client id and issuer and never its secret.
 *
 * A client_credentials request answers it in one call. Only `invalid_client`
 * is treated as proof of failure: it means the provider rejected the client
 * AUTHENTICATION, which is exactly the question being asked. Any other error -
 * an unsupported grant, a missing scope, no consent - happened after the
 * credentials were accepted, so it says the secret is good and says nothing
 * about the rest.
 *
 * Being unable to reach the provider is reported and is NOT fatal, and it is
 * worth reading rather than skipping: ZITADEL needs the same network path from
 * the same network, so a seeder that cannot reach the issuer is a sign-in that
 * will not work either.
 */
async function verifyIdpCredentials(issuer, clientId, clientSecret) {
  const base = issuer.replace(/\/+$/, '');
  let tokenEndpoint;
  try {
    const disc = await fetch(`${base}/.well-known/openid-configuration`, {
      headers: { Accept: 'application/json' }, signal: AbortSignal.timeout(15000),
    });
    if (!disc.ok) return { unreachable: `${base} answered ${disc.status} for its discovery document` };
    tokenEndpoint = (await disc.json()).token_endpoint;
    if (!tokenEndpoint) return { unreachable: `${base} published no token endpoint` };
  } catch (e) {
    return { unreachable: `${base}: ${e.message}` };
  }

  const body = new URLSearchParams({
    grant_type: 'client_credentials', client_id: clientId, client_secret: clientSecret,
  });
  // Entra's v2 endpoint requires a scope; an app's own `.default` needs no
  // consent and no permissions, so it tests authentication and nothing else.
  if (/login\.microsoftonline\.com/.test(base)) body.set('scope', `${clientId}/.default`);

  try {
    const r = await fetch(tokenEndpoint, {
      method: 'POST', body,
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      signal: AbortSignal.timeout(15000),
    });
    if (r.ok) return { ok: true, detail: 'a token was issued' };
    const doc = await r.json().catch(() => ({}));
    if (doc.error === 'invalid_client') {
      return { rejected: true, detail: (doc.error_description || 'invalid_client').split(/\r?\n/)[0] };
    }
    return { ok: true, detail: `the provider accepted them and answered ${doc.error || r.status} to the rest` };
  } catch (e) {
    return { unreachable: `${tokenEndpoint}: ${e.message}` };
  }
}

/* Uploads a file to one of ZITADEL's asset endpoints.
 *
 * Multipart is assembled by hand because fetch would be simpler and drops the
 * Host header, which ZITADEL answers 404 without - the same reason the rest of
 * this file uses node:http. Returns "missing" when there is no such file,
 * which is not an error: a deployment that supplies no logo keeps ZITADEL's.
 */
async function uploadAsset(path, file) {
  let body;
  try { body = await fs.readFile(file); } catch { return 'missing'; }
  const b = '----swgw' + Date.now();
  const type = file.endsWith('.svg') ? 'image/svg+xml'
    : file.endsWith('.jpg') || file.endsWith('.jpeg') ? 'image/jpeg' : 'image/png';
  const payload = Buffer.concat([
    Buffer.from(`--${b}\r\nContent-Disposition: form-data; name="file"; filename="${file.split('/').pop()}"\r\nContent-Type: ${type}\r\n\r\n`),
    body, Buffer.from(`\r\n--${b}--\r\n`)]);
  const r = await request('POST', path, payload, `multipart/form-data; boundary=${b}`);
  return r.status;
}

let PAT = '', ORG_ID = '';

/* node:http rather than fetch, and this is not a style choice: the Fetch spec
 * lists Host as a forbidden header, so undici DROPS it silently. ZITADEL then
 * sees Host: zitadel:8080, does not recognise it as its external domain, and
 * answers 404 to every call while being perfectly healthy. node:http is the
 * only built-in that lets us set it. */
function request(method, path, body, contentType) {
  const u = new URL(Z + path);
  // A Buffer is sent as it is - that is an asset upload, whose body is already
  // encoded. Anything else is this API's JSON.
  const payload = body === undefined ? null
    : Buffer.isBuffer(body) ? body : JSON.stringify(body);
  const headers = {
    'Host': ZHOST,
    'Content-Type': contentType || 'application/json',
    'Authorization': `Bearer ${PAT}`,
  };
  if (payload) headers['Content-Length'] = Buffer.byteLength(payload);
  if (ORG_ID) headers['x-zitadel-orgid'] = ORG_ID;
  return new Promise((resolve, reject) => {
    const req = http.request(
      { host: u.hostname, port: u.port || 80, path: u.pathname + u.search, method, headers },
      res => {
        let data = '';
        res.on('data', c => (data += c));
        res.on('end', () => resolve({ status: res.statusCode, text: data }));
      });
    req.on('error', reject);
    if (payload) req.write(payload);
    req.end();
  });
}

/* ZITADEL answers a write that changes nothing with an ERROR - "has not been
 * changed" - so on a seeder that is run repeatedly by design, the second run
 * of a correct configuration reports failures. Treated as success, because it
 * is: the desired state is the state. */
const unchanged = r => r.__status >= 400 && /not\s*been\s*changed|NotChanged/i.test(r.message || '');

async function api(method, path, body) {
  const r = await request(method, path, body);
  let json; try { json = JSON.parse(r.text); } catch { json = { raw: r.text }; }
  if (r.status >= 400) json.__status = r.status;
  return json;
}

/* --- 1. wait for ZITADEL and the PAT the first-instance step writes -------- */
process.stdout.write('waiting for zitadel');
for (let i = 0; i < 180; i++) {
  try {
    const r = await request('GET', '/.well-known/openid-configuration');
    const pat = await fs.readFile(PAT_FILE, 'utf8').catch(() => '');
    if (r.status === 200 && pat.trim()) { PAT = pat.trim(); break; }
  } catch { /* not up yet */ }
  process.stdout.write('.');
  await new Promise(r => setTimeout(r, 2000));
}
if (!PAT) { console.error('\nFATAL: no PAT at ' + PAT_FILE + ' after 360s'); process.exit(1); }
console.log(' ok');

/* --- 2. the organization is the tenant ------------------------------------ */
const TENANT = process.env.GATEWAY_TENANT || 'default';
{
  const found = await api('POST', '/admin/v1/orgs/_search', {});
  ORG_ID = (found.result || []).find(o => o.name === TENANT)?.id || '';
  if (!ORG_ID) {
    ORG_ID = (await api('POST', '/management/v1/orgs', { name: TENANT })).id;
    say(`tenant '${TENANT}' created (${ORG_ID})`);
  } else {
    say(`tenant '${TENANT}' exists (${ORG_ID})`);
  }
}

/* --- 3. idempotent project + roles ---------------------------------------- */
async function ensureProject(name) {
  const found = await api('POST', '/management/v1/projects/_search', { query: { limit: 500 } });
  let id = (found.result || []).find(p => p.name === name)?.id;
  if (!id) {
    id = (await api('POST', '/management/v1/projects',
      { name, projectRoleAssertion: true })).id;
    say(`project ${name} created`);
  }
  // Assertion must be on or roles never reach a token, and creating with the
  // flag does not always persist it. Set it explicitly every run.
  await api('PUT', `/management/v1/projects/${id}`, { name, projectRoleAssertion: true });
  return id;
}

async function ensureRoles(projectId, roles) {
  const have = new Set(((await api('POST', `/management/v1/projects/${projectId}/roles/_search`,
    { query: { limit: 500 } })).result || []).map(r => r.key));
  for (const key of roles) {
    if (have.has(key)) continue;
    await api('POST', `/management/v1/projects/${projectId}/roles`,
      { roleKey: key, displayName: key, group: 'gateway' });
    say(`  + role ${key}`);
  }
}

/* --- 4. org-wide roles live on `platform` and name no product ------------- */
say('platform project (org-wide roles)');
const PLATFORM = await ensureProject('platform');
await ensureRoles(PLATFORM,
  list(process.env.GATEWAY_ORG_ROLES, 'org-admin,org-operator,org-security,org-reader'));

/* --- 5. one project per product -------------------------------------------
 *
 * Role keys are namespaced "<product>:<role>". ZITADEL emits roles under a
 * claim keyed by PROJECT ID - an opaque number - so a bare `product-owner` in
 * a token cannot say WHICH product it refers to. Putting the product in the
 * key makes the token self-describing, which is what lets pkg/authz stay
 * stateless: no project-id map to ship, no ZITADEL credential in any service
 * just to resolve a name. See pkg/authz/identity.go splitProductRole.
 */
const products = list(process.env.GATEWAY_PRODUCTS, '');
const productRoles = list(process.env.GATEWAY_PRODUCT_ROLES,
  'product-owner,product-operator,product-reader');
const projectOf = new Map();          // product name -> project id
for (const p of products) {
  say(`product ${p}`);
  const pid = await ensureProject(p);
  projectOf.set(p, pid);
  await ensureRoles(pid, productRoles.map(r => `${p}:${r}`));
}

/* --- 6. the web application (OIDC client) ---------------------------------
 *
 * The client id is GENERATED by ZITADEL, so it cannot be a variable in .env
 * and it cannot be baked into the SPA at build time. It used to be printed
 * here and nowhere else, which meant the one value the browser needs in order
 * to start a login existed only in this container's logs. It is written to a
 * shared volume now, and the web tier renders it into a runtime config the SPA
 * reads before it boots. See deploy/web/docker-entrypoint.sh.
 */
{
  const WEB = process.env.WEB_PUBLIC_URL || 'http://localhost:8000';
  const redirectUri = `${WEB}/auth/callback`;
  const oidc = {
    redirectUris: [redirectUri],
    postLogoutRedirectUris: [`${WEB}/`],
    responseTypes: ['OIDC_RESPONSE_TYPE_CODE'],
    grantTypes: ['OIDC_GRANT_TYPE_AUTHORIZATION_CODE', 'OIDC_GRANT_TYPE_REFRESH_TOKEN'],
    appType: 'OIDC_APP_TYPE_USER_AGENT',
    authMethodType: 'OIDC_AUTH_METHOD_TYPE_NONE',   // public client + PKCE
    devMode: String(process.env.OIDC_DEV_MODE ?? 'true') === 'true',
    accessTokenType: 'OIDC_TOKEN_TYPE_JWT',
    accessTokenRoleAssertion: true, idTokenRoleAssertion: true,
    /* WHO the person is, in the ID token.
     *
     * Without this ZITADEL asserts the subject and the roles and nothing else,
     * in either token, and the interface can say what somebody may do while
     * being unable to say their name - so the navigation showed a person their
     * own user id, which is a nineteen digit number.
     *
     * It belongs on the ID TOKEN and not on the access token, which is the
     * split OpenID Connect draws: the access token is for the Coordinator and
     * says what the bearer may do, the ID token is for the client and says who
     * they are. Putting a name in the access token would send it to the API on
     * every request for the benefit of a card in a sidebar. */
    idTokenUserinfoAssertion: true,
  };
  const apps = await api('POST', `/management/v1/projects/${PLATFORM}/apps/_search`, {});
  const existing = (apps.result || []).find(a => a.name === 'software-gateway-web');
  let clientId = '';

  if (!existing) {
    const r = await api('POST', `/management/v1/projects/${PLATFORM}/apps/oidc`,
      { name: 'software-gateway-web', ...oidc });
    clientId = r.clientId || '';
    say(`web client created, client_id: ${clientId || '(see console)'}`);
  } else {
    clientId = existing.oidcConfig?.clientId || '';
    say('web client exists');
    /* The redirect URI is where ZITADEL is willing to send a browser back to,
     * and it is derived from WEB_PORT. Change that port on a stack that has
     * already been seeded and every login ends on ZITADEL's own error page
     * saying the redirect_uri is invalid - true, unhelpful, and impossible to
     * connect back to a .env edit made a week ago. So it is RECONCILED rather
     * than only created. */
    /* RECONCILED unconditionally, like every other thing this file derives
     * from configuration. It used to be written only when the redirect URI had
     * drifted, so any OTHER setting on the app - the claims it asserts, the
     * grant types - stayed at whatever the first run created and a change here
     * reached no stack that had ever been seeded. */
    const r = await api('PUT',
      `/management/v1/projects/${PLATFORM}/apps/${existing.id}/oidc_config`, oidc);
    if (r.__status >= 400 && !unchanged(r)) {
      say(`  ! could not update the web client: ${JSON.stringify(r).slice(0, 120)}`);
    } else if (!(existing.oidcConfig?.redirectUris || []).includes(redirectUri)) {
      say(`  redirect URI updated to ${redirectUri}`);
    }
  }

  const issuer = process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090';
  if (clientId) {
    await writeShared('/oidc/web.json',
      JSON.stringify({ issuer, clientId, redirectUri }, null, 2) + '\n');
    say(`  published ${issuer} + client id for the web tier`);
  } else {
    say('  ! no client id to publish - the SPA will not be able to sign anyone in');
  }
}

/* --- 6b. the login service's own account ----------------------------------
 *
 * ZITADEL's sign-in screens are a separate service (see the zitadel-login
 * service in docker-compose.yml) and it drives the login flow through the
 * API as a service user holding IAM_LOGIN_CLIENT.
 *
 * ZITADEL can create that account itself at FIRST BOOT
 * (ZITADEL_FIRSTINSTANCE_ORG_LOGINCLIENT_*), and this deliberately does not
 * use it. First-instance settings are ignored on an instance that already
 * exists, so that mechanism fixes a fresh stack and leaves every stack that
 * has ever been started before unable to show a login page - which is every
 * stack that would be upgrading INTO this fix. Doing it here works on both,
 * and is the same idempotent path everything else in this file takes.
 */
{
  const path = '/pat/login-client.pat';
  const have = await fs.readFile(path, 'utf8').catch(() => '');
  if (have.trim()) {
    say('login service account exists');
  } else {
    const found = await api('POST', '/management/v1/users/_search',
      { queries: [{ userNameQuery: { userName: 'login-client' } }] });
    let uid = (found.result || [])[0]?.id;
    if (!uid) {
      const r = await api('POST', '/management/v1/users/machine', {
        userName: 'login-client', name: 'login-client',
        description: 'ZITADEL sign-in screens (Login V2)',
        accessTokenType: 'ACCESS_TOKEN_TYPE_BEARER',
      });
      uid = r.userId;
      if (!uid) { console.error('FATAL: could not create login-client:', JSON.stringify(r)); process.exit(1); }
      say('login service account created');
    }
    /* An INSTANCE-level membership, not a project role: the login screens
     * serve every organization in the instance, not just this tenant. It is
     * also the narrowest role ZITADEL has for the job - it can drive a login
     * and nothing else, which is why this is not the seeder's own PAT. */
    await api('POST', '/admin/v1/members', { userId: uid, roles: ['IAM_LOGIN_CLIENT'] });
    const pat = await api('POST', `/management/v1/users/${uid}/pats`,
      { expirationDate: '2100-01-01T00:00:00Z' });
    if (!pat.token) { console.error('FATAL: could not issue the login client PAT:', JSON.stringify(pat)); process.exit(1); }
    await writeShared(path, pat.token, 0o600);
    say('  token issued for the sign-in service');
  }
}

/* --- 6c. the data plane's own account -------------------------------------
 *
 * WHY WORKERS NEED ONE AT ALL, and why it is not a token somebody types in.
 *
 * A worker leases jobs from the Coordinator over the same authenticated API a
 * person uses. With authentication on and no credential of its own it got
 * `UNAUTHENTICATED: no bearer token` five seconds apart forever, which is a
 * fleet that looks healthy - the process is up, its probes are green, the
 * queue simply never moves.
 *
 * The obvious fixes are both wrong. A shared static token in .env is a
 * password that never expires, is copied into every deployment manifest, and
 * cannot be revoked without a redeploy. Exempting the worker plane from
 * authentication is worse: those routes hand out work and accept its results,
 * so an unauthenticated one lets anything that can reach the Coordinator claim
 * every job in the queue and report it finished.
 *
 * So the data plane gets what the CI accounts already get: a MACHINE USER in
 * the identity provider, authenticating with client_credentials and holding
 * exactly one role. The token it receives is a short-lived JWT the Coordinator
 * verifies with the same keys and the same code path as a person's - no second
 * trust root, no separate verification to keep in step.
 *
 * ONE identity for the whole fleet, deliberately. A worker is not a security
 * principal: it holds no data, decides nothing, and is interchangeable with
 * every other worker by design. Its id names it in the queue, which is
 * scheduling rather than authorization. Per-worker credentials would buy no
 * containment - they would all carry the same grant - and would cost the one
 * property the fleet exists for, which is that `replicas: 20` needs no
 * conversation with anybody.
 *
 * The credentials are written to the volume the other service accounts already
 * use, and mounted read-only into the workers. Nothing is configured by hand
 * and nothing is committed: a worker that starts finds them or says why not.
 */
{
  const WORKER_ROLE = 'org-worker';
  /* Its OWN volume, not the one carrying the SPA's client id. That one is
   * mounted into the web tier, and its stated invariant is that it holds
   * public values only - a public client has no secret. This file does have
   * one, so it goes where only the seeder and the workers can see it. */
  const path = '/workercreds/worker.json';

  /* Ensured separately from GATEWAY_ORG_ROLES, which an operator may replace
   * wholesale. This role is not a choice about how the estate is organised -
   * without it the data plane cannot authenticate at all. */
  await ensureRoles(PLATFORM, [WORKER_ROLE]);

  const found = await api('POST', '/management/v1/users/_search',
    { queries: [{ userNameQuery: { userName: 'swgw-worker' } }] });
  let uid = (found.result || [])[0]?.id;
  let created = false;
  if (!uid) {
    const r = await api('POST', '/management/v1/users/machine', {
      userName: 'swgw-worker', name: 'Software Gateway worker',
      description: 'the data plane: leases jobs, reports results',
      /* A JWT rather than an opaque token, because the Coordinator verifies it
       * OFFLINE against the issuer's public keys. An opaque one would have to
       * be introspected, which puts a network call to the identity provider in
       * front of every lease and makes a blip there stop the fleet. */
      accessTokenType: 'ACCESS_TOKEN_TYPE_JWT',
    });
    uid = r.userId;
    if (!uid) {
      console.error('FATAL: could not create the worker account:', JSON.stringify(r));
      process.exit(1);
    }
    created = true;
    say('worker service account created');
  } else {
    say('worker service account exists');
  }

  /* The ONE role it holds. org-worker maps to leasing work and reporting on
   * it, and to nothing else at all - see internal/api/middleware/oidc.go. A
   * stolen worker credential can therefore take jobs and lie about their
   * results, which is bad, and cannot read the audit trail, request a
   * download, or see a product it was not handed work for. */
  await grantRoles(uid, PLATFORM, [WORKER_ROLE]);

  /* The secret is REISSUED whenever the file the workers read is missing.
   *
   * ZITADEL stores a hash and will not show a secret twice, so a lost file
   * cannot be recovered - only replaced. That is safe precisely because this
   * credential belongs to the fleet rather than to a person: nothing else
   * holds it, and a worker that is restarted with the new one loses no work
   * it had not already reported. It is also the recovery procedure, which is
   * `docker compose run --rm zitadel-init` and nothing more.
   */
  const have = await fs.readFile(path, 'utf8').then(t => JSON.parse(t)).catch(() => null);
  if (have?.clientSecret && !created && !process.env.WORKER_ROTATE_SECRET) {
    say('  credentials already published for the data plane');
  } else {
    const sec = await api('PUT', `/management/v1/users/${uid}/secret`, {});
    if (!sec.clientId || !sec.clientSecret) {
      console.error('FATAL: could not issue worker credentials:', JSON.stringify(sec));
      process.exit(1);
    }
    /* The project id travels WITH the credentials because the worker cannot
     * derive it. ZITADEL keys its role claim by project id and asserts roles
     * only for a project that is in the token's audience, which is requested
     * as a scope - so a token fetched without it verifies perfectly and
     * arrives carrying no roles, which reads as a permission problem rather
     * than as a missing scope. */
    await writeShared(path, JSON.stringify({
      issuer: process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090',
      tokenUrl: (process.env.ZITADEL_INTERNAL_URL || 'http://zitadel:8080') + '/oauth/v2/token',
      clientId: sec.clientId,
      clientSecret: sec.clientSecret,
      projectId: PLATFORM,
    }, null, 2) + '\n', 0o600);
    /* Mode 0600 belongs to the user that will READ it, and this container is
     * root while the worker image is distroless nonroot. Without this the file
     * is perfectly written, perfectly mounted, and unreadable by the only
     * process that wants it - which surfaces as a permission error naming a
     * path that plainly exists. */
    const owner = Number(process.env.WORKER_UID ?? 65532);
    try {
      await fs.chown(path, owner, owner);
    } catch (e) {
      say(`  ! could not give ${path} to uid ${owner}: ${e.message}`);
      say('    the worker runs as a non-root user and will not be able to read it');
    }
    say(`  credentials published for the data plane (client_id ${sec.clientId})`);
    // No restart. A worker reads this file when it needs a token rather than
    // once at boot, so the fleet moves over on its own within one lease
    // interval - and a worker that started before this ran recovers by itself.
    if (!created) say('  running workers take the new secret within a few seconds');
  }
}

/* --- 7. Sign-in: the connector, and what the login screen offers ----------
 *
 * THREE things, and only the first is obvious.
 *
 * Creating a connector does not put it on the sign-in screen: an identity
 * provider is OFFERED because it is attached to the organization's LOGIN
 * POLICY, and an organization that has never been given one of its own
 * inherits the instance's, which cannot name an org-owned connector. That is
 * why the connector could exist, read as correct in the console, and the
 * screen still show a username box and nothing else.
 *
 * And the login policy is also what decides what ELSE the screen offers. A
 * gateway whose people arrive through Microsoft has no use for a registration
 * link or a password box, and both are on by default.
 */
{
  const ssoName = process.env.SSO_DISPLAY_NAME || 'Microsoft';
  const ssoOn = Boolean(process.env.SSO_ISSUER && process.env.SSO_CLIENT_ID);

  /* The organization's own login policy, COPIED from whatever it is
   * inheriting rather than invented here, then amended in the two places this
   * product has an opinion about. Writing a policy from a fresh set of
   * opinions would quietly change this deployment's MFA and password rules as
   * a side effect of configuring SSO. */
  const current = await api('GET', '/management/v1/policies/login');
  const p = current.policy || {};
  /* Password sign-in is off once SSO is configured, unless asked for.
   *
   * This locks out `zitadel-admin` too - it is a member of this same
   * organization and has no Microsoft account - so it is worth knowing that it
   * is RECOVERABLE: this seeder authenticates with a machine token rather than
   * through the login screen, so re-running it with
   * SSO_ALLOW_PASSWORD_LOGIN=true always puts the password box back. */
  const allowPassword = ssoOn
    ? String(process.env.SSO_ALLOW_PASSWORD_LOGIN ?? 'false') === 'true'
    : true;
  const policy = {
    allowUsernamePassword: allowPassword,
    // Never. Everyone who may use this gateway is provisioned, by the seeder
    // or by users.json; a self-service registration link on the sign-in screen
    // of a software distribution system offers something nobody should take.
    allowRegister: false,
    allowExternalIdp: true,
    forceMfa: p.forceMfa ?? false,
    forceMfaLocalOnly: p.forceMfaLocalOnly ?? false,
    passwordlessType: p.passwordlessType || 'PASSWORDLESS_TYPE_ALLOWED',
    hidePasswordReset: ssoOn ? true : (p.hidePasswordReset ?? false),
    ignoreUnknownUsernames: p.ignoreUnknownUsernames ?? false,
    allowDomainDiscovery: p.allowDomainDiscovery ?? true,
    disableLoginWithEmail: p.disableLoginWithEmail ?? false,
    disableLoginWithPhone: p.disableLoginWithPhone ?? false,
  };
  const wrote = current.isDefault
    ? await api('POST', '/management/v1/policies/login', policy)
    : await api('PUT', '/management/v1/policies/login', policy);
  if (wrote.__status >= 400 && !unchanged(wrote)) {
    console.error('FATAL: could not set the sign-in policy:', JSON.stringify(wrote));
    process.exit(1);
  }
  say('sign-in policy set');
  say(`  self-registration: off      password sign-in: ${allowPassword ? 'on' : 'off'}`);
  if (ssoOn && !allowPassword) {
    say(`    'zitadel-admin' can no longer sign in with a password either. To put`);
    say('    it back: SSO_ALLOW_PASSWORD_LOGIN=true and re-run this container,');
    say('    which authenticates with a machine token rather than that screen.');
  }

  if (!ssoOn) {
    say('SSO not configured - username/password sign-in is active');
  } else {
    const secret = process.env.SSO_CLIENT_SECRET || '';
    const issuer = process.env.SSO_ISSUER;

    /* Checked HERE, where the answer is one line, rather than at the identity
     * provider, where it is an opaque code after somebody has already typed
     * their password. Both of these produce the same Microsoft failure:
     *
     *   AADSTS7000215: Invalid client secret provided. Ensure the secret being
     *   sent in the request is the client secret VALUE, not the client secret ID
     *
     * A secret ID is a GUID; a secret value never is. Azure's portal shows the
     * two side by side, the ID is the one that stays on screen, and the value
     * is shown once and then hidden forever - so copying the wrong column is
     * the normal mistake rather than a careless one. */
    if (!secret) {
      console.error('FATAL: SSO_CLIENT_ID is set but SSO_CLIENT_SECRET is empty.');
      console.error('       This connector authenticates to the identity provider with a');
      console.error('       secret; without one every sign-in fails at the token exchange.');
      process.exit(1);
    }
    if (/^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(secret)) {
      console.error('FATAL: SSO_CLIENT_SECRET is a GUID, so it is the secret ID rather than');
      console.error('       the secret VALUE. In the Azure portal, App registrations ->');
      console.error('       Certificates and secrets, the Value column is the one to copy,');
      console.error('       and it is only shown when the secret is created. If it has been');
      console.error('       lost, add a new client secret and copy the Value.');
      process.exit(1);
    }

    /* Microsoft gets ZITADEL's OWN Microsoft connector rather than a generic
     * OIDC one, and the difference is visible: the generic connector renders
     * as a plain button with the provider's name, the Microsoft one renders
     * with Microsoft's mark, which is what a person is looking for on a
     * sign-in screen. It also knows Entra's endpoints, so it needs the tenant
     * rather than a discovery URL.
     *
     * Anything else stays generic OIDC, which is the whole point of
     * configuring an issuer. */
    const tenantSeg = /login\.microsoftonline\.com\/([^/]+)/.exec(issuer)?.[1];
    const azure = Boolean(tenantSeg);
    const options = {
      isLinkingAllowed: true, isCreationAllowed: true,
      isAutoCreation: true, isAutoUpdate: true,
      /* THE ONE THAT MATTERS for a stack whose people are provisioned before
       * they ever sign in. Without it, signing in through Microsoft creates a
       * SECOND, brand new user with no roles, and asks them to invent a
       * username - while the account seeded for them, holding org-admin, sits
       * beside it untouched. Linking on the e-mail address means the person
       * who was granted a role is the person who arrives. */
      autoLinking: 'AUTO_LINKING_OPTION_EMAIL',
    };
    const shared = {
      name: ssoName,
      clientId: process.env.SSO_CLIENT_ID,
      clientSecret: secret,
      scopes: ['openid', 'profile', 'email'],
      providerOptions: options,
    };
    const body = azure
      ? { ...shared, emailVerified: true, tenant:
          ['common', 'organizations', 'consumers'].includes(tenantSeg)
            ? { tenantType: `AZURE_AD_TENANT_TYPE_${tenantSeg === 'consumers' ? 'CONSUMERS' : tenantSeg === 'common' ? 'COMMON' : 'ORGANISATIONS'}` }
            : { tenantId: tenantSeg } }
      : { ...shared, issuer, usePkce: false };
    const kind = azure ? 'azure' : 'generic_oidc';

    /* Found through the LOGIN POLICY, not through /management/v1/idps/_search.
     * That search only lists connectors of the legacy type, so a connector
     * created through any of the current endpoints is invisible to it and the
     * seeder would make a second one on every run. The policy's list carries
     * every attached connector whatever its type, which is also the only list
     * that matters: attached is what "offered on the sign-in screen" means. */
    const attached = await api('POST', '/management/v1/policies/login/idps/_search', {});
    const legacy = await api('POST', '/management/v1/idps/_search', {});
    let idpId = (attached.result || []).find(i => i.idpName === ssoName)?.idpId
      || (legacy.result || []).find(i => i.name === ssoName)?.id;

    let before = {};
    if (idpId) {
      before = (await api('GET', `/v2/idps/${idpId}`)).idp || {};
      /* A connector created as one type cannot become another, so a stack
       * seeded before this change keeps its generic connector unless it is
       * replaced. Replacing it is safe and is what gets the Microsoft mark
       * onto the button: the connector holds no user data, only configuration
       * that .env is the source of truth for. */
      const wantType = azure ? 'IDP_TYPE_AZURE_AD' : 'IDP_TYPE_OIDC';
      if (before.type && before.type !== wantType && azure) {
        await api('DELETE', `/management/v1/idps/${idpId}`);
        await api('DELETE', `/v2/idps/${idpId}`);
        say(`SSO connector '${ssoName}' replaced with ZITADEL's Microsoft connector`);
        idpId = '';
      }
    }

    if (!idpId) {
      const r = await api('POST', `/management/v1/idps/${kind}`, body);
      idpId = r.id || r.idpId;
      if (!idpId) { console.error('FATAL: could not create the SSO connector:', JSON.stringify(r)); process.exit(1); }
      say(`SSO connector '${ssoName}' created`);
    } else {
      /* RECONCILED, not merely found. The connector used to be created once
       * and never touched again, so a corrected secret in .env reached
       * nothing: the seeder said "exists", the old credentials stayed, and
       * every sign-in kept failing with a Microsoft error about a secret that
       * had already been fixed.
       *
       * ALWAYS a write, never a write-if-different. The client id can be read
       * back and compared; the secret cannot, and skipping the write when the
       * readable fields happen to match would leave a secret changed by hand
       * in the console standing in place of the one in .env. */
      const r = await api('PUT', `/management/v1/idps/${kind}/${idpId}`, body);
      if (r.__status >= 400 && !unchanged(r)) {
        console.error(`FATAL: could not update the '${ssoName}' connector:`, JSON.stringify(r));
        process.exit(1);
      }
      say(`SSO connector '${ssoName}' reconciled from .env`);
      const storedId = before.config?.azureAd?.clientId || before.config?.oidc?.clientId;
      if (storedId && storedId !== body.clientId) {
        say(`  client id CHANGED: ${storedId} -> ${body.clientId}`);
      }
    }

    const stillAttached = await api('POST', '/management/v1/policies/login/idps/_search', {});
    if (!(stillAttached.result || []).some(i => i.idpId === idpId)) {
      const r = await api('POST', '/management/v1/policies/login/idps',
        { idpId, ownerType: 'IDP_OWNER_TYPE_ORG' });
      if (r.__status >= 400) { console.error(`FATAL: could not offer '${ssoName}' on the sign-in screen:`, JSON.stringify(r)); process.exit(1); }
      say(`  '${ssoName}' offered on the sign-in screen`);
    } else say(`  '${ssoName}' already offered on the sign-in screen`);
    say('  people are matched to their existing account by e-mail address');

    /* Whether the SECRET moved, which is the one thing here that cannot be
     * answered by reading it back: ZITADEL returns a connector's client id and
     * never its secret, by design.
     *
     * So the seeder remembers a FINGERPRINT of what it last wrote and compares
     * that. A truncated SHA-256 of a forty character high-entropy secret says
     * "the same" or "not the same" and nothing else - it cannot be turned back
     * into the secret, and it lives in the same volume as the machine tokens,
     * which is already the most privileged thing in this stack. */
    const fingerprintFile = '/pat/sso-fingerprint.json';
    const fingerprint = createHash('sha256')
      .update(`${issuer}\n${body.clientId}\n${secret}`)
      .digest('hex').slice(0, 16);
    const previous = await fs.readFile(fingerprintFile, 'utf8')
      .then(t => { try { return JSON.parse(t); } catch { return {}; } })
      .catch(() => ({}));
    if (!previous.secret) say(`  client secret RECORDED (${secret.length} characters)`);
    else if (previous.secret !== fingerprint) say(`  client secret CHANGED (${secret.length} characters)`);
    else say(`  client secret unchanged (${secret.length} characters)`);
    await writeShared(fingerprintFile,
      JSON.stringify({ secret: fingerprint, clientId: body.clientId, issuer }, null, 2) + '\n', 0o600);

    /* What was actually sent, in terms that can be checked against the portal.
     *
     * A length alone turned out not to be enough: a count that disagrees with
     * the portal says something is wrong and nothing about what, and the
     * obvious next question - "is that even my secret?" - had no answer. So
     * the ends are shown and the middle is not. */
    say(`  client id     : ${body.clientId}`);
    say(`  client secret : ${mask(secret)}`);

    const check = await verifyIdpCredentials(issuer, body.clientId, secret);
    if (check.rejected) {
      console.error(`\nFATAL: ${issuer} rejected these credentials.`);
      console.error(`       ${check.detail}`);
      console.error('       The connector was written, and every sign-in through it will');
      console.error('       fail until SSO_CLIENT_ID and SSO_CLIENT_SECRET are correct.');
      console.error('       In the Azure portal, App registrations -> Certificates and');
      console.error('       secrets, copy the VALUE column, not the Secret ID; the value');
      console.error('       is shown once, when the secret is created.');
      process.exit(1);
    }
    if (check.unreachable) {
      say(`  ! could not verify the credentials: ${check.unreachable}`);
      say('    ZITADEL needs this same network path to sign anybody in, so this is');
      say('    worth fixing even though the seeding itself succeeded.');
    } else {
      say(`  credentials verified: ${check.detail}`);
    }

    const zitadelURL = process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090';
    say(`  register this redirect URI at '${ssoName}': ${zitadelURL}/idps/callback`);
    say('    Microsoft Entra: register it under the WEB platform, not');
    say('    Single-page application. ZITADEL redeems the code server side with');
    say('    a client secret, and Entra requires PKCE for anything registered as');
    say('    an SPA - which is the AADSTS9002325 sign-in failure.');
  }
}

/* --- 7b. what the sign-in screen looks like -------------------------------
 *
 * The screen a person meets before they are anybody is the product's, not
 * ZITADEL's. Without this it carries ZITADEL's mark and ZITADEL's palette, and
 * the first thing somebody sees of this system is a name they have never heard
 * of asking for their password.
 *
 * The colours come from the same brand as everything else. The logo is a file
 * so it can be replaced without touching code; BRANDING_LOGO_FILE points at
 * another one.
 */
{
  const brand = {
    primaryColor: process.env.BRANDING_PRIMARY_COLOR || '#0b7285',
    backgroundColor: '#ffffff', warnColor: '#c92a2a', fontColor: '#111827',
    primaryColorDark: process.env.BRANDING_PRIMARY_COLOR_DARK || '#22b8cf',
    backgroundColorDark: '#111827', warnColorDark: '#ff6b6b', fontColorDark: '#f8f9fa',
    // The suffix is the ZITADEL organization's domain, which means nothing to
    // the person reading it and is not part of what they type.
    hideLoginNameSuffix: true,
    // ZITADEL's own "powered by" mark. This screen is the product's.
    disableWatermark: true,
    themeMode: 'THEME_MODE_AUTO',
  };
  const current = await api('GET', '/management/v1/policies/label');
  const r = current.isDefault
    ? await api('POST', '/management/v1/policies/label', brand)
    : await api('PUT', '/management/v1/policies/label', brand);
  if (r.__status >= 400 && !unchanged(r)) {
    say(`  ! could not set the sign-in branding: ${JSON.stringify(r).slice(0, 140)}`);
  } else {
    for (const [file, path] of [
      [process.env.BRANDING_LOGO_FILE || '/branding/logo.svg', '/assets/v1/org/policy/label/logo'],
      [process.env.BRANDING_LOGO_DARK_FILE || process.env.BRANDING_LOGO_FILE || '/branding/logo-dark.svg',
       '/assets/v1/org/policy/label/logo/dark'],
    ]) {
      const up = await uploadAsset(path, file);
      if (up === 'missing') continue;
      if (up >= 400) say(`  ! could not upload ${file}: ${up}`);
    }
    /* Nothing is visible until the draft is activated, which is the step that
     * is easy to leave out: every write above succeeds, the console shows the
     * new colours, and the sign-in screen keeps the old ones. */
    await api('POST', '/management/v1/policies/label/_activate', {});
    say('sign-in screen branded');
  }

  /* One language, so the picker on the sign-in screen cannot change anything.
   * ZITADEL's login still draws the control - there is no setting that removes
   * it - but with a single allowed language it has nothing to offer. */
  const langs = list(process.env.BRANDING_LANGUAGES, 'en');
  const lr = await api('PUT', '/admin/v1/restrictions', { allowedLanguages: { list: langs } });
  if (lr.__status >= 400 && !unchanged(lr)) say(`  ! could not restrict languages: ${JSON.stringify(lr).slice(0, 120)}`);
  else say(`  languages: ${langs.join(', ')}`);

  /* The sign-in service CACHES all of this.
   *
   * On a first run the ordering already handles it - zitadel-login does not
   * start until this container has finished - so this only matters when the
   * seeder is re-run against a stack that is already up, which is exactly what
   * somebody does after changing any of these settings. Without the restart
   * every write above succeeds, the console shows the new values, and the
   * sign-in screen keeps the old ones, which reads as the change not having
   * worked. */
  say('  changes to the sign-in screen need: docker compose restart zitadel-login');
}

/* --- 8. the first administrator ------------------------------------------- */
{
  const user  = process.env.BOOTSTRAP_ADMIN_USERNAME || 'admin';
  const pw    = process.env.BOOTSTRAP_ADMIN_PASSWORD || '';
  const email = process.env.BOOTSTRAP_ADMIN_EMAIL || 'admin@example.com';
  const found = await findUserId(user, email);
  let id = found.id;

  if (!id) {
    const body = {
      userName: user,
      profile: { firstName: 'Platform', lastName: 'Administrator' },
      email: { email, isEmailVerified: true },
    };
    if (pw) body.password = pw;
    const r = await api('POST', '/management/v1/users/human/_import', body);
    id = r.userId;
    if (!id) { console.error('FATAL: could not create admin:', JSON.stringify(r)); process.exit(1); }
    say(`administrator '${user}' created - ${pw ? 'password login' : 'passwordless (no password ever set)'}`);
    await api('POST', `/management/v1/users/${id}/grants`,
      { projectId: PLATFORM, roleKeys: ['org-admin'] });
    await api('POST', '/management/v1/orgs/me/members', { userId: id, roles: ['ORG_OWNER'] });
    say('  granted org-admin + ORG_OWNER');
  } else {
    say(`administrator '${user}' exists (matched on ${found.by})`);
    /* GRANTED EVERY RUN, not only at creation.
     *
     * The account may not be the one this seeder made. When somebody signs in
     * through Microsoft before the seeder has ever run - or with an address
     * this file did not know about - ZITADEL creates the account itself, and
     * it creates it with no project grant at all. Found here by email, that
     * person is the administrator and simply has not been told so: every
     * screen works, holds no permissions, and says "No roles" under their own
     * name. Granting is idempotent, so doing it unconditionally costs one
     * search on a run that changes nothing. */
    await grantRoles(id, PLATFORM, ['org-admin']);
    await api('POST', '/management/v1/orgs/me/members', { userId: id, roles: ['ORG_OWNER'] });
    if (pw) say('  WARNING: BOOTSTRAP_ADMIN_PASSWORD still set. Unset it once a real admin exists.');
  }

  /* Auto-linking is by EMAIL ADDRESS, so an address that cannot match is a
   * guaranteed second account.
   *
   * The connector is configured with AUTO_LINKING_OPTION_EMAIL (§7): somebody
   * arriving through Microsoft is joined to the account already holding their
   * address. Leave the administrator on the example address and there is
   * nothing to join them to, so ZITADEL does the other thing it is configured
   * to do - creates the user - and the new account holds no roles. What that
   * looks like is two entries in the account switcher, one of which cannot be
   * signed in to, and a profile page reporting no permissions. It is worth
   * one loud paragraph here rather than an afternoon there. */
  if (process.env.SSO_ISSUER && email === 'admin@example.com') {
    say('');
    say('  ! BOOTSTRAP_ADMIN_EMAIL is still admin@example.com while SSO is on.');
    say('    Sign-ins are matched to existing accounts by email address, and');
    say('    nobody at the identity provider has that one - so the first person');
    say('    to sign in gets a brand new account holding no roles, beside this');
    say('    one holding all of them.');
    say('    Fix: set BOOTSTRAP_ADMIN_EMAIL to the address that signs in, or add');
    say('    that person to deploy/zitadel/users.json, then re-run this container.');
    say('');
  }
}

/* --- 9. additional users, from a file that IS the deployment mechanism -----
 *
 * Add a person to deploy/zitadel/users.json, commit it, re-run this container.
 * That is the whole user-provisioning story, and it is reviewable in a pull
 * request rather than being clicks in a console that nobody can audit later.
 */
/* Find a person by either of the two things they are known by.
 *
 * THE EMAIL DECIDES, and that ordering is the whole of this function.
 *
 * A person who signs in through Microsoft is created by ZITADEL, not by this
 * seeder, and ZITADEL names them whatever the connector hands over - usually
 * their address, which is almost never the username an operator typed into
 * .env. So two accounts exist for one human: the one this file made, and the
 * one they actually sign in as.
 *
 * Searching by username first finds the seeder's own account every time, and
 * grants it the roles. The person then signs in through Microsoft, lands on
 * the OTHER account, and reads "Tenant roles: none" on their own profile page
 * while users.json plainly says otherwise - with the roles sitting on an
 * account they cannot sign in to, because it has no Microsoft identity and
 * password sign-in is off. That is not a hypothetical: it is what shipped, and
 * username-first is why the previous attempt at this fixed nothing.
 *
 * The address is the durable identity. It is what the identity provider
 * asserts, it is what ZITADEL's auto-linking keys on, and it is the same
 * string on both systems. A username is a local artifact of whichever side
 * created the account first. So email wins, case-insensitively, because an
 * address is; username is the fallback for accounts that have no address to
 * match on, which is every machine account.
 */
async function findUserId(username, email) {
  let byEmail = '';
  if (email) {
    const r = await api('POST', '/management/v1/users/_search',
      { queries: [{ emailQuery: { emailAddress: email, method: 'TEXT_QUERY_METHOD_EQUALS_IGNORE_CASE' } }] });
    byEmail = (r.result || [])[0]?.id || '';
  }
  let byName = '';
  if (username) {
    const r = await api('POST', '/management/v1/users/_search',
      { queries: [{ userNameQuery: { userName: username } }] });
    byName = (r.result || [])[0]?.id || '';
  }

  /* BOTH matched, and they are different people as far as ZITADEL is
   * concerned. Recorded rather than resolved: deleting somebody's account is
   * not a thing a seeder should decide to do on a re-run. */
  if (byEmail && byName && byEmail !== byName) {
    duplicates.push({ username, email, keep: byEmail, leftover: byName });
  }
  if (byEmail) return { id: byEmail, by: 'email' };
  if (byName) return { id: byName, by: 'username' };
  return { id: '', by: '' };
}

async function grantRoles(userId, projectId, roleKeys) {
  if (!roleKeys.length) return;
  const existing = await api('POST', '/management/v1/users/grants/_search',
    { queries: [{ userIdQuery: { userId } }] });
  const current = (existing.result || []).find(g => g.projectId === projectId);
  const want = [...new Set([...(current?.roleKeys || []), ...roleKeys])];
  if (current) {
    if (want.length === (current.roleKeys || []).length) return;   // nothing new
    await api('PUT', `/management/v1/users/${userId}/grants/${current.id}`, { roleKeys: want });
  } else {
    await api('POST', `/management/v1/users/${userId}/grants`, { projectId, roleKeys: want });
  }
}

let doc = null;
{
  const path = process.env.BOOTSTRAP_USERS_FILE || '/bootstrap-users.json';
  try { doc = JSON.parse(await fs.readFile(path, 'utf8')); }
  catch { say('no users file - skipping additional users'); }

  for (const u of (doc?.users || [])) {
    if (!u.username) continue;
    const found = await findUserId(u.username, u.email);
    let uid = found.id;

    if (!uid) {
      const body = {
        userName: u.username,
        profile: { firstName: u.firstName || u.username, lastName: u.lastName || 'User' },
        email: { email: u.email || `${u.username}@example.invalid`, isEmailVerified: true },
      };
      // A password here is for local and test use. With SSO configured the
      // person signs in through Microsoft and never needs one.
      if (u.password && !process.env.SSO_ISSUER) body.password = u.password;
      const r = await api('POST', '/management/v1/users/human/_import', body);
      uid = r.userId;
      if (!uid) { say(`  ! could not create ${u.username}: ${JSON.stringify(r).slice(0, 160)}`); continue; }
      say(`user ${u.username} created`);
    } else {
      // Which identifier matched, because it is the difference between "the
      // account this file made" and "the account Microsoft made for them".
      say(`user ${u.username} exists (matched on ${found.by})`);
    }

    if (u.orgRoles?.length) {
      await grantRoles(uid, PLATFORM, u.orgRoles);
      say(`  org roles: ${u.orgRoles.join(', ')}`);
    }
    for (const [product, roles] of Object.entries(u.products || {})) {
      const pid = projectOf.get(product);
      if (!pid) { say(`  ! ${u.username}: product '${product}' is not in GATEWAY_PRODUCTS`); continue; }
      await grantRoles(uid, pid, roles.map(r => `${product}:${r}`));
      say(`  ${product}: ${roles.join(', ')}`);
    }
  }
}

/* --- 10. API users -------------------------------------------------------
 *
 * Machine accounts for CI and scripts. They authenticate with
 * client_credentials - no browser, no password - and carry exactly the same
 * roles as a person, so an automated caller is gated by the same policy.
 * Their secrets are printed ONCE, here, because ZITADEL does not store them
 * retrievably; capture them from these logs or re-run to rotate.
 */
for (const a of (doc?.apiUsers || [])) {
  if (!a.username) continue;
  // Machine accounts have no email, so this is the username lookup it always
  // was. It goes through the same function so there is one way to find a user.
  const found = await findUserId(a.username, '');
  let uid = found.id;
  let created = false;
  if (!uid) {
    const r = await api('POST', '/management/v1/users/machine', {
      userName: a.username, name: a.name || a.username,
      description: a.description || 'API user',
      accessTokenType: 'ACCESS_TOKEN_TYPE_JWT',
    });
    uid = r.userId;
    if (!uid) { say(`  ! could not create API user ${a.username}`); continue; }
    created = true;
  }
  if (created || a.rotateSecret) {
    const sec = await api('PUT', `/management/v1/users/${uid}/secret`, {});
    say(`API user ${a.username}`);
    say(`  client_id     : ${sec.clientId}`);
    say(`  client_secret : ${sec.clientSecret}   <-- shown once`);
  } else {
    say(`API user ${a.username} exists (set "rotateSecret": true to reissue)`);
  }
  if (a.orgRoles?.length) await grantRoles(uid, PLATFORM, a.orgRoles);
  for (const [product, roles] of Object.entries(a.products || {})) {
    const pid = projectOf.get(product);
    if (pid) await grantRoles(uid, pid, roles.map(r => `${product}:${r}`));
  }
}

console.log('\nbootstrap complete.');
console.log(`  tenant   : ${TENANT} (${ORG_ID})`);
console.log(`  products : ${products.join(', ') || 'none'}`);
console.log(`  console  : ${process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090'}`);

/* --- 11. say plainly what is not production-ready -------------------------
 *
 * The stack runs with no .env so a first run always works. The price is that
 * defaults are insecure, and an insecure default nobody mentions is how a
 * demo ends up on a network. This names each one, once, where it cannot be
 * missed.
 */
/* --- 10a. people who can sign in and do nothing ---------------------------
 *
 * THE SYMPTOM THIS FILE EXISTS TO PREVENT, checked directly rather than
 * inferred. A human account with no project grant can sign in perfectly well
 * and holds no permission at all: every screen loads, the profile page says
 * "Tenant roles: none", and nothing anywhere says why.
 *
 * Worth checking on its own because the duplicate detection above cannot see
 * this case. When the seeded administrator carries the DEFAULT address, the
 * lookup by address and the lookup by username both land on that same seeded
 * account - one account, no duplicate to report - while the person's real
 * account, created by their first sign-in, sits beside it ungranted and
 * unmentioned.
 *
 * Machine accounts are excluded: theirs is a different question, and the ones
 * this file creates are granted a few lines above.
 */
{
  const all = await api('POST', '/management/v1/users/_search', { query: { limit: 500 } });
  const humans = (all.result || []).filter(u => u.human);
  const granted = new Set();
  const gr = await api('POST', '/management/v1/users/grants/_search', { query: { limit: 1000 } });
  for (const g of (gr.result || [])) granted.add(g.userId);

  /* Somebody who administers the DIRECTORY is not a person who can sign in
   * and do nothing - they can do rather a lot. ZITADEL's own break-glass
   * account holds an instance membership and no project grant by design, so
   * without this the banner opens by reporting the one account that is
   * supposed to look like that, every single run. A warning that is wrong on
   * its first line is a warning people learn to skip. */
  for (const path of ['/management/v1/orgs/me/members/_search', '/admin/v1/members/_search']) {
    const m = await api('POST', path, { query: { limit: 500 } });
    for (const x of (m.result || [])) granted.add(x.userId);
  }

  const ungranted = humans.filter(u => !granted.has(u.id));
  if (ungranted.length) {
    console.log('\n  ' + '-'.repeat(66));
    console.log('  THESE PEOPLE CAN SIGN IN AND HOLD NO ROLES:');
    for (const u of ungranted.slice(0, 20)) {
      console.log(`    ${u.userName}   ${u.human?.email?.email || '(no address)'}`);
    }
    if (ungranted.length > 20) console.log(`    ... and ${ungranted.length - 20} more`);
    console.log('');
    console.log('  Their screens will load and every permission will be denied. Usually');
    console.log('  this is an account the identity provider created at somebody\'s first');
    console.log('  sign-in, which this file has never been told about.');
    console.log('');
    console.log('  Fix: add each of them to deploy/zitadel/users.json WITH THE ADDRESS');
    console.log('  THEY SIGN IN WITH - that address is what matches them to the account');
    console.log('  that already exists - then re-run this container. For the');
    console.log('  administrator, BOOTSTRAP_ADMIN_EMAIL is the same thing.');
    console.log('  ' + '-'.repeat(66));
  }
}

/* --- 10b. two accounts for one person -------------------------------------
 *
 * Said here, at the end, in full, because it explains a symptom that otherwise
 * reads as the product being broken: signing in and finding no roles at all,
 * on a stack whose configuration plainly grants them.
 *
 * ZITADEL keeps its directory in the database, and `docker compose down` keeps
 * volumes - only `down -v` discards them. So an account created by an earlier
 * sign-in survives every rebuild, and a duplicate made once is there until
 * somebody removes it.
 */
if (duplicates.length) {
  console.log('\n  ' + '-'.repeat(66));
  console.log('  TWO ACCOUNTS FOR THE SAME PERSON:');
  for (const d of duplicates) {
    console.log(`    ${d.email}`);
    console.log(`      roles granted to the account holding that address  (${d.keep})`);
    console.log(`      a second account named '${d.username}' also exists (${d.leftover})`);
  }
  console.log('');
  console.log('  This is what a sign-in through the identity provider leaves behind when');
  console.log('  the account seeded for that person carried a different address: ZITADEL');
  console.log('  matches on the address, finds nothing, and creates one. The roles are on');
  console.log('  the account that signs in, so nobody is locked out.');
  console.log('');
  console.log('  The leftover cannot sign in - it has no identity at the provider, and');
  console.log('  password sign-in is off - so it is safe to delete, and this file will not');
  console.log('  do it for you. In the console: Users, open it, Delete. Or re-create the');
  console.log('  stack from empty with `docker compose down -v`, which discards the');
  console.log('  database and every account in it.');
  console.log('  ' + '-'.repeat(66));
}

{
  const todo = [];
  if (process.env.STACK_POSTGRES_PASSWORD_SET !== 'yes')
    todo.push('POSTGRES_PASSWORD is the built-in default');
  if (process.env.STACK_MASTERKEY_SET !== 'yes')
    todo.push('ZITADEL_MASTERKEY is the built-in default (it encrypts every stored secret)');
  if (!process.env.SSO_ISSUER)
    todo.push('no SSO: sign-in is username and password');
  if (process.env.BOOTSTRAP_ADMIN_PASSWORD)
    todo.push(`the '${process.env.BOOTSTRAP_ADMIN_USERNAME || 'admin'}' account has a password from .env; disable it once a real admin exists`);

  if (todo.length) {
    console.log('\n  ' + '-'.repeat(66));
    console.log('  NOT READY FOR ANYTHING OTHER PEOPLE CAN REACH:');
    for (const t of todo) console.log(`    - ${t}`);
    console.log('  Fix: cp .env.example .env, fill it in, then');
    console.log('       docker compose up -d && docker compose run --rm zitadel-init');
    console.log('  ' + '-'.repeat(66));
  }
}
