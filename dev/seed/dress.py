#!/usr/bin/env python3
"""Dress the development database with the two things a local run cannot produce.

Everything else here is real: dev/fakeregistry serves the release trees, the
Coordinator discovers them, and a Worker moves the bytes. Two facts have no
local source, so they are written here rather than faked in the interface:

  scanner findings  there is no Xray on a laptop, and the vulnerability pages
                    are most of what this database exists to exercise;
  signatures        there is no cosign in a local loop either, and an
                    unsigned estate hides the verification column entirely;
  artifact sizes    a fake registry that really served 30 GB layers would need
                    30 GB of memory, and a registry that only CLAIMED to breaks
                    the push - the worker sets Content-Length from the
                    descriptor. So the seeded trees are kilobytes, real bytes
                    move, and the display sizes are scaled afterwards.

Run it once discovery and the transfers have settled:

    python3 dev/seed/dress.py dev/swgw.db
"""
import hashlib, json, random, sqlite3, sys
from datetime import datetime, timedelta, timezone

DB = sys.argv[1] if len(sys.argv) > 1 else 'dev/swgw.db'
random.seed(20260824)
NOW = datetime.now(timezone.utc)
GB = 1024 ** 3
MB = 1024 ** 2

def ts(dt): return dt.strftime('%Y-%m-%dT%H:%M:%S.000Z')
def parse(s):
    if not s: return NOW
    return datetime.strptime(s[:19], '%Y-%m-%dT%H:%M:%S').replace(tzinfo=timezone.utc)

c = sqlite3.connect(DB)
c.execute('PRAGMA foreign_keys=OFF')

# --------------------------------------------------------------- artifacts --
def short(name):
    return name.rsplit('/', 1)[-1].split(':', 1)[0]

def artifact_name(annotations, digest):
    """What the release calls one of its artifacts - the same rule the API uses."""
    if not annotations: return digest[7:19]
    a = json.loads(annotations)
    ref = a.get('org.opencontainers.image.ref.name', '')
    if ref: return short(ref)
    return a.get('org.opencontainers.image.title') or digest[7:19]

def is_chart(name):
    return name.endswith(('-crds', '-core')) or name in ('cloud-ran', 'cmm')

def size_for(name):
    """A component's weight, derived from its NAME so it is stable across
    releases: a User Plane Function that is 3.4 GB in one release and 0.9 GB in
    the next would make every comparison read as a rebuild."""
    if is_chart(name):
        return int(1.2 * MB) + int(hashlib.sha256(name.encode()).hexdigest()[:6], 16) % (3 * MB)
    seed = int(hashlib.sha256(name.encode()).hexdigest()[:8], 16)
    return int((0.6 + (seed % 3400) / 1000.0) * GB)

# Scale the artifact tree, base layer largest, the way an image on a
# distribution base is. The transfers have already moved the real bytes.
for pkg_id, in list(c.execute('SELECT id FROM packages')):
    total = 0
    for art_id, digest, annotations, depth, manifest_bytes in c.execute(
            'SELECT id, digest, annotations, depth, size_bytes FROM package_artifacts'
            ' WHERE package_id=? ORDER BY id', (pkg_id,)).fetchall():
        if depth == 0:
            continue  # the index itself weighs what its own manifest weighs
        target = size_for(artifact_name(annotations, digest))
        blobs = c.execute('SELECT digest, kind FROM artifact_blobs WHERE artifact_id=?'
                          ' ORDER BY ordinal', (art_id,)).fetchall()
        layers = [b for b in blobs if b[1] == 'layer']
        remaining = target
        for i, (bdig, _) in enumerate(layers):
            if len(layers) > 2 and i == 0:
                share = int(target * 0.45)
            elif i == len(layers) - 1:
                share = remaining
            else:
                share = remaining // (len(layers) - i)
            share = max(share, 64 * 1024)
            remaining = max(remaining - share, 0)
            c.execute('UPDATE blobs SET size_bytes=? WHERE digest=?', (share, bdig))
        for bdig, kind in blobs:
            if kind == 'config':
                c.execute('UPDATE blobs SET size_bytes=4096 WHERE digest=?', (bdig,))
        # NOT package_artifacts.size_bytes. That column is what the REFERRING
        # DESCRIPTOR said the manifest weighs - a couple of kilobytes of JSON -
        # and the API adds it to the blob sum to produce ContentBytes. Writing
        # the content size into it made every image report twice its weight.
        total += target + manifest_bytes
    c.execute('UPDATE packages SET total_bytes=? WHERE id=?', (total, pkg_id))

# A transfer's recorded plan is scaled with it, so no page contradicts another.
for tid, pkg_id in c.execute('SELECT id, package_id FROM transfers').fetchall():
    total = c.execute('SELECT total_bytes FROM packages WHERE id=?', (pkg_id,)).fetchone()[0] or 0
    c.execute('UPDATE transfers SET planned_bytes=?, dedupe_skipped_bytes=?, mountable_bytes=?'
              ' WHERE id=?', (total, int(total * 0.28), int(total * 0.16), tid))

# ------------------------------------------------------------- signatures --
# The vendor signs its releases; there is no cosign in the loop locally, so the
# verification records are written rather than produced.
for pkg_id, root, in [(r[0], r[1]) for r in c.execute('SELECT id, manifest_digest FROM packages')]:
    c.execute("UPDATE packages SET signature_status='signed' WHERE id=?", (pkg_id,))
    done = c.execute("SELECT id, target_repo_id, completed_at FROM transfers"
                     " WHERE package_id=? AND state='succeeded' LIMIT 1", (pkg_id,)).fetchone()
    if not done:
        continue
    tid, repo_id, completed = done
    if c.execute('SELECT count(*) FROM verifications WHERE package_id=?', (pkg_id,)).fetchone()[0]:
        continue
    n = c.execute('SELECT count(*) FROM package_artifacts WHERE package_id=? AND depth>0',
                  (pkg_id,)).fetchone()[0]
    c.execute("""INSERT INTO verifications
        (package_id, transfer_id, repository_id, stage, state, policy, subject_digest,
         details, started_at, completed_at)
        VALUES (?,?,?,?,?,?,?,?,?,?)""",
        (pkg_id, tid, repo_id, 'destination', 'passed', 'enforce', root,
         json.dumps({'signatures': n, 'identity': 'release-signing@vendor.example.com',
                     'issuer': 'https://fulcio.sigstore.dev'}), completed, completed))

# ---------------------------------------------------------- vulnerabilities --
CVES = [
    ('CVE-2024-6387',  'critical', 9.8, 'openssh-server', '1:8.9p1-3ubuntu0.6', 'deb', 'regreSSHion: unauthenticated remote code execution in OpenSSH server', ['1:8.9p1-3ubuntu0.10']),
    ('CVE-2024-3094',  'critical',10.0, 'xz-utils',       '5.4.5-0.3',          'deb', 'Malicious backdoor planted in upstream xz/liblzma release tarballs', ['5.4.5-0.4']),
    ('CVE-2023-4911',  'critical', 7.8, 'glibc',          '2.35-0ubuntu3.4',    'deb', 'Looney Tunables: buffer overflow in the dynamic loader GLIBC_TUNABLES parser', ['2.35-0ubuntu3.5']),
    ('CVE-2024-21626', 'critical', 8.6, 'runc',           '1.1.7',              'go',  'Container escape through a file descriptor leaked into the container process', ['1.1.12']),
    ('CVE-2024-45491', 'high',     7.5, 'libexpat1',      '2.4.7-1ubuntu0.2',   'deb', 'Integer overflow parsing XML DTDs on 32-bit platforms', ['2.4.7-1ubuntu0.4']),
    ('CVE-2024-2961',  'high',     8.8, 'glibc',          '2.35-0ubuntu3.4',    'deb', 'Out-of-bounds write in the iconv ISO-2022-CN-EXT converter', ['2.35-0ubuntu3.7']),
    ('CVE-2023-44487', 'high',     7.5, 'golang.org/x/net','0.14.0',            'go',  'HTTP/2 Rapid Reset: denial of service through repeated stream cancellation', ['0.17.0']),
    ('CVE-2024-24790', 'high',     9.8, 'stdlib',         'go1.21.6',           'go',  'net/netip misclassifies IPv4-mapped IPv6 addresses as private', ['go1.21.11', 'go1.22.4']),
    ('CVE-2023-45853', 'high',     9.8, 'zlib1g',         '1:1.2.11.dfsg-2',    'deb', 'Heap buffer overflow in the MiniZip zipOpenNewFileInZip4_64 helper', []),
    ('CVE-2024-28180', 'high',     7.1, 'gopkg.in/square/go-jose.v2', '2.6.0',  'go',  'Improper handling of highly compressed JWE data allows resource exhaustion', ['3.0.1']),
    ('CVE-2024-37891', 'medium',   4.4, 'urllib3',        '1.26.18',            'pypi','Proxy-Authorization header is retained across cross-origin redirects', ['1.26.19', '2.2.2']),
    ('CVE-2024-35195', 'medium',   5.6, 'requests',       '2.31.0',             'pypi','A session that once set verify=False keeps skipping verification', ['2.32.0']),
    ('CVE-2024-6119',  'medium',   5.9, 'libssl3',        '3.0.2-0ubuntu1.15',  'deb', 'Type confusion comparing X.509 GENERAL_NAME entries', ['3.0.2-0ubuntu1.18']),
    ('CVE-2023-39325', 'medium',   7.5, 'stdlib',         'go1.21.1',           'go',  'Rapid HTTP/2 stream resets consume excessive server resources', ['go1.21.3']),
    ('CVE-2024-34156', 'medium',   5.5, 'stdlib',         'go1.22.2',           'go',  'encoding/gob stack exhaustion on deeply nested structures', ['go1.22.7']),
    ('CVE-2022-48174', 'medium',   6.5, 'busybox',        '1.30.1-7ubuntu3',    'deb', 'Stack overflow in the ash shell arithmetic evaluator', []),
    ('CVE-2024-26462', 'low',      5.5, 'krb5-libs',      '1.19.2-2ubuntu0.3',  'deb', 'Memory leak handling principals in kadmin', ['1.19.2-2ubuntu0.4']),
    ('CVE-2023-50495', 'low',      5.5, 'libncursesw6',   '6.3-2ubuntu0.1',     'deb', 'Segmentation fault in the ncurses _nc_wrap_entry helper', []),
    ('CVE-2024-52533', 'low',      3.7, 'libglib2.0-0',   '2.72.4-0ubuntu2.2',  'deb', 'Buffer overflow negotiating a SOCKS4 proxy connection', ['2.72.4-0ubuntu2.5']),
    ('XRAY-612094',    'low',      0.0, 'log4j-api',      '2.17.1',             'maven','Vendor advisory: verbose stack traces disclose the deployed classpath layout', []),
]
SEV = ['critical', 'high', 'medium', 'low', 'unknown']
PREFIX = {'deb': 'deb', 'go': 'go', 'pypi': 'pypi', 'maven': 'gav', 'npm': 'npm'}

def finding(cv):
    ident, sev, score, comp, ver, ctype, summary, fixed = cv
    f = {
        'severity': sev, 'summary': summary, 'provider': 'jfrog-xray',
        'component': {'id': '%s://%s:%s' % (PREFIX.get(ctype, 'generic'), comp, ver),
                      'name': comp, 'version': ver, 'type': ctype},
        'fixable': bool(fixed),
        'references': ['https://nvd.nist.gov/vuln/detail/' + ident] +
                      (['https://ubuntu.com/security/' + ident] if ctype == 'deb' else []),
        'published': ts(NOW - timedelta(days=random.randint(40, 500))),
    }
    if fixed: f['fixedIn'] = fixed
    if score:
        f['cvssScore'] = score
        f['cvssVector'] = ('CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H' if score >= 9
                           else 'CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:N')
    if sev in ('critical', 'high'): f['policy'] = 'Production Gate'
    if ident.startswith('CVE-'): f['cve'] = ident
    else: f['id'] = ident
    return f

def counts_of(fs):
    n = {s: 0 for s in SEV}; fx = {s: 0 for s in SEV}
    for f in fs:
        n[f['severity']] += 1
        if f['fixable']: fx[f['severity']] += 1
    return n, fx

for t in ['security_findings', 'security_details', 'security_scans', 'package_security']:
    c.execute('DELETE FROM %s' % t)

# Which advisories a release carries. Ordered oldest-first per product so a
# newer release genuinely fixes some and, once, introduces one - which is what
# makes the release comparison say something rather than shrink a number.
products = {r[0]: r[1] for r in c.execute('SELECT id, name FROM products WHERE active=1')}

for pid, prod_name in products.items():
    releases = list(c.execute(
        'SELECT id, tag, published_at FROM packages WHERE product_id=? ORDER BY published_at',
        (pid,)))
    for rel_index, (pkg_id, tag, published) in enumerate(releases):
        newest = rel_index == len(releases) - 1
        artifacts = list(c.execute(
            'SELECT digest, media_type, annotations, size_bytes, depth FROM package_artifacts'
            ' WHERE package_id=? ORDER BY id', (pkg_id,)))
        artifacts = [a for a in artifacts if a[4] > 0]
        if not artifacts:
            continue

        repo_path = c.execute(
            'SELECT r.repository_path FROM packages p JOIN repositories r'
            ' ON r.id = p.source_repo_id WHERE p.id=?', (pkg_id,)).fetchone()[0]
        scanned_at = min(parse(published) + timedelta(days=1), NOW - timedelta(hours=6))
        scope_repo = 'jfrog-lab'

        # An upgrade fixes the worst problems first and leaves a long tail, so
        # a newer release drops advisories off the FRONT of the pool - which is
        # ordered worst-first. Trimming the tail instead made every release look
        # like it was accumulating criticals, which is the opposite story.
        drop = min(rel_index, 4)
        pool_all = CVES[drop:]
        # One advisory arrives WITH the newest release. A comparison whose every
        # row is an improvement teaches a reader to stop reading it.
        if newest:
            pool_all = pool_all + [CVES[3]]

        agg = {s: 0 for s in SEV}; aggfix = {s: 0 for s in SEV}
        distinct = set()
        cov = dict(artifacts=len(artifacts), scanned=0, not_scanned=0,
                   unsupported=0, unavailable=0, disabled=0)

        for ai, (digest, media, annotations, size, depth) in enumerate(artifacts):
            name = artifact_name(annotations, digest)
            chart = is_chart(name)
            if chart:
                status, fs = 'unsupported', []
                message = 'A Helm chart carries no filesystem for the scanner to index.'
            elif newest and ai == len(artifacts) - 2:
                status, fs = 'not_scanned', []
                message = 'Xray has not finished indexing this image yet.'
            else:
                status, message = 'scanned', ''
                pool = pool_all
                # Each image carries a subset, deterministic in its own name so
                # the same image reports the same thing on every run.
                rnd = random.Random(name + tag)
                keep = rnd.sample(pool, max(2, int(len(pool) * rnd.uniform(0.45, 0.9))))
                fs = [finding(cv) for cv in keep]
                fs.sort(key=lambda f: (SEV.index(f['severity']), f.get('cve', f.get('id', ''))))
            cov[{'scanned': 'scanned', 'not_scanned': 'not_scanned',
                 'unsupported': 'unsupported'}[status]] += 1

            n, fx = counts_of(fs)
            for s in SEV:
                agg[s] += n[s]; aggfix[s] += fx[s]
            for f in fs:
                distinct.add((f.get('cve') or f.get('id'), f['component']['id']))

            report = {
                'artifact': {'name': name, 'tag': tag, 'digest': digest,
                             'repository': repo_path + '/' + name,
                             'registry': 'artifactory.internal.example.com',
                             'mediaType': media, 'kind': 'chart' if chart else 'image',
                             'sizeBytes': size, 'platform': 'linux/amd64'},
                'status': status, 'provider': 'jfrog-xray', 'message': message,
                'findings': fs,
                'counts': dict(total=sum(n.values()), fixable=sum(fx.values()),
                               **{s: n[s] for s in SEV},
                               **{'fix' + s.capitalize(): fx[s] for s in SEV}),
                'scannedAt': ts(scanned_at),
                'retrievedAt': ts(scanned_at + timedelta(minutes=6)),
            }
            fp = hashlib.sha256(json.dumps(fs, sort_keys=True).encode()).hexdigest()[:32]

            c.execute("""INSERT INTO security_scans
                (product, repository, role, provider, artifact_ref, artifact_key, artifact_tag,
                 artifact_kind, artifact_repo, status, message, total, fixable,
                 critical, high, medium, low, unknown,
                 fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
                 scanned_at, retrieved_at, fingerprint, evictable_at)
                VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
                (prod_name, scope_repo, 'target', 'jfrog-xray', digest, name, tag,
                 'chart' if chart else 'image', repo_path + '/' + name, status, message,
                 sum(n.values()), sum(fx.values()),
                 n['critical'], n['high'], n['medium'], n['low'], n['unknown'],
                 fx['critical'], fx['high'], fx['medium'], fx['low'], fx['unknown'],
                 ts(scanned_at), ts(scanned_at + timedelta(minutes=6)), fp,
                 ts(NOW + timedelta(days=7))))
            scan_id = c.execute(
                'SELECT id FROM security_scans WHERE product=? AND repository=? AND provider=?'
                ' AND artifact_ref=?', (prod_name, scope_repo, 'jfrog-xray', digest)).fetchone()[0]
            for f in fs:
                c.execute("""INSERT OR IGNORE INTO security_findings
                    (scan_id, cve, issue_id, severity, fixable, component_id, component_name,
                     component_version, component_type, fixed_in, summary)
                    VALUES (?,?,?,?,?,?,?,?,?,?,?)""",
                    (scan_id, f.get('cve', ''), f.get('id', ''), f['severity'],
                     1 if f['fixable'] else 0, f['component']['id'], f['component']['name'],
                     f['component']['version'], f['component']['type'],
                     (f.get('fixedIn') or [''])[0], f['summary']))
            # `codec` says how the payload is stored; the seed writes plain
            # JSON, which is what 'json' means. See internal/store/securitydocs.go.
            c.execute("""INSERT OR REPLACE INTO security_details
                (product, repository, provider, artifact_ref, payload, codec, bytes,
                 source_bytes, fingerprint, retrieved_at, evictable_at)
                VALUES (?,?,?,?,?,?,?,?,?,?,?)""",
                (prod_name, scope_repo, 'jfrog-xray', digest, json.dumps(report).encode(),
                 'json', len(json.dumps(report)), len(json.dumps(report)),
                 fp, ts(scanned_at + timedelta(minutes=6)), ts(NOW + timedelta(days=7))))

        log = [
            dict(at=ts(scanned_at), level='info',
                 message='Asked JFrog Xray about %d artifacts in %s' % (len(artifacts), scope_repo)),
            dict(at=ts(scanned_at + timedelta(minutes=3)), level='info',
                 message='%d scanned · %d unsupported · %d awaiting indexing'
                         % (cov['scanned'], cov['unsupported'], cov['not_scanned'])),
            dict(at=ts(scanned_at + timedelta(minutes=6)), level='info',
                 message='Stored %d findings across %d distinct problems'
                         % (sum(agg.values()), len(distinct))),
        ]
        c.execute("""INSERT INTO package_security
            (package_id, state, provider, repository, role, total, fixable,
             critical, high, medium, low, unknown,
             fix_critical, fix_high, fix_medium, fix_low, fix_unknown,
             distinct_total, distinct_cves,
             artifacts, scanned, not_scanned, unsupported, unavailable, disabled,
             scanned_at, synced_at, started_at, fingerprint, log)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
            (pkg_id, 'synced', 'jfrog-xray', scope_repo, 'target',
             sum(agg.values()), sum(aggfix.values()),
             agg['critical'], agg['high'], agg['medium'], agg['low'], agg['unknown'],
             aggfix['critical'], aggfix['high'], aggfix['medium'], aggfix['low'], aggfix['unknown'],
             # distinct is a set of (advisory, component) PAIRS; the advisories
             # alone are the other number, and the two are labelled for what
             # each counts. See pkg/apis/.../security.go.
             len(distinct), len({cve for cve, _ in distinct if cve}),
             cov['artifacts'], cov['scanned'], cov['not_scanned'],
             cov['unsupported'], cov['unavailable'], cov['disabled'],
             ts(scanned_at), ts(scanned_at + timedelta(minutes=6)), ts(scanned_at),
             hashlib.sha256(('%s%s' % (prod_name, tag)).encode()).hexdigest()[:32],
             json.dumps(log)))

c.commit()
print('sized      ', c.execute('SELECT count(*) FROM package_artifacts').fetchone()[0], 'artifacts')
print('scans      ', c.execute('SELECT count(*) FROM security_scans').fetchone()[0])
print('findings   ', c.execute('SELECT count(*) FROM security_findings').fetchone()[0])
print('postures   ', c.execute('SELECT count(*) FROM package_security').fetchone()[0])
for r in c.execute('SELECT p.tag, ps.total, ps.critical, ps.high, ps.distinct_total'
                   ' FROM package_security ps JOIN packages p ON p.id=ps.package_id'
                   ' ORDER BY p.product_id, p.published_at'):
    print('   %-16s total=%-4d critical=%-3d high=%-3d unique=%d' % r)
