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
const say = (...a) => console.log('  ' + a.join(' '));
const list = (v, d) => (v ?? d).split(',').map(s => s.trim()).filter(Boolean);

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

let PAT = '', ORG_ID = '';

/* node:http rather than fetch, and this is not a style choice: the Fetch spec
 * lists Host as a forbidden header, so undici DROPS it silently. ZITADEL then
 * sees Host: zitadel:8080, does not recognise it as its external domain, and
 * answers 404 to every call while being perfectly healthy. node:http is the
 * only built-in that lets us set it. */
function request(method, path, body) {
  const u = new URL(Z + path);
  const payload = body === undefined ? null : JSON.stringify(body);
  const headers = {
    'Host': ZHOST,
    'Content-Type': 'application/json',
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
    if (!(existing.oidcConfig?.redirectUris || []).includes(redirectUri)) {
      const r = await api('PUT',
        `/management/v1/projects/${PLATFORM}/apps/${existing.id}/oidc_config`, oidc);
      if (r.__status >= 400) say(`  ! could not update redirect URI to ${redirectUri}`);
      else say(`  redirect URI updated to ${redirectUri}`);
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

/* --- 7. Microsoft SSO, only when configured -------------------------------
 *
 * TWO steps, and the second one is the one that is easy to miss: creating the
 * connector does not put it on the sign-in screen. An identity provider is
 * OFFERED to a person because it is attached to the organization's LOGIN
 * POLICY, and an organization that has never been given one of its own
 * inherits the instance's, which cannot name an org-owned connector. So the
 * connector existed, the configuration read as correct in the console, and the
 * sign-in screen showed a username box and nothing else - which is exactly the
 * "SSO does not appear" this stack shipped with.
 */
if (process.env.SSO_ISSUER && process.env.SSO_CLIENT_ID) {
  const name = process.env.SSO_DISPLAY_NAME || 'Microsoft';
  const idps = await api('POST', '/management/v1/idps/_search', {});
  let idpId = (idps.result || []).find(i => i.name === name)?.id;
  if (!idpId) {
    const r = await api('POST', '/management/v1/idps/oidc', {
      name, issuer: process.env.SSO_ISSUER,
      clientId: process.env.SSO_CLIENT_ID,
      clientSecret: process.env.SSO_CLIENT_SECRET || '',
      scopes: ['openid', 'profile', 'email'],
      isCreationAllowed: true, isLinkingAllowed: true,
      isAutoCreation: true, isAutoUpdate: true,
    });
    // The create response names it `idpId`; a search result names it `id`.
    idpId = r.idpId;
    if (!idpId) { console.error('FATAL: could not create the SSO connector:', JSON.stringify(r)); process.exit(1); }
    say(`SSO connector '${name}' created`);
  } else say(`SSO connector '${name}' exists`);

  /* The organization's own login policy, COPIED from whatever it is
   * inheriting rather than invented here. This step exists only so the
   * connector below can be attached to something; deciding on this
   * deployment's registration, MFA or password rules is not its business,
   * and a policy written from a fresh set of opinions would quietly change
   * all three. */
  const policy = await api('GET', '/management/v1/policies/login');
  if (policy.isDefault) {
    const p = policy.policy || {};
    const r = await api('POST', '/management/v1/policies/login', {
      allowUsernamePassword: p.allowUsernamePassword ?? true,
      allowRegister: p.allowRegister ?? false,
      allowExternalIdp: true,
      forceMfa: p.forceMfa ?? false,
      forceMfaLocalOnly: p.forceMfaLocalOnly ?? false,
      passwordlessType: p.passwordlessType || 'PASSWORDLESS_TYPE_ALLOWED',
      hidePasswordReset: p.hidePasswordReset ?? false,
      ignoreUnknownUsernames: p.ignoreUnknownUsernames ?? false,
      allowDomainDiscovery: p.allowDomainDiscovery ?? true,
      disableLoginWithEmail: p.disableLoginWithEmail ?? false,
      disableLoginWithPhone: p.disableLoginWithPhone ?? false,
    });
    if (r.__status >= 400) { console.error('FATAL: could not give the tenant its own login policy:', JSON.stringify(r)); process.exit(1); }
    say('  tenant login policy created (copied from the instance default)');
  }

  const attached = await api('POST', '/management/v1/policies/login/idps/_search', {});
  if (!(attached.result || []).some(i => i.idpId === idpId)) {
    const r = await api('POST', '/management/v1/policies/login/idps',
      { idpId, ownerType: 'IDP_OWNER_TYPE_ORG' });
    if (r.__status >= 400) { console.error(`FATAL: could not offer '${name}' on the sign-in screen:`, JSON.stringify(r)); process.exit(1); }
    say(`  '${name}' offered on the sign-in screen`);
  } else say(`  '${name}' already offered on the sign-in screen`);

  /* The one value the identity provider needs and this container cannot set
   * for it. Printed every run, because the alternative is finding it in
   * ZITADEL's documentation while looking at an error that does not mention
   * it: a redirect URI that is not registered fails LATE, after a person has
   * typed their password, and the message comes back from the identity
   * provider in its own vocabulary. */
  const zitadelURL = process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090';
  say(`  register this redirect URI at '${name}': ${zitadelURL}/idps/callback`);
  say('    Microsoft Entra: register it under the WEB platform, not');
  say('    Single-page application. ZITADEL redeems the code server side with');
  say('    a client secret, and Entra requires PKCE for anything registered as');
  say('    an SPA - which is the AADSTS9002325 sign-in failure.');
} else {
  say('SSO not configured - username/password login is active');
}

/* --- 8. the first administrator ------------------------------------------- */
{
  const user = process.env.BOOTSTRAP_ADMIN_USERNAME || 'admin';
  const pw   = process.env.BOOTSTRAP_ADMIN_PASSWORD || '';
  const found = await api('POST', '/management/v1/users/_search',
    { queries: [{ userNameQuery: { userName: user } }] });
  let id = (found.result || [])[0]?.id;

  if (!id) {
    const body = {
      userName: user,
      profile: { firstName: 'Platform', lastName: 'Administrator' },
      email: { email: process.env.BOOTSTRAP_ADMIN_EMAIL || 'admin@example.com', isEmailVerified: true },
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
    say(`administrator '${user}' exists`);
    if (pw) say('  WARNING: BOOTSTRAP_ADMIN_PASSWORD still set. Unset it once a real admin exists.');
  }
}

/* --- 9. additional users, from a file that IS the deployment mechanism -----
 *
 * Add a person to deploy/zitadel/users.json, commit it, re-run this container.
 * That is the whole user-provisioning story, and it is reviewable in a pull
 * request rather than being clicks in a console that nobody can audit later.
 */
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
    const found = await api('POST', '/management/v1/users/_search',
      { queries: [{ userNameQuery: { userName: u.username } }] });
    let uid = (found.result || [])[0]?.id;

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
      say(`user ${u.username} exists`);
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
  const found = await api('POST', '/management/v1/users/_search',
    { queries: [{ userNameQuery: { userName: a.username } }] });
  let uid = (found.result || [])[0]?.id;
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
