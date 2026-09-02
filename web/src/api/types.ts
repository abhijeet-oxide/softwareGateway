/**
 * TypeScript mirror of pkg/apis/softwaregateway/v1/types.go.
 *
 * This file is the contract. Nothing else in the application invents a field
 * name, and a field that is optional in Go is optional here - an absent value
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
 * Never do arithmetic on this directly - use `bytes()` from format.ts, which
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
  /** A native promotion waiting on the registry. It has no BYTE progress and
   *  never will - what it moves is names. See PromotionProgress. */
  | 'PROMOTING'
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

/**
 * How a transfer was performed. Anything but `copy` moved bytes we did not
 * count, so every byte column on it is structurally zero - which is "we did
 * not move those and cannot count them", never "nothing happened".
 *
 * `relocate` is a promotion the registry carried out inside itself.
 */
export type Strategy = 'copy' | 'mirror' | 'proxy' | 'relocate'

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
  /** `lab`, `production`. Targets only - the only thing that marks production. */
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

/** One validation failure inside a rejected product document. */
export interface ConfigIssue {
  /** The path within the document, e.g. `spec.targets[0].registry`. */
  field?: string
  message: string
  /** Why the rule exists, when that is not obvious from the message. */
  hint?: string
}

/**
 * Why a product's configuration was rejected.
 *
 * Read `loaded` before drawing anything. A product can be rejected and still be
 * RUNNING: loading is fail-closed per product, so a bad edit to a working
 * product leaves the previous good version in place. "Your change did not take
 * effect" and "this product does nothing" are different sentences.
 */
export interface ConfigError {
  /** The whole failure as one line. */
  message: string
  /** The document that was rejected, so the reader knows what to open. */
  file?: string
  /** Whether an earlier, valid version of this product is still running. */
  loaded: boolean
  /** The failure broken into its parts, when it was a validation failure. */
  details?: ConfigIssue[]
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
  /** Set when this product's document failed to parse or validate. */
  configError?: ConfigError
}

export interface ListProductsResponse { products: Product[]; nextPageToken?: string }

// ---------------------------------------------------------------------------
// Packages - "Software" on screen
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
  /** The resolved registry host and repository path this transfer landed in. */
  repository?: string
  state: TransferState
  /**
   * REPLICATE (downloaded from a vendor) or PROMOTE (moved between two of our
   * own targets).
   *
   * It is what stops a release's history reading as one long download: a
   * promotion is a different event, reached by a different decision. Absent on
   * a transfer recorded before the field existed, which reads as a download -
   * which is what every one of them was.
   */
  operation?: string
  failureReason?: string
  createdAt?: string
  completedAt?: string
}

export interface Package {
  name: string
  packageId: string
  product: string
  /**
   * What a vulnerability sync recorded, or a never-synced summary where none
   * has run. See PackageSecuritySummary - absent is not "no vulnerabilities".
   */
  security?: PackageSecuritySummary
  /**
   * What a standards check recorded, or a never-run summary where none has.
   *
   * ABSENT IS NOT "COMPLIANT". A release nobody has checked and a release that
   * passed everything are different facts, and rendering them the same way is
   * the bug this whole feature exists to prevent.
   */
  compliance?: PackageComplianceSummary
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
  /**
   * "analyzing" while a walk is in flight, "failed" when the last one gave up,
   * empty otherwise - `expandedAt` is what says a walk succeeded.
   *
   * Three states because `expandedAt` has two: a release being walked right now
   * must not read as one nobody has touched.
   */
  analysisState?: 'analyzing' | 'failed'
  /** Why the last walk gave up. */
  analysisError?: string
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
   * type - the field that separates a Helm chart from an image - is not
   * available until the tree is walked.
   */
  kind?: ArtifactKind
  /**
   * What the referencing descriptor says this MANIFEST weighs - a couple of
   * kilobytes of JSON. Right for planning a manifest push, wrong for "how big
   * is this image".
   */
  sizeBytes: Int64String
  /**
   * What the artifact weighs: manifest, config and layers.
   *
   * Absent for an artifact nobody has walked, because until then its blobs are
   * unknown - which is not the same as its weighing nothing.
   */
  contentBytes?: Int64String
  platform?: string
  depth?: number
  annotations?: Record<string, string>
  /**
   * Whether this manifest was ever pulled and verified. False for a child the
   * index merely listed - which is every child until the release is analysed,
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
 * out of `artifacts` - an evictable cache, not part of the record.
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
  /**
   * The walk was HANDED OFF rather than performed, so every count above is
   * zero because nothing has been counted yet. The package's `analysisState`
   * is what to watch.
   */
  started?: boolean
}

export interface ListArtifactsResponse {
  artifacts: Artifact[]
  nextPageToken?: string
}

/**
 * What stopping an analysis achieved.
 *
 * Two booleans rather than one, because they are two different promises. The
 * claim lives in the database, so releasing it works from any replica and is
 * what stops the release reading as `Analyzing`. The WALKING is a goroutine,
 * and only the Coordinator running it can cancel that - one somewhere else
 * carries on reading the vendor's registry until its own deadline, and its
 * result is discarded because it no longer holds the claim.
 */
export interface CancelAnalysisResponse {
  product: string
  package: string
  /**
   * A claim was released, so the release can be analysed again now.
   *
   * False is not a failure: the walk finished between the reader deciding to
   * stop it and the request arriving.
   */
  stopped: boolean
  /** The walking itself has ended, because this Coordinator was doing it. */
  stoppedHere: boolean
  /** The release as it stands now, so nothing has to be re-read to find out. */
  packageState: Package
}

// ---------------------------------------------------------------------------
// Transfers - "Download" on screen
// ---------------------------------------------------------------------------

/**
 * One reason a transfer moved no bytes, and how much that saved.
 *
 * `trusted` says whether the skip rests on an ACTION the registry took - a
 * mount - rather than on a claim it or we made. That distinction is the whole
 * value of the number: a destination that answers about its whole storage
 * rather than the repository asked about makes an untrusted skip worth nothing.
 */
export interface SkipBreakdown {
  reason: string
  jobs: number
  bytes?: Int64String
  trusted?: boolean
}

/**
 * One component the destination already held.
 *
 * `partial` says only PART of it was there and the rest is still to move - an
 * ordinary state, and a different claim from "this was already there", which
 * is what the list is otherwise making.
 */
export interface PresentComponent {
  name?: string
  digest: string
  kind: string
  bytes: Int64String
  partial?: boolean
}

/** GET /transfers/{id}/present - WHAT the destination already held, by name. */
export interface ListPresentComponentsResponse {
  transferId: string
  components: PresentComponent[]
  totalBytes?: Int64String
}

/** What a transfer is made OF, and how each kind went. */
export interface ContentGroup {
  kind: string
  total: number
  copied: number
  present: number
  failed?: number
  outstanding?: number
  /**
   * What this kind did not have to move, and what it did.
   *
   * Per JOB, unlike the counts above which are per component. A blob is one job
   * however many components reference it, so these partition the transfer's
   * bytes exactly and the parts add up to the whole.
   */
  savedBytes?: Int64String
  copiedBytes?: Int64String
  /**
   * The pushes beneath these components - every layer, config and manifest
   * that is a job of its own - and how each of them went.
   *
   * # Why the counts above are not enough
   *
   * A component is `copied` only once its last layer AND its manifest have
   * landed, because half an image is not an image. That makes the component
   * counts the right answer to "what is at the destination" and a useless one
   * to "how far along is this": they sit at zero for the whole download and
   * then all move at once, while tens of thousands of layers are visibly
   * streaming underneath.
   *
   * `unitsCopied + unitsPresent` over `units` is how much of this kind the
   * destination now holds, and it moves with every layer.
   */
  units?: number
  unitsCopied?: number
  unitsPresent?: number
  unitsFailed?: number
  unitsOutstanding?: number
  /**
   * How many FILES this kind holds, where that is a different number from
   * `total` - which is to say, on the `file` kind and nowhere else.
   *
   * A vendor ships its configuration as one `generic` component carrying a
   * hundred and twelve named layers. Two such bundles are two components and a
   * hundred and twelve files, and the release page has counted the files since
   * it learnt to list them. This is the same count, so the two pages cannot
   * report different numbers of files for the same release.
   */
  files?: number
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
  /**
   * The byte account over DISTINCT content - each piece weighed once, however
   * many repositories it has to reach.
   *
   * The per-(repository, digest) figures below are right for bookkeeping and
   * wrong for bytes: the second copy of a component is a mount that moves
   * nothing, and counting it doubled every total. These three are one
   * population - moved + present converges on `contentBytes` and never exceeds
   * it - and are what a bar and a saving are drawn from.
   */
  contentMovedBytes?: Int64String
  contentPresentBytes?: Int64String
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
  packageName?: string
  /**
   * WHICH package this moves - the only unambiguous answer.
   *
   * A vendor tag is not unique within a product: one NEAR release appears under
   * the same tag in every repository the product watches. Joining transfers to
   * packages by (product, tag) lights up all of them for one download.
   */
  packageId?: string
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
   * those bytes and cannot count them" - not "nothing happened".
   */
  /** REPLICATE or PROMOTE - what was ASKED for. See PackageTransfer. */
  operation?: string
  strategy?: Strategy
  /** Present only on a `relocate` transfer, and its only honest progress. */
  promotion?: PromotionProgress
  currentWave: number
  maxWave: number
  /**
   * ABSENT on a summary listing. `?view=summary` skips the per-transfer job
   * rollup - twelve correlated subqueries a row, and the whole cost of the
   * listing - so a page that only reads names and states does not pay for
   * counts it never draws. Read it optionally; a page that needs the numbers
   * asks without `view`.
   */
  progress?: TransferProgress
  failureReason?: string
  waves?: TransferWave[]
  content?: ContentGroup[]
  createdAt?: string
  /** When the first job was leased, not when the transfer was asked for. */
  startedAt?: string
  completedAt?: string
  /**
   * How long there was work of this transfer IN A WORKER'S HANDS.
   *
   * A different quantity from `completedAt - startedAt`, and the one a person
   * means by "how long did it take". The two are equal on a transfer that ran
   * without interruption and diverge by exactly the interruption on one that
   * did not: a fleet down overnight adds that night to the wall clock and
   * nothing to this.
   *
   * It is also the right denominator for a throughput. Dividing bytes by wall
   * clock after an outage reports a healthy link at a fraction of its speed -
   * the outage is in the denominator and none of it was spent transferring.
   *
   * Absent on a transfer no worker has ever held, which is honest rather than
   * zero: nothing has spent any time on it.
   */
  activeSeconds?: number
}

export interface ListTransfersResponse { transfers: Transfer[]; nextPageToken?: string }

/**
 * What the estate is doing, as three numbers.
 *
 * The shell shows one line on every page and used to compute it by asking for
 * the hundred most recent transfers every few seconds. A transfer listing
 * carries a dozen aggregates over each transfer's jobs, so its cost is set by
 * how much work the estate has done rather than by how many numbers the caller
 * wanted - 158ms for a hundred rows against 1ms for this, on a database whose
 * connection pool is deliberately a single connection.
 */
export interface TransferActivity {
  /** Live transfers with at least one job in a worker's hands. */
  moving: number
  /**
   * Live transfers with none: planned, queued, or waiting for a fleet that is
   * not there. Apart from `moving` because a queue being drained and a queue
   * nothing is draining are the same count of "running".
   */
  held: number
  failed: number
}

/**
 * The artifact a job belongs to - what makes a digest legible.
 *
 * A blob on its own is not something anybody can recognise. The image or chart
 * that references it is.
 */
export interface JobParent {
  digest: string
  mediaType?: string
  /** The vendor's own name, from org.opencontainers.image.ref.name. */
  ref?: string
  /** Several artifacts reference this blob, so the attribution is an example. */
  shared?: boolean
}

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
  sourceRepository?: string
  targetRepository?: string
  targetTags?: string[]
  parent?: JobParent
}

export interface ListJobsResponse { transferId?: string; jobs: Job[]; nextPageToken?: string }

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

export interface TransferEndpoint {
  name: string
  role?: string
  registry: string
  repository?: string
  environment?: string
}

export interface CreateTransferResponse {
  requestId?: string
  /** False when an identical request already existed - a replay, not an error. */
  created: boolean
  /** REPLICATE or PROMOTE, derived from what `from` resolved to. */
  operation: string
  from: TransferEndpoint
  to?: TransferEndpoint[]
  /** One per destination, in the same order as `to`. Empty on a dry run. */
  transferIds?: string[]
}

// ---------------------------------------------------------------------------
// Promotion
// ---------------------------------------------------------------------------

/**
 * HOW a promotion would be carried out.
 *
 * RELOCATE is the registry moving it internally: no bytes over the wire, and
 * seconds regardless of how large the release is. COPY is our workers reading
 * from one target and writing to the other - always correct, and within one
 * registry still cheap, but a manifest walk and a request per blob.
 */
export type PromotionMethod = 'RELOCATE' | 'COPY'

/** Whether a release is already at a destination, or on its way. */
export type PromotionDestinationState = 'ABSENT' | 'PRESENT' | 'IN_FLIGHT'

export interface PromotionOrigin {
  name: string
  environment?: string
  registry: string
  repository?: string
  /** A transfer to this target SUCCEEDED, which is what makes it a candidate. */
  holds: boolean
  lastTransferId?: string
}

export interface PromotionDestination {
  name: string
  environment?: string
  registry: string
  repository?: string
  /** Reachable ONLY by promotion - a registry a vendor may never push into. */
  promotionOnly?: boolean
  default?: boolean

  method: PromotionMethod
  /** Why, whichever answer came back. On COPY it is the diagnosis. */
  methodReason?: string

  state: PromotionDestinationState
  transferId?: string
  /** Why this destination cannot be chosen at all. Empty means it can. */
  unavailable?: string
}

export interface PromotionOptionsResponse {
  product: string
  package: string
  tag: string

  origins: PromotionOrigin[]
  /** Where the release was downloaded to. Empty when several targets hold it. */
  defaultOrigin?: string

  destinations: PromotionDestination[]
  /** Pre-selected in the dialog. Empty rather than guessed when ambiguous. */
  defaultDestinations?: string[]

  /** Whether the manifest tree has been walked. Gates the fast path. */
  analysed: boolean

  promotable: boolean
  reason?: string
}

/** A native promotion, as it stands. NAMES rather than bytes - see below. */
export interface PromotionProgress {
  promoter: string
  state: 'REQUESTED' | 'RUNNING' | 'SUCCEEDED' | 'FAILED'
  namesTotal: number
  namesDone: number
  attempts?: number
  lastError?: string
  requestedAt?: string
  startedAt?: string
  finishedAt?: string
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
 * Required on the wire even though 0 is a legal value - the server refuses an
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
// Replication - the "Configure Mirror to Quay" step
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
   * The NAMED FILES inside this component, and what became of each.
   *
   * Read from the manifests, not from the archives: an OCI artifact names one
   * file per layer and states its content digest, so aligning two of those
   * lists by path answers it exactly and costs nothing.
   *
   * Every file of a component that differs, unchanged ones included. Empty for
   * a component that agrees, where nothing inside it can differ.
   */
  files?: CompareFile[]
}

/**
 * One named file inside a component, and what became of it.
 *
 * Both digests and both sizes, because "changed" prompts the next question: a
 * reader looking at a changed file wants what it was and what it is.
 */
export interface CompareFile {
  path: string
  verdict: CompareVerdict
  sizeA?: Int64String
  sizeB?: Int64String
  digestA?: string
  digestB?: string
}

/**
 * Compares one package against another place, another version, or both.
 *
 * The package being compared is named in the URL; everything about the other
 * side is here. `from` and `to` are ENDPOINTS - configured source or target
 * names - and `against` is the other VERSION. Naming only `against` compares
 * two versions in one place; naming only `to` compares one version in two
 * places; naming both answers each at once.
 */
export interface CompareRequest {
  from?: string
  to?: string
  against?: string
  /** A caller-minted id to poll for progress while the request is open. */
  progressToken?: string
}

/** One end's position in a comparison. */
/** One named file inside a release. */
export interface PackageFile {
  /** The publisher's own name for it - `CONFIGURATION/nodes.json`. */
  path: string
  /** The artifact it came from, by that artifact's own name. */
  component?: string
  sizeBytes: Int64String
  digest: string
  mediaType?: string
}

export interface ListPackageFilesResponse {
  files: PackageFile[]
  /**
   * Layers carrying no name of their own - image layers, which are archives of
   * an unknown number of paths. Counted rather than listed.
   */
  opaqueLayers?: number
  /** Whether the release has been walked. An empty list before that means
   * "nobody has looked", not "there are none". */
  analysed: boolean
}

/**
 * One file's content, read out of the registry that publishes it.
 *
 * `content` is empty whenever `binary` or `tooLarge` is set - the server states
 * what it did instead of sending a screenful of replacement characters.
 */
export interface PackageFileContentResponse {
  path: string
  component?: string
  digest: string
  mediaType?: string
  sizeBytes: Int64String
  content?: string
  binary?: boolean
  tooLarge?: boolean
  limit?: number
}

export interface CompareProgressSide {
  /** Which end this is - "a" or "b". The label is not an identity: the two
   * sides of a version comparison are the same place and share it. */
  key?: string
  side: string
  phase: string
  done: number
  /**
   * What is KNOWN so far. A manifest tree is discovered by walking it, so
   * during that phase the denominator grows and `estimated` is true.
   */
  total: number
  estimated?: boolean
  /**
   * How many requests this side may have in flight at once. "Is it going as
   * fast as it can" is the second question anybody watching a four-minute bar
   * asks, and one request at a time looks identical to thirty-two.
   */
  concurrency?: number
}

/**
 * GET /api/v1/comparisons/{token} - where a comparison has got to.
 *
 * Polled while the comparison's own request is still open, using the token that
 * request carried. A 404 is a normal answer: progress lives in the memory of
 * the replica running it.
 */
export interface CompareProgressResponse {
  sides: CompareProgressSide[]
  done: boolean
  startedAt?: string
  updatedAt?: string
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
   * Zero until enumeration finishes - which itself says the scan is still
   * waiting on the registry's catalog, and is worth showing as its own state
   * rather than as 0%.
   */
  repositoriesTotal?: number
  repositoriesDone?: number
  /** Without this, a concurrent scan looks stalled for its first minute. */
  repositoriesInFlight?: number
  currentRepository?: string
  /** Whichever tag most recently started. A hint, not a position. */
  currentTag?: string
  tagsTotal?: number
  tagsResolved?: number
  /**
   * Tags resolved to a digest - one HEAD each, and the bulk of a scan. It moves
   * continuously; `tagsResolved` does not, and a bar built on the wrong one
   * sits still through the longest part of every scan.
   */
  tagsChecked?: number
  /** How many turned out to be new, and how many of those have been read. */
  tagsToFetch?: number
  tagsFetched?: number
  /** Tags being read right now - the configured concurrency actually in use. */
  tagsInFlight?: number
  /**
   * The whole scan's progress, 0 to 1 - the only number a bar is drawn from.
   *
   * One scale for every phase, and monotonic: the counters above are in three
   * different units, and a bar drawn from whichever one was live filled, reset
   * and filled again.
   */
  progress?: number
  /** Releases recorded so far, and the subset nobody had seen. Both live. */
  packages?: number
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

// ---------------------------------------------------------------------------
// Calibration - "is this speed the best this path can do?"
// ---------------------------------------------------------------------------
//
// A sibling of the connectivity check and a different question: not "can we
// reach it" but "how fast is it, and what setting would make it faster". It
// moves real data in both directions and takes minutes, which is why it is
// asked for explicitly and never on a timer.

export interface CalibrateRequest {
  /** Configured names. Empty picks the product's only source and its default target. */
  source?: string
  target?: string
  sourceRepository?: string
  /** The concurrency levels to sweep. Empty uses the server's default. */
  levels?: number[]
  /** How long ONE level runs. */
  budgetSeconds?: number
  /**
   * Whether to probe the WRITE half, which opens upload sessions on the target
   * and cancels them. Nothing is committed. Absent means the server's default,
   * which is on - a calibration that measured only reading would recommend a
   * concurrency for the wrong end of the path.
   */
  write?: boolean
  /** Projects the measured ceiling onto a transfer of this size. */
  bundleBytes?: Int64String
}

/** One concurrency level's measurement. */
export interface CalibrationLevel {
  concurrency: number
  bytes: Int64String
  seconds: number
  rateBytesPerSecond: number
  perStreamBytesPerSecond: number
  requests: number
  errors?: number
  throttled?: number
  ttfbMs?: number
  firstError?: string
}

/** What the traffic goes through, and what it would do the other way. */
export interface CalibrationRoute {
  configured: string
  proxyInUse: boolean
  directTested?: boolean
  directReachable?: boolean
  directDetail?: string
  proxiedRateBytesPerSecond?: number
  directRateBytesPerSecond?: number
}

/** Everything measured about one end of the path. */
export interface CalibrationSide {
  role: string
  name: string
  registry: string
  repository?: string
  route: CalibrationRoute
  rttMs?: number
  /**
   * What the read probe opened. Carried so a throughput measured over
   * signature blobs cannot be mistaken for one measured over layers.
   */
  samples?: number
  largestSampleBytes?: Int64String
  levels?: CalibrationLevel[]
  /** The smallest concurrency within a tenth of the best measured. */
  knee?: number
  /** The sweep ended before the path did, so the ceiling is higher than this. */
  stillClimbing?: boolean
  /** Why there are no measurements, when there are none. */
  skipped?: string
}

/** One thing to change, or one reason not to. */
export interface CalibrationSuggestion {
  severity: string
  /** The configuration key, in the spelling the file uses. */
  setting?: string
  scope?: string
  current?: string
  suggested?: string
  /** The measurement it rests on. Never empty: advice without a number is guesswork. */
  evidence: string
}

export interface CalibrateResponse {
  product: string
  /**
   * The host that ran the probes, and it is load-bearing: a measurement of the
   * Coordinator's network describes the workers' network only when they share
   * one.
   */
  measuredFrom: string
  startedAt: string
  durationSeconds: number
  source: CalibrationSide
  target: CalibrationSide
  suggestions: CalibrationSuggestion[]
  notes?: string[]
}

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
   * zero - see docs/design/18 §6.1.
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
  /** Deployment-wide switches, off a config file rather than a role. */
  features: Features
}

export interface Features {
  /** Whether a reader may save a file's raw bytes, not just view it. */
  fileDownloads: boolean
}

// ---------------------------------------------------------------------------
// Security
// ---------------------------------------------------------------------------

/**
 * The five severities, in the order everything shows them.
 *
 * A tuple rather than a union alone, so a component can iterate the ladder
 * without re-listing it - and so that "critical, high, medium, low" is never
 * assembled by hand in two places and never assembled differently.
 */
export const SEVERITIES = ['critical', 'high', 'medium', 'low', 'unknown'] as const
export type Severity = (typeof SEVERITIES)[number]

/**
 * What is known about one artifact.
 *
 * `scanned` with no findings is a clean artifact. `not_scanned` with no
 * findings is an artifact nobody looked at. Reading the second as the first is
 * the failure this whole feature exists to prevent, so nothing in the interface
 * may render a finding list without also rendering this.
 */
/**
 * `not_found` is the platform's own answer, not the scanner's: the image is not
 * in the JFrog repository at all. Xray reports that with the same sentence it
 * uses for an image it has simply not indexed yet, and the two are different
 * jobs for different people - a transfer versus a scan.
 */
export type ScanStatus =
  | 'scanned' | 'not_scanned' | 'not_found' | 'unsupported' | 'disabled' | 'unavailable'

/**
 * Whether a release's numbers can be trusted, and why not.
 *
 * `not_synced` is the state a fresh estate is mostly in, and it is emphatically
 * not `ok` with zero findings: nobody has looked. `stale` is a failed re-sync
 * over counts that are still worth showing, dated.
 */
export type SecurityState =
  | 'ok' | 'partial' | 'unavailable' | 'disabled' | 'not_synced' | 'syncing' | 'stale'

/** Where a release's vulnerability sync has got to. */
export type SyncState = '' | 'syncing' | 'synced' | 'failed'

export interface SecurityProgressStage {
  name: string
  label: string
  done: number
  /** Zero means "not yet known", which is honest while a tree is walked. */
  total: number
}

export interface SecuritySyncStatus {
  state: SyncState
  label: string
  error?: string
  syncedAt?: string
  startedAt?: string
  /** The last time the process running the sync said it was alive. */
  heartbeatAt?: string
  /** Whether the Coordinator answering is the one running the sync. */
  here?: boolean
  /**
   * The claim has stopped beating: nothing is running.
   *
   * `state: 'syncing'` says a sync was STARTED and nothing else, so without
   * this a Coordinator killed mid-sync is indistinguishable from one working
   * away on another replica - and was shown as the second.
   */
  stalled?: boolean
  /** Whether any repository of this product has a scanner switched on. */
  canSync: boolean
  /** Which knob turns one on, when canSync is false. */
  reason?: string
  /** The CONFIGURED repository whose scanner answers - usually a JFrog target. */
  repository?: string
  provider?: string
  /** Live position, present only while this replica runs the sync. */
  stages?: SecurityProgressStage[]
  notes?: string[]
  /** The run's transcript: live while it runs here, the stored one otherwise. */
  log?: SecurityLogEntry[]
}

export interface SecurityLogEntry {
  at?: string
  level: 'info' | 'warning' | 'error'
  message: string
  /** Identical consecutive lines, collapsed. */
  repeat?: number
}

export interface SyncSecurityResponse {
  product: string
  package: string
  /** started | already_running. The second is not a failure. */
  status: string
  started: boolean
  artifacts: number
  sync: SecuritySyncStatus
}

export interface CancelSecuritySyncResponse {
  product: string
  package: string
  /** False when the sync finished before the request arrived - not a failure. */
  stopped: boolean
  sync: SecuritySyncStatus
}

/**
 * A release's counts, carried on the package itself.
 *
 * Undefined means the server sent none. Null-ish is never zero vulnerabilities,
 * and a component that renders it as such has written the bug the whole feature
 * exists to prevent.
 */
export interface PackageSecuritySummary {
  state: SyncState
  label: string
  /**
   * A `syncing` row whose claim has stopped beating: nothing is running.
   *
   * A listing that reads `state` alone shows a spinner on a release whose
   * Coordinator was killed, until a sweep half an hour later notices.
   */
  stalled?: boolean
  counts: SecurityCounts
  /**
   * distinctTotal collapses (CVE, package) PAIRS; distinctCves collapses the
   * advisory alone. Two right answers to two questions - openssl and libssl3
   * carrying one advisory are two things to upgrade and one advisory to read -
   * and the panel used to print the first under the second's name.
   */
  distinctTotal: number
  distinctCves: number
  distinctCounts: SecurityCounts
  uniqueCveCounts: SecurityCounts
  complete: boolean
  /**
   * What "0 vulnerabilities" actually means. Zero of zero is "nobody looked"
   * and must never render as "none found"; zero of fourteen is a clean release.
   */
  scanned: number
  scannable: number
  syncedAt?: string
  error?: string
  canSync: boolean
  reason?: string
}

export type Verdict = 'better' | 'worse' | 'unchanged' | 'inconclusive'

export type ChangeType =
  | 'introduced' | 'resolved' | 'unchanged'
  | 'severity_increased' | 'severity_decreased'
  | 'remediation_changed' | 'removed_artifact'

export type ArtifactChange = 'common' | 'upgraded' | 'added' | 'removed'

export type SecuritySeverityCounts = Record<Severity, number>

export interface SecurityCounts {
  total: number
  fixable: number
  nonFixable: number
  bySeverity: SecuritySeverityCounts
  fixableBySeverity: SecuritySeverityCounts
}

/** Always rendered beside the counts: 1,286 means one thing at full coverage. */
export interface SecurityCoverage {
  artifacts: number
  scanned: number
  notScanned: number
  unsupported: number
  unavailable: number
  disabled: number
  /** Not in the scanned repository at all: a transfer to run, not a scan. */
  missing: number
  /** The denominator a percentage should use - excludes the unscannable. */
  scannable: number
  complete: boolean
}

export interface SecurityComponent {
  /** Excludes the version, because this is the key two releases align on. */
  id: string
  name: string
  version?: string
  type?: string
  path?: string
}

export interface SecurityArtifact {
  name: string
  tag?: string
  digest?: string
  repository?: string
  kind?: string
  mediaType?: string
  platform?: string
  display?: string
}

export interface SecurityFinding {
  cve?: string
  id?: string
  severity: Severity
  severityLabel: string
  summary?: string
  description?: string
  component: SecurityComponent
  fixedIn?: string[]
  fixable: boolean
  cvssScore?: number
  cvssVector?: string
  references?: string[]
  published?: string
  provider: string
  policy?: string
  /**
   * Every scanner that reported this finding.
   *
   * `provider` says which row this came from; `sources` says who agrees. One
   * entry on a single-scanner deployment, where the column is hidden - a column
   * reading "JFrog Xray" on every row costs width and says nothing.
   */
  sources?: string[]
}

/**
 * One breach of a configured policy - the gate, rather than the backlog.
 *
 * Not a finding with a policy field. A finding is "this image contains
 * CVE-2026-31789"; a violation is "your Production watch forbids critical
 * fixable issues and this image has four". It exists because somebody wrote a
 * rule, it disappears when the rule changes, and it can be raised against a
 * licence with no CVE anywhere near it.
 */
export interface SecurityViolation {
  id?: string
  type?: string
  severity: Severity
  severityLabel: string
  watch?: string
  policy?: string
  rule?: string
  summary?: string
  description?: string
  cve?: string
  component: SecurityComponent
  fixedIn?: string[]
  created?: string
  provider?: string
}

/** A scanner body held for one image, named and measured but not carried. */
export interface SecurityDocumentRef {
  kind: 'vulnerabilities' | 'sbom' | 'policy' | 'malware'
  label: string
  /**
   * False for a body the scanner was asked for and did not have. Worth saying:
   * the alternative is a button that silently downloads nothing.
   */
  available: boolean
  contentType?: string
  bytes?: number
  fetchedAt?: string
  message?: string
  url?: string
}

/**
 * One scanner's contribution to a release.
 *
 * `onlyHere` is the number the comparison exists for: advisories this scanner
 * reported and no other did.
 */
export interface SecuritySourceCounts {
  provider: string
  label: string
  counts: SecurityCounts
  uniqueCves: number
  onlyHere: number
  artifacts: number
}

export interface SecurityReport {
  artifact: SecurityArtifact
  status: ScanStatus
  statusLabel: string
  provider?: string
  message?: string
  findings?: SecurityFinding[]
  counts: SecurityCounts
  /**
   * What the scanner found that is not a vulnerability.
   *
   * Its own list rather than findings with a flag, because it is read by a
   * different person for a different reason: a vulnerability count is a
   * backlog, a malware hit is a release that does not ship tonight.
   */
  malware?: SecurityFinding[]
  /** What the scanner's configured policies say about this image. */
  violations?: SecurityViolation[]
  /** Which scanner bodies are held, for the download menu. */
  documents?: SecurityDocumentRef[]
  scannedAt?: string
  retrievedAt?: string
  fromCache?: boolean
  /** The scanner's own page for this image, built from configuration. */
  scanUrl?: string
}

/**
 * The deployment's rule about how old an answer may be, and where this release
 * sits against it.
 *
 * Sent by the server rather than decided here, because "how old is too old" is
 * one number in one configuration file and a page that guessed it would be
 * wrong in every deployment that chose differently - silently, and in the
 * direction of calling stale data current.
 *
 * Nothing expires. Past `maxAgeSeconds` an answer is still served, still
 * counted and still exported; it is shown with its age and a refresh beside it.
 */
export interface SecurityFreshness {
  maxAgeSeconds?: number
  /** Normally absent: an SBOM describes bytes that cannot change. */
  sbomMaxAgeSeconds?: number
  stale?: boolean
  staleAt?: string
}

export interface PackageSecurityResponse {
  product: string
  package: string
  /** Read this first: everything below it is meaningless until a sync has run. */
  sync: SecuritySyncStatus
  provider?: string
  enabled: boolean
  repository?: string
  state: SecurityState
  message?: string
  counts: SecurityCounts
  uniqueCounts: SecurityCounts
  uniqueCveCounts: SecurityCounts
  /** See PackageSecuritySummary: two questions, two numbers, named for what they count. */
  distinctTotal: number
  distinctCves: number
  coverage: SecurityCoverage
  reports: SecurityReport[]
  providers?: string[]
  /**
   * Per-scanner counts, present only where more than one scanner contributed.
   * A segmented control with one position is a control that should not be drawn.
   */
  sources?: SecuritySourceCounts[]
  scannedAt?: string
  syncedAt?: string
  freshness?: SecurityFreshness
  fingerprint?: string
  detail: boolean
}

export interface SecurityChange {
  type: ChangeType
  typeLabel: string
  cve?: string
  id?: string
  severity: Severity
  severityLabel: string
  fromSeverity?: Severity
  toSeverity?: Severity
  fixable: boolean
  fixedIn?: string[]
  summary?: string
  description?: string
  component: SecurityComponent
  artifact: SecurityArtifact
  artifactChange: ArtifactChange
  viaRemoval?: boolean
  provider?: string
}

export interface SecurityArtifactDelta {
  key: string
  change: ArtifactChange
  a?: SecurityArtifact
  b?: SecurityArtifact
  statusA?: ScanStatus
  statusB?: ScanStatus
  countsA: SecurityCounts
  countsB: SecurityCounts
  introduced: number
  resolved: number
  unchanged: number
  severityChanged: number
  /** False when one side has no scan result: the zeroes mean "not computed". */
  comparable: boolean
}

export interface SecurityArtifactSummary {
  common: number
  upgraded: number
  added: number
  removed: number
  notComparable: number
}

export interface SecurityComparisonEnd {
  label: string
  package?: string
  tag?: string
  digest?: string
  repository?: string
  provider?: string
  enabled: boolean
  counts: SecurityCounts
  uniqueCveCounts: SecurityCounts
  coverage: SecurityCoverage
  scannedAt?: string
  /** So the interface can offer the sync rather than only reporting a verdict. */
  sync: SecuritySyncStatus
}

export interface SecurityComparisonResponse {
  product: string
  a: SecurityComparisonEnd
  b: SecurityComparisonEnd
  verdict: Verdict
  verdictLabel: string
  headline: string
  explanation: string
  caveats?: string[]
  introduced: SecurityCounts
  resolved: SecurityCounts
  unchanged: SecurityCounts
  severityIncreased: SecurityCounts
  severityDecreased: SecurityCounts
  remediationChanged: SecurityCounts
  removedArtifact: SecurityCounts
  netScore: number
  /**
   * The classified findings, worst first, and possibly only the first of
   * them - `changesTotal` is how many there are. Count from the totals above,
   * never from this array's length.
   */
  changes: SecurityChange[]
  changesTotal: number
  artifacts: SecurityArtifactDelta[]
  artifactSummary: SecurityArtifactSummary
  fingerprint?: string
  retrievedAt?: string
}

export interface SecurityCompareRequest {
  against?: string
  repository?: string
}

export interface SecurityRelease {
  packageId: string
  tag: string
  displayTag?: string
  digest?: string
}

export type SearchKind = 'cve' | 'package' | 'image'

export interface SecuritySearchHit {
  cve?: string
  issueId?: string
  severity: Severity
  severityLabel: string
  fixable: boolean
  summary?: string
  component: SecurityComponent
  fixedIn?: string
  artifact: SecurityArtifact
  provider?: string
  repository?: string
  scannedAt?: string
  /** The edge that makes the relationship navigable in both directions. */
  releases?: SecurityRelease[]
}

export interface SecuritySearchScope {
  artifacts: number
  releases: number
  note?: string
}

export interface SecuritySearchResponse {
  product: string
  kind: SearchKind
  query: string
  exact?: boolean
  hits: SecuritySearchHit[]
  truncated?: boolean
  searched: SecuritySearchScope
}

// ---------------------------------------------------------------------------
// Errors - RFC 9457 problem details
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

/** '' never run | running | complete | failed | cancelled. */
export type ComplianceState = '' | 'running' | 'complete' | 'failed' | 'cancelled'

/**
 * pass | conditional | fail | inconclusive.
 *
 * `inconclusive` is not a milder `fail`: it means something could not be
 * decided, so the release has not been SHOWN to comply with anything. It
 * outranks pass and conditional for exactly that reason.
 */
export type ComplianceVerdict = '' | 'pass' | 'conditional' | 'fail' | 'inconclusive'

/** pass | fail | skip | error | waived. */
export type ComplianceOutcome = 'pass' | 'fail' | 'skip' | 'error' | 'waived'

/**
 * fixed | configurable | unknown | na.
 *
 * The difference between the vendor's defect and the site's decision: a value
 * the chart template fixes is theirs to change, and one a values file can
 * override is a question for whoever writes those values.
 */
export type ComplianceDeterminacy = 'fixed' | 'configurable' | 'unknown' | 'na'

/** A release's standards result, as the listing shows it. */
export interface PackageComplianceSummary {
  state?: ComplianceState
  verdict?: ComplianceVerdict
  /** The verdict in the words the interface states it, sent by the server. */
  label?: string
  blocking: number
  warning: number
  /**
   * Checks that could not be decided. A release with three hundred passes and
   * one of these is INCONCLUSIVE, not compliant - a column that showed only
   * blocking and warning would draw it as clean.
   */
  error: number
  pass: number
  /** RFC 3339. Empty while running, and empty for a release never checked. */
  checkedAt?: string
  /** Whether this Coordinator can start a check at all, and why not. */
  canRun?: boolean
  reason?: string
}

export interface ComplianceCounts {
  pass: number
  fail: number
  skip: number
  error: number
  waived: number
  blocking: number
  warning: number
  info: number
}

/** One run, without its results. */
export interface ComplianceRun {
  id: string
  state: ComplianceState
  error?: string
  verdict?: ComplianceVerdict
  verdictLabel?: string
  /**
   * What produced it. A report that cannot say which rulebook, which helm and
   * which Kubernetes version produced it cannot be re-derived - and re-deriving
   * it is what happens when a vendor disputes a finding.
   */
  bundleDigest?: string
  helmVersion?: string
  kubeVersion?: string
  checks: number
  counts: ComplianceCounts
  /** The result list was cut short. A truncated report LOOKS complete. */
  truncated?: boolean
  trigger?: string
  startedAt: string
  finishedAt?: string
}

/** One chart's contribution - the run's denominator. */
export interface ComplianceChart {
  name: string
  version?: string
  digest?: string
  ref?: string
  /** ok | failed | skipped. */
  status: string
  error?: string
  resources: number
}

/**
 * One finding, addressed precisely enough to act on without this tool.
 *
 * Every address field is present even where a client could derive it, because
 * deriving it needs the release - and the most important consumer of this shape
 * is an export a vendor opens with no access to this platform.
 */
export interface ComplianceResult {
  check: string
  title?: string
  severity: 'block' | 'warn' | 'info'
  category?: string
  pack?: string
  tier?: number
  remediation?: string
  reference?: string

  outcome: ComplianceOutcome
  outcomeLabel: string
  determinacy?: ComplianceDeterminacy
  determinacyLabel?: string

  chart?: string
  chartVersion?: string
  subchartPath?: string
  artifactDigest?: string
  artifactRef?: string
  sourceFile?: string
  renderedLine?: number
  apiVersion?: string
  kind?: string
  namespace?: string
  name?: string
  container?: string
  containerType?: string
  locus?: string

  observed?: string
  expected?: string
  message?: string
  error?: string

  waiver?: string
  waiverExpires?: string
  fingerprint?: string
}

/** What a run reports while it is working. */
export interface ComplianceProgress {
  runId: string
  stage: 'fetching' | 'rendering' | 'evaluating' | 'recording'
  label: string
  /** Counts of the CURRENT stage, not of the whole run. */
  done: number
  total: number
  note?: string
  started: string
}

/** Whether this Coordinator can render charts at all. */
export interface ComplianceHelm {
  available: boolean
  version?: string
  reason?: string
}

export interface PackageComplianceResponse {
  product: string
  release: string
  /** ABSENT MEANS NOT CHECKED. Never render it as a pass. */
  run?: ComplianceRun
  /** Present only while a run is live, so one endpoint serves both. */
  progress?: ComplianceProgress
  charts?: ComplianceChart[]
  results?: ComplianceResult[]
  /** The count BEFORE the page was taken. */
  total: number
  helm: ComplianceHelm
  /**
   * Whether this release's manifest tree has been walked.
   *
   * A run needs each chart artifact's layer digest, and those come from the
   * walk. False means there is nothing to fetch yet - so the tab offers the
   * walk rather than a button that fails.
   */
  analysed: boolean
}

export interface ComplianceRunsResponse {
  runs: ComplianceRun[]
}

/** One rule, in full - what a vendor reads before they ship. */
export interface PolicyCheck {
  id: string
  title: string
  description?: string
  /** WHY the organization requires it. What stops a check being cargo-culted. */
  rationale?: string
  severity: 'block' | 'warn' | 'info'
  tier?: number
  category?: string
  remediation?: string
  reference?: string
  pack?: string
  engine?: string
  /** What the check judges, as a sentence. */
  appliesTo?: string
  deprecated?: boolean
  supersededBy?: string
}

export interface PolicyPack {
  name: string
  prefixes?: string[]
  version?: string
  description?: string
  maintainer?: string
  reference?: string
  builtin?: boolean
  checks: number
  /**
   * Why a pack did not load. Surfaced rather than logged: the checks it owns
   * will report `error`, and a reader has to know which and why.
   */
  errors?: string[]
}

export interface PolicyCatalogueResponse {
  bundleDigest?: string
  packs: PolicyPack[]
  checks: PolicyCheck[]
}
