{{/*
ORDERED STARTUP, WITHOUT A CRASH LOOP.

Kubernetes has no `depends_on`, and the usual answer - let the pod start, fail,
and be restarted until its dependency appears - is the behaviour this is here to
remove. A pod in CrashLoopBackOff looks identical whether it is waiting for its
database or genuinely broken, so it trains everybody to ignore the one state
that should never be ignored.

An init container waiting on a condition is the opposite. The pod sits in
`Init:0/1`, which reads as "not started yet" rather than "failing", it logs one
line saying what it is waiting for, and the container it gates has not run at
all - so its restart count stays zero and means something.

Three waits, and they cover every dependency in this release:

  swgw.waitForTCP       a port answers          (the database)
  swgw.waitForHTTP      an endpoint is ready    (the coordinator, ZITADEL)
  swgw.waitForFile      a projected key exists  (what the seeder publishes)

Each takes a deadline. Passing it is a FAILURE for a hard dependency - the pod
reports `Init:Error` and says which one, which is a truthful and actionable
state - and for a soft dependency the wait gives up and lets the container
start, because a UI that cannot reach its identity provider must still load in
order to say so.
*/}}

{{/* (dict "ctx" $ "name" "database" "host" "..." "port" "5432") */}}
{{- define "swgw.waitForTCP" -}}
- name: wait-for-{{ .name }}
  image: {{ .ctx.Values.images.node }}
  imagePullPolicy: {{ .ctx.Values.image.pullPolicy }}
  command: ["node", "-e"]
  args:
    - |
      const net = require('node:net');
      const [host, port, budget] = [{{ .host | quote }}, {{ .port }}, {{ .ctx.Values.database.waitTimeoutSeconds }}];
      const deadline = Date.now() + budget * 1000;
      const once = () => new Promise(resolve => {
        const s = net.connect({ host, port });
        const done = ok => { s.destroy(); resolve(ok); };
        s.setTimeout(3000);
        s.on('connect', () => done(true));
        s.on('error',   () => done(false));
        s.on('timeout', () => done(false));
      });
      let said = false;
      while (Date.now() < deadline) {
        if (await once()) { console.log(`{{ .name }} is accepting connections at ${host}:${port}`); process.exit(0); }
        if (!said) { console.log(`waiting for {{ .name }} at ${host}:${port}`); said = true; }
        await new Promise(r => setTimeout(r, 2000));
      }
      console.error(`{{ .name }} did not accept a connection at ${host}:${port} within ${budget}s.`);
      console.error(`This pod has not started its application container, so nothing has failed yet.`);
      process.exit(1);
  securityContext:
    {{- include "swgw.containerSecurityContext" .ctx | nindent 4 }}
  resources:
    requests: {cpu: 10m, memory: 32Mi}
    limits: {memory: 64Mi}
{{- end -}}

{{/* (dict "ctx" $ "name" "zitadel" "url" "http://zitadel:8080/debug/ready") */}}
{{- define "swgw.waitForHTTP" -}}
- name: wait-for-{{ .name }}
  image: {{ .ctx.Values.images.node }}
  imagePullPolicy: {{ .ctx.Values.image.pullPolicy }}
  command: ["node", "-e"]
  args:
    - |
      const [url, budget, soft] = [{{ .url | quote }}, {{ default .ctx.Values.database.waitTimeoutSeconds .timeout }}, {{ default false .soft }}];
      const deadline = Date.now() + budget * 1000;
      let said = false;
      while (Date.now() < deadline) {
        try {
          const r = await fetch(url, { signal: AbortSignal.timeout(3000) });
          if (r.ok) { console.log(`{{ .name }} is ready`); process.exit(0); }
        } catch {}
        if (!said) { console.log(`waiting for {{ .name }} at ${url}`); said = true; }
        await new Promise(r => setTimeout(r, 3000));
      }
      if (soft) {
        console.log(`{{ .name }} is not ready after ${budget}s. Starting anyway:`);
        console.log(`this container is useful without it and says so on the screen.`);
        process.exit(0);
      }
      console.error(`{{ .name }} did not become ready at ${url} within ${budget}s.`);
      process.exit(1);
  securityContext:
    {{- include "swgw.containerSecurityContext" .ctx | nindent 4 }}
  resources:
    requests: {cpu: 10m, memory: 32Mi}
    limits: {memory: 64Mi}
{{- end -}}

{{/* (dict "ctx" $ "name" "credentials" "path" "/etc/.../worker.json" "soft" true)
     Mounts nothing itself - the caller gives it the same volumeMounts the
     application container has, because the thing being waited for IS that
     mount. A Secret key that does not exist yet is simply an absent file, and
     the kubelet materialises it when the seeder writes it. */}}
{{- define "swgw.waitForFile" -}}
- name: wait-for-{{ .name }}
  image: {{ .ctx.Values.images.node }}
  imagePullPolicy: {{ .ctx.Values.image.pullPolicy }}
  command: ["node", "-e"]
  args:
    - |
      const fs = require('node:fs');
      const [path, budget, soft] = [{{ .path | quote }}, {{ default .ctx.Values.database.waitTimeoutSeconds .timeout }}, {{ default false .soft }}];
      const deadline = Date.now() + budget * 1000;
      let said = false;
      while (Date.now() < deadline) {
        try { if (fs.statSync(path).size > 0) { console.log(`{{ .name }} is present at ${path}`); process.exit(0); } } catch {}
        if (!said) { console.log(`waiting for {{ .name }} at ${path} - the seeding Job publishes it`); said = true; }
        await new Promise(r => setTimeout(r, 3000));
      }
      if (soft) {
        console.log(`{{ .name }} was not published within ${budget}s. Starting anyway:`);
        console.log(`this container is useful without it and says so on the screen.`);
        process.exit(0);
      }
      console.error(`{{ .name }} was not published at ${path} within ${budget}s.`);
      console.error(`Read the seeding Job's log: kubectl logs -l app.kubernetes.io/component=seed`);
      process.exit(1);
  volumeMounts:
{{ .mounts | indent 4 }}
  securityContext:
    {{- include "swgw.containerSecurityContext" .ctx | nindent 4 }}
  resources:
    requests: {cpu: 10m, memory: 32Mi}
    limits: {memory: 64Mi}
{{- end -}}

