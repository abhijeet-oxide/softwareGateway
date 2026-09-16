// What the interface asks for, under load, with the thresholds CI holds it to.
//
// Run it with k6 (https://k6.io) - a single static binary, no runtime, no
// agent. `task bench` fetches it, starts a Coordinator and runs this file.
//
// # What this measures, and what it deliberately does not
//
// The READ PATH: the eight endpoints the interface polls while somebody
// watches a download. Those are what a user experiences as the software being
// quick or slow, and they are the ones that degrade first when the estate
// grows, because several of them project aggregates over `jobs`.
//
// It does NOT measure transfer throughput - bytes moved per second is a
// property of the registries and the network, not of this process, and a load
// generator pointed at the API cannot see it. `internal/store` carries the
// queue's own concurrency tests for the part that is ours.
//
// # Why the thresholds are loose
//
// These are wall-clock numbers on whatever runner GitHub gives us, sharing
// four vCPUs with k6 itself. A threshold tight enough to catch a ten percent
// drift would fail on a noisy box, and a benchmark that cries wolf is one that
// gets removed. So the budgets below sit roughly an order of magnitude above
// what the code does today: they catch a projection that started reading the
// whole table, not a regression of a few percent.
//
// The numbers to WATCH rather than gate on are printed in the summary and
// belong in the pull request that moves them. See test/load/README.md.
import http from 'k6/http'
import { check } from 'k6'
import { Trend } from 'k6/metrics'

const BASE = __ENV.BASE_URL || 'http://localhost:8080'
const API = `${BASE}/api/v1`

// CALIBRATION. /healthz touches no database and no product configuration, so
// it measures the runner rather than this code. A listing that is slow BECAUSE
// the box is slow moves both numbers; one that is slow because its query got
// worse moves only its own. The summary prints the ratio for that reason.
const calibration = new Trend('calibration_healthz', true)

// The listings, each on its own trend so a regression names the endpoint
// rather than the average of eight.
const listing = new Trend('listing_duration', true)

/*
  THE POLLED READS, and what each one costs the interface.

  `weight` is how often the interface asks for it relative to the others, taken
  from web/src/api/queries.ts: the Downloads page polls transfers every five
  seconds while anything is live, the shell polls the activity line every ten
  on every page, and the configuration reads are five-minute cached.

  A uniform mix would flatter the result by spending most of its requests on
  the cheap endpoints nobody waits for.
*/
const READS = [
  { path: '/transfers?pageSize=25&operation=replicate', weight: 5, name: 'transfers' },
  { path: '/transfers:activity', weight: 4, name: 'activity' },
  { path: '/packages?pageSize=25', weight: 3, name: 'packages' },
  { path: '/workers', weight: 2, name: 'workers' },
  { path: '/discovery', weight: 2, name: 'discovery' },
  { path: '/products', weight: 1, name: 'products' },
  { path: '/whoami', weight: 1, name: 'whoami' },
]

// Expanded once, so the hot loop indexes an array rather than walking weights.
const MIX = READS.flatMap((r) => Array(r.weight).fill(r))

export const options = {
  scenarios: {
    // ONE USER, to get the latency a person actually sees. Everything below
    // this is queueing, and queueing is reported separately.
    alone: {
      executor: 'constant-vus',
      vus: 1,
      duration: '10s',
      tags: { phase: 'alone' },
    },
    // TWENTY, which is a team watching a release go out. This is the number
    // the thresholds are written against.
    team: {
      executor: 'constant-vus',
      vus: 20,
      duration: '20s',
      startTime: '10s',
      tags: { phase: 'team' },
    },
  },
  thresholds: {
    // Nothing may fail. A 5xx under load is a bug whatever the latency is.
    http_req_failed: ['rate==0'],
    // An order of magnitude above today's numbers, for the reason in the
    // header comment. Today: p95 ~55ms at twenty users on an empty estate.
    'listing_duration{phase:team}': ['p(95)<2000'],
    // A single reader waits for nothing. Today: ~2ms.
    'listing_duration{phase:alone}': ['p(95)<500'],
  },
  summaryTrendStats: ['avg', 'p(95)', 'p(99)', 'max'],
}

export default function () {
  const r = MIX[Math.floor(Math.random() * MIX.length)]
  const res = http.get(`${API}${r.path}`, {
    tags: { name: r.name },
    // A benchmark that follows a redirect to a login page measures the login
    // page. `task bench` runs with authentication disabled; anything else is
    // a misconfiguration worth failing on.
    redirects: 0,
  })
  listing.add(res.timings.duration, { name: r.name })
  check(res, { 'status is 200': (x) => x.status === 200 })
}

// The calibration probe, once per iteration of its own tiny scenario, so the
// summary can say whether a slow run was a slow box.
export function healthz() {
  const res = http.get(`${BASE}/healthz`)
  calibration.add(res.timings.duration)
  check(res, { 'healthz is 200': (x) => x.status === 200 })
}

options.scenarios.calibrate = {
  executor: 'constant-vus',
  vus: 1,
  duration: '10s',
  exec: 'healthz',
  tags: { phase: 'calibrate' },
}
