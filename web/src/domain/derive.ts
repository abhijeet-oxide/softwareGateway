import type {
  Package, PackageTransfer, Product, Repository, Transfer, TransferState, SignatureStatus,
} from '../api/types'

/**
 * The vocabulary bridge (UI brief §5, docs/design/19 §3.1).
 *
 * The API has no `status` field and no `location` field. Both are DERIVED, and
 * the only way ten pages can agree about what a release's status is, is for
 * all ten to derive it here.
 *
 * The mapping is fixed, one term to one term:
 *
 *   Software        → Package
 *   Version         → tag, pinned to a manifest digest
 *   Location        → target repository
 *   Download        → TransferRequest and its Transfers
 *   Saved           → deduplication
 *
 * The engine's words — package, transfer, blob, wave, placement — do not
 * appear on screen, and none of them appear in any component that imports
 * from here.
 */

/** The six statuses from the brief, and no others. */
export type SoftwareStatus =
  | 'NEW'
  | 'DOWNLOADING'
  | 'DOWNLOADED'
  | 'READY FOR PRODUCTION'
  | 'PRODUCTION'
  | 'VERIFICATION FAILED'

/** The three verification states, stated in words as well as colour. */
export type VerificationState = 'SIGNED' | 'NOT_SIGNED' | 'VERIFICATION_FAILED' | 'UNKNOWN'

const LIVE_STATES: ReadonlySet<TransferState> = new Set<TransferState>([
  'PENDING', 'PLANNING', 'READY', 'RUNNING', 'PAUSED', 'VERIFYING', 'CANCELLING',
])

/** Whether a transfer is still doing something, and therefore worth polling. */
export function isLive(state: TransferState): boolean {
  return LIVE_STATES.has(state)
}

/** Verification, as three unmistakable states plus "nobody looked". */
export function verification(pkg: Pick<Package, 'signatureStatus'>): VerificationState {
  const status: SignatureStatus | undefined = pkg.signatureStatus
  if (status === 'SIGNED') return 'SIGNED'
  if (status === 'UNSIGNED') return 'NOT_SIGNED'
  // UNKNOWN is honest: a source whose layout does not attempt signature
  // discovery has not told us the release is unsigned, only that it did not
  // look. Those are different facts and must not collapse.
  //
  // VERIFICATION_FAILED is in the type and nothing returns it yet, on purpose.
  // The API carries a signature STATUS but no verification RESULT — nothing
  // writes verification records (see internal/store/reports.go) — so claiming
  // a release failed verification would be inventing an outcome. The state
  // exists so the badge, the status and the attention band are all written for
  // it now, and start working the day the result is served.
  return 'UNKNOWN'
}

/** Names the targets that represent production, from the product's config. */
export function productionTargets(product: Product | undefined): Set<string> {
  const names = new Set<string>()
  for (const t of product?.targets ?? []) {
    if (t.environment === 'production') names.add(t.name)
  }
  return names
}

/**
 * The status of one release.
 *
 * Order matters: a verification failure outranks everything, because it is the
 * one state that should stop somebody doing the next thing. After that,
 * anything in flight beats anything settled — a release that reached
 * production and is being copied somewhere else is still downloading.
 */
export function deriveStatus(pkg: Package, product?: Product): SoftwareStatus {
  if (verification(pkg) === 'VERIFICATION_FAILED') return 'VERIFICATION FAILED'

  const transfers = pkg.transfers ?? []
  if (transfers.some((t) => isLive(t.state))) return 'DOWNLOADING'

  const succeeded = transfers.filter((t) => t.state === 'SUCCEEDED')
  if (succeeded.length === 0) {
    // No successful transfer anywhere. The package's own state can still say
    // it is mid-flight, on a listing where transfers were not expanded.
    if (pkg.state === 'TRANSFERRING' || pkg.state === 'QUEUED') return 'DOWNLOADING'
    if (pkg.state === 'FAILED') return 'VERIFICATION FAILED'
    return 'NEW'
  }

  const production = productionTargets(product)
  if (succeeded.some((t) => production.has(t.target))) return 'PRODUCTION'

  // Downloaded somewhere, and there is a production target it has not reached.
  return production.size > 0 ? 'READY FOR PRODUCTION' : 'DOWNLOADED'
}

/** One place a release exists. */
export interface Location {
  /** The name shown on the chip. */
  name: string
  /** vendor | storage | mirror | production — picks the icon. */
  kind: 'vendor' | 'storage' | 'mirror' | 'production'
  /** The external URL, where one can be built. */
  url?: string
}

const MIRROR_HINTS = ['quay', 'mirror']

function targetKind(target: Repository): Location['kind'] {
  if (target.environment === 'production') return 'production'
  if (target.type === 'quay') return 'mirror'
  if (MIRROR_HINTS.some((h) => target.name.toLowerCase().includes(h))) return 'mirror'
  return 'storage'
}

/**
 * Where a release currently is, as a chain.
 *
 * Reads `Vendor: Nokia` before anything has been downloaded, and
 * `JFrog + Quay` once it has. The vendor is always the first location — the
 * release does not stop existing there once it has been copied.
 */
export function deriveLocations(pkg: Package, product?: Product): Location[] {
  const out: Location[] = []

  const vendor = product?.sources?.[0]
  out.push({
    name: vendor?.vendor
      ? titleCase(vendor.vendor)
      : (vendor?.name ?? 'Vendor'),
    kind: 'vendor',
    url: repositoryUrl(vendor, pkg.sourceRepository),
  })

  const targetsByName = new Map((product?.targets ?? []).map((t) => [t.name, t]))
  for (const transfer of pkg.transfers ?? []) {
    if (transfer.state !== 'SUCCEEDED') continue
    const target = targetsByName.get(transfer.target)
    if (!target) {
      out.push({ name: transfer.target, kind: 'storage' })
      continue
    }
    out.push({
      name: target.name,
      kind: targetKind(target),
      url: repositoryUrl(target, target.repository),
    })
  }
  return out
}

/**
 * An external repository URL, where one can be built honestly.
 *
 * Returns undefined rather than guessing: a link that 404s is worse than no
 * link, and the brief requires every repository URL shown to actually open.
 */
export function repositoryUrl(repo: Repository | undefined, repository?: string): string | undefined {
  if (!repo?.registry) return undefined
  const host = repo.registry.replace(/^https?:\/\//, '')
  const path = repository ?? repo.repository
  if (!path) return `https://${host}`
  return `https://${host}/${path}`
}

/** The lifecycle stages, in the order the brief fixes them. */
export type LifecycleStage = 'Vendor' | 'Downloading' | 'Downloaded' | 'Production'

export interface LifecycleStep {
  stage: LifecycleStage
  reached: boolean
  current: boolean
  /** When this stage was reached. Absent where we cannot say. */
  at?: string
}

/** `Vendor → Downloading → Downloaded → Production`, with timestamps. */
export function deriveLifecycle(pkg: Package, product?: Product): LifecycleStep[] {
  const status = deriveStatus(pkg, product)
  const transfers = pkg.transfers ?? []
  const live = transfers.find((t) => isLive(t.state))
  const succeeded = transfers.filter((t) => t.state === 'SUCCEEDED')
  const production = productionTargets(product)
  const inProduction = succeeded.find((t) => production.has(t.target))

  const downloadedAt = succeeded
    .map((t) => t.completedAt)
    .filter((t): t is string => Boolean(t))
    .sort()[0]

  const reachedDownloading = status !== 'NEW'
  const reachedDownloaded = succeeded.length > 0
  const reachedProduction = Boolean(inProduction)

  return [
    {
      stage: 'Vendor',
      reached: true,
      current: status === 'NEW',
      at: pkg.publishedAt || pkg.discoveredAt,
    },
    {
      stage: 'Downloading',
      reached: reachedDownloading,
      current: status === 'DOWNLOADING',
      at: live?.createdAt,
    },
    {
      stage: 'Downloaded',
      reached: reachedDownloaded,
      current: status === 'DOWNLOADED' || status === 'READY FOR PRODUCTION',
      at: downloadedAt,
    },
    {
      stage: 'Production',
      reached: reachedProduction,
      current: status === 'PRODUCTION',
      at: inProduction?.completedAt,
    },
  ]
}

/**
 * How long a release's download actually took, in seconds.
 *
 * Live elapsed while one is running, the measured total once it has finished,
 * and undefined for a release nobody has downloaded — which renders as `—`,
 * never as zero.
 */
export function downloadSeconds(pkg: Package): number | undefined {
  const transfers = pkg.transfers ?? []
  const live = transfers.find((t) => isLive(t.state))
  if (live?.createdAt) {
    return (Date.now() - Date.parse(live.createdAt)) / 1000
  }

  const done = transfers.filter((t) => t.state === 'SUCCEEDED' && t.createdAt && t.completedAt)
  if (done.length === 0) return undefined

  // Grouped by the REQUEST that asked for the work, because one download fans
  // out to several targets and a promotion performed the next morning is a
  // different operation entirely. Spanning both would report a two-hour
  // download as fifteen hours — measuring how long somebody waited before
  // promoting, not how long the download took.
  const byRequest = new Map<string, Attempt[]>()
  for (const t of done as Attempt[]) {
    const key = t.requestId ?? t.id
    const existing = byRequest.get(key)
    if (existing) existing.push(t)
    else byRequest.set(key, [t])
  }

  // The FIRST request is the download; anything later is a promotion or a
  // re-run. Within it, wall clock across its targets rather than the sum of
  // them, because they overlap.
  const groups = [...byRequest.values()].sort(
    (a, b) => Date.parse(a[0]!.createdAt!) - Date.parse(b[0]!.createdAt!))
  const first = groups[0]!
  const starts = first.map((t) => Date.parse(t.createdAt!))
  const ends = first.map((t) => Date.parse(t.completedAt!))
  return (Math.max(...ends) - Math.min(...starts)) / 1000
}

/**
 * The version to show. The vendor's own shortened spelling where there is one,
 * because it is what the user has in their hand.
 */
export function version(pkg: Pick<Package, 'tag' | 'displayTag'>): string {
  return pkg.displayTag || pkg.tag
}

/**
 * One attempt, carrying the request that asked for it.
 *
 * A superset of the API's PackageTransfer: the single-package read omits
 * `requestId`, and the transfer listing supplies it. It matters because one
 * download request fans out to several targets, and telling that apart from a
 * PROMOTION performed the next morning is the difference between reporting a
 * download as two hours and reporting it as fifteen.
 */
export interface Attempt extends PackageTransfer {
  requestId?: string
}

/**
 * An index of what has been attempted, keyed by product and version.
 *
 * # Why this exists
 *
 * `package.transfers` is populated on the SINGLE-package read only — a page of
 * fifty packages would be fifty extra queries to render one column, so the API
 * deliberately omits it from listings.
 *
 * That means a LISTING cannot derive a status on its own: every row would read
 * NEW, including releases that reached production. The fix is not to fetch each
 * package individually — it is to read the transfer listing, which the pages
 * already fetch, and join the two by product and version.
 */
export function transferIndex(transfers: Transfer[]): Map<string, Attempt[]> {
  const index = new Map<string, Attempt[]>()
  for (const t of transfers) {
    // The identity is the raw tag; displayTag is cosmetic and may be absent on
    // one side of the join.
    const key = `${t.product}\u0000${t.tag}`
    const entry: Attempt = {
      id: t.id,
      requestId: t.requestId,
      target: t.targetName || t.target,
      state: t.state,
      failureReason: t.failureReason,
      createdAt: t.createdAt,
      completedAt: t.completedAt,
    }
    const existing = index.get(key)
    if (existing) existing.push(entry)
    else index.set(key, [entry])
  }
  return index
}

/**
 * Fills in a listed package's transfer history from the index above.
 *
 * Returns the package unchanged when it already carries its own transfers —
 * the single-package read is authoritative and must not be overwritten by a
 * listing that may be paginated.
 */
export function withTransfers(pkg: Package, index: Map<string, Attempt[]>): Package {
  if (pkg.transfers?.length) return pkg
  const found = index.get(`${pkg.product}\u0000${pkg.tag}`)
  return found ? { ...pkg, transfers: found } : pkg
}

/**
 * The address of one release in this interface.
 *
 * # Why the repository has to be carried
 *
 * A vendor's version tag is NOT unique within a product. NEAR publishes
 * `orb_25.7_mp2604_2131` into every one of a product's ten repositories, so a
 * link that names only the tag asks the API a question with ten answers and
 * gets an ambiguity error back instead of a release.
 *
 * The repository rides as a query parameter rather than inside the path
 * segment because it contains slashes — `orbs/cfx-5000-k8s` — and a slash
 * cannot survive a path segment: %2F is decoded before routing, so the router
 * would see two segments and match neither.
 */
export function releaseHref(
  product: string,
  pkg: Pick<Package, 'tag' | 'sourceRepository'>,
): string {
  const base = `/software/${encodeURIComponent(product)}/${encodeURIComponent(pkg.tag)}`
  return pkg.sourceRepository
    ? `${base}?repository=${encodeURIComponent(pkg.sourceRepository)}`
    : base
}

/** The transfer equivalent, for the download pages. */
export function transferVersion(t: Pick<Transfer, 'tag' | 'displayTag'>): string {
  return t.displayTag || t.tag
}

function titleCase(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1)
}

/**
 * The word a person uses for one kind of component.
 *
 * The API speaks the OCI vocabulary — image, chart, file, index, signature —
 * and it speaks the SAME vocabulary on a release page and in a transfer's
 * breakdown. Both must therefore render it the same way. The download page
 * printed the raw token, so a release shown as 160 Images and 97 Helm Charts
 * read as `image` and `chart` one screen later, in a table with no icons.
 *
 * Plural, because these name a count of things rather than one thing.
 */
export function kindName(kind: string): string {
  switch (kind) {
    case 'image': return 'Images'
    case 'chart': return 'Helm Charts'
    case 'file': return 'Files'
    case 'index': return 'Index'
    case 'signature': return 'Signatures'
    case 'artifact': return 'Other'
    // A kind this client has not been taught. Shown as the API named it
    // rather than hidden — an unnamed component still moved.
    default: return kind ? titleCase(kind) : 'Other'
  }
}
