/**
 * TypeScript mirror of pkg/apis/softwaregateway/v1/types.go.
 *
 * This file is the contract. Nothing else in the application invents a field
 * name, and a field that is optional in Go is optional here — an absent value
 * is load-bearing throughout this API, because "not measured" and "zero" are
 * deliberately different answers.
 *
 * Organised in the same order as the Go file so the two can be read side by
 * side.
 */

/**
 * A 64-bit quantity carried as a string (AIP-141). JSON numbers are doubles
 * and lose precision above 2^53; byte counts here already reach 10^11.
 *
 * Never do arithmetic on this directly — use `bytes()` from format.ts, which
 * returns a number, or `BigInt` where the total could overflow.
 */
export type Int64String = string

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

export type PackageState =
  | 'DISCOVERED' | 'QUEUED' | 'TRANSFERRING' | 'TRANSFERRED'
  | 'VERIFYING' | 'VERIFIED' | 'FAILED' | 'SUPERSEDED'

export type TransferState =
  | 'PENDING' | 'PLANNING' | 'READY' | 'RUNNING' | 'PAUSED'
  | 'VERIFYING' | 'SUCCEEDED' | 'FAILED' | 'CANCELLING' | 'CANCELLED'

export type JobState =
  | 'BLOCKED' | 'PENDING' | 'LEASED' | 'SUCCEEDED'
  | 'SKIPPED' | 'FAILED' | 'CANCELLED'

/**
 * Three values, and the third is the one that matters: "we looked and found
 * none" and "nobody looked" are completely different facts when the question
 * is whether to trust something.
 */
export type SignatureStatus = 'UNKNOWN' | 'SIGNED' | 'UNSIGNED'

/** How a transfer was performed. Anything but `copy` moved bytes we did not count. */
export type Strategy = 'copy' | 'mirror' | 'proxy'

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

export interface Filters { include?: string[]; exclude?: string[] }

export interface Discovery {
  enabled: boolean
  intervalSeconds: number
  includePatterns?: string[]
  excludePatterns?: string[]
}

export interface Concurrency { perRegistry: number; requestsPerSecond?: number }

/** A source or target. Credentials are never included, in any form. */
export interface Repository {
  name: string
  enabled: boolean
  registry: string
  repository?: string
  repositories?: string[]
  repositoryDiscovery?: boolean
  repositoryFilters?: Filters
  type: string
  /** `lab`, `production`. Targets only — the only thing that marks production. */
  environment?: string
  vendor?: string
  role: string
  default?: boolean
  promotionOnly?: boolean
  discovery?: Discovery
  concurrency: Concurrency
}

export interface AutoDownloadRule {
  name: string
  tagPattern: string
  targets?: string[]
  priority: number
}

export interface AutoDownloadSummary { enabled: boolean; rules?: AutoDownloadRule[] }

export interface VerificationSummary {
  enabled: boolean
  policy?: string
  mode?: string
  atSource: boolean
  atDestination: boolean
}

export interface Product {
  name: string
  productId: string
  displayName?: string
  description?: string
  owner?: string
  labels?: Record<string, string>
  enabled: boolean
  sources: Repository[]
  targets: Repository[]
  autoDownload: AutoDownloadSummary
  verification: VerificationSummary
  configHash: string
}

export interface ListProductsResponse { products: Product[]; nextPageToken?: string }

// ---------------------------------------------------------------------------
// Packages — "Software" on screen
// ---------------------------------------------------------------------------

export interface RelatedArtifact {
  role: string
  digest: string
  tag?: string
  mediaType?: string
  sizeBytes?: Int64String
  blobDigest?: string
  blobMediaType?: string
  blobSize?: Int64String
  annotations?: Record<string, string>
  resolvedAt?: string
}

/** One attempt to move a package to one destination. */
export interface PackageTransfer {
  id: string
  target: string
  state: TransferState
  failureReason?: string
  createdAt?: string
  completedAt?: string
}

export interface Package {
  name: string
  packageId: string
  product: string
  tag: string
  manifestDigest: string
  mediaType: string
  /**
   * ABSENT, not zero, until the manifest tree has been walked. A wrong size is
   * worse than a missing one, because nobody questions a number.
   */
  totalBytes?: Int64String
  artifactCount: number
  blobCount?: number
  state: PackageState
  /** When WE first saw it. An observation. */
  discoveredAt: string
  /** When the VENDOR says it was built. A claim, and often absent. */
  publishedAt?: string
  supersededBy?: string
  accessoryOf?: string
  sourceRepository?: string
  displayRepository?: string
  displayTag?: string
  expandedAt?: string
  signatureStatus?: SignatureStatus
  related?: RelatedArtifact[]
  /** Present on the single-package read only. Where transfer history lives. */
  transfers?: PackageTransfer[]
  transferRootTag?: string
}

export interface ListPackagesResponse { packages: Package[]; nextPageToken?: string }

/** What an artifact IS, as the API classifies it. A bounded set. */
export type ArtifactKind =
  | 'index' | 'image' | 'chart' | 'file' | 'signature' | 'artifact'

export interface Artifact {
  artifactId: string
  parentId?: string
  digest: string
  mediaType: string
  artifactType?: string
  /**
   * Derived server-side from the OCI fields and, where the source declares a
   * vendor layout, from the vendor's own annotations.
   *
   * Group on THIS. Classifying in the client would mean the client knowing a
   * vendor's annotation keys, and it would get charts wrong: an index's
   * children are recorded from what the index listed, so their config media
   * type — the field that separates a Helm chart from an image — is not
   * available until the tree is walked.
   */
  kind?: ArtifactKind
  sizeBytes: Int64String
  platform?: string
  depth?: number
  annotations?: Record<string, string>
  /**
   * Whether this manifest was ever pulled and verified. False for a child the
   * index merely listed — which is every child until the release is analysed,
   * and is why its size and contents are not known.
   */
  fetched?: boolean
  /** Whether the manifest BYTES are still held locally. */
  cached?: boolean
}

/**
 * What measuring a release found.
 *
 * `fetched` is manifests this call pulled from the registry; zero with
 * `alreadyExpanded` means the tree was already recorded and the vendor was not
 * troubled again. `cachedManifests` is how many bodies are still held locally
 * out of `artifacts` — an evictable cache, not part of the record.
 */
export interface InspectPackageResponse {
  package: Package
  fetched: number
  alreadyExpanded: boolean
  artifacts: number
  blobs: number
  totalBytes: Int64String
  cachedManifests: number
  cachedBytes?: Int64String
  signatureResolved?: number
}

export interface ListArtifactsResponse {
  artifacts: Artifact[]
  nextPageToken?: string
}

// ---------------------------------------------------------------------------
// Transfers — "Download" on screen
// ---------------------------------------------------------------------------

export interface SkipBreakdown { reason: string; jobs: number; bytes?: Int64String }

/** What a transfer is made OF, and how each kind went. */
export interface ContentGroup {
  kind: string
  total: number
  copied: number
  present: number
  failed?: number
  outstanding?: number
}

export interface TransferWave {
  wave: number
  kind: string
  current?: boolean
  total: number
  done: number
  running: number
  pending: number
  waiting: number
  blocked: number
  failed: number
  plannedBytes: Int64String
  transferredBytes: Int64String
}

export interface TransferProgress {
  /** The size of the RELEASE. Not plannedBytes, and not meant to be. */
  contentBytes?: Int64String
  jobsPlanned: number
  jobsDone: number
  jobsFailed: number
  jobsOutstanding: number
  jobsInFlight: number
  workers: number
  jobsWaiting: number
  /** The number that explains an idle-looking fleet. */
  jobsBlocked: number
  jobsRepaired?: number
  outstandingBytes?: Int64String
  quietestInFlight?: string
  skips?: SkipBreakdown[]
  plannedBytes: Int64String
  bytesTransferred: Int64String
  dedupeSkippedBytes: Int64String
  skippedBytes?: Int64String
  /** Everything this transfer did not have to move. "Saved" on screen. */
  savedBytes?: Int64String
}

export interface Transfer {
  id: string
  requestId: string
  product: string
  tag: string
  displayTag?: string
  source: string
  target: string
  sourceName?: string
  targetName?: string
  state: TransferState
  priority: number
  /**
   * The field every byte column must be read against. For anything but `copy`
   * the progress numbers are structurally zero, and that is "we did not move
   * those bytes and cannot count them" — not "nothing happened".
   */
  strategy?: Strategy
  currentWave: number
  maxWave: number
  progress: TransferProgress
  failureReason?: string
  waves?: TransferWave[]
  content?: ContentGroup[]
  createdAt?: string
  /** When the first job was leased, not when the transfer was asked for. */
  startedAt?: string
  completedAt?: string
}

export interface ListTransfersResponse { transfers: Transfer[]; nextPageToken?: string }

export interface Job {
  id: string
  kind: string
  digest: string
  sizeBytes: Int64String
  state: JobState
  skipReason?: string
  wave: number
  attempts: number
  maxAttempts: number
  bytesTransferred: Int64String
  lastError?: string
  lastErrorClass?: string
  leaseOwner?: string
  updatedAt?: string
}

export interface ListJobsResponse { jobs: Job[]; nextPageToken?: string }

export interface FailureGroup {
  message: string
  errorClass?: string
  failed: number
  retryable: boolean
  exampleDigest?: string
  exampleJobId?: string
}

export interface ListFailuresResponse {
  transferId: string
  failures: FailureGroup[]
}

export interface CreateTransferRequest {
  product: string
  package: string
  from?: string
  to?: string[]
  toEnvironment?: string
  promote?: boolean
  priority?: number
  validateOnly?: boolean
}

export interface TransferEndpoint { name: string; registry: string; repository: string; environment?: string }

export interface CreateTransferResponse {
  requestId: string
  created: boolean
  transfers?: Transfer[]
  endpoints?: TransferEndpoint[]
  validateOnly?: boolean
}

export interface TransferControlResponse {
  transferId: string
  state: string
  jobs: number
  inFlight?: number
  /** Where the transfer now sits in the queue. Set by :setPriority. */
  priority?: number
}

/**
 * The body of :setPriority. 0-1000, higher runs first, 50 by default.
 *
 * Required on the wire even though 0 is a legal value — the server refuses an
 * omitted field rather than defaulting it, because 0 means "behind everything"
 * and guessing which of the two a caller meant is not something it can do.
 */
export interface SetPriorityRequest {
  priority: number
}

export interface TransferRetry { transferId: string; requeued: number; state: string }
export interface RetryTransferResponse { transfers: TransferRetry[]; requeued: number }

// ---------------------------------------------------------------------------
// Downloads and auto-download rules
// ---------------------------------------------------------------------------

/** Tri-state. "inherit" is not "false", and rendering it as false lies. */
export type VerifyState = 'true' | 'false' | 'inherit'

export interface DownloadView {
  product: string
  name?: string
  /** What the download NAMES. */
  targets?: string[]
  /** What that resolves to, closed over the targets' own mirror.from. */
  chain?: string[]
  chainText?: string
  chainError?: string
  priority: number
  default: boolean
  verifyBefore?: VerifyState
  verifyAfter?: VerifyState
  verifyPolicy?: string
  revision: string
}

export interface ListDownloadsResponse { downloads: DownloadView[] }

export interface AutoDownloadRuleView {
  product: string
  name: string
  /** The whole of what a rule decides. Where software goes is the download's business. */
  tagPattern: string
  sources?: string[]
  download?: string
  chain?: string[]
  chainText?: string
  chainError?: string
  /** Configuration, from Git, and the only way a rule is turned off. */
  enabled: boolean
  inline?: boolean
}

export interface ListAutoDownloadRulesResponse {
  enabled: boolean
  rules: AutoDownloadRuleView[]
}

export interface RunDownloadRequest {
  /** Names the software. Note what is absent: any pattern or filter. */
  tags: string[]
  download?: string
  validateOnly?: boolean
}

export interface RunDownloadResponse {
  product: string
  download: string
  chain: string[]
  requested?: string[]
  created?: string[]
  alreadyRequested?: string[]
  validateOnly?: boolean
}

export interface MatchesResponse { product: string; rule: string; matches: string[] }

// ---------------------------------------------------------------------------
// Replication — the "Configure Mirror to Quay" step
// ---------------------------------------------------------------------------

export interface ReplicationField { field: string; desired: string; observed: string }

export interface ReplicationDrift {
  drifted: boolean
  fields?: ReplicationField[]
  reason?: string
}

export interface MirrorSyncView {
  target: string
  state: string
  startedAt?: string
  completedAt?: string
  /** Quay's own message, verbatim. "Our side is fine" is the confusion to pre-empt. */
  message?: string
  itemsSynced?: number
}

export interface ReplicationView {
  product: string
  target: string
  mode: string
  desired?: Record<string, unknown>
  applied?: Record<string, unknown>
  observed?: Record<string, unknown>
  drift?: ReplicationDrift
  appliedAt?: string
  lastSync?: MirrorSyncView
  error?: string
}

export interface ListReplicationResponse { replication: ReplicationView[] }
export interface ListSyncsResponse { syncs: MirrorSyncView[]; nextPageToken?: string }

// ---------------------------------------------------------------------------
// Compare
// ---------------------------------------------------------------------------

export interface CompareEnd {
  /** The configured endpoint, plus the version where the two sides differ. */
  label: string
  /** What was actually walked, as a pullable reference. */
  reference: string
}

export interface CompareSide {
  digest: string
  tag?: string
  size?: Int64String
  repository?: string
  /** Where this component should be pullable AS ITSELF on this side. */
  namedRepository?: string
  namedPresent?: boolean
  namedTagDigest?: string
}

export type CompareVerdict = 'same' | 'changed' | 'only-a' | 'only-b'

export interface CompareRow {
  /** index | image | chart | file | signature */
  type: string
  /** The vendor's name, from org.opencontainers.image.ref.name. */
  name: string
  verdict: CompareVerdict
  a?: CompareSide
  b?: CompareSide
  /** Each disagreement stated as a fact. Empty when the sides agree. */
  differences?: string[]
  /**
   * The FILES inside the component's layers, read out of the archives.
   * Three lists rather than two: an edited file is changed, not added and
   * removed.
   */
  filesAdded?: string[]
  filesRemoved?: string[]
  filesChanged?: string[]
  /** A layer was left unopened — past the budget, or not an archive. */
  filesTruncated?: boolean
}

/**
 * Compares one package against another place, another version, or both.
 *
 * The package being compared is named in the URL; everything about the other
 * side is here. `from` and `to` are ENDPOINTS — configured source or target
 * names — and `against` is the other VERSION. Naming only `against` compares
 * two versions in one place; naming only `to` compares one version in two
 * places; naming both answers each at once.
 */
export interface CompareRequest {
  from?: string
  to?: string
  against?: string
  /**
   * How much layer content may be downloaded to say which FILES changed
   * rather than which layers. Zero uses the server's default; negative leaves
   * every layer opaque.
   */
  fileBudgetBytes?: number
}

export interface CompareResponse {
  product: string
  a: CompareEnd
  b: CompareEnd
  rows: CompareRow[]
  /** These four partition `rows`. */
  same: number
  changed: number
  onlyA: number
  onlyB: number
  /** Tags in each side's repository the bundle does not account for. */
  extraTagsA?: string[]
  extraTagsB?: string[]
  extraTruncatedA?: boolean
  extraTruncatedB?: boolean
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

/**
 * What one source is doing, and what it last did.
 *
 * The richest thing this API serves and the reason the Overview can show a
 * discovery in flight rather than a spinner: phase, repositories, tags,
 * artifacts and new packages all move while a scan runs.
 */
export interface DiscoverySourceState {
  product: string
  source: string

  /** Whether a scan is in flight right now. */
  scanning: boolean
  /** ENUMERATING_REPOSITORIES, LISTING_TAGS or RESOLVING_TAGS. Empty when idle. */
  phase?: string
  elapsedMs?: number

  /**
   * Zero until enumeration finishes — which itself says the scan is still
   * waiting on the registry's catalog, and is worth showing as its own state
   * rather than as 0%.
   */
  repositoriesTotal?: number
  repositoriesDone?: number
  /** Without this, a concurrent scan looks stalled for its first minute. */
  repositoriesInFlight?: number
  currentRepository?: string
  tagsTotal?: number
  tagsResolved?: number
  /** Manifests fetched. On a large artifact tree this is the only counter that moves. */
  artifacts?: number
  newPackages?: number
  errors?: number

  /** The last COMPLETED scan. */
  lastRunAt?: string
  lastError?: string
  lastRepositories?: number
  lastTagsListed?: number
  lastNewPackages?: number
  lastDurationMs?: number
  /** How often this source is polled when nobody asks. */
  intervalSeconds?: number
}

export interface DiscoveryStatusResponse {
  /** Discovery runs on the leader only; a follower answers false rather than erroring. */
  running: boolean
  sources: DiscoverySourceState[]
}

export interface ScanIssue {
  repository?: string
  tag?: string
  displayRepository?: string
  displayTag?: string
  /** `not_entitled` is the entitlement check working, not a fault. Empty is an ordinary failure. */
  class?: string
  message?: string
}

/** Reports what a triggered scan did, or that it started. */
export interface DiscoverPackagesResponse {
  repositories: number
  repositoriesFromCatalog?: number
  repositoriesFiltered?: number
  tagsListed: number
  tagsAdmitted: number
  /** Genuinely new packages. Zero on a re-scan is the expected, correct result. */
  packagesDiscovered: number
  superseded: number
  requestsCreated: number
  renamed?: number
  regrouped?: number
  durationMs: number
  tagErrors?: ScanIssue[]
  repositoryErrors?: string[]
  /** A scan was already running, so these numbers come from that one. */
  collapsed?: boolean
  /** Set instead of the counters when the caller asked not to wait. */
  started?: DiscoverStarted
}

export interface DiscoverStarted {
  /** How many sources began a NEW scan. */
  sources: number
  /** How many were already scanning and were left alone. A different fact. */
  alreadyRunning?: number
}

export interface DiscoverAllProduct {
  product: string
  sources: number
  alreadyRunning?: number
  /** Per product rather than failing the call: one broken source must not stop thirty. */
  error?: string
}

export interface DiscoverAllResponse {
  started: number
  alreadyRunning?: number
  products: DiscoverAllProduct[]
}

export interface UnavailablePackage {
  repository: string
  tag: string
  displayRepository?: string
  displayTag?: string
  reason: string
  detail?: string
  firstSeenAt: string
  lastSeenAt: string
}

export interface ListUnavailableResponse { packages: UnavailablePackage[] }

// ---------------------------------------------------------------------------
// Repositories health
// ---------------------------------------------------------------------------

export type CheckStatus = 'OK' | 'FAILED' | 'SKIPPED'

export interface CheckStep { name: string; status: CheckStatus; detail?: string; durationMs?: number }

export interface RepositoryCheck {
  name: string
  role: string
  registry: string
  repository?: string
  status: CheckStatus
  steps?: CheckStep[]
  error?: string
}

export interface ProductCheck { product: string; status: CheckStatus; repositories: RepositoryCheck[] }
export interface CheckConnectivityResponse { products: ProductCheck[] }

// ---------------------------------------------------------------------------
// Workers and system
// ---------------------------------------------------------------------------

export interface Worker {
  workerId: string
  version?: string
  maxConcurrency: number
  activeJobs: number
  leasedJobs: number
  state: string
  lastHeartbeat?: string
}

export interface ListWorkersResponse { workers: Worker[] }

export interface VersionResponse {
  version: string
  commit?: string
  buildDate?: string
  goVersion?: string
  component?: string
}

export interface HealthCheck { name: string; status: string; detail?: string; durationMs?: number }
export interface HealthCheckResponse { status: string; checks: HealthCheck[] }

// ---------------------------------------------------------------------------
// Audit and reports
// ---------------------------------------------------------------------------

export interface AuditEvent {
  name: string
  id: string
  occurredAt: string
  eventType: string
  actor: string
  /** user, system, worker, schedule or auto_rule. */
  actorKind: string
  product?: string
  subjectKind?: string
  subjectId?: string
  requestId?: string
  traceId?: string
  outcome: string
  detail?: unknown
}

export interface ListAuditEventsResponse { auditEvents: AuditEvent[]; nextPageToken?: string }

export interface ReportPeriod { since?: string; until?: string; label?: string }

export interface ReportTotals {
  downloadsCompleted: number
  downloadsFailed: number
  downloadsCancelled: number
  downloadsRunning: number
  promotions: number
  bytesTransferred: Int64String
  savedBytes: Int64String
  savedPercent?: number
  /**
   * ABSENT when the period contains no transfer whose bytes we counted. Not
   * zero — see docs/design/18 §6.1.
   */
  averageBytesPerSecond?: Int64String
  successRate?: number
}

export interface ProductReport { product: string; totals: ReportTotals }
export interface FailureCause { product: string; class: string; jobs: number }
export interface DailyVolume { day: string; bytesTransferred: Int64String; downloads: number }

export interface ReportSummary {
  period: ReportPeriod
  totals: ReportTotals
  products: ProductReport[]
  failureCauses?: FailureCause[]
  volume?: DailyVolume[]
}

// ---------------------------------------------------------------------------
// Identity
// ---------------------------------------------------------------------------

export interface WhoAmIResponse {
  subject: string
  method: string
  authenticated: boolean
  tenant?: string
  roles?: string[]
  /** `["*"]` means everything, which is what an unauthenticated deployment reports. */
  permissions: string[]
  /** Empty means every product. */
  products?: string[]
}

// ---------------------------------------------------------------------------
// Errors — RFC 9457 problem details
// ---------------------------------------------------------------------------

export type ErrorCode =
  | 'INVALID_ARGUMENT' | 'NOT_FOUND' | 'ALREADY_EXISTS' | 'FAILED_PRECONDITION'
  | 'ABORTED' | 'RESOURCE_EXHAUSTED' | 'UNAVAILABLE' | 'INTERNAL'
  | 'UNAUTHENTICATED' | 'PERMISSION_DENIED'

export interface Problem {
  type?: string
  title?: string
  status?: number
  detail?: string
  /** Clients switch on THIS, never on the HTTP status or the prose. */
  code: ErrorCode
  requestId?: string
}
