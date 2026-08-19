# Software Gateway - web interface

The browser client for the Coordinator API.

Ten pages, eight of them navigable:

```
Overview · Products · Software · Downloads · Repositories · Activity · Reports · Settings
```

with **Software detail** and **Download** reached from them as drill-downs. The
lifecycle they exist to make obvious is
**Discover → Review → Verify → Download & Replicate → Compare → Promote**.

## Running it

The Coordinator installs no CORS middleware, so the browser must reach the API
on its own origin. In development Vite proxies it; in production the bundle is
served from the same origin as the API.

```sh
# terminal 1 - the API
task dev:coordinator

# terminal 2 - the interface, proxying /api to localhost:8080
cd web && npm install && npm run dev
```

Point it at a different Coordinator with `COORDINATOR_URL=http://host:8080 npm run dev`.

```sh
npm run build       # static bundle in dist/
npm run typecheck
```

## How it is put together

| Path | What lives there |
|---|---|
| `src/api/types.ts` | TypeScript mirror of `pkg/apis/softwaregateway/v1/types.go`. **The contract** - nothing else invents a field name. |
| `src/api/client.ts` | Fetch wrapper mirroring `v1/client.go`: paging, RFC 9457 problem details. |
| `src/api/queries.ts` | Every server call, with its polling policy. |
| `src/domain/derive.ts` | The vocabulary bridge - status, location, lifecycle. |
| `src/domain/format.ts` | Sizes, durations, speeds, timestamps. Returns `null` when a value is unavailable. |
| `src/components/value.tsx` | `<Value>` / `<NA>` / `<Stat>` - how the application says "we do not have this". |
| `src/components/discovery.tsx` | The discovery panel and the one Run Discovery control. |
| `src/components/icons.tsx` | Every icon, chosen once - brand marks for vendors and registries, ecosystem marks for artifact kinds. |
| `src/auth/permissions.tsx` | `useCan(action, scope)`. |
| `src/components/` | The reusable vocabulary: chips, badges, progress, page furniture. |

### Three rules the code enforces rather than documents

**1. No number we cannot defend.** `src/components/progress.tsx` exports two
components that are not interchangeable. `<MeasuredProgress>` takes real byte
counts and is the only thing that may render a bar, a percentage, a speed or an
ETA - and it *refuses to render* for a transfer whose `strategy` is not `copy`.
`<StateStrip>` takes timestamps and cannot be styled into looking like a bar.

For the JFrog step we move the bytes, so we count them. For the Quay step, Quay
pulls the content itself once we have configured it: we can see *that* a sync
started, *that* it finished and *what* it produced, but not how many bytes
moved. A progress bar there would be a number derived from a timer, and someone
would make a decision from it. See `docs/design/18-quay-replication.md` §6.1.

The same rule is why a value we do not have renders as an italic secondary
`N/A` - with the reason on hover - and never as `0`. Every formatter in
`domain/format.ts` returns `string | null`, and `<Value>` turns the `null` into
that one treatment, so a site that forgets the absent case does not typecheck.

**2. Status and location are derived in exactly one place.** The API has no
`status` field and no `location` field. `deriveStatus`, `deriveLocations` and
`deriveLifecycle` in `src/domain/derive.ts` are the only implementations, so ten
pages cannot disagree about whether a release reached production.

One consequence worth knowing: `package.transfers` is populated on the
*single-package* read only - a page of fifty packages would be fifty extra
queries - so listings join it in from the transfer listing via `transferIndex`.
Without that join every row reads `NEW`.

**3. Icons are compiled in, not fetched.** Iconify's runtime resolves unknown
icons over the network from `api.iconify.design`, which in an air-gapped
deployment means every icon silently fails. `unplugin-icons` turns
`~icons/simple-icons/nokia` into an inline SVG component at build time instead,
so only the icons actually used are bundled and nothing is requested at
runtime.

`icons.tsx` picks them from what a thing IS, in order of reliability: the
vendor it declares, then the registry protocol, then the environment, and only
then its name - which an operator can rename on a whim. Container images carry
Docker's mark and charts carry Helm's, because those are what a Product Owner
recognises from every other tool they use.

**4. An action always reports what it did.** `POST /products:discover` answers
`{started, alreadyRunning}`, and "nothing started because four sources were
already scanning" is a real outcome, not a no-op. Discarding it is what made
Run Discovery look broken. There is one `<RunDiscoveryButton>` for that reason:
a second copy would eventually be a bare `mutate()` with no feedback.

Measuring a release is the same rule with a different shape. It is synchronous
and has no progress feed - the size of the manifest tree is the thing being
discovered, so there is no denominator until the work is done - so it gets
`<WorkingBar>`: an animated stripe that travels rather than fills, plus elapsed
time. It states no position, because there is none to state. Measured against
an unreachable registry the call runs past ninety seconds, so the wording
escalates rather than repeating itself.

Discovery is started with `wait: false` and its progress is polled from
`GET /products/{p}/discovery`, which reports phase, repositories, tags,
artifacts, new packages and errors per source. The default holds the HTTP
request open for the whole scan - minutes against a slow registry, with every
intermediary's idle timeout becoming part of the control plane.

**5. The interface never edits configuration.** Products, downloads, rules,
intervals and verification policy come from Git and are reconciled by Flux. A
write from here would be a second source of truth that gets silently reverted
minutes later. Configuration is shown with a `Managed in Git` badge; requesting
work - downloads, promotions, syncs - is not configuration and is fully
available. See `docs/design/19-user-interface.md` §4.

## Permissions

Every mutating control asks `useCan(action, scope)` before it renders as
enabled. Today `/api/v1/whoami` answers `anonymous` with permissions `["*"]`, so
nothing is hidden - the point is that the question is already being asked, so
switching authentication on changes what people can do without changing any
page.

The other half of that rule lives on the server: **the interface never removes a
row for authorization reasons.** Doing so would break pagination and leak the
shape of what it hid. It gates actions; the server filters data.

## Performance

- Server-side pagination and filtering. Nothing is aggregated here that the API
  can aggregate.
- Polling is scoped: a transfer is re-fetched only while it is in a live state,
  and stops the moment it settles. There is no server-sent-events channel yet
  (gate G4 in `docs/design/19` §5).
- Route-level code splitting. The date picker and its date library load only
  with the Activity page.
- Air-gapped by construction: no CDN, no external fonts, no analytics.
