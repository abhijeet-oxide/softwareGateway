import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, query, packageRef } from './client'
import type {
  CheckConnectivityResponse, CompareRequest, CompareResponse, DiscoverAllResponse,
  DiscoverPackagesResponse, DiscoveryStatusResponse, HealthCheckResponse, ListArtifactsResponse,
  ListAuditEventsResponse, ListAutoDownloadRulesResponse, ListDownloadsResponse,
  ListFailuresResponse, ListJobsResponse, ListPackagesResponse, ListProductsResponse,
  ListReplicationResponse, ListSyncsResponse, ListTransfersResponse, ListUnavailableResponse,
  ListWorkersResponse, Package, ReportSummary, RunDownloadRequest, RunDownloadResponse,
  Transfer, TransferControlResponse, VersionResponse,
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

export function useDiscoveryStatus(product: string | undefined, live: boolean) {
  return useQuery({
    queryKey: ['discovery', product],
    queryFn: () => api.get<DiscoveryStatusResponse>(
      `/products/${encodeURIComponent(product!)}/discovery`),
    enabled: Boolean(product),
    // Only while a scan is actually running. A finished scan is not news.
    refetchInterval: live ? 3000 : false,
  })
}

export function useRunDiscovery() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (product?: string): Promise<DiscoverPackagesResponse | DiscoverAllResponse> =>
      product
        ? api.post<DiscoverPackagesResponse>(
            `/products/${encodeURIComponent(product)}/packages:discover`)
        : api.post<DiscoverAllResponse>('/products:discover'),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['packages'] })
      void qc.invalidateQueries({ queryKey: ['discovery'] })
    },
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

export function usePackage(product: string | undefined, ref: string | undefined) {
  return useQuery({
    queryKey: ['package', product, ref],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      return api.get<Package>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}${q}`)
    },
    enabled: Boolean(product && ref),
  })
}

export function useArtifacts(product: string | undefined, ref: string | undefined, enabled = true) {
  return useQuery({
    queryKey: ['artifacts', product, ref],
    queryFn: () => {
      const { segment, query: q } = packageRef(ref!)
      return api.get<ListArtifactsResponse>(
        `/products/${encodeURIComponent(product!)}/packages/${encodeURIComponent(segment)}/artifacts${q}`)
    },
    enabled: enabled && Boolean(product && ref),
  })
}

/** Expanding a package writes artifacts, blobs and a measured size. */
export function useInspectPackage(product: string, ref: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => {
      const { segment, query: q } = packageRef(ref)
      return api.post(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}:inspect${q}`)
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['package', product, ref] })
      void qc.invalidateQueries({ queryKey: ['artifacts', product, ref] })
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
    mutationFn: (verb: 'retry' | 'pause' | 'resume' | 'stop') =>
      api.post<TransferControlResponse>(`/transfers/${encodeURIComponent(id)}:${verb}`),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ['transfer', id] })
      void qc.invalidateQueries({ queryKey: ['failures', id] })
      void qc.invalidateQueries({ queryKey: ['transfers'] })
    },
  })
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

export function useCompare() {
  return useMutation({
    mutationFn: ({ product, ref, body }: { product: string; ref: string; body: CompareRequest }) => {
      const { segment, query: q } = packageRef(ref)
      return api.post<CompareResponse>(
        `/products/${encodeURIComponent(product)}/packages/${encodeURIComponent(segment)}:compare${q}`,
        body)
    },
  })
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
