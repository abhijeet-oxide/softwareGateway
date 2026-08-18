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

export interface Artifact {
  artifactId: string
  parentId?: string
  digest: string
  mediaType: string
  artifactType?: string
  sizeBytes: Int64String
  platform?: string
  depth?: number
  tag?: string
  annotations?: Record<string, string>
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

export interface CompareEnd { endpoint: string; reference: string; registry?: string; repository?: string }

export interface CompareSide {
  digest?: string
  tag?: string
  sizeBytes?: Int64String
  mediaType?: string
  platform?: string
}

export interface CompareRow {
  name: string
  kind: string
  /** ADDED, REMOVED, CHANGED, UNCHANGED. */
  change: string
  a?: CompareSide
  b?: CompareSide
  detail?: string
}

export interface CompareRequest {
  to: string
  toEndpoint?: string
  fromEndpoint?: string
  fileBudget?: number
}

export interface CompareResponse {
  product: string
  a: CompareEnd
  b: CompareEnd
  rows: CompareRow[]
  added: number
  removed: number
  changed: number
  unchanged: number
  aTotalBytes?: Int64String
  bTotalBytes?: Int64String
  truncated?: boolean
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

export interface DiscoverySourceState {
  source: string
  running: boolean
  lastStartedAt?: string
  lastCompletedAt?: string
  lastError?: string
  repositoriesScanned?: number
  packagesFound?: number
}

export interface DiscoveryStatusResponse {
  product: string
  running: boolean
  sources: DiscoverySourceState[]
}

export interface DiscoverPackagesResponse {
  product: string
  discovered: number
  superseded?: number
  scanned?: number
  issues?: { source: string; message: string }[]
}

export interface DiscoverAllProduct { product: string; started: boolean; alreadyRunning?: boolean; error?: string }
export interface DiscoverAllResponse { products: DiscoverAllProduct[]; started: number; alreadyRunning: number }

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
