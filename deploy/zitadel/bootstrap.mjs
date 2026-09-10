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
/* WHAT THIS CONTAINER PRINTS IS A REPORT, not a commentary on itself.
 *
 * It is read once, under time pressure, by somebody deciding whether a
 * deployment is usable - and pasted into a ticket when it is not. So every
 * line is a fact in a fixed place: a section, a label in one column, a value
 * in the next. Nothing addresses the reader, nothing narrates progress in the
 * first person, and nothing hedges.
 *
 *   head()   a section, one per thing this file provisions
 *   item()   a fact: label in a fixed column, value after it
 *   sub()    a fact about the fact above it
 *   note()   a sentence qualifying the section, indented under it
 *   warn()   something that did not work and did not stop the run
 *   panel()  a condition that needs a paragraph, boxed and kept for the end
 */
const COL  = 30;
const out  = (s = '') => console.log(s);
const head = (title) => { console.log(''); console.log(title); };
const item = (label, value = '') => console.log(`    ${String(label).padEnd(COL)}${value}`.trimEnd());
const sub  = (label, value = '') => console.log(`      ${String(label).padEnd(COL - 2)}${value}`.trimEnd());
const note = (text = '') => console.log(text ? `    ${text}` : '');
const warn = (text) => console.log(`    ! ${text}`);
/* A fixed-width table sized to its own contents.
 *
 * padEnd against a guessed width is fine until one value is longer than the
 * guess, and then the row runs into the next column and the table stops being
 * a table - `zitadel-admin@default.localhost` is 31 characters and plenty of
 * real login names are longer than the one in front of you. Sized from the
 * data, two spaces of gutter, and the last column never padded. */
const table = (header, rows) => {
  const widths = header.map((h, i) =>
    Math.max(h.length, ...rows.map(row => String(row[i] ?? '').length)) + 2);
  const line = cells => cells.map((v, i) =>
    i === cells.length - 1 ? String(v ?? '') : String(v ?? '').padEnd(widths[i])).join('');
  return [line(header), ...rows.map(line)];
};

const panel = (title, lines) => {
  console.log('');
  console.log('  ' + '-'.repeat(72));
  console.log('  ' + title);
  console.log('  ' + '-'.repeat(72));
  for (const l of lines) console.log(l ? `  ${l}` : '');
  console.log('  ' + '-'.repeat(72));
};

/* Accounts replaced because they were never initialised, named at the end.
 *
 * Declared HERE for the same reason as `duplicates` below: the function that
 * pushes to it is hoisted and runs long before the line that would declare it
 * further down, and a `const` does not hoist - it throws
 * `Cannot access '...' before initialization` on the first repair, which is
 * the run somebody is doing because nothing else worked. */
const uninitialised = [];

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

/* ZITADEL AGREEING, dressed as a refusal.
 *
 * Writing a value that is already the stored value answers 400 with one of
 * several spellings - "has not been changed", "NotChanged", "No changes
 * (COMMAND-1m88i)" - and every one of them means the seeder asked for exactly
 * what is already true. That is the normal outcome of an idempotent re-run, so
 * it is not reported as a fault; anything else is. */
/* --- reading config/ -------------------------------------------------------
 *
 * A DELIBERATELY SMALL YAML READER, and the reason it is here rather than a
 * dependency: this container is `node:alpine` and nothing else. Adding js-yaml
 * means an npm install at deploy time, which means a registry, which means
 * this file stops working in the air-gapped estates the product ships into -
 * for the sake of parsing two documents whose shape this repository defines.
 *
 * So it reads a SUBSET, and says so. Maps, lists, lists of maps, inline `[]`
 * and `{}`, quoted and plain scalars, comments, blank lines. Not anchors, not
 * multi-line scalars, not flow maps with nested structure, not multiple
 * documents. config/access/roles.yaml and config/users/users.yaml are written
 * inside that subset and `go test ./deploy/...` parses both with a real YAML
 * library on every build, so a document this cannot read fails in CI rather
 * than at three in the morning.
 *
 * PRODUCT FILES ARE NOT PARSED HERE. They are arbitrary user documents with a
 * schema the Go side owns, and all this file needs from one is its name - so
 * it takes the name and nothing else. See productNames.
 */
function parseYaml(text) {
  const lines = [];
  for (const raw of String(text).split(/\r?\n/)) {
    const line = stripComment(raw);
    if (!line.trim()) continue;
    lines.push({ indent: line.length - line.trimStart().length, text: line.trim() });
  }
  const [value] = parseBlock(lines, 0, 0);
  return value;
}

/* A `#` inside quotes is content, not a comment. Anywhere else it ends the
 * line - which is what lets every file in config/ carry its reasoning with it. */
function stripComment(line) {
  let quote = '';
  for (let i = 0; i < line.length; i++) {
    const ch = line[i];
    if (quote) { if (ch === quote) quote = ''; continue; }
    if (ch === '"' || ch === "'") { quote = ch; continue; }
    if (ch === '#' && (i === 0 || /\s/.test(line[i - 1]))) return line.slice(0, i);
  }
  return line;
}

function parseBlock(lines, i, indent) {
  if (i >= lines.length) return [null, i];
  const isList = lines[i].text.startsWith('- ') || lines[i].text === '-';
  return isList ? parseList(lines, i, indent) : parseMap(lines, i, indent);
}

function parseMap(lines, i, indent) {
  const out = {};
  while (i < lines.length && lines[i].indent >= indent) {
    if (lines[i].indent > indent) { i++; continue; }        // stray deeper line
    const { text } = lines[i];
    const at = splitKey(text);
    if (at < 0) break;
    const key = unquote(text.slice(0, at).trim());
    const rest = text.slice(at + 1).trim();
    i++;
    if (rest) { out[key] = scalar(rest); continue; }
    if (i < lines.length && lines[i].indent > indent) {
      const [child, next] = parseBlock(lines, i, lines[i].indent);
      out[key] = child; i = next;
    } else {
      out[key] = null;
    }
  }
  return [out, i];
}

function parseList(lines, i, indent) {
  const out = [];
  while (i < lines.length && lines[i].indent === indent && lines[i].text.startsWith('-')) {
    const first = lines[i].text.replace(/^-\s*/, '');
    i++;
    if (!first) {                                            // `-` then a block
      if (i < lines.length && lines[i].indent > indent) {
        const [child, next] = parseBlock(lines, i, lines[i].indent);
        out.push(child); i = next;
      } else out.push(null);
      continue;
    }
    if (splitKey(first) < 0) { out.push(scalar(first)); continue; }
    /* `- key: value`, and every following line indented past the dash belongs
     * to the same entry. Re-parsed as a map with the dash's own text put back
     * at that indent, so one code path handles both halves of the entry. */
    const inner = [{ indent: indent + 2, text: first }];
    while (i < lines.length && lines[i].indent > indent) { inner.push(lines[i]); i++; }
    const [entry] = parseMap(inner, 0, indent + 2);
    out.push(entry);
  }
  return [out, i];
}

/* The first `:` that ends a key: not one inside quotes, and not one inside a
 * URL, which is the case that makes a naive indexOf wrong on real data. */
function splitKey(text) {
  let quote = '';
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (quote) { if (ch === quote) quote = ''; continue; }
    if (ch === '"' || ch === "'") { quote = ch; continue; }
    if (ch === ':' && (i + 1 === text.length || /\s/.test(text[i + 1]))) return i;
  }
  return -1;
}

function unquote(v) {
  if ((v.startsWith('"') && v.endsWith('"')) || (v.startsWith("'") && v.endsWith("'"))) {
    return v.slice(1, -1);
  }
  return v;
}

function scalar(v) {
  if (v === '[]') return [];
  if (v === '{}') return {};
  if (v.startsWith('[') && v.endsWith(']')) {
    const body = v.slice(1, -1).trim();
    return body ? body.split(',').map(x => scalar(x.trim())) : [];
  }
  if (v === 'true') return true;
  if (v === 'false') return false;
  if (v === 'null' || v === '~') return null;
  if (/^-?\d+$/.test(v)) return Number(v);
  return unquote(v);
}

/* The NAME of every product, and nothing else from the file.
 *
 * A product document belongs to the Go side, which validates it properly and
 * hot-reloads it; this file only has to know which projects to create. So it
 * looks for the name where the schema puts it - `metadata.name` at the top
 * level - and refuses the file rather than guessing if it is not there, because
 * a product silently skipped here is a product nobody can be granted access to.
 */
async function productNames(dir) {
  let entries = [];
  try { entries = await fs.readdir(dir); }
  catch { return { names: [], missing: true }; }
  const names = [];
  const unnamed = [];
  for (const file of entries.filter(f => /\.ya?ml$/i.test(f)).sort()) {
    const text = await fs.readFile(`${dir}/${file}`, 'utf8');
    let inMetadata = false, name = '';
    for (const raw of text.split(/\r?\n/)) {
      const line = stripComment(raw);
      if (!line.trim()) continue;
      const indent = line.length - line.trimStart().length;
      if (indent === 0) { inMetadata = line.trim() === 'metadata:'; continue; }
      if (inMetadata && /^name:\s*\S/.test(line.trim())) {
        name = unquote(line.trim().slice(5).trim());
        break;
      }
    }
    if (name) names.push({ name, file }); else unnamed.push(file);
  }
  return { names, unnamed, missing: false };
}

const sleep = ms => new Promise(r => setTimeout(r, ms));

const unchanged = r => r.__status >= 400 &&
  /not\s*been\s*changed|NotChanged|no\s*changes/i.test(r.message || '');

async function api(method, path, body) {
  const r = await request(method, path, body);
  let json; try { json = JSON.parse(r.text); } catch { json = { raw: r.text }; }
  if (r.status >= 400) json.__status = r.status;
  return json;
}

/* --- 1. wait for ZITADEL and the PAT the first-instance step writes -------- */
out('Software Gateway - identity bootstrap');
out(`  ${new Date().toISOString().replace('T', ' ').slice(0, 19)}   ${process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090'}`);
const startedAt = Date.now();
for (let i = 0; i < 180; i++) {
  try {
    const r = await request('GET', '/.well-known/openid-configuration');
    const pat = await fs.readFile(PAT_FILE, 'utf8').catch(() => '');
    if (r.status === 200 && pat.trim()) { PAT = pat.trim(); break; }
  } catch { /* not up yet */ }
  await new Promise(r => setTimeout(r, 2000));
}
if (!PAT) { console.error('\nFATAL: ZITADEL did not publish a credential at ' + PAT_FILE + ' within 360s'); process.exit(1); }
head('ZITADEL');
item('reachable', `after ${Math.round((Date.now() - startedAt) / 1000)}s`);
item('credential', PAT_FILE);

/* --- 1b. what this deployment is, read from config/ --------------------------
 *
 * ONE DIRECTORY, read before anything is written.
 *
 * The roles used to be three environment variables, the product list a fourth,
 * and the people a JSON file somewhere else again - four formats, four places,
 * four ways to apply a change, and no way for any of them to check the others.
 * A product could exist with nobody able to administer it and nothing would
 * say so.
 *
 * They are one subject, so they are one directory, and the checks between them
 * happen HERE, before the first write. A run that would produce a deployment
 * nobody can administer refuses to start rather than half-applying itself.
 */
const CONFIG_DIR = process.env.GATEWAY_CONFIG_DIR || '/config';
const ROLES_FILE = `${CONFIG_DIR}/access/roles.yaml`;
const USERS_FILE = `${CONFIG_DIR}/users/users.yaml`;
const PRODUCTS_DIR = `${CONFIG_DIR}/products`;

const dataError = (...lines) => {
  console.error('');
  for (const l of lines) console.error(l);
  console.error('');
  process.exit(1);
};

let roles, doc;
try {
  roles = parseYaml(await fs.readFile(ROLES_FILE, 'utf8')) || {};
} catch (e) {
  dataError(`FATAL: ${ROLES_FILE} could not be read: ${e.message}`,
    '  It declares the roles this deployment grants. Without it there is',
    '  nothing to create and nothing for a policy to refer to.');
}
try {
  doc = parseYaml(await fs.readFile(USERS_FILE, 'utf8')) || {};
} catch (e) {
  dataError(`FATAL: ${USERS_FILE} could not be read: ${e.message}`,
    '  It declares who may use this gateway. An empty file is a valid answer;',
    '  a missing one is a mistake, because nobody would be provisioned.');
}

const orgRoles = roles.tenant?.roles || [];
const productRoles = roles.product?.roles || [];
const ownerRole = roles.product?.ownerRole || '';
if (!orgRoles.length || !productRoles.length) {
  dataError(`FATAL: ${ROLES_FILE} declares no roles.`,
    '  Both tenant.roles and product.roles are required: the first covers',
    '  products that do not exist yet, the second is granted per product.');
}

const found = await productNames(PRODUCTS_DIR);
if (found.missing) {
  dataError(`FATAL: ${PRODUCTS_DIR} does not exist.`,
    '  It holds one document per product this gateway replicates, and it is',
    '  what decides which projects exist to grant access to.');
}
if (found.unnamed?.length) {
  dataError(`FATAL: ${found.unnamed.length} document(s) in ${PRODUCTS_DIR} declare no metadata.name:`,
    ...found.unnamed.map(f => `    ${f}`),
    '  A product with no name cannot be granted access to. Every document',
    '  here needs `metadata.name` at the top level - see config/products/README.md.');
}
const products = found.names.map(p => p.name);

/* EVERY PRODUCT NEEDS AN OWNER, and this is where that is enforced.
 *
 * A product nobody holds the owner role on is a product whose downloads
 * nobody can approve and whose configuration nobody is accountable for. It is
 * a deployment mistake, not a runtime one, so it is refused at the point the
 * two files are first read together - naming the product, the role and the
 * file to add it to. `go test ./deploy/...` makes the same check on the pull
 * request, so this one is the backstop rather than the first line of defence. */
if (ownerRole) {
  const owned = new Set();
  for (const u of [...(doc.users || []), ...(doc.apiUsers || [])]) {
    for (const [product, granted] of Object.entries(u.products || {})) {
      if ((granted || []).includes(ownerRole)) owned.add(product);
    }
  }
  const orphans = products.filter(p => !owned.has(p));
  if (orphans.length) {
    dataError('FATAL: these products have no owner.',
      '',
      ...orphans.map(p => `    ${p}`),
      '',
      `  Every product in ${PRODUCTS_DIR} needs at least one person or machine`,
      `  account in ${USERS_FILE} holding the '${ownerRole}' role on it.`,
      '  Nobody can approve a download for a product nobody owns.',
      '',
      '  Add it under that person\'s `products:` mapping:',
      '',
      `      products:`,
      `        ${orphans[0]}: [${ownerRole}]`,
      '',
      `  The role that must be held is config/access/roles.yaml's product.ownerRole.`);
  }
}

head('Configuration');
item('source', CONFIG_DIR);
item('tenant roles', orgRoles.join(', '));
item('product roles', productRoles.join(', '));
item('owner role', ownerRole || 'not required');
item('products', String(products.length));
item('people', `${(doc.users || []).length} human, ${(doc.apiUsers || []).length} machine`);

/* --- 2. the organization is the tenant ------------------------------------ */
const TENANT = process.env.GATEWAY_TENANT || 'default';
{
  /* SEARCH, CREATE, SEARCH AGAIN - and the third step is the whole point.
   *
   * ZITADEL answers a search from a PROJECTION, and the projection is behind
   * the write that filled it. On a stack coming up for the first time the
   * organization ZITADEL just made for itself is not in that projection yet,
   * so the search misses, the create is refused because the name is taken, and
   * `.id` on a failed create is undefined.
   *
   * That used to be the end of it: the run reported `default created
   * (undefined)` and carried on. It appeared to work, because a request with
   * no org header lands in the PAT's own organization, which happens to be
   * this one. It stops appearing to work the moment GATEWAY_TENANT names an
   * organization the seeder's own account is not in - and on a loaded machine
   * the same lag reaches further, into the project that was created one line
   * later, which is how a stack comes up with no OIDC client and nobody able
   * to sign in.
   *
   * So it waits for the projection instead of assuming it. Ten attempts over
   * ten seconds, and a FATAL if the tenant cannot be resolved at all, because
   * everything below this is addressed relative to it. */
  let created = false;
  for (let attempt = 0; attempt < 10 && !ORG_ID; attempt++) {
    if (attempt) await sleep(1000);
    const found = await api('POST', '/admin/v1/orgs/_search', {});
    const hit = (found.result || []).find(o => o.name === TENANT);
    if (hit) { ORG_ID = hit.id; break; }
    const made = await api('POST', '/management/v1/orgs', { name: TENANT });
    if (made.id) { ORG_ID = made.id; created = true; }
  }
  head('Tenant');
  if (!ORG_ID) {
    console.error(`FATAL: the tenant '${TENANT}' could neither be found nor created.`);
    console.error('  ZITADEL answered a search that did not list it and a create that was');
    console.error('  refused. Re-running this container is the first thing to try: this is');
    console.error('  usually a projection that has not caught up on a first start.');
    process.exit(1);
  }
  item(TENANT, `${created ? 'created' : 'exists'} (${ORG_ID})`);
}

/* --- 3. idempotent project + roles ---------------------------------------- */
const created = new Set();
async function ensureProject(name) {
  const found = await api('POST', '/management/v1/projects/_search', { query: { limit: 500 } });
  let id = (found.result || []).find(p => p.name === name)?.id;
  if (!id) {
    id = (await api('POST', '/management/v1/projects',
      { name, projectRoleAssertion: true })).id;
    created.add(name);
  }
  // Assertion must be on or roles never reach a token, and creating with the
  // flag does not always persist it. Set it explicitly every run.
  await api('PUT', `/management/v1/projects/${id}`, { name, projectRoleAssertion: true });
  return id;
}

async function ensureRoles(projectId, roles) {
  const added = [];
  const have = new Set(((await api('POST', `/management/v1/projects/${projectId}/roles/_search`,
    { query: { limit: 500 } })).result || []).map(r => r.key));
  for (const key of roles) {
    if (have.has(key)) continue;
    await api('POST', `/management/v1/projects/${projectId}/roles`,
      { roleKey: key, displayName: key, group: 'gateway' });
    added.push(key);
  }
  return added;
}

/* --- 4. org-wide roles live on `platform` and name no product ------------- */
head('Projects and roles');
const PLATFORM = await ensureProject('platform');
{
  const added = await ensureRoles(PLATFORM, orgRoles);
  item('platform', created.has('platform') ? 'created' : 'exists');
  sub('tenant-wide roles', orgRoles.join(', '));
  if (added.length && !created.has('platform')) sub('roles added this run', added.join(', '));
}

/* --- 5. one project per product -------------------------------------------
 *
 * Role keys are namespaced "<product>:<role>". ZITADEL emits roles under a
 * claim keyed by PROJECT ID - an opaque number - so a bare `product-owner` in
 * a token cannot say WHICH product it refers to. Putting the product in the
 * key makes the token self-describing, which is what lets pkg/authz stay
 * stateless: no project-id map to ship, no ZITADEL credential in any service
 * just to resolve a name. See pkg/authz/identity.go splitProductRole.
 */
const projectOf = new Map();          // product name -> project id
for (const p of products) {
  const pid = await ensureProject(p);
  projectOf.set(p, pid);
  const added = await ensureRoles(pid, productRoles.map(r => `${p}:${r}`));
  item(p, created.has(p) ? 'created' : 'exists');
  sub('roles', productRoles.join(', '));
  if (added.length && !created.has(p)) sub('roles added this run', added.join(', '));
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
    /* Retried for the same reason the tenant above is: the project this app is
     * created ON was written moments ago, and ZITADEL's projection of it can
     * be behind. A create that answers without a client id has failed, however
     * it is dressed, and publishing an empty one leaves a web tier that loads
     * and can sign nobody in. The search is repeated first, because a create
     * that failed on the second attempt may well have succeeded on the first. */
    let r = await api('POST', `/management/v1/projects/${PLATFORM}/apps/oidc`,
      { name: 'software-gateway-web', ...oidc });
    for (let attempt = 0; !r.clientId && attempt < 5; attempt++) {
      await sleep(1000);
      const again = await api('POST', `/management/v1/projects/${PLATFORM}/apps/_search`, {});
      const made = (again.result || []).find(a => a.name === 'software-gateway-web');
      if (made?.oidcConfig?.clientId) { r = { clientId: made.oidcConfig.clientId }; break; }
      r = await api('POST', `/management/v1/projects/${PLATFORM}/apps/oidc`,
        { name: 'software-gateway-web', ...oidc });
    }
    clientId = r.clientId || '';
    head('Applications');
    item('software-gateway-web', clientId ? 'created' : 'NOT created');
    if (!clientId) warn(`ZITADEL refused the application: ${JSON.stringify(r).slice(0, 160)}`);
  } else {
    clientId = existing.oidcConfig?.clientId || '';
    head('Applications');
    item('software-gateway-web', 'exists');
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
      warn(`software-gateway-web was not updated: ${JSON.stringify(r).slice(0, 120)}`);
    } else if (!(existing.oidcConfig?.redirectUris || []).includes(redirectUri)) {
      sub('redirect uri', `${redirectUri} (updated)`);
    }
  }

  const issuer = process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090';
  if (clientId) {
    await writeShared('/oidc/web.json',
      JSON.stringify({ issuer, clientId, redirectUri }, null, 2) + '\n');
    sub('client id', clientId);
    sub('redirect uri', redirectUri);
    sub('issuer', issuer);
    sub('published to', '/oidc/web.json');
  } else {
    warn('no client id was issued; the web tier cannot start a sign-in');
  }
}

/* --- 7. the login service's own account -----------------------------------
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
    item('login-client', 'exists');
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
      item('login-client', 'created');
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
    sub('token', 'issued for the sign-in screens');
  }
}

/* --- 8. the data plane's own account --------------------------------------
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

  /* Ensured separately from the tenant roles, which an operator may replace
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
    item('swgw-worker', 'created');
  } else {
    item('swgw-worker', 'exists');
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
    sub('credentials', 'already published to /workercreds/worker.json');
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
      warn(`${path} could not be given to uid ${owner}: ${e.message}`);
      note('  The worker runs as a non-root user and cannot read it as written.');
    }
    sub('client id', sec.clientId);
    sub('published to', '/workercreds/worker.json');
    // No restart. A worker reads this file when it needs a token rather than
    // once at boot, so the fleet moves over on its own within one lease
    // interval - and a worker that started before this ran recovers by itself.
    if (!created) sub('rotation', 'running workers take the new secret within seconds');
  }
}

/* --- 9. sign-in: the connector, and what the login screen offers ----------
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
  /* A second factor is OFF unless the operator asks for one.
   *
   * ZITADEL ships an organization with passwordlessType ALLOWED, and what that
   * produces is not an option - it is a step. A person arriving at the sign-in
   * screen is shown "Select the method you would like to authenticate" with
   * Passkey and Password side by side, and is expected to ENROL something
   * before they can get in. On a gateway whose people are provisioned by this
   * seeder and arrive through a corporate directory, that is a second
   * credential nobody asked for, on a screen that was supposed to be a
   * formality.
   *
   * So all three are named explicitly and all three are off. Named rather than
   * inherited (`p.forceMfa ?? false`), because inheriting means the behaviour
   * of this deployment depends on what the instance policy happened to say,
   * which is exactly the kind of thing that is fine on a laptop and a surprise
   * in a cluster.
   *
   * LOGIN_REQUIRE_MFA=true puts all of it back for an estate that wants it. */
  const mfaOn = String(process.env.LOGIN_REQUIRE_MFA ?? 'false') === 'true';
  const policy = {
    allowUsernamePassword: allowPassword,
    // Never. Everyone who may use this gateway is provisioned, by the seeder
    // or by users.json; a self-service registration link on the sign-in screen
    // of a software distribution system offers something nobody should take.
    allowRegister: false,
    allowExternalIdp: true,
    forceMfa: mfaOn,
    forceMfaLocalOnly: false,
    passwordlessType: mfaOn
      ? (p.passwordlessType || 'PASSWORDLESS_TYPE_ALLOWED')
      : 'PASSWORDLESS_TYPE_NOT_ALLOWED',
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
  /* Setting the policy is not on its own enough: a second factor that was
   * already added to this organization stays on the list and stays demanded,
   * whatever forceMfa now says. The list is emptied here for the same reason
   * the fields above are named rather than inherited. */
  let cleared = 0;
  if (!mfaOn) {
    const listed = await api('POST', '/management/v1/policies/login/second_factors/_search', {});
    for (const type of listed.result || []) {
      const gone = await api('DELETE', `/management/v1/policies/login/second_factors/${type}`);
      if (gone.__status === undefined || unchanged(gone)) cleared++;
    }
  }

  head('Sign-in policy');
  item('self-registration', 'off');
  item('password sign-in', allowPassword ? 'on' : 'off');
  item('second factor', mfaOn
    ? 'required (LOGIN_REQUIRE_MFA=true)'
    : 'not required; no passkey enrolment step');
  if (cleared) sub('cleared', `${cleared} second factor${cleared === 1 ? '' : 's'} this organization had inherited`);
  if (ssoOn && !allowPassword) {
    note("'zitadel-admin' cannot sign in with a password either. Restore the");
    note('password box with SSO_ALLOW_PASSWORD_LOGIN=true and re-run this');
    note('container, which authenticates with a machine token rather than');
    note('through the sign-in screen.');
  }

  if (!ssoOn) {
    item('identity provider', 'none configured; sign-in is username and password');
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
    /* NOBODY IS CREATED BY SIGNING IN.
     *
     * This is a closed system: who may use this gateway is decided by an
     * administrator, in config/users/users.yaml, reviewable in a pull
     * request. It is not decided by who happens to hold an account at the
     * identity provider - which, federating a corporate directory, is
     * everybody who works here.
     *
     * With creation ALLOWED, which is what this used to say, an unprovisioned
     * person signing in through Microsoft got a brand new ZITADEL account with
     * no roles on it, and - the part that matters - a VALID TOKEN. Every
     * defence after that point is then working to contain a caller who should
     * never have been issued a credential at all. Authorization has to hold
     * that line anyway, and it does; but the line belongs here, at the front
     * door, where the answer is "we have never heard of you" rather than "you
     * may do nothing".
     *
     * What each flag does, because three of them look interchangeable and are
     * not:
     *
     *   isCreationAllowed  a NEW ZITADEL user may be created from this
     *                      connector, with the person filling in a form. Off.
     *   isAutoCreation     the same thing without even the form, done silently
     *                      on first sign-in. Off. This is the one that was
     *                      minting the accounts.
     *   isLinkingAllowed   an external identity may be attached to a user that
     *                      ALREADY EXISTS. On - this is the whole mechanism by
     *                      which a provisioned person signs in.
     *   autoLinking        which field to match them on. The e-mail address:
     *                      it is what the provider asserts and what the seeder
     *                      writes, so the person granted the role is the person
     *                      who arrives.
     *   isAutoUpdate       keep the linked account's name and address in step
     *                      with the directory afterwards. On.
     *
     * So: provisioned first, then sign in. An unknown address reaches the
     * identity provider and stops there, with no user created and no token
     * issued. */
    const options = {
      isLinkingAllowed: true, isCreationAllowed: false,
      isAutoCreation: false, isAutoUpdate: true,
      /* WHICH FIELD the provider's assertion is matched against.
       *
       * The e-mail address by default, because it is the one string both
       * systems hold and the one a directory reliably asserts. Some do not:
       * an Entra tenant whose users have no `mail` attribute asserts the
       * user principal name and nothing else, and then matching on address
       * can never succeed however correct the configuration looks. Set
       * SSO_LINK_ON=username to match the ZITADEL username instead. */
      autoLinking: (process.env.SSO_LINK_ON || 'email').toLowerCase() === 'username'
        ? 'AUTO_LINKING_OPTION_USERNAME'
        : 'AUTO_LINKING_OPTION_EMAIL',
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
    let replaced = false;
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
        replaced = true;
        idpId = '';
      }
    }

    if (!idpId) {
      const r = await api('POST', `/management/v1/idps/${kind}`, body);
      idpId = r.id || r.idpId;
      if (!idpId) { console.error('FATAL: could not create the SSO connector:', JSON.stringify(r)); process.exit(1); }
      head('Identity provider');
      item(ssoName, replaced ? `created (${kind}, replacing a connector of another type)` : `created (${kind})`);
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
      head('Identity provider');
      item(ssoName, `reconciled from .env (${kind})`);
      const storedId = before.config?.azureAd?.clientId || before.config?.oidc?.clientId;
      if (storedId && storedId !== body.clientId) {
        sub('client id changed', `${storedId} -> ${body.clientId}`);
      }
    }

    const stillAttached = await api('POST', '/management/v1/policies/login/idps/_search', {});
    if (!(stillAttached.result || []).some(i => i.idpId === idpId)) {
      const r = await api('POST', '/management/v1/policies/login/idps',
        { idpId, ownerType: 'IDP_OWNER_TYPE_ORG' });
      if (r.__status >= 400) { console.error(`FATAL: could not offer '${ssoName}' on the sign-in screen:`, JSON.stringify(r)); process.exit(1); }
      sub('sign-in screen', 'now offers it');
    } else sub('sign-in screen', 'already offers it');
    sub('matched on', options.autoLinking === 'AUTO_LINKING_OPTION_USERNAME'
      ? 'the ZITADEL username' : 'the e-mail address');
    sub('may create accounts', 'no');

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
    const secretState = !previous.secret ? 'recorded'
      : previous.secret !== fingerprint ? 'changed' : 'unchanged';
    await writeShared(fingerprintFile,
      JSON.stringify({ secret: fingerprint, clientId: body.clientId, issuer }, null, 2) + '\n', 0o600);

    /* What was actually sent, in terms that can be checked against the portal.
     *
     * A length alone turned out not to be enough: a count that disagrees with
     * the portal says something is wrong and nothing about what, and the
     * obvious next question - "is that even my secret?" - had no answer. So
     * the ends are shown and the middle is not. */
    sub('client id', body.clientId);
    sub('client secret', `${mask(secret)}, ${secretState}`);

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
      warn(`the credentials could not be verified: ${check.unreachable}`);
      note('  ZITADEL needs this same network path to sign anybody in, so this');
      note('  remains a fault even though the seeding itself succeeded.');
    } else {
      sub('credentials', `verified: ${check.detail}`);
    }

    const zitadelURL = process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090';
    sub('redirect uri to register', `${zitadelURL}/idps/callback`);
    note('  In Microsoft Entra this belongs under the Web platform, not');
    note('  Single-page application: ZITADEL redeems the code server side with a');
    note('  client secret, and Entra requires PKCE for anything registered as an');
    note('  SPA, which fails as AADSTS9002325.');
  }
}

/* --- 10. no connector, anywhere, may create users -------------------------
 *
 * Section 9 configures the connector this file MANAGES, which is the one whose
 * display name matches SSO_DISPLAY_NAME. Any other connector attached to the
 * sign-in policy is left exactly as it was found - including, on a stack
 * seeded before this rule existed, with creation switched on.
 *
 * That is the same silent hole in a different place: the screen offers it, an
 * unprovisioned person signs in through it, and ZITADEL makes them an account
 * with no roles and issues a token. So every attached connector is checked,
 * not just ours, and one that can still mint accounts is named.
 *
 * It is REPORTED rather than corrected. Updating a connector requires the
 * endpoint for its own type, and guessing that for a connector this file did
 * not create is how a seeder deletes somebody's working SSO on a Tuesday. The
 * fix is one line of configuration and it is printed with the finding.
 */
{
  const attached = await api('POST', '/management/v1/policies/login/idps/_search', {});
  const offenders = [];
  for (const a of (attached.result || [])) {
    const d = await api('GET', `/v2/idps/${a.idpId}`);
    const o = d.idp?.config?.options || {};
    // Absent means false: protobuf JSON omits default values, so a connector
    // written with these off comes back with the keys simply missing.
    if (o.isCreationAllowed || o.isAutoCreation) {
      offenders.push({ name: a.idpName, id: a.idpId });
    }
  }
  if (offenders.length) {
    panel('A SIGN-IN CONNECTOR CAN STILL CREATE ACCOUNTS', [
      ...offenders.map(o => `  ${o.name}  (${o.id})`),
      '',
      'Anybody at that identity provider can sign in, be given a new account',
      'with no roles, and be issued a valid token with it. Who may use this',
      'stack is decided in config/users/users.yaml, not by the directory.',
      '',
      'Remedy: re-run this container with SSO_DISPLAY_NAME set to the name',
      'above, which reconciles that connector; or delete it in the console',
      'under Settings, Identity Providers.',
    ]);
  } else {
    sub('account creation', 'refused by every attached connector');
  }
}

/* --- 11. what the sign-in screen looks like -------------------------------
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
    warn(`the sign-in branding was not applied: ${JSON.stringify(r).slice(0, 140)}`);
  } else {
    for (const [file, path] of [
      [process.env.BRANDING_LOGO_FILE || '/branding/logo.svg', '/assets/v1/org/policy/label/logo'],
      [process.env.BRANDING_LOGO_DARK_FILE || process.env.BRANDING_LOGO_FILE || '/branding/logo-dark.svg',
       '/assets/v1/org/policy/label/logo/dark'],
    ]) {
      const up = await uploadAsset(path, file);
      if (up === 'missing') continue;
      if (up >= 400) warn(`${file} was not uploaded: HTTP ${up}`);
    }
    /* Nothing is visible until the draft is activated, which is the step that
     * is easy to leave out: every write above succeeds, the console shows the
     * new colours, and the sign-in screen keeps the old ones. */
    await api('POST', '/management/v1/policies/label/_activate', {});
    head('Sign-in screen');
    item('branding', 'applied');
  }

  /* One language, so the picker on the sign-in screen cannot change anything.
   * ZITADEL's login still draws the control - there is no setting that removes
   * it - but with a single allowed language it has nothing to offer. */
  const langs = list(process.env.BRANDING_LANGUAGES, 'en');
  const lr = await api('PUT', '/admin/v1/restrictions', { allowedLanguages: { list: langs } });
  if (lr.__status >= 400 && !unchanged(lr)) warn(`languages were not restricted: ${JSON.stringify(lr).slice(0, 120)}`);
  else item('languages', langs.join(', '));

  /* The sign-in service CACHES all of this.
   *
   * On a first run the ordering already handles it - zitadel-login does not
   * start until this container has finished - so this only matters when the
   * seeder is re-run against a stack that is already up, which is exactly what
   * somebody does after changing any of these settings. Without the restart
   * every write above succeeds, the console shows the new values, and the
   * sign-in screen keeps the old ones, which reads as the change not having
   * worked. */
  item('takes effect after', 'docker compose restart zitadel-login');
}

/* --- 12. the first administrator ------------------------------------------ */
{
  const user  = process.env.BOOTSTRAP_ADMIN_USERNAME || 'admin';
  const pw    = process.env.BOOTSTRAP_ADMIN_PASSWORD || '';
  const email = process.env.BOOTSTRAP_ADMIN_EMAIL || 'admin@example.com';
  const found = await replaceIfUninitialised(await findUserId(user, email), `administrator '${user}'`);
  let id = found.id;

  if (!id) {
    const r = await createHuman({
      username: user, email, password: pw,
      firstName: process.env.BOOTSTRAP_ADMIN_FIRST_NAME || '',
      lastName: process.env.BOOTSTRAP_ADMIN_LAST_NAME || '',
    });
    id = r.id;
    if (!id) { console.error('FATAL: could not create admin:', r.error); process.exit(1); }
    head('People');
    item(user, `created, administrator, ${pw ? 'password sign-in' : 'no password set'}`);
    await api('POST', `/management/v1/users/${id}/grants`,
      { projectId: PLATFORM, roleKeys: ['org-admin'] });
    await api('POST', '/management/v1/orgs/me/members', { userId: id, roles: ['ORG_OWNER'] });
    sub('granted', 'org-admin, ORG_OWNER');
    sub('address', email);
  } else {
    head('People');
    item(user, `exists, administrator, matched on ${found.by}`);

    /* THE ADDRESS IS RECONCILED, like everything else this file derives from
     * configuration.
     *
     * It used to be written only at creation, so changing BOOTSTRAP_ADMIN_EMAIL
     * on a stack that had ever been seeded did nothing at all: the run reported
     * success and the account kept the address it was born with. That is not a
     * cosmetic field - with SSO it is the SIGN-IN IDENTITY, the string
     * auto-linking matches on - so an address that cannot be corrected is an
     * administrator who cannot be let in, and the advice "set
     * BOOTSTRAP_ADMIN_EMAIL and re-run" quietly did not work.
     *
     * Verified rather than merely set: an unverified address does not match. */
    const current = await api('GET', `/management/v1/users/${id}`);
    const held = current.user?.human?.email?.email || '';
    const verified = Boolean(current.user?.human?.email?.isEmailVerified);
    if (held.toLowerCase() !== email.toLowerCase() || !verified) {
      /* v2, not `PUT /management/v1/users/{id}/email`. The v1 endpoint takes
       * `isEmailVerified` and does not always act on it; v2 states the flag as
       * `isVerified` and is the one that leaves the address matchable. */
      const r = await api('POST', `/v2/users/${id}/email`, { email, isVerified: true });
      if (r.__status >= 400 && !unchanged(r)) {
        warn(`the address was not set to ${email}: ${JSON.stringify(r).slice(0, 120)}`);
      } else if (held.toLowerCase() !== email.toLowerCase()) {
        sub('address', `${held || '(none)'} -> ${email}`);
      } else {
        sub('address', `${email}, verified`);
      }
    }
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
    if (pw) warn('BOOTSTRAP_ADMIN_PASSWORD is still set. Unset it once a real administrator exists.');
  }

  /* Auto-linking is by EMAIL ADDRESS, so an address that cannot match is a
   * guaranteed second account.
   *
   * The connector is configured with AUTO_LINKING_OPTION_EMAIL (§9): somebody
   * arriving through Microsoft is joined to the account already holding their
   * address. Leave the administrator on the example address and there is
   * nothing to join them to, so ZITADEL does the other thing it is configured
   * to do - creates the user - and the new account holds no roles. What that
   * looks like is two entries in the account switcher, one of which cannot be
   * signed in to, and a profile page reporting no permissions. It is worth
   * one loud paragraph here rather than an afternoon there. */
  if (process.env.SSO_ISSUER && email === 'admin@example.com') {
    warn('BOOTSTRAP_ADMIN_EMAIL is still admin@example.com while SSO is configured.');
    note('  A sign-in is matched to an existing account by address, and nobody');
    note('  at the identity provider holds that one, so no sign-in reaches this');
    note('  account.');
    note('  Remedy: set BOOTSTRAP_ADMIN_EMAIL to the address that signs in, or');
    note('  add that person to config/users/users.yaml, then re-run this');
    note('  container.');
  }
}

/* --- 13. additional users, from a file that IS the deployment mechanism ----
 *
 * Add a person to config/users/users.yaml, commit it, re-run this container.
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
  let byEmail = null;
  if (email) {
    const r = await api('POST', '/management/v1/users/_search',
      { queries: [{ emailQuery: { emailAddress: email, method: 'TEXT_QUERY_METHOD_EQUALS_IGNORE_CASE' } }] });
    byEmail = (r.result || [])[0] || null;
  }
  let byName = null;
  if (username) {
    const r = await api('POST', '/management/v1/users/_search',
      { queries: [{ userNameQuery: { userName: username } }] });
    byName = (r.result || [])[0] || null;
  }

  /* BOTH matched, and they are different people as far as ZITADEL is
   * concerned. Recorded rather than resolved: deleting somebody's account is
   * not a thing a seeder should decide to do on a re-run. */
  if (byEmail && byName && byEmail.id !== byName.id) {
    duplicates.push({ username, email, keep: byEmail.id, leftover: byName.id });
  }
  const hit = byEmail || byName;
  if (!hit) return { id: '', by: '', state: '' };
  return { id: hit.id, by: byEmail ? 'email' : 'username', state: hit.state || '' };
}

/* An account that was never initialised is not an account.
 *
 * ZITADEL puts a human in USER_STATE_INITIAL when it is created with neither a
 * password nor a verified address, and that state is a DEAD END reachable only
 * by the initialisation mail: the address cannot be corrected, cannot be
 * verified, and a password cannot be set on it. Every one of those answers
 * `User is not yet initialized`.
 *
 * It also cannot be signed in to. Auto-linking joins an arriving external
 * identity to an existing user by VERIFIED address, so an uninitialised
 * account matches nothing, the sign-in comes back Errors.User.NotFound, and
 * the person is told they are not recognised while a console plainly shows
 * them sitting there with the right address on them.
 *
 * Nothing is lost by replacing one. Nobody has ever signed in to it - they
 * could not - and its grants are written again by the run that replaces it.
 * The alternative is a stack that reports success forever and lets no one in.
 */
async function replaceIfUninitialised(found, label) {
  if (!found.id || found.state !== 'USER_STATE_INITIAL') return found;
  const r = await api('DELETE', `/v2/users/${found.id}`);
  if (r.__status >= 400) {
    warn(`${label} was never initialised, cannot sign in, and could not be replaced: ${JSON.stringify(r).slice(0, 120)}`);
    return found;
  }
  uninitialised.push(label);
  return { id: '', by: '', state: '' };
}

/* Creating a person, through the API that leaves them able to sign in.
 *
 * NOT /management/v1/users/human/_import, which is what this used to call.
 * That endpoint accepts `isEmailVerified: true`, answers 200, and - when no
 * password is supplied - stores the address UNVERIFIED and leaves the account
 * in USER_STATE_INITIAL. No error, no warning, and the search result reads
 * correctly enough that the seeder went on to report the person as able to
 * sign in.
 *
 * With SSO configured this seeder deliberately sets no password, because the
 * person signs in through the identity provider. So every human it made was
 * uninitialised, every address was unverified, auto-linking matched none of
 * them, and every SSO sign-in on a correctly configured stack came back
 * Errors.User.NotFound. /v2/users/human honours the flag: ACTIVE, verified,
 * with or without a password.
 */
async function createHuman({ username, firstName, lastName, email, password }) {
  const body = {
    username,
    profile: nameFor({ username, email, firstName, lastName }),
    email: { email, isVerified: true },
  };
  if (password) body.password = { password, changeRequired: false };
  const r = await api('POST', '/v2/users/human', body);
  return { id: r.userId || '', error: r.userId ? '' : JSON.stringify(r).slice(0, 200) };
}

/* A person's NAME, in order of how likely each source is to be true.
 *
 * This file does not know who anybody is. It knows a username, an address, and
 * whatever an operator typed into users.json - and ZITADEL requires BOTH a
 * given and a family name, so something has to be written. What used to be
 * written was 'Platform Administrator', or 'User' as a surname: a fabricated
 * person's name, on a real person's account, shown to them on their own
 * profile page under their own photograph.
 *
 * It does not stay fabricated forever. The connector is configured with
 * isAutoUpdate, so the directory's own name replaces all of this - but NOT on
 * the sign-in that links the account, only on the next one. So there is a real
 * window, usually somebody's first impression of the product, where whatever
 * is chosen here is what they read. It should be true.
 *
 *   1. what the operator supplied. Two fields in users.json, or
 *      BOOTSTRAP_ADMIN_FIRST_NAME / BOOTSTRAP_ADMIN_LAST_NAME.
 *   2. what the ADDRESS spells, when it spells a name: alex.hart@example.com
 *      is Alex Hart in every directory that issues addresses that way. Only
 *      when every part is letters - ap999e@ spells nothing, and a corporate
 *      login id capitalised into a surname is worse than no name at all.
 *   3. the username. It is the one label that is certainly this person's, and
 *      it goes in the DISPLAY name so the page reads `ap999e` rather than
 *      `ap999e ap999e` - which is what setting only the two halves gives you,
 *      because ZITADEL's `name` claim is the display name.
 */
function nameFor({ username, email, firstName, lastName }) {
  if (firstName && lastName) {
    return { givenName: firstName, familyName: lastName, displayName: `${firstName} ${lastName}` };
  }
  const parts = String(email || '').split('@')[0].split(/[._-]+/).filter(Boolean);
  if (parts.length >= 2 && parts.every(w => /^[a-z]+$/i.test(w))) {
    const cap = w => w[0].toUpperCase() + w.slice(1).toLowerCase();
    const givenName = cap(parts[0]);
    const familyName = parts.slice(1).map(cap).join(' ');
    return { givenName, familyName, displayName: `${givenName} ${familyName}` };
  }
  return { givenName: username, familyName: username, displayName: username };
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

{
  for (const u of (doc?.users || [])) {
    if (!u.username) continue;
    const found = await replaceIfUninitialised(
      await findUserId(u.username, u.email), `user ${u.username}`);
    let uid = found.id;

    if (!uid) {
      const r = await createHuman({
        username: u.username,
        firstName: u.firstName || '',
        lastName: u.lastName || '',
        email: u.email || `${u.username}@example.invalid`,
        // A password here is for local and test use. With SSO configured the
        // person signs in through Microsoft and never needs one.
        password: (u.password && !process.env.SSO_ISSUER) ? u.password : '',
      });
      uid = r.id;
      if (!uid) { warn(`${u.username} was not created: ${r.error}`); continue; }
      item(u.username, `created${u.email ? ', ' + u.email : ''}`);
    } else {
      // Which identifier matched, because it is the difference between "the
      // account this file made" and "the account Microsoft made for them".
      item(u.username, `exists, matched on ${found.by}`);
    }

    if (u.orgRoles?.length) {
      await grantRoles(uid, PLATFORM, u.orgRoles);
      sub('tenant-wide', u.orgRoles.join(', '));
    }
    for (const [product, roles] of Object.entries(u.products || {})) {
      const pid = projectOf.get(product);
      if (!pid) { warn(`${u.username}: no product named '${product}' in ${PRODUCTS_DIR}`); continue; }
      await grantRoles(uid, pid, roles.map(r => `${product}:${r}`));
      sub(product, roles.join(', '));
    }
  }
}

/* --- 14. API users --------------------------------------------------------
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
    if (!uid) { warn(`API user ${a.username} was not created`); continue; }
    created = true;
  }
  if (created || a.rotateSecret) {
    const sec = await api('PUT', `/management/v1/users/${uid}/secret`, {});
    item(a.username, 'API user, secret issued');
    sub('client id', sec.clientId);
    sub('client secret', sec.clientSecret);
    note('  ZITADEL does not store this retrievably. It appears here once.');
  } else {
    item(a.username, 'API user, exists');
    sub('secret', 'unchanged; set "rotateSecret": true in users.json to reissue');
  }
  if (a.orgRoles?.length) await grantRoles(uid, PLATFORM, a.orgRoles);
  for (const [product, roles] of Object.entries(a.products || {})) {
    const pid = projectOf.get(product);
    if (pid) await grantRoles(uid, pid, roles.map(r => `${product}:${r}`));
  }
}

/* --- 15. people who can sign in and do nothing ----------------------------
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
    const lines = [];
    for (const u of ungranted.slice(0, 20)) {
      lines.push(`  ${String(u.userName).padEnd(30)}${u.human?.email?.email || '(no address)'}`);
    }
    if (ungranted.length > 20) lines.push(`  ... and ${ungranted.length - 20} more`);
    lines.push('');
    lines.push('Every screen loads for these accounts and every permission is denied.');
    lines.push('Usually the account was created by the identity provider at a first');
    lines.push('sign-in, and this file has never been told about it.');
    lines.push('');
    lines.push('Remedy: add each of them to config/users/users.yaml under the');
    lines.push('address that signs in - that address is what matches them to the');
    lines.push('account that already exists - then re-run this container. For the');
    lines.push('administrator, BOOTSTRAP_ADMIN_EMAIL is the same thing.');
    panel('ACCOUNTS THAT CAN SIGN IN AND HOLD NO ROLES', lines);
  }
}

/* --- 16. can ANYBODY actually sign in? ------------------------------------
 *
 * THE CHECK THAT STOPS THIS FILE PRODUCING AN UNUSABLE STACK.
 *
 * With SSO configured, password sign-in off, and the connector refusing to
 * create accounts, the ONLY way anybody gets in is by holding an address the
 * identity provider will assert and that a ZITADEL user already carries.
 * Seed a stack where no user carries such an address and the result is a
 * deployment nobody can sign in to at all - the person picks their account,
 * the provider authenticates them perfectly, and ZITADEL answers
 * `Errors.User.NotFound` because there is nothing to link them to.
 *
 * That is what happened: `BOOTSTRAP_ADMIN_EMAIL` was left at its example
 * default, so the administrator this file created carried an address nobody
 * can sign in with, and the first sign-in after a rebuild was locked out.
 *
 * Refusing to seed is the right answer rather than a warning, because the
 * remedy is one line of configuration and this is the last moment anybody is
 * looking. A warning at this point is read after the lockout, if at all.
 */
if (process.env.SSO_ISSUER && process.env.SSO_CLIENT_ID) {
  /* Only when there is no other way in. With password sign-in deliberately
   * left on, a stack with no linkable address is awkward rather than dead. */
  const passwordLoginOn = String(process.env.SSO_ALLOW_PASSWORD_LOGIN ?? 'false') === 'true';

  const all = await api('POST', '/management/v1/users/_search', { query: { limit: 500 } });
  /* An address nobody at the identity provider can hold is not a way in.
   * These are the placeholder domains this file and its examples emit; a real
   * one that happens to be unroutable is beyond what can be checked here. */
  const placeholder = /@(example\.(com|org|net|invalid)|localhost|.*\.localhost)$/i;
  /* THREE conditions, and holding an address is only the first of them.
   *
   * Auto-linking matches an arriving external identity against an address that
   * is VERIFIED, on an account that is ACTIVE. An account in
   * USER_STATE_INITIAL, or one carrying an address ZITADEL has not marked
   * verified, is matched by nothing - so listing it here as a way in is a
   * promise the sign-in screen then breaks. That is precisely what this
   * listing used to do, and it is why "the seeder says this address can sign
   * in" and "the sign-in says the account is not recognised" were both true at
   * once. */
  const matchable = (all.result || []).filter(u =>
    u.human?.email?.email &&
    !placeholder.test(u.human.email.email) &&
    u.human.email.isEmailVerified &&
    u.state === 'USER_STATE_ACTIVE');
  const signInAddresses = matchable.map(u => u.human.email.email);

  if (signInAddresses.length === 0 && !passwordLoginOn) {
    console.error('');
    console.error('FATAL: this would seed a stack that nobody can sign in to.');
    console.error('');
    console.error(`  identity provider   ${process.env.SSO_ISSUER}`);
    console.error('  password sign-in    off');
    console.error('  accounts that can be matched to a sign-in    0');
    console.error('');
    console.error('  The only way in is an account holding a verified address that the');
    console.error('  identity provider asserts. No account here holds one: every address');
    console.error('  is a placeholder, or is unverified, or sits on an account that was');
    console.error('  never initialised.');
    console.error('');
    console.error('  A sign-in would reach the provider, authenticate correctly, and come');
    console.error('  back Errors.User.NotFound, because accounts are provisioned here and');
    console.error('  are deliberately never created by signing in.');
    console.error('');
    console.error('  Remedy, then re-run this container:');
    console.error('    BOOTSTRAP_ADMIN_EMAIL=<the address that signs in>');
    console.error('  or add that person to config/users/users.yaml with their address.');
    console.error('  To keep the password box instead: SSO_ALLOW_PASSWORD_LOGIN=true');
    console.error('');
    process.exit(1);
  }

  /* WHO CAN GET IN, listed. A closed system should be able to say who it is
   * closed to, and this is the answer to "why can this person not sign in"
   * without anybody having to open a console. */
  const linkOn = (process.env.SSO_LINK_ON || 'email').toLowerCase() === 'username' ? 'username' : 'address';
  head(`Accounts that can sign in through ${process.env.SSO_DISPLAY_NAME || 'Microsoft'}`);
  item('matched on', linkOn === 'username' ? 'the ZITADEL username' : 'a verified e-mail address');
  item('count', String(signInAddresses.length));
  note();
  /* Username AND address, because the failure this is meant to catch is that
   * the provider asserts one and this directory holds the other. Printing
   * only the matched field hides exactly the mismatch worth seeing. */
  for (const row of table(['USERNAME', 'ADDRESS'],
    matchable.slice(0, 20).map(u => [u.userName, u.human?.email?.email || '']))) note(row);
  if (matchable.length > 20) note(`... and ${matchable.length - 20} more`);
  note();
  note('A sign-in is refused unless the provider asserts one of these exactly.');
  note('The section below reports what it did assert.');
  if (passwordLoginOn) item('password sign-in', 'also on');

  /* AND WHO CANNOT, with the reason.
   *
   * The list above is the answer to "can this person get in". Its complement
   * is the answer to "why can they not", and until this existed there was
   * nowhere to read it: an account that cannot be matched is simply absent
   * from the list, which looks the same as an account nobody has added.
   *
   * The reason that matters is the one nothing announces. Creating a person in
   * ZITADEL's console leaves their address UNVERIFIED unless the "Email
   * Verified" box is ticked, and that box is off by default. The account looks
   * finished - active, addressed, listed among the users - and auto-linking
   * matches a verified address only, so every sign-in comes back
   * Errors.User.NotFound. There is no message anywhere that connects those two
   * facts. This is that message. */
  const humans = (all.result || []).filter(u => u.human);
  const blocked = [];
  for (const u of humans) {
    if (matchable.includes(u)) continue;
    const address = u.human?.email?.email || '';
    const why = !address ? 'no address on the account'
      : placeholder.test(address) ? 'the address is a placeholder that no directory asserts'
      : !u.human.email.isEmailVerified ? 'the address is not verified'
      : u.state !== 'USER_STATE_ACTIVE' ? `the account is ${String(u.state).replace('USER_STATE_', '').toLowerCase()}`
      : 'unknown';
    blocked.push({ user: u.userName || '', address: address || '(none)', why });
  }
  if (blocked.length) {
    head(`Accounts that cannot sign in through ${process.env.SSO_DISPLAY_NAME || 'Microsoft'}`);
    item('count', String(blocked.length));
    note();
    for (const row of table(['USERNAME', 'ADDRESS', 'REASON'],
      blocked.slice(0, 12).map(b => [b.user, b.address, b.why]))) note(row);
    if (blocked.length > 12) note(`... and ${blocked.length - 12} more`);
    note();
    note('A sign-in is matched to a VERIFIED address on an ACTIVE account. In');
    note("ZITADEL's console that is the \"Email Verified\" box on the create-user");
    note('form, which is off by default; on an account that already exists it is');
    note('under Contact Information. An account listed here can still be signed');
    note('in to with a password where password sign-in is on.');
  }

/* --- 17. what the directory actually asserted -----------------------------
 *
 * The one fact nobody could get at, and the reason a sign-in failure here used
 * to be diagnosed by redeploying.
 *
 * A refused sign-in says `Errors.User.NotFound`, which is also what a genuinely
 * unknown person gets, and the screen names no address. So an administrator
 * looking at a correctly provisioned account and a person who cannot get in has
 * nothing to compare: the directory asserted SOMETHING, it matched nothing, and
 * neither string is visible anywhere.
 *
 * ZITADEL records it. Every attempt writes an `idpintent.succeeded` event -
 * succeeded meaning the provider authenticated the person, not that the sign-in
 * worked - carrying the provider's own document about them. That is the string
 * auto-linking was given, so printing it beside the addresses this stack holds
 * turns "not recognised" into a diff.
 *
 * Read-only, and the last few. This is a report, not a log: what is wanted is
 * the most recent handful of distinct people, not an audit trail.
 */
if (process.env.SSO_ISSUER) {
  const ev = await api('POST', '/admin/v1/events/_search',
    { limit: 50, asc: false, eventTypes: ['idpintent.succeeded'] });

  /* Newest per person. Somebody who tries four times is one line. */
  const seen = new Map();
  for (const e of (ev.events || [])) {
    const key = e.payload?.idpUserId || e.payload?.idpUserName;
    if (!key || seen.has(key)) continue;
    let raw = {};
    try { raw = JSON.parse(Buffer.from(e.payload.idpUser || '', 'base64').toString('utf8')); } catch { /* not fatal */ }
    seen.set(key, { when: (e.creationDate || '').replace('T', ' ').slice(0, 19), raw,
      name: e.payload.idpUserName || '' });
  }

  if (seen.size) {
    /* WHICH FIELD becomes the address is the connector's rule, not ours.
     * ZITADEL's Microsoft connector reads Graph: the `mail` attribute, and the
     * user principal name when the directory holds no mail on that account. A
     * generic OIDC connector reads the `email` claim and has no fallback. */
    const asserted = (r) => r.mail || r.email || r.userPrincipalName || r.preferred_username || '';
    const held = new Map(matchable.map(u => [String(u.human.email.email).toLowerCase(), u.userName]));

    head(`Sign-in attempts through ${process.env.SSO_DISPLAY_NAME || 'Microsoft'}`);
    item('recorded', String(seen.size));
    note();
    const rows = [...seen.values()].slice(0, 10).map(a => {
      const addr = asserted(a.raw) || a.name;
      return [a.when, addr, held.get(addr.toLowerCase()) || 'none'];
    });
    for (const row of table(['WHEN', 'ASSERTED ADDRESS', 'MATCHED ACCOUNT'], rows)) note(row);
    note();
    note("The asserted address is the directory's own value for that person: the");
    note('Graph mail attribute, or the user principal name when the account has');
    note('no mail. A sign-in is refused when no active account holds it as a');
    note('verified address.');
    note();
    note('Remedy for an unmatched address: correct it in .env or in');
    note('config/users/users.yaml, then re-run this container.');
  }
}
}

/* --- 18. two accounts for one person --------------------------------------
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
  const lines = [];
  for (const d of duplicates) {
    lines.push(`  ${d.email}`);
    lines.push(`    roles granted to the account holding that address  (${d.keep})`);
    lines.push(`    a second account named '${d.username}' also exists (${d.leftover})`);
  }
  lines.push('');
  lines.push('This is what a sign-in through the identity provider leaves behind when');
  lines.push('the account seeded for that person carried a different address: ZITADEL');
  lines.push('matches on the address, finds nothing, and creates one. The roles are');
  lines.push('on the account that signs in, so nobody is locked out.');
  lines.push('');
  lines.push('The leftover cannot sign in: it has no identity at the provider, and');
  lines.push('password sign-in is off. It is safe to delete, and this file does not');
  lines.push('delete accounts it did not create. In the console: Users, open it,');
  lines.push('Delete. Or re-create the stack from empty with `docker compose down -v`,');
  lines.push('which discards the database and every account in it.');
  panel('TWO ACCOUNTS FOR THE SAME PERSON', lines);
}

/* --- 19. state plainly what is not production-ready -----------------------
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
    panel('NOT READY FOR ANYTHING OTHER PEOPLE CAN REACH', [
      ...todo.map(t => `  - ${t}`),
      '',
      'Remedy: cp .env.example .env, fill it in, then',
      '        docker compose up -d && docker compose run --rm zitadel-init',
    ]);
  }
}

head('Summary');
item('result', 'bootstrap complete');
item('tenant', `${TENANT} (${ORG_ID})`);
item('products', products.join(', ') || 'none');
item('admin console', process.env.ZITADEL_PUBLIC_URL || 'http://localhost:8090');
item('application', process.env.WEB_PUBLIC_URL || 'http://localhost:8000');
if (uninitialised.length) {
  item('accounts replaced', String(uninitialised.length));
  for (const label of uninitialised) sub(label, 'was never initialised and could not sign in');
}
