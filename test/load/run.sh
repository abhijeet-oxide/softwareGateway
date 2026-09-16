#!/usr/bin/env bash
# Load-test the read path: start a Coordinator, run k6 against it, stop it.
#
#   test/load/run.sh              a fresh empty estate
#   test/load/run.sh --keep       reuse dev/bench.db, so a seeded estate is
#                                 measured rather than an empty one
#
# k6 is fetched to bin/ on first use - a single static binary, no runtime and
# nothing installed system-wide.
#
#   K6=/usr/local/bin/k6         use one that is already installed
#   K6_BASE_URL=https://mirror   fetch from somewhere other than GitHub
#
# The second is there for the same reason ci.yml takes POSTGRES_IMAGE from a
# repository variable: a self-hosted runner behind the corporate proxy cannot
# be assumed to reach GitHub releases.
#
# WHAT AN EMPTY ESTATE PROVES, AND WHAT IT DOES NOT. With no transfers in the
# database the listing's aggregates have no jobs to read, so this measures the
# per-request floor: the routing, the authorization, the projection's own cost.
# That is a real regression signal and it is stable enough to gate on. It is
# NOT the number an operator feels, which is what the same page costs once the
# estate has half a million job rows in it - use --keep against a seeded
# database for that, and see docs/design/32-performance.md.
set -euo pipefail
cd "$(dirname "$0")/../.."
ROOT=$PWD

K6_VERSION=${K6_VERSION:-v0.54.0}
PORT=${PORT:-8099}
DB=${DB:-$ROOT/dev/bench.db}
KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true

mkdir -p bin dev

# ---------------------------------------------------------------------- k6 --
K6=${K6:-}
if [ -z "$K6" ]; then
  K6=$ROOT/bin/k6
  if [ ! -x "$K6" ]; then
    arch=$(uname -m); case "$arch" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; esac
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    pkg="k6-${K6_VERSION}-${os}-${arch}"
    echo "==> fetching $pkg"
    tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
    base=${K6_BASE_URL:-https://github.com/grafana/k6/releases/download}
    curl -fsSL "${base}/${K6_VERSION}/${pkg}.tar.gz" | tar xz -C "$tmp"
    mv "$tmp/$pkg/k6" "$K6"
    chmod +x "$K6"
  fi
fi
echo "==> $("$K6" version)"

# ------------------------------------------------------------- coordinator --
$KEEP || rm -f "$DB" "$DB"-wal "$DB"-shm

if [ ! -x ./bin/coordinator ]; then
  echo "==> building coordinator"
  go build -o ./bin/coordinator ./cmd/coordinator
fi

echo "==> starting Coordinator on :$PORT"
# AUTHENTICATION OFF, deliberately. A benchmark that spends its time in an
# OIDC round trip measures the identity provider. The authorization middleware
# still runs - it is part of what every request costs - and `config/config.yaml`
# already has authentication disabled for local use.
SWGW_OBSERVABILITY_LOG_FORMAT=text \
SWGW_TLS_ALLOWNEGATIVESERIALNUMBERS=true \
SWGW_SERVER_ADDRESS=":$PORT" \
SWGW_DATABASE_DRIVER=sqlite \
SWGW_DATABASE_DSN="$DB" \
  ./bin/coordinator --config ./config/config.yaml > dev/bench-coordinator.log 2>&1 &
COORD=$!
cleanup() {
  kill "$COORD" 2>/dev/null || true
  wait "$COORD" 2>/dev/null || true
}
trap cleanup EXIT

# Wait for it rather than sleeping: migrations take a moment on a fresh file,
# and a fixed sleep is either slower than it needs to be or occasionally short.
for _ in $(seq 1 100); do
  if curl -fsS -o /dev/null "http://localhost:$PORT/healthz" 2>/dev/null; then
    ready=yes; break
  fi
  sleep 0.2
done
if [ "${ready:-}" != yes ]; then
  echo "coordinator did not come up; last log lines:" >&2
  tail -20 dev/bench-coordinator.log >&2
  exit 1
fi

# --------------------------------------------------------------------- run --
echo "==> k6"
BASE_URL="http://localhost:$PORT" "$K6" run \
  --summary-export dev/bench-summary.json \
  "$ROOT/test/load/api.js"
