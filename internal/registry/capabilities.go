package registry

// Capabilities records what a registry supports.
//
// PROBED, not hardcoded by vendor. The same product behaves differently across
// versions and storage backends - an Artifactory on S3 and one on filesystem
// differ - so a table keyed by registry_type would encode a lie that ages
// badly. See docs/design/06 §3.
type Capabilities struct {
	// SupportsMount is cross-repository blob mount. M3's biggest lever for
	// promotion: zero bytes over the wire regardless of blob size.
	SupportsMount bool
	// SupportsChunkedUpload is PATCH with Range.
	SupportsChunkedUpload bool
	// SupportsResumeUpload means an upload session survives a client
	// disconnect. Cannot be determined by probing - it requires actually
	// dropping a connection - so it starts at the vendor default and is
	// corrected by observation. See docs/design/05 §4.6.
	SupportsResumeUpload bool
	// SupportsReferrersAPI is the OCI 1.1 /v2/<name>/referrers/<digest>
	// endpoint. When false, signature discovery falls back to the tag schema.
	SupportsReferrersAPI bool
	// SupportsCatalog is /v2/_catalog. We do not use it for discovery -
	// repositories come from configuration - but it is reported for
	// diagnostics.
	SupportsCatalog bool
	// TagPagination is how this registry paginates tags/list.
	TagPagination PaginationStyle
	// MaxChunkSize is the largest accepted upload chunk; 0 means unknown.
	MaxChunkSize int64
}

// PaginationStyle is how a registry signals more pages.
type PaginationStyle string

const (
	// PaginationLink is the conventional RFC 8288 Link header. The default.
	PaginationLink PaginationStyle = "link"
	// PaginationNone means the registry returns every tag in one response.
	PaginationNone PaginationStyle = "none"
)

// DefaultCapabilities is the conservative starting assumption: the registry
// speaks base OCI Distribution v2 and nothing optional.
//
// Assuming absence is the safe direction - a capability we wrongly believe
// present produces a failed operation, while one we wrongly believe absent
// only costs a slower path.
func DefaultCapabilities() Capabilities {
	return Capabilities{TagPagination: PaginationLink}
}

// Probe results are memoised by each backend, not here.
//
// An earlier draft had a keyed capabilityCache in this package. It was deleted:
// a Repository already probes once behind a sync.Once and is itself long-lived,
// so the cache duplicated machinery that exists where it belongs. Two caches
// for one fact is how they drift.
