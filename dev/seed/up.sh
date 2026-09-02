#!/usr/bin/env bash
# Bring up a complete local deployment with data in it.
#
#   dev/seed/up.sh            fresh database, discovery, transfers, dressing
#   dev/seed/up.sh --keep     leave the existing database alone
#
# Three processes: the fake vendor registries, the Coordinator and one Worker.
# Everything they need is in dev/ - no Docker, no Postgres, no cluster.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$PWD
BIN=${BIN:-/tmp/swgw-dev}
mkdir -p "$BIN"

# The development registries listen on hostnames the products name, so /etc/hosts
# has to resolve them locally. Without this every scan is a Bad Gateway.
for h in registry.mavenir.example.com registry.ericsson.example.com \
         registry.nokia.example.com registry.near.example.com \
         artifactory.internal.example.com; do
  grep -q "$h" /etc/hosts || echo "127.0.0.1 $h" >> /etc/hosts
done

# dev/products/ is gitignored - it is where a developer's own product
# configuration lives - so the demo estate ships beside it and is copied in only
# when there is nothing there to overwrite.
if [ -z "$(ls -A dev/products 2>/dev/null)" ]; then
  echo "==> installing the example products"
  mkdir -p dev/products
  cp dev/products.example/*.yaml dev/products/
fi

echo "==> building"
go build -o "$BIN/coordinator" ./cmd/coordinator
go build -o "$BIN/worker"      ./cmd/worker
go build -o "$BIN/fakeregistry" ./dev/fakeregistry

echo "==> stopping anything already running"
pkill -9 -x coordinator  2>/dev/null || true
pkill -9 -x worker       2>/dev/null || true
pkill -9 -x fakeregistry 2>/dev/null || true
sleep 1

[ "${1:-}" = "--keep" ] || rm -f dev/swgw.db*

# The agent sandbox injects an HTTPS proxy that cannot reach a loopback
# listener, so the two services that talk to the registries run without it.
run() {
  name=$1; shift
  env -u HTTPS_PROXY -u https_proxy -u HTTP_PROXY -u http_proxy \
      NO_PROXY='*' no_proxy='*' setsid nohup "$@" > "/tmp/$name.log" 2>&1 < /dev/null &
}

echo "==> starting registries"
run fakeregistry "$BIN/fakeregistry"
sleep 2

echo "==> starting coordinator and worker"
run coordinator "$BIN/coordinator" --config ./dev/config.yaml
sleep 6
run worker "$BIN/worker" --config ./dev/config.yaml

echo "==> waiting for discovery"
for _ in $(seq 1 40); do
  n=$(python3 -c "import sqlite3;print(sqlite3.connect('dev/swgw.db').execute('select count(*) from packages').fetchone()[0])" 2>/dev/null || echo 0)
  [ "$n" -ge 12 ] && break
  sleep 3
done
echo "    $n packages discovered"

echo "==> requesting the downloads discovery did not trigger"
for p in ericsson-ran nokia-cmm; do
  for t in $(python3 - "$p" <<'PY'
import sqlite3, sys
c = sqlite3.connect('dev/swgw.db')
pid = c.execute('SELECT id FROM products WHERE name=? AND active=1', (sys.argv[1],)).fetchone()[0]
rows = list(c.execute('SELECT tag FROM packages WHERE product_id=? ORDER BY published_at', (pid,)))
print(' '.join(r[0] for r in rows[:-1]))
PY
  ); do
    curl -s -o /dev/null -X POST localhost:8080/api/v1/transfers \
      -H 'content-type: application/json' \
      -d "{\"product\":\"$p\",\"package\":\"$t\",\"to\":[\"jfrog-lab\"]}"
  done
done

echo "==> waiting for transfers"
for _ in $(seq 1 40); do
  pending=$(python3 -c "import sqlite3;print(sqlite3.connect('dev/swgw.db').execute(\"select count(*) from transfers where state not in ('succeeded','failed','skipped')\").fetchone()[0])" 2>/dev/null || echo 1)
  [ "$pending" = 0 ] && break
  sleep 3
done
python3 -c "
import sqlite3
c=sqlite3.connect('dev/swgw.db')
print('   transfers', dict(c.execute('select state,count(*) from transfers group by state')))"

echo "==> dressing in scanner findings and signatures"
python3 dev/seed/dress.py dev/swgw.db

# The release comparison reads the registry LIVE rather than the database, so it
# would report the kilobytes that actually moved while every other page reports
# the scaled size. Restarting the registry with -inflate makes it declare the
# same sizes dress.py wrote. Both derive them from the component name, so they
# agree. See the comment on `inflate` in dev/fakeregistry.
echo "==> restarting the registries with realistic sizes"
pkill -9 -x fakeregistry 2>/dev/null || true
sleep 1
run fakeregistry "$BIN/fakeregistry" -inflate
sleep 2

echo
echo "Coordinator  http://localhost:8080"
echo "Interface    cd web && pnpm dev   (http://localhost:5173)"
echo
echo "The registries now DECLARE sizes they do not store, so a new transfer will"
echo "fail. Re-run this script to move bytes again."
