#!/usr/bin/env node
/**
 * The bridge between a compose VOLUME and a cluster SECRET.
 *
 * # The problem this solves
 *
 * bootstrap.mjs produces four things that outlive the run and are read by other
 * containers:
 *
 *   /pat/pat.txt             ZITADEL writes it at first boot; the seeder
 *                            authenticates with it on every run afterwards
 *   /pat/login-client.pat    the sign-in screens' service-user token
 *   /oidc/web.json           the SPA's OIDC client id (public values only)
 *   /workercreds/worker.json the data plane's machine credentials
 *
 * Under compose these are named volumes: written once, mounted by whoever needs
 * them, still there on the next `docker compose up`. A Kubernetes Job has no
 * such thing. Its filesystem is gone when it finishes, and a ReadWriteOnce PVC
 * shared with pods on other nodes is not a mechanism, it is a scheduling
 * constraint that eventually fails at 03:00.
 *
 * So in a cluster the volume is a SECRET, and this file is the only thing that
 * knows it. bootstrap.mjs is untouched: it reads and writes files, exactly as
 * it does on a laptop.
 *
 *     node k8s-state.mjs pull    # Secret -> files, before the seeder runs
 *     node bootstrap.mjs         # unchanged
 *     node k8s-state.mjs push    # files -> Secret, after it succeeds
 *
 * # Why the Kubernetes API and not kubectl
 *
 * Same reason bootstrap.mjs parses YAML by hand: `apk add kubectl` is a network
 * call to a distro CDN at deploy time, and this product ships into estates that
 * do not have one. The in-cluster API is an HTTPS endpoint, the service
 * account token is a file, and node has had `fetch` since 18.
 *
 * # What it is allowed to do
 *
 * One Secret, by name, in its own namespace. The Role in the chart grants get
 * and patch on that name and create on the resource - `create` cannot be
 * restricted to a name by the RBAC API, which is a Kubernetes limitation and
 * not a decision here. It reads no other object and holds no cluster-scoped
 * permission at all.
 */
import fs from 'node:fs/promises';

const SA = '/var/run/secrets/kubernetes.io/serviceaccount';
const API = `https://${process.env.KUBERNETES_SERVICE_HOST}:${process.env.KUBERNETES_SERVICE_PORT_HTTPS || 443}`;

/* The map, and it is the whole contract with the chart. A key here is a key in
 * the Secret, which is a FILE NAME in every volume that projects it - so the
 * chart mounts `worker.json` into the workers and `login-client.pat` into the
 * sign-in screens with no rename anywhere. */
const FILES = [
  { path: '/pat/pat.txt', key: 'pat.txt', mode: 0o600 },
  { path: '/pat/login-client.pat', key: 'login-client.pat', mode: 0o600 },
  { path: '/pat/sso-fingerprint.json', key: 'sso-fingerprint.json', mode: 0o600 },
  { path: '/oidc/web.json', key: 'web.json', mode: 0o644 },
  { path: '/workercreds/worker.json', key: 'worker.json', mode: 0o600 },
];

const NAME = process.env.K8S_STATE_SECRET;
if (!NAME) {
  console.error('FATAL: K8S_STATE_SECRET is not set. This file is only for the cluster path;');
  console.error('       under docker compose the shared volumes do this job and it is not run.');
  process.exit(1);
}

const namespace = (await fs.readFile(`${SA}/namespace`, 'utf8')).trim();
const token = (await fs.readFile(`${SA}/token`, 'utf8')).trim();
const url = `${API}/api/v1/namespaces/${namespace}/secrets/${NAME}`;

async function call(method, body, contentType = 'application/json') {
  const res = await fetch(method === 'POST'
    ? `${API}/api/v1/namespaces/${namespace}/secrets`
    : url, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: 'application/json',
      ...(body ? { 'Content-Type': contentType } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  return { status: res.status, body: text ? JSON.parse(text) : {} };
}

async function pull() {
  const res = await call('GET');
  if (res.status === 404) {
    console.log(`state: ${NAME} does not exist yet - this is a first run`);
    return;
  }
  if (res.status >= 300) {
    console.error(`FATAL: reading Secret ${NAME}: ${res.status} ${JSON.stringify(res.body)}`);
    process.exit(1);
  }
  const data = res.body.data || {};
  const restored = [];
  for (const f of FILES) {
    const b64 = data[f.key];
    if (!b64) continue;
    await fs.mkdir(f.path.slice(0, f.path.lastIndexOf('/')), { recursive: true });
    await fs.writeFile(f.path, Buffer.from(b64, 'base64'), { mode: f.mode });
    restored.push(f.key);
  }
  console.log(`state: restored ${restored.length ? restored.join(', ') : 'nothing'} from Secret ${NAME}`);
}

async function push() {
  const data = {};
  for (const f of FILES) {
    const buf = await fs.readFile(f.path).catch(() => null);
    /* An EMPTY file is not a value. ZITADEL creates /pat/pat.txt on the run
     * that initialises the instance and leaves it alone afterwards; writing a
     * zero-byte key would replace a good token with nothing on the next run. */
    if (!buf || buf.length === 0) continue;
    data[f.key] = buf.toString('base64');
  }
  if (Object.keys(data).length === 0) {
    console.log('state: nothing to publish');
    return;
  }

  /* A MERGE PATCH, not a replace. The Secret may hold keys written by a
   * previous version of this file, and a run that did not reach step 8 must
   * not delete the worker credentials it did not rewrite. */
  let res = await call('PATCH', { data }, 'application/merge-patch+json');
  if (res.status === 404) {
    res = await call('POST', {
      apiVersion: 'v1',
      kind: 'Secret',
      metadata: {
        name: NAME,
        namespace,
        labels: { 'app.kubernetes.io/managed-by': 'software-gateway-seeder' },
        annotations: {
          'softwaregateway.io/description':
            'Written by the seeding Job. The cluster equivalent of the patshare, ' +
            'oidcshare and workercreds volumes in docker-compose.yml.',
        },
      },
      type: 'Opaque',
      data,
    });
  }
  if (res.status >= 300) {
    console.error(`FATAL: writing Secret ${NAME}: ${res.status} ${JSON.stringify(res.body)}`);
    process.exit(1);
  }
  console.log(`state: published ${Object.keys(data).sort().join(', ')} to Secret ${NAME}`);
}

const verb = process.argv[2];
if (verb === 'pull') await pull();
else if (verb === 'push') await push();
else {
  console.error(`usage: node k8s-state.mjs pull|push  (got ${verb || 'nothing'})`);
  process.exit(1);
}
