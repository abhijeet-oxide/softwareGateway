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

const fs   = await import('node:fs/promises');
const http = await import('node:http');
const say = (...a) => console.log('  ' + a.join(' '));
const list = (v, d) => (v ?? d).split(',').map(s => s.trim()).filter(Boolean);

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

/* --- 5. one project per product ------------------------------------------- */
const products = list(process.env.GATEWAY_PRODUCTS, '');
for (const p of products) {
  say(`product ${p}`);
  await ensureRoles(await ensureProject(p),
    list(process.env.GATEWAY_PRODUCT_ROLES, 'product-owner,product-operator,product-reader'));
}

/* --- 6. the web application (OIDC client) --------------------------------- */
{
  const WEB = process.env.WEB_PUBLIC_URL || 'http://localhost:8000';
  const apps = await api('POST', `/management/v1/projects/${PLATFORM}/apps/_search`, {});
  if (!(apps.result || []).some(a => a.name === 'software-gateway-web')) {
    const r = await api('POST', `/management/v1/projects/${PLATFORM}/apps/oidc`, {
      name: 'software-gateway-web',
      redirectUris: [`${WEB}/auth/callback`],
      postLogoutRedirectUris: [`${WEB}/`],
      responseTypes: ['OIDC_RESPONSE_TYPE_CODE'],
      grantTypes: ['OIDC_GRANT_TYPE_AUTHORIZATION_CODE', 'OIDC_GRANT_TYPE_REFRESH_TOKEN'],
      appType: 'OIDC_APP_TYPE_USER_AGENT',
      authMethodType: 'OIDC_AUTH_METHOD_TYPE_NONE',   // public client + PKCE
      devMode: String(process.env.OIDC_DEV_MODE ?? 'true') === 'true',
      accessTokenType: 'OIDC_TOKEN_TYPE_JWT',
      accessTokenRoleAssertion: true, idTokenRoleAssertion: true,
    });
    say(`web client created, client_id: ${r.clientId ?? '(see console)'}`);
  } else say('web client exists');
}

/* --- 7. Microsoft SSO, only when configured ------------------------------- */
if (process.env.SSO_ISSUER && process.env.SSO_CLIENT_ID) {
  const name = process.env.SSO_DISPLAY_NAME || 'Microsoft';
  const idps = await api('POST', '/management/v1/idps/_search', {});
  if (!(idps.result || []).some(i => i.name === name)) {
    await api('POST', '/management/v1/idps/oidc', {
      name, issuer: process.env.SSO_ISSUER,
      clientId: process.env.SSO_CLIENT_ID,
      clientSecret: process.env.SSO_CLIENT_SECRET || '',
      scopes: ['openid', 'profile', 'email'],
      isCreationAllowed: true, isLinkingAllowed: true,
      isAutoCreation: true, isAutoUpdate: true,
    });
    say(`SSO connector '${name}' created`);
  } else say('SSO connector exists');
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
    if (process.env.SSO_ISSUER && pw) {
      console.error('FATAL: BOOTSTRAP_ADMIN_PASSWORD is set while SSO is configured.');
      console.error('       The password shortcut is for local use only. Unset it.');
      process.exit(1);
    }
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

console.log('\nbootstrap complete.');
console.log(`  tenant   : ${TENANT} (${ORG_ID})`);
console.log(`  products : ${products.join(', ') || 'none'}`);
console.log(`  console  : ${process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090'}`);
