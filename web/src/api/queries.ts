import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef } from 'react'
import { api, fetchText, query, packageRef } from './client'
import type {
  CalibrateRequest, CalibrateResponse,
  CancelAnalysisResponse, CancelSecuritySyncResponse,
  CheckConnectivityResponse, CompareProgressResponse, CompareRequest, CompareResponse,
  DiscoverAllResponse,
  DiscoverPackagesResponse, DiscoveryStatusResponse, HealthCheckResponse, ListArtifactsResponse,
  ListAuditEventsResponse, ListAutoDownloadRulesResponse, ListDownloadsResponse,
  InspectPackageResponse, ListFailuresResponse, ListJobsResponse, ListPackagesResponse, ListProductsResponse,
  ListReplicationResponse, ListSyncsResponse, ListTransfersResponse, ListUnavailableResponse,
  ListPackageFilesResponse, ListPresentComponentsResponse, ListWorkersResponse, Package,
  CreateTransferRequest, CreateTransferResponse,
  PackageFileContentResponse, PromotionOptionsResponse, ReportSummary, RunDownloadRequest,
  RunDownloadResponse,
  PackageSecurityResponse, SearchKind, SecurityCompareRequest, SecurityComparisonResponse,
  SecuritySearchResponse, SyncSecurityResponse,
  SetPriorityRequest, Transfer, TransferActivity, TransferControlResponse, VersionResponse,
  ComplianceProgress,
  ComplianceRunsResponse,
  PackageComplianceResponse,
  PolicyCatalogueResponse,
} from './types'
import { isLive } from '../domain/derive'

/**
 * Every server call the application makes, in one place.
 *
 * # On polling
 *
 * There is no server-sent-events channel yet - gate G4 in docs/design/19 §5 is
 * unmet - so live progress is polled. The rule enforced here is that polling
 * is SCOPED: a transfer is re-fetched on an interval only while it is in a
 * live state, and the interval drops to false the moment it settles. Job lists
 * never poll; the summary does. Nothing polls a page nobody is looking at,
 * because TanStack stops when the component unmounts.
 *
 * # On pagination
 *
 * Server-side, always. The API provides `pageSize`/`pageToken` and the UI
 * never fetches everything to count it (docs/design/19 §6).
 */

const MINUTE = 60_000

// ---------------------------------------------------------------------------
// Products
// ---------------------------------------------------------------------------

export function useProducts() {
  return useQuery({
    queryKey: ['products'],
    queryFn: () => api.get<ListProductsResponse>('/products'),
    staleTime: 5 * MINUTE,
  })
}

export function useProduct(name: string | undefined) {
  return useQuery({
    queryKey: ['product', name],
    queryFn: () => api.get<import('./types').Product>(`/products/${encodeURIComponent(name!)}`),
    enabled: Boolean(name),
    staleTime: 5 * MINUTE,
  })
}

/**
 * Start a scan and return immediately.
 *
 * `wait: false` is the important part. The default holds the HTTP request open
 * for the whole scan, which against a slow registry means minutes of a spinner
 * and every intermediary's idle timeout becoming part of the control plane.
 * Starting and polling GET .../discovery is what lets the interface show the
 * scan progressing instead of a button that appears to do nothing.
 *
 * Note what the response says when nothing started: `alreadyRunning`. That is
 * not a failure and not a no-op - it is "a scan is already going" - and the
 * caller is expected to say so rather than discard it.
 */
export function useRunDiscovery() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (product?: string): Promise<DiscoverPackagesResponse | DiscoverAllResponse> =>
      product
        ? api.post<DiscoverPackagesResponse>(
            `/products/${encodeURIComponent(product)}/packages:discover`, { wait: false })
        : api.post<DiscoverAllResponse>('/products:discover'),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['packages'] })
      void qc.invalidateQueries({ queryKey: ['discovery'] })
      void qc.invalidateQueries({ queryKey: ['audit'] })
    },
  })
}

/** Discovery status for several products at once, for the estate view. */
export function useDiscoveryStatuses(products: string[]) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['discovery', product],
      queryFn: () => api.get<DiscoveryStatusResponse>(
        `/products/${encodeURIComponent(product)}/discovery`),
      refetchInterval: (q: { state: { data?: DiscoveryStatusResponse } }) =>
        (q.state.data?.sources ?? []).some((s) => s.scanning) ? 2000 : 15_000,
    })),
  })
}

export function useConnectivity(enabled: boolean) {
  return useQuery({
    queryKey: ['connectivity'],
    queryFn: () => api.post<CheckConnectivityResponse>('/products:checkConnectivity'),
    enabled,
    // A registry probe makes real outbound calls, so it runs when asked for
    // and is not refreshed behind the user's back.
    staleTime: Infinity,
    retry: false,
  })
}

// ---------------------------------------------------------------------------
// Software (packages)
// ---------------------------------------------------------------------------

export interface PackageFilters {
  repository?: string
  tag?: string
  state?: string
  pageSize?: number
  pageToken?: string
}

export function usePackages(product: string | undefined, filters: PackageFilters = {}) {
  return useQuery({
    queryKey: ['packages', product, filters],
    queryFn: () => api.get<ListPackagesResponse>(
      `/products/${encodeURIComponent(product!)}/packages${query({ ...filters })}`),
    enabled: Boolean(product),
    staleTime: MINUTE,
    // Only while a release on THIS page is being walked, and it stops the
    // moment none is. Discovery analyses new releases on its own, so a listing
    // that never refreshed would show `Analyzing` on rows that finished
    // minutes ago.
    refetchInterval: (q) =>
      (q.state.data?.packages ?? []).some((p) => p.analysisState === 'analyzing')
        ? 5000
        : false,
  })
}

/** Package listings for several products at once, preserving per-product query keys. */
export function usePackagesByProducts(products: string[], filters: PackageFilters = {}) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['packages', product, filters],
      queryFn: () => api.get<ListPackagesResponse>(
        `/products/${encodeURIComponent(product)}/packages${query({ ...filters })}`),
      staleTime: MINUTE,
      // Poll only while this product has releases currently being analysed.
      refetchInterval: (q: { state: { data?: ListPackagesResponse } }) =>
        (q.state.data?.packages ?? []).some((p) => p.analysisState === 'analyzing')
          ? 5000
          : false,
    })),
  })
}

/**
 * One release.
 *
 * `repository` scopes the lookup, and for most vendors it is REQUIRED rather
 * than optional: the same version tag is published into every repository of a
 * product, so the tag alone matches ten packages and the API answers with an
 * ambiguity error rather than a guess.
 */
export function usePackage(
  product: string | undefined, ref: string | undefined, repository?: string,
) {
  const qc = useQueryClient()
  const result = useQuery({
    queryKey: ['package', product, ref, repository],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      return api.get<Package>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}` +
        scopeQuery(q, repository))
    },
    enabled: Boolean(product && ref),
    // SCOPED POLLING, by the same rule transfers follow: only while something
    // is actually happening. A release being walked in the background finishes
    // without anybody pressing anything, and a page that did not notice would
    // sit on "Analyzing" until somebody reloaded it.
    refetchInterval: (q) => (q.state.data?.analysisState === 'analyzing' ? 4000 : false),
  })

  /*
   * Tell the rest of the page when a WALK ends.
   *
   * This query polls itself while one runs, so it learns the moment the tree
   * has been recorded - and it is the only thing that does. Everything a walk
   * PRODUCES is read by another query: the artifacts under the release, its
   * files, its compliance. Those were invalidated when the walk was ASKED FOR,
   * which is the one moment there is nothing yet to fetch, and never again -
   * so the Contents tab sat on "no files" over a release that had been fully
   * walked, until somebody reloaded the page.
   *
   * The TRANSITION is what matters, not the state: invalidating on every
   * settled poll would refetch the release's contents forever.
   */
  const wasAnalysing = useRef(false)
  const state = result.data?.analysisState
  const loaded = result.data !== undefined
  useEffect(() => {
    if (state === 'analyzing') {
      wasAnalysing.current = true
      return
    }
    if (!wasAnalysing.current || !loaded) return
    wasAnalysing.current = false
    void qc.invalidateQueries({ queryKey: ['artifacts', product, ref] })
    void qc.invalidateQueries({ queryKey: ['package-files', product, ref] })
    // A walk changes what compliance has to look at - a release with no charts
    // recorded has none to check.
    void qc.invalidateQueries({ queryKey: ['package-compliance', product, ref] })
    void qc.invalidateQueries({ queryKey: ['packages'] })
  }, [state, loaded, qc, product, ref])

  return result
}

/**
 * Merges an explicit repository into whatever the reference already implied.
 *
 * The `repo:tag` spelling and `?repository=` are two ways to say the same
 * thing and the API accepts either; this keeps a caller from emitting both and
 * producing a malformed query.
 */
function scopeQuery(existing: string, repository?: string): string {
  if (!repository) return existing
  if (existing) return existing
  return `?repository=${encodeURIComponent(repository)}`
}

export function useArtifacts(
  product: string | undefined, ref: string | undefined, repository?: string, enabled = true,
) {
  return useQuery({
    queryKey: ['artifacts', product, ref, repository],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      return api.get<ListArtifactsResponse>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}/artifacts` +
        scopeQuery(q, repository))
    },
    enabled: enabled && Boolean(product && ref),
  })
}

/**
 * What is inside a release, as files.
 *
 * Read from what analysis recorded, so it troubles no registry - and it is
 * empty until the release has been analysed, which the response says rather
 * than leaving the page to guess.
 */
export function usePackageFiles(product: string | undefined, ref: string | undefined,
  repository?: string) {
  const { segment, query: q } = packageRef(ref ?? '')
  return useQuery({
    queryKey: ['package-files', product, ref, repository],
    queryFn: () => api.get<ListPackageFilesResponse>(
      `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}/files`
      + scopeQuery(q, repository)),
    enabled: Boolean(product && ref),
  })
}

/**
 * ONE file's content, fetched on demand.
 *
 * Enabled only when a digest is asked for - the query is what a modal opening
 * looks like, so mounting the hook without one must fetch nothing. Cached by
 * digest, which is the whole identity of the content: the same file opened
 * twice is served from memory, and content addressing means it cannot be stale.
 */
export function usePackageFileContent(
  product: string | undefined, ref: string | undefined, digest: string | undefined,
  repository?: string,
) {
  const { segment, query: q } = packageRef(ref ?? '')
  return useQuery({
    queryKey: ['package-file', product, ref, repository, digest],
    queryFn: () => api.get<PackageFileContentResponse>(
      packageFileContentUrl(product!, segment, q, repository, digest!)),
    enabled: Boolean(product && ref && digest),
    staleTime: Infinity,
  })
}

function packageFileContentUrl(
  product: string, segment: string, q: string, repository: string | undefined, digest: string,
): string {
  const scope = scopeQuery(q, repository)
  return `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}`
    + `/files/content${scope ? `${scope}&` : '?'}digest=${encodeURIComponent(digest)}`
}

/**
 * The URL of one file's raw bytes.
 *
 * A URL rather than a fetch, matching packageSecurityExportUrl: a download is
 * a link, the browser streams it and names it from Content-Disposition, and
 * this endpoint has no size bound to trip over - unlike the content endpoint
 * above, which reads into memory to render text and so caps what it will read.
 */
export function packageFileDownloadUrl(
  product: string, ref: string, digest: string, repository?: string,
): string {
  const { segment, query: q } = packageRef(ref)
  const scope = scopeQuery(q, repository)
  return `/api/v1/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}`
    + `/files/download${scope ? `${scope}&` : '?'}digest=${encodeURIComponent(digest)}`
}

/**
 * WHAT a transfer did not have to move, by name.
 *
 * Its own query rather than a field on the transfer, because the transfer is
 * polled every couple of seconds while a download runs and this is hundreds of
 * rows that change only when a job settles. `enabled` is what makes that true:
 * it is fetched when somebody actually asks what the saving was made of.
 */
export function usePresentComponents(transferId: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ['transfer-present', transferId],
    queryFn: () => api.get<ListPresentComponentsResponse>(
      `/transfers/${encodeURIComponent(transferId!)}/present`),
    enabled: enabled && Boolean(transferId),
    staleTime: 10_000,
  })
}

/**
 * Analyse a release: walk its manifest tree and record what it is made of.
 *
 * Synchronous by design - the API has no progress feed for it, because the
 * extent of the work is the thing being discovered. A caller can report that
 * it is running and for how long, and nothing more honest than that.
 */
export function useInspectPackage(product: string, ref: string, repository?: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => {
      const { segment, query: q } = packageRef(ref)
      return api.post<InspectPackageResponse>(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}:inspect` +
        scopeQuery(q, repository),
        // ASKED FOR, NOT WAITED FOR.
        //
        // Walking a release is minutes of round trips, and a request held open
        // for it is cancelled by navigating away - which used to cancel the
        // walk with it and leave the release claimed by a request that no
        // longer existed. The server hands it to the same background analyser
        // discovery uses and answers immediately; the package's analysisState
        // is what the page watches.
        { wait: false })
    },
    onSuccess: (res) => {
      /*
       * SEED THE RELEASE, do not merely invalidate it.
       *
       * The response carries the release as the server left it, read AFTER the
       * claim was taken - so it already says `analyzing`. Writing it into the
       * cache closes the window between the button being pressed and the first
       * poll noticing, and that window is where the bug lived: for a release
       * that had been walked before, the page saw a finished-looking response
       * over an unchanged `analysed` release and reported the walk as over the
       * instant it began - "Analyzed in 0s", 0 artifacts, 0 blobs, which are
       * the zeros a handoff response is documented to carry.
       *
       * It also starts usePackage polling, which is what actually watches the
       * walk, and whose ending invalidates everything the walk produces.
       */
      if (res.package) qc.setQueryData(['package', product, ref, repository], res.package)
      void qc.invalidateQueries({ queryKey: ['package', product, ref] })
      // And the LISTING, so a reader who goes back sees the release marked as
      // being analysed rather than offering to analyse it again.
      void qc.invalidateQueries({ queryKey: ['packages'] })
      // Nothing else is invalidated HERE. The artifacts, the files and the
      // compliance of a release are what the walk produces, and at this moment
      // it has not produced any of it: refetching now caches the empty answer
      // that was already on screen. usePackage invalidates them when the walk
      // ends, which is the moment there is something new to read.
    },
  })
}

/**
 * Stop a walk that is under way.
 *
 * # Why a walk needs a stop at all
 *
 * It is minutes of round trips against the vendor's registry, started by one
 * button, and until this existed the only ways out of one started by mistake -
 * the wrong release, a source whose request budget a download needed more -
 * were to wait twenty minutes for the server's deadline or to restart the
 * Coordinator. A job somebody can start and cannot stop is a job they learn
 * not to start.
 *
 * The release goes back to UNANALYSED rather than failed, so the same button
 * that stopped it will start it again.
 */
export function useCancelAnalysis(product: string, ref: string, repository?: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => {
      const { segment, query: q } = packageRef(ref)
      return api.post<CancelAnalysisResponse>(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}` +
        `:cancelAnalysis` + scopeQuery(q, repository), {})
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['package', product, ref] })
      // And the LISTING, whose row carries the same tag.
      void qc.invalidateQueries({ queryKey: ['packages'] })
    },
  })
}

export function useUnavailable(product: string | undefined) {
  return useQuery({
    queryKey: ['unavailable', product],
    queryFn: () => api.get<ListUnavailableResponse>(
      `/products/${encodeURIComponent(product!)}/unavailable`),
    enabled: Boolean(product),
  })
}

// ---------------------------------------------------------------------------
// Downloads (transfers)
// ---------------------------------------------------------------------------

/**
 * Transfers, newest first.
 *
 * `operation` narrows to downloads or to promotions, and the Downloads page
 * asks TWICE rather than splitting one page of a hundred rows: a busy estate's
 * hundred most recent transfers are all downloads, and the promotions table
 * would be empty on exactly the deployments that have the most of them.
 */
/**
 * A page of transfers.
 *
 * `view: 'summary'` drops the `progress` object, and with it the twelve
 * correlated aggregates the server computes over each transfer's jobs to
 * produce it. That rollup IS the cost of this listing - a hundred rows take
 * about 154ms with it and about 1ms without - and the pages that join transfer
 * history onto a package listing (Overview, Packages, Products) read only
 * names, states and timestamps. They ask for the summary; a page that draws a
 * progress bar leaves `view` off and pays for the counts it draws.
 */
export function useTransfers(
  filters: {
    product?: string; state?: string; operation?: string
    pageSize?: number; pageToken?: string; view?: 'summary'
  } = {},
) {
  return useQuery({
    queryKey: ['transfers', filters],
    queryFn: () => api.get<ListTransfersResponse>(`/transfers${query({ ...filters })}`),
    // The estate view refreshes while anything is running, so Home does not go
    // stale under an operator watching a download.
    refetchInterval: (q) =>
      (q.state.data?.transfers ?? []).some((t) => isLive(t.state)) ? 5000 : false,
  })
}

/**
 * The estate's activity, as three numbers.
 *
 * # Why this is not `useTransfers` with a big page
 *
 * It was. The shell mounts on every page and needs "how many are moving, how
 * many are waiting, how many failed", and it got them by fetching a hundred
 * fully-projected transfers every few seconds and counting in the browser.
 *
 * Each of those rows carries a dozen aggregates over that transfer's jobs, so
 * the request costs what the ESTATE has done rather than what the shell asked
 * for - and it was issued from every open tab, for the whole life of a
 * download, against a Coordinator already busy leasing and completing jobs.
 * The server answers the same question in about a hundred and fiftieth of the
 * time.
 *
 * Polled slowly and unconditionally: it is a status line rather than a page
 * somebody is watching, and the transfer that a reader IS watching has its own
 * tight poll on the page they are on.
 */
export function useTransferActivity() {
  return useQuery({
    queryKey: ['transfer-activity'],
    queryFn: () => api.get<TransferActivity>('/transfers:activity'),
    refetchInterval: 10_000,
    // A status line that briefly disagrees with a page is better than one that
    // vanishes: the previous numbers stay on screen while the next arrive.
    placeholderData: (previous) => previous,
  })
}

export function useTransfer(id: string | undefined) {
  return useQuery({
    queryKey: ['transfer', id],
    queryFn: () => api.get<Transfer>(`/transfers/${encodeURIComponent(id!)}`),
    enabled: Boolean(id),
    // The one place a tight interval is justified: somebody is watching a
    // download happen. It stops the instant the transfer settles.
    refetchInterval: (q) => (q.state.data && isLive(q.state.data.state) ? 2000 : false),
  })
}

export function useTransferJobs(id: string | undefined, state?: string) {
  return useQuery({
    queryKey: ['jobs', id, state],
    queryFn: () => api.get<ListJobsResponse>(
      `/transfers/${encodeURIComponent(id!)}/jobs${query({ state, pageSize: 200 })}`),
    enabled: Boolean(id),
  })
}

export function useTransferFailures(id: string | undefined) {
  return useQuery({
    queryKey: ['failures', id],
    queryFn: () => api.get<ListFailuresResponse>(
      `/transfers/${encodeURIComponent(id!)}/failures`),
    enabled: Boolean(id),
  })
}

/** Retry resumes rather than restarts, and the UI says so where it is offered. */
export function useTransferControl(id: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (verb: 'retry' | 'pause' | 'resume' | 'stop' | 'delete') =>
      api.post<TransferControlResponse>(`/transfers/${encodeURIComponent(id)}:${verb}`),
    onSuccess: () => invalidateTransfer(qc, id),
  })
}

/**
 * Reordering the queue.
 *
 * Its own hook rather than another verb on the one above, because it is the one
 * control verb that carries a value - and a mutation typed `(verb, body?)`
 * would let any of the others be called with a body they do not read.
 */
export function useTransferPriority(id: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (priority: number) =>
      api.post<TransferControlResponse>(
        `/transfers/${encodeURIComponent(id)}:setPriority`,
        { priority } satisfies SetPriorityRequest),
    onSuccess: () => invalidateTransfer(qc, id),
  })
}

/** Everything that shows a transfer, so one verb refreshes all of it. */
function invalidateTransfer(qc: ReturnType<typeof useQueryClient>, id: string) {
  void qc.invalidateQueries({ queryKey: ['transfer', id] })
  void qc.invalidateQueries({ queryKey: ['failures', id] })
  void qc.invalidateQueries({ queryKey: ['transfers'] })
}

// ---------------------------------------------------------------------------
// Downloads and rules (configuration)
// ---------------------------------------------------------------------------

export function useDownloads(product: string | undefined) {
  return useQuery({
    queryKey: ['downloads', product],
    queryFn: () => api.get<ListDownloadsResponse>(
      `/products/${encodeURIComponent(product!)}/downloads`),
    enabled: Boolean(product),
    staleTime: 5 * MINUTE,
  })
}

/**
 * Every product's downloads and rules at once.
 *
 * The Downloads page shows configuration for the whole estate rather than for
 * whichever product a selector happens to be on: a page that answers "what
 * comes in automatically" with one product's answer is a page that hides the
 * rule somebody came to find. Bounded by the product count, which is 5-50.
 *
 * # `enabled`, because this is a REQUEST PER PRODUCT
 *
 * Fifty products is fifty requests, and these two feed tabs that are not the
 * one the page opens on. They were issued on every visit to Downloads whether
 * or not anybody opened Rules - a hundred requests, ahead of the two that
 * populate the table actually on screen, competing with them for the browser's
 * six connections per host.
 *
 * Configuration also does not move: five minutes of staleness means opening the
 * tab a second time costs nothing, so deferring the first fetch to the moment
 * it is needed loses nothing either.
 */
export function useDownloadsForAll(products: string[], enabled = true) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['downloads', product],
      queryFn: () => api.get<ListDownloadsResponse>(
        `/products/${encodeURIComponent(product)}/downloads`),
      staleTime: 5 * MINUTE,
      enabled,
    })),
  })
}

export function useRulesForAll(products: string[], enabled = true) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['rules', product],
      queryFn: () => api.get<ListAutoDownloadRulesResponse>(
        `/products/${encodeURIComponent(product)}/autoDownloadRules`),
      staleTime: 5 * MINUTE,
      enabled,
    })),
  })
}

/**
 * Every product's replication targets, for the drift banner.
 *
 * CACHED LIKE THE CONFIGURATION IT IS. This is the most expensive read on the
 * Downloads page by a wide margin: one request per product, and each one asks
 * every delegated target's registry what it currently looks like - a network
 * round trip apiece, and a row written recording the observation. On the shared
 * 30-second default the whole estate was re-read every time somebody navigated
 * back to this page, against a Coordinator that may be leasing and completing
 * jobs at the same time.
 *
 * Five minutes, which is what the two sibling product-configuration reads above
 * already use. Drift is somebody editing a registry by hand: it does not need
 * spotting within thirty seconds, and the target's own page reads it fresh.
 */
export function useReplicationForAll(products: string[]) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['replication', product],
      queryFn: () => api.get<ListReplicationResponse>(
        `/products/${encodeURIComponent(product)}/replication`),
      staleTime: 5 * MINUTE,
    })),
  })
}

export function useAutoDownloadRules(product: string | undefined) {
  return useQuery({
    queryKey: ['rules', product],
    queryFn: () => api.get<ListAutoDownloadRulesResponse>(
      `/products/${encodeURIComponent(product!)}/autoDownloadRules`),
    enabled: Boolean(product),
    staleTime: 5 * MINUTE,
  })
}

/**
 * Run a download by hand.
 *
 * Takes SOFTWARE, never a pattern: a pattern decides what to download when
 * nobody is asking, and this is somebody asking.
 */
export function useRunDownload(product: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: RunDownloadRequest) =>
      api.post<RunDownloadResponse>(
        `/products/${encodeURIComponent(product)}/downloads:run`, body),
    onSuccess: (_data, variables) => {
      if (variables.validateOnly) return
      void qc.invalidateQueries({ queryKey: ['transfers'] })
      void qc.invalidateQueries({ queryKey: ['packages'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Promotion
// ---------------------------------------------------------------------------

/**
 * Where a release can go next, and what sending it there would do.
 *
 * ONE call for the whole dialog, and that is deliberate. The answer needs the
 * product's promotion path, promotionOnly, which targets already hold the
 * release, whether its tree has been walked, and which promoter plugin would
 * claim each hop. Assembling it here from four endpoints would be
 * re-implementing the server's resolution rules in TypeScript, and the copy
 * that drifted would be the one people clicked.
 *
 * Not polled. It is configuration and history, and the dialog is open for
 * seconds; `enabled` is what keeps it from being fetched at all until somebody
 * opens one.
 */
export function usePromotionOptions(
  product: string | undefined, ref: string | undefined, repository?: string, enabled = true,
) {
  return useQuery({
    queryKey: ['promotion-options', product, ref, repository],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      return api.get<PromotionOptionsResponse>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}` +
        `/promotionOptions` + scopeQuery(q, repository))
    },
    enabled: Boolean(enabled && product && ref),
    staleTime: 0,
  })
}

/**
 * Promote a release from one of your targets to another.
 *
 * The SAME endpoint a replication uses, with `promote: true`. The operation is
 * derived from what `from` resolves to rather than declared, so this flag only
 * ever ASSERTS the intent - a request that named a source would be refused
 * rather than quietly copied from a vendor.
 */
export function usePromote(product: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (body: Omit<CreateTransferRequest, 'product' | 'promote'>) =>
      api.post<CreateTransferResponse>('/transfers',
        { ...body, product, promote: true } satisfies CreateTransferRequest),
    onSuccess: (_data, variables) => {
      if (variables.validateOnly) return
      void qc.invalidateQueries({ queryKey: ['transfers'] })
      void qc.invalidateQueries({ queryKey: ['packages'] })
      void qc.invalidateQueries({ queryKey: ['package'] })
      // The dialog's own answer, so reopening it says the release is on its
      // way rather than offering to send it again.
      void qc.invalidateQueries({ queryKey: ['promotion-options'] })
    },
  })
}

// ---------------------------------------------------------------------------
// Replication (the Quay mirror step)
// ---------------------------------------------------------------------------

export function useReplication(product: string | undefined) {
  return useQuery({
    queryKey: ['replication', product],
    queryFn: () => api.get<ListReplicationResponse>(
      `/products/${encodeURIComponent(product!)}/replication`),
    enabled: Boolean(product),
  })
}

export function useSyncs(product: string | undefined, target: string | undefined) {
  return useQuery({
    queryKey: ['syncs', product, target],
    queryFn: () => api.get<ListSyncsResponse>(
      `/products/${encodeURIComponent(product!)}/targets/${encodeURIComponent(target!)}/syncs`),
    enabled: Boolean(product && target),
  })
}

// ---------------------------------------------------------------------------
// Compare
// ---------------------------------------------------------------------------

/**
 * Where a comparison has got to, polled while its own request is open.
 *
 * `enabled` is the token: polling starts when a comparison starts and stops
 * when it ends. A 404 is not an error to show anybody - the comparison may be
 * running on another replica, or may have just finished - so the query is
 * allowed to fail quietly and the page falls back to saying that work is in
 * progress without a position.
 */
export function useCompareProgress(token: string | undefined) {
  return useQuery({
    queryKey: ['compare-progress', token],
    queryFn: () => api.get<CompareProgressResponse>(
      `/comparisons/${encodeURIComponent(token!)}`),
    enabled: Boolean(token),
    refetchInterval: 1000,
    retry: false,
    // A miss is a normal answer, and an error boundary would turn it into a
    // red panel over a comparison that is working perfectly well.
    throwOnError: false,
  })
}

export function useCompare() {
  return useMutation({
    mutationFn: async ({ product, ref, repository, body }: {
      product: string
      ref: string
      /** Scopes the left-hand reference, for the usual ambiguous tag. */
      repository?: string
      body: CompareRequest
    }) => {
      const { segment, query: q } = packageRef(ref)
      const path =
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}:compare` +
        scopeQuery(q, repository)

      try {
        return await api.post<CompareResponse>(path, body)
      } catch (e) {
        /*
         * An older Coordinator does not know `progressToken`, and the API
         * rejects unknown fields rather than ignoring them - deliberately, so
         * a client sending `bytesTransfered` is told rather than silently
         * reported as having moved nothing.
         *
         * The consequence for this one field is disproportionate: the whole
         * comparison fails because the client asked to be told how it was
         * going. So it is asked for ONCE, and dropped if the server has not
         * heard of it. The comparison then runs exactly as it did before -
         * without a position, which is what an old server could report anyway.
         */
        if (!isUnknownField(e, 'progressToken') || !body.progressToken) throw e
        const { progressToken: _dropped, ...rest } = body
        return await api.post<CompareResponse>(path, rest)
      }
    },
  })
}

/** Whether a failure is a server refusing a field it has never heard of. */
function isUnknownField(error: unknown, field: string): boolean {
  const message = error instanceof Error ? error.message : String(error)
  return message.includes('unknown field') && message.includes(field)
}

// ---------------------------------------------------------------------------
// Activity, reports, system
// ---------------------------------------------------------------------------

export interface AuditFilters {
  product?: string
  eventType?: string
  actor?: string
  outcome?: string
  subjectKind?: string
  subjectId?: string
  since?: string
  until?: string
  pageSize?: number
  pageToken?: string
}

export function useAuditEvents(filters: AuditFilters = {}) {
  return useQuery({
    queryKey: ['audit', filters],
    queryFn: () => api.get<ListAuditEventsResponse>(`/auditEvents${query({ ...filters })}`),
  })
}

export function useReports(params: { period?: string; product?: string } = {}) {
  return useQuery({
    queryKey: ['reports', params],
    queryFn: () => api.get<ReportSummary>(`/reports/summary${query({ ...params })}`),
    staleTime: MINUTE,
  })
}

export function useWorkers() {
  return useQuery({
    queryKey: ['workers'],
    queryFn: () => api.get<ListWorkersResponse>('/workers'),
    refetchInterval: 15_000,
  })
}

/**
 * Measure what one source-to-target path can actually do.
 *
 * # Why this is a mutation and not a query
 *
 * It has real side effects. The read probe pulls blobs from the source, and
 * the write probe opens upload sessions on the target and streams bytes into
 * them before cancelling - nothing is committed anywhere, and both registries
 * see genuine load. That is not something to run because a component mounted,
 * or to refetch when a window regains focus.
 *
 * # Why the answer is not stored
 *
 * A calibration is a measurement of a network AS IT WAS. Keeping one would
 * produce exactly the stale number this feature exists to replace: the point
 * is to answer "is the speed I am looking at right now the best this path can
 * do", and that has to be measured now.
 */
export function useCalibrate(product: string | undefined) {
  return useMutation({
    mutationFn: (body: CalibrateRequest) =>
      api.post<CalibrateResponse>(
        `/products/${encodeURIComponent(product!)}:calibrate`, body),
    // A path being slow enough to investigate is a path whose probes may take
    // minutes and may fail. Retrying doubles the load for no new information.
    retry: false,
  })
}

export function useVersion() {
  return useQuery({
    queryKey: ['version'],
    queryFn: () => api.get<VersionResponse>('/system/version'),
    staleTime: Infinity,
  })
}

export function useDeepHealth(enabled: boolean) {
  return useQuery({
    queryKey: ['health'],
    queryFn: () => api.get<HealthCheckResponse>('/system:healthCheck'),
    enabled,
    staleTime: 30_000,
    retry: false,
  })
}

// ---------------------------------------------------------------------------
// Security
// ---------------------------------------------------------------------------

/**
 * A release's stored security state.
 *
 * # Why this polls, and only sometimes
 *
 * The read is a database query, so it is cheap enough to poll - and while a
 * sync is running it is the only channel that says how far it has got, because
 * the sync's live position travels in this same response rather than on an
 * endpoint of its own. The moment the sync settles the interval drops to false
 * and the page stops asking.
 *
 * # Why `detail` is a parameter
 *
 * The findings of a large release are megabytes. A page showing counts and a
 * coverage bar needs none of them; a page listing CVEs needs all of them.
 */
export function usePackageSecurity(
  product: string | undefined,
  ref: string | undefined,
  opts: { repository?: string; detail?: boolean; enabled?: boolean } = {},
) {
  const { repository, detail = false, enabled = true } = opts
  const qc = useQueryClient()
  const result = useQuery({
    queryKey: ['package-security', product, ref, repository, detail],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      const scoped = scopeQuery(q, repository)
      const extra = query({ detail })
      const suffix = scoped ? scoped + (extra ? '&' + extra.slice(1) : '') : extra
      return api.get<PackageSecurityResponse>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}/security` +
        suffix)
    },
    enabled: enabled && Boolean(product && ref),
    // While a sync runs this is the progress feed; once it settles there is
    // nothing to watch, and a page nobody is looking at stops polling anyway
    // because TanStack unmounts the query.
    // A stalled claim is not a running sync, and polling one twice a second
    // asks the server the same question forever about work nobody is doing.
    refetchInterval: (q) => {
      const sync = q.state.data?.sync
      return sync?.state === 'syncing' && !sync.stalled ? 1500 : false
    },
    /*
     * A deployment with no security storage answers 404 on this route,
     * deliberately - an honest absence rather than a route that always fails.
     * Not worth retrying and not worth an error panel: the tab renders "not
     * configured" and the rest of the page is unaffected.
     */
    retry: false,
    throwOnError: false,
  })

  /*
   * TELL THE REST OF THE PAGE WHEN THE SYNC ENDS.
   *
   * This query polls itself while a sync runs, so it learns the moment the job
   * finishes - but the release itself carries the same state, and the header
   * tag, the tab label and the listing row all read THAT. Invalidating only
   * when the sync STARTS left them saying "syncing…" over a panel that had
   * already reported the counts, until something else happened to refetch.
   *
   * The transition is what matters, not the state: firing on every settled
   * poll would invalidate the release query forever.
   */
  const wasSyncing = useRef(false)
  const state = result.data?.sync.state
  useEffect(() => {
    if (state === 'syncing') {
      wasSyncing.current = true
      return
    }
    if (!wasSyncing.current || state === undefined) return
    wasSyncing.current = false
    void qc.invalidateQueries({ queryKey: ['package'] })
    void qc.invalidateQueries({ queryKey: ['packages'] })
  }, [state, qc])

  return result
}

/**
 * Start a vulnerability sync.
 *
 * Returns as soon as the work is CLAIMED, never when it is finished: a real
 * release is minutes of scanner queries. The page then polls usePackageSecurity
 * and watches `sync`.
 *
 * A status of `already_running` is not a failure - somebody else pressed the
 * button first, and the thing the caller wanted is happening.
 */
export function useSyncPackageSecurity() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ product, ref, repository, force }: {
      product: string
      ref: string
      repository?: string
      /**
       * Ask the scanner about every image, even ones already answered for.
       *
       * The default reuses a stored answer that is inside the deployment's max
       * age, which for a release sharing its images with a recently synced one
       * is most of the release. Forcing is minutes of somebody else's scanner,
       * so it is a separate thing to press.
       */
      force?: boolean
    }) => {
      const { segment, query: q } = packageRef(ref)
      return api.post<SyncSecurityResponse>(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}:syncSecurity` +
        scopeQuery(q, repository), force ? { force: true } : {})
    },
    onSuccess: () => {
      // The listing carries the same counts, so it has to learn that one of
      // its rows just changed state.
      void qc.invalidateQueries({ queryKey: ['package-security'] })
      void qc.invalidateQueries({ queryKey: ['packages'] })
      void qc.invalidateQueries({ queryKey: ['package'] })
      // And any comparison this release is an end of. Marking it stale now
      // rather than refetching now is the point: the sync takes minutes, so
      // the useful moment to re-run a comparison is when somebody next looks
      // at it, which is exactly what a stale key does.
      void qc.invalidateQueries({ queryKey: ['compare-security'] })
    },
  })
}

/**
 * Stop a running vulnerability sync.
 *
 * The claim is released server-side rather than only here, so a sync running on
 * another Coordinator stops too - it notices at its next heartbeat that it no
 * longer holds one. A job somebody can start and cannot stop is a job they
 * learn not to start.
 */
export function useCancelPackageSecuritySync() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ product, ref, repository }: {
      product: string
      ref: string
      repository?: string
    }) => {
      const { segment, query: q } = packageRef(ref)
      return api.post<CancelSecuritySyncResponse>(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}:cancelSecuritySync` +
        scopeQuery(q, repository), {})
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['package-security'] })
      void qc.invalidateQueries({ queryKey: ['packages'] })
      void qc.invalidateQueries({ queryKey: ['package'] })
    },
  })
}

/**
 * The security comparison of two releases.
 *
 * Both sides come from storage, so this is two indexed reads and an in-memory
 * classification - fast enough to run on a page load, with no progress worth
 * reporting. A release nobody has synced comes back as inconclusive rather than
 * quietly starting a multi-minute scan behind a button.
 *
 * # Why a query and not a mutation, given that it POSTs
 *
 * Because it READS. The verb is POST only because the second release will not
 * survive a URL path segment, and TanStack's queries are indifferent to the
 * verb. Three things follow from making it a query, and all three are the
 * reason:
 *
 *   - it is keyed and cached, so switching to the contents tab and back does
 *     not re-run a comparison of a hundred and seventy thousand findings;
 *   - it runs from `enabled` rather than from an effect that fires a mutation,
 *     which is a shape that cannot be got wrong on a re-render;
 *   - it survives StrictMode. A mutation started in a mount effect is orphaned
 *     by React's development double-mount - the fresh observer is not attached
 *     to the request the previous one started - so the response arrived and the
 *     page sat on its loading skeleton for ever. That was every local run of
 *     this page.
 *
 * `staleTime: Infinity` because the answer changes only when a sync writes new
 * results, and the sync path invalidates this key itself.
 */
export function useCompareSecurity(
  args: {
    product: string | undefined
    ref: string | undefined
    repository?: string
    against: string | undefined
  },
  enabled: boolean,
) {
  const { product, ref, repository, against } = args
  return useQuery({
    queryKey: ['compare-security', product, ref, repository, against],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      const body: SecurityCompareRequest = { against }
      return api.post<SecurityComparisonResponse>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}:compareSecurity` +
        scopeQuery(q, repository), body)
    },
    enabled: enabled && Boolean(product && ref && against),
    staleTime: Infinity,
    retry: false,
    throwOnError: false,
  })
}

/**
 * Search across everything a sync has recorded.
 *
 * Debouncing is the caller's job - this fires on whatever query it is given -
 * because the right delay depends on whether somebody is typing a package name
 * or pasting a CVE id, and the page knows which.
 */
export function useSecuritySearch(
  product: string | undefined,
  kind: SearchKind,
  q: string,
  opts: { exact?: boolean; limit?: number } = {},
) {
  return useQuery({
    queryKey: ['security-search', product, kind, q, opts.exact, opts.limit],
    queryFn: () => api.get<SecuritySearchResponse>(
      `/products/${encodeURIComponent(product!)}/security/search` +
      query({ kind, q, exact: opts.exact, limit: opts.limit })),
    enabled: Boolean(product) && q.trim().length > 0,
    staleTime: MINUTE,
    retry: false,
    throwOnError: false,
  })
}

/**
 * The URL of an export.
 *
 * A URL rather than a fetch, because a download is a link: the browser streams
 * it, names it from the Content-Disposition, and shows its own progress. Doing
 * it through fetch would mean holding a large file in memory to hand it back to
 * the browser, and losing the filename on the way.
 */
export function packageSecurityExportUrl(
  product: string, ref: string,
  opts: {
    format: string
    view: string
    repository?: string
    severity?: string
    fixable?: boolean | undefined
    status?: string
    q?: string
    /**
     * Which table a single-table format writes.
     *
     * A workbook holds every table and a CSV holds one, and which one depends
     * on what the reader is about to do: somebody pivoting on packages wants
     * All findings, somebody handing a list of advisories to a vendor wants
     * Unique CVEs. Guessing produced the complaint this answers - the file came
     * back with a table that was not the one on screen.
     */
    table?: string
  },
): string {
  const { segment, query: scoped } = packageRef(ref)
  const params = query({
    format: opts.format,
    view: opts.view,
    table: opts.table,
    repository: opts.repository ?? (scoped ? decodeURIComponent(scoped.replace('?repository=', '')) : ''),
    severity: opts.severity,
    fixable: opts.fixable === undefined ? undefined : String(opts.fixable),
    status: opts.status,
    q: opts.q,
  })
  return `/api/v1/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}` +
    `/security/export${params}`
}

/**
 * One image's scanner document, as text, for reading in the drawer.
 *
 * # Why this is a query and not part of the security response
 *
 * Because a release is 157 images and an SBOM is tens of megabytes: a response
 * carrying them would be gigabytes of JSON to render a table of numbers. The
 * security response says WHICH documents exist and how big each is; this
 * fetches one, when somebody opens it.
 *
 * `enabled` is what makes that true - it is false until a reader picks a
 * document - and `url` comes from the server rather than being rebuilt here,
 * so there is one place that knows how a document is addressed.
 */
export function useSecurityDocument(url: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ['security-document', url],
    queryFn: () => fetchText(url!),
    enabled: Boolean(url) && enabled,
    // A scanner's answer for one image does not change until the next sync, and
    // a reader flicking between four tabs should not re-fetch megabytes each
    // time they come back.
    staleTime: 10 * MINUTE,
    retry: false,
    throwOnError: false,
  })
}

/**
 * The URL of one image's scanner document - its SBOM, its raw vulnerability
 * response, its policy verdict, its malware list.
 *
 * A URL rather than a fetch for the same reason as an export, and with one
 * extra: an SBOM for a large image is tens of megabytes, and pulling it through
 * JavaScript to hand it back to the browser is a tab that hangs on a file the
 * browser would have streamed to disk.
 *
 * The server usually gives us this URL on the report itself. This builder is
 * the fallback for a client assembling one before the report has loaded.
 */
export function securityDocumentUrl(
  product: string, ref: string, digest: string,
  kind: 'vulnerabilities' | 'sbom' | 'policy' | 'malware',
  repository?: string,
): string {
  const { segment } = packageRef(ref)
  const params = query({ digest, repository })
  return `/api/v1/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}` +
    `/security/documents/${encodeURIComponent(kind)}${params}`
}

/** The URL of a security comparison export. */
export function securityComparisonExportUrl(
  product: string, ref: string, against: string,
  opts: {
    format: string
    view: string
    repository?: string
    change?: string
    severity?: string
    fixable?: boolean | undefined
    q?: string
  },
): string {
  const { segment } = packageRef(ref)
  const params = query({
    against,
    format: opts.format,
    view: opts.view,
    repository: opts.repository,
    change: opts.change,
    severity: opts.severity,
    fixable: opts.fixable === undefined ? undefined : String(opts.fixable),
    q: opts.q,
  })
  return `/api/v1/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}` +
    `/security/compare/export${params}`
}

/** The URL of a search export. */
export function securitySearchExportUrl(
  product: string, kind: SearchKind, q: string, format: string, exact?: boolean,
): string {
  return `/api/v1/products/${encodeURIComponent(product)}/security/search/export` +
    query({ kind, q, format, exact })
}

/* ---------------------------------------------------------------------------
 * Compliance: does this release follow the organization's own Kubernetes and
 * CNF standards.
 * ------------------------------------------------------------------------ */

/**
 * One release's compliance, and the live position of a run when one is going.
 *
 * Both travel in one response on purpose: a tab that polled progress on one
 * endpoint and results on another would show a finished run with a spinner
 * still on it for as long as the two were out of step.
 */
export function usePackageCompliance(
  product: string | undefined,
  ref: string | undefined,
  opts: {
    repository?: string
    enabled?: boolean
    /** Include passes and skips - the coverage half of the report. */
    all?: boolean
    outcome?: string[]
    severity?: string[]
    chart?: string[]
    determinacy?: string[]
    search?: string
    limit?: number
  } = {},
) {
  const { repository, enabled = true, all, outcome, severity, chart, determinacy, search, limit } = opts
  const qc = useQueryClient()

  const result = useQuery({
    queryKey: ['package-compliance', product, ref, repository,
      all, outcome, severity, chart, determinacy, search, limit],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      const scoped = scopeQuery(q, repository)
      const extra = query({
        all: all ? 'true' : '',
        outcome: outcome?.join(','),
        severity: severity?.join(','),
        chart: chart?.join(','),
        determinacy: determinacy?.join(','),
        q: search,
        limit,
      })
      const suffix = scoped ? scoped + (extra ? '&' + extra.slice(1) : '') : extra
      return api.get<PackageComplianceResponse>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}/compliance` +
        suffix)
    },
    enabled: enabled && Boolean(product && ref),
    /*
     * Polls only while a run is live. A run takes minutes and the stages move
     * visibly, so a second is the interval at which the number on screen is
     * worth reading; once it settles there is nothing to watch.
     */
    refetchInterval: (q) => (q.state.data?.progress ? 1000 : false),
    /*
     * A deployment with compliance switched off answers 404 here, deliberately
     * - an honest absence rather than a route that always fails. Not worth
     * retrying and not worth an error panel: the tab says "not configured" and
     * the rest of the page is unaffected.
     */
    retry: false,
    throwOnError: false,
  })

  /*
   * Tell the rest of the page when a run ENDS.
   *
   * This query polls itself while a run is going, so it learns the moment the
   * work finishes - but the release itself carries the summary that the tab
   * label and the listing row read. Without this they would say "running" over
   * a panel that had already reported the verdict.
   *
   * The TRANSITION is what matters: firing on every settled poll would
   * invalidate the release query forever.
   */
  const wasRunning = useRef(false)
  const running = Boolean(result.data?.progress)
  useEffect(() => {
    if (running) {
      wasRunning.current = true
      return
    }
    if (!wasRunning.current) return
    wasRunning.current = false
    void qc.invalidateQueries({ queryKey: ['package'] })
    void qc.invalidateQueries({ queryKey: ['packages'] })
  }, [running, qc])

  return result
}

/**
 * Start a compliance check.
 *
 * Returns as soon as the work is CLAIMED, never when it is finished: a real
 * release is minutes of fetching, rendering and evaluating. The page then polls
 * usePackageCompliance and watches `progress`.
 *
 * A run already in flight is NOT a failure - somebody pressed the button first,
 * and the thing the caller wanted is happening.
 */
export function useRunCompliance() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ product, ref, repository }: {
      product: string
      ref: string
      repository?: string
    }) => {
      const { segment, query: q } = packageRef(ref)
      return api.post<ComplianceProgress>(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}/compliance:run` +
        scopeQuery(q, repository), undefined)
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['package-compliance'] })
      void qc.invalidateQueries({ queryKey: ['package'] })
    },
  })
}

/** Stop a running check. */
export function useCancelCompliance() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ product, ref, repository }: {
      product: string
      ref: string
      repository?: string
    }) => {
      const { segment, query: q } = packageRef(ref)
      return api.post<{ cancelled: boolean }>(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}/compliance:cancel` +
        scopeQuery(q, repository), undefined)
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['package-compliance'] })
    },
  })
}

/** A release's run history, including the ones that failed. */
export function usePackageComplianceRuns(
  product: string | undefined,
  ref: string | undefined,
  opts: { repository?: string; enabled?: boolean } = {},
) {
  const { repository, enabled = true } = opts
  return useQuery({
    queryKey: ['package-compliance-runs', product, ref, repository],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      return api.get<ComplianceRunsResponse>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}/compliance/runs` +
        scopeQuery(q, repository))
    },
    enabled: enabled && Boolean(product && ref),
    retry: false,
    throwOnError: false,
  })
}

/**
 * The rulebook: every loaded check and what it asserts.
 *
 * Not scoped to a product or a release - it is what WILL be checked, and the
 * person most likely to want it is a vendor who has not shipped yet.
 *
 * Cached hard: it changes when somebody edits a policy pack on the Coordinator,
 * which is not something a page needs to poll for.
 */
export function usePolicies(opts: { enabled?: boolean } = {}) {
  const { enabled = true } = opts
  return useQuery({
    queryKey: ['policies'],
    queryFn: () => api.get<PolicyCatalogueResponse>('/policies'),
    enabled,
    staleTime: 5 * 60 * 1000,
    retry: false,
    throwOnError: false,
  })
}
