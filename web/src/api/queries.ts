import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, query, packageRef } from './client'
import type {
  CheckConnectivityResponse, CompareProgressResponse, CompareRequest, CompareResponse,
  DiscoverAllResponse,
  DiscoverPackagesResponse, DiscoveryStatusResponse, HealthCheckResponse, ListArtifactsResponse,
  ListAuditEventsResponse, ListAutoDownloadRulesResponse, ListDownloadsResponse,
  InspectPackageResponse, ListFailuresResponse, ListJobsResponse, ListPackagesResponse, ListProductsResponse,
  ListReplicationResponse, ListSyncsResponse, ListTransfersResponse, ListUnavailableResponse,
  ListPackageFilesResponse, ListWorkersResponse, Package, PackageFileContentResponse,
  ReportSummary, RunDownloadRequest,
  RunDownloadResponse,
  SetPriorityRequest, Transfer, TransferControlResponse, VersionResponse,
} from './types'
import { isLive } from '../domain/derive'

/**
 * Every server call the application makes, in one place.
 *
 * # On polling
 *
 * There is no server-sent-events channel yet — gate G4 in docs/design/19 §5 is
 * unmet — so live progress is polled. The rule enforced here is that polling
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
 * not a failure and not a no-op — it is "a scan is already going" — and the
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
  return useQuery({
    queryKey: ['package', product, ref, repository],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      return api.get<Package>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}` +
        scopeQuery(q, repository))
    },
    enabled: Boolean(product && ref),
  })
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
 * Read from what analysis recorded, so it troubles no registry — and it is
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
 * Enabled only when a digest is asked for — the query is what a modal opening
 * looks like, so mounting the hook without one must fetch nothing. Cached by
 * digest, which is the whole identity of the content: the same file opened
 * twice is served from memory, and content addressing means it cannot be stale.
 */
export function usePackageFileContent(
  product: string | undefined, ref: string | undefined, digest: string | undefined,
  repository?: string,
) {
  const { segment, query: q } = packageRef(ref ?? '')
  const scope = scopeQuery(q, repository)
  return useQuery({
    queryKey: ['package-file', product, ref, repository, digest],
    queryFn: () => api.get<PackageFileContentResponse>(
      `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}`
      + `/files/content${scope ? `${scope}&` : '?'}digest=${encodeURIComponent(digest!)}`),
    enabled: Boolean(product && ref && digest),
    staleTime: Infinity,
  })
}

/**
 * Analyse a release: walk its manifest tree and record what it is made of.
 *
 * Synchronous by design — the API has no progress feed for it, because the
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
        scopeQuery(q, repository))
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['package', product, ref] })
      void qc.invalidateQueries({ queryKey: ['artifacts', product, ref] })
      // The files ARE the point of analysing for most readers: the walk is what
      // turns "two layers" into two hundred named paths.
      void qc.invalidateQueries({ queryKey: ['package-files', product, ref] })
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

export function useTransfers(filters: { product?: string; state?: string; pageSize?: number } = {}) {
  return useQuery({
    queryKey: ['transfers', filters],
    queryFn: () => api.get<ListTransfersResponse>(`/transfers${query({ ...filters })}`),
    // The estate view refreshes while anything is running, so Home does not go
    // stale under an operator watching a download.
    refetchInterval: (q) =>
      (q.state.data?.transfers ?? []).some((t) => isLive(t.state)) ? 5000 : false,
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
 * control verb that carries a value — and a mutation typed `(verb, body?)`
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
 */
export function useDownloadsForAll(products: string[]) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['downloads', product],
      queryFn: () => api.get<ListDownloadsResponse>(
        `/products/${encodeURIComponent(product)}/downloads`),
      staleTime: 5 * MINUTE,
    })),
  })
}

export function useRulesForAll(products: string[]) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['rules', product],
      queryFn: () => api.get<ListAutoDownloadRulesResponse>(
        `/products/${encodeURIComponent(product)}/autoDownloadRules`),
      staleTime: 5 * MINUTE,
    })),
  })
}

export function useReplicationForAll(products: string[]) {
  return useQueries({
    queries: products.map((product) => ({
      queryKey: ['replication', product],
      queryFn: () => api.get<ListReplicationResponse>(
        `/products/${encodeURIComponent(product)}/replication`),
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
 * when it ends. A 404 is not an error to show anybody — the comparison may be
 * running on another replica, or may have just finished — so the query is
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
         * rejects unknown fields rather than ignoring them — deliberately, so
         * a client sending `bytesTransfered` is told rather than silently
         * reported as having moved nothing.
         *
         * The consequence for this one field is disproportionate: the whole
         * comparison fails because the client asked to be told how it was
         * going. So it is asked for ONCE, and dropped if the server has not
         * heard of it. The comparison then runs exactly as it did before —
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
