// Package product owns product configuration: the schema, loading, validation
// and hot reload.
//
// See docs/design/02-configuration.md.
//
// A Product is the root aggregate - the unit of configuration, ownership and
// blast radius. One product is one ConfigMap, one YAML document; everything
// about that product lives in that one place.
package product

import (
	"strings"
	"time"
)

// APIVersion and Kind identify the document. These exist even though products
// are ConfigMaps rather than CRDs, so the schema is versioned and migratable
// if the CRD decision is ever revisited. See docs/design/02 section 2.
const (
	APIVersion = "softwaregateway.io/v1alpha1"
	Kind       = "Product"
)

// Product is one vendor product and everything needed to replicate it.
type Product struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`

	// SourceFile records where this document was loaded from, for error
	// messages and audit. Not part of the document itself.
	SourceFile string `json:"-"`
	// ConfigHash is the sha256 of the canonical document bytes, recorded so an
	// audit record from March still resolves to the config in force in March.
	ConfigHash string `json:"-"`

	// Deprecations lists superseded keys this document still uses.
	//
	// Warnings, not errors: a document that keeps working is worth more than a
	// tidy schema, and an operator who has just been paged is not the person to
	// hand a migration to. `transferctl config check` reports them.
	Deprecations []string `json:"-"`

	// Warnings lists configurations that are valid and probably not intended.
	//
	// Distinct from Deprecations, which are about the schema moving on. A
	// warning is about THIS document being self-defeating - see warnings.go.
	Warnings []Warning `json:"-"`
}

type Metadata struct {
	// Name is the resource ID: lowercase alphanumeric and hyphens, <=63 chars
	// (AIP-122). Immutable; appears in API paths, metric labels and audit.
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName,omitempty"`
	Description string            `json:"description,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`

	// Enabled turns the whole product off without deleting its configuration.
	// Defaults to true; use Product.IsEnabled rather than reading it.
	//
	// Deleting a document to pause a product loses the thing you most want
	// back: the exact registries, credentials, filters and rules that were
	// working. Re-creating it from memory during an incident is how a
	// "temporary" pause becomes a subtly different configuration.
	//
	// A disabled product still loads and still VALIDATES, so a mistake in it is
	// reported now rather than discovered on the day someone re-enables it.
	// Its already-discovered packages are kept.
	Enabled *bool `json:"enabled,omitempty"`
}

type Spec struct {
	Sources   []Source   `json:"sources"`
	Targets   []Target   `json:"targets"`
	Promotion *Promotion `json:"promotion,omitempty"`
	// Download is WHAT happens; AutoDownload is WHEN it happens by itself.
	// They are separate because an auto-download rule does not do the
	// downloading - it triggers a download, which is the same operation a
	// person performs by hand.
	Download      []Download          `json:"download,omitempty"`
	AutoDownload  AutoDownload        `json:"autoDownload,omitempty"`
	Verification  Verification        `json:"verification,omitempty"`
	Notifications Notifications       `json:"notifications,omitempty"`
	Network       Network             `json:"network,omitempty"`
	Retention     map[string]Duration `json:"retention,omitempty"`
}

// Source is a vendor-side registry location, read-only, polled by discovery.
//
// A source names ONE REGISTRY and one or more repositories on it. A product
// whose packages are spread across several repositories - one per component -
// declares them all under a single source, because they share a registry host,
// one credential and one rate-limit budget. Splitting them into separate
// sources would duplicate all three and let the per-repository budgets multiply
// against a vendor that only sees one client.
type Source struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`

	// Enabled turns this source off entirely. Defaults to true.
	//
	// Distinct from `discovery.enabled`, and the difference is worth knowing:
	//
	//	enabled: false             the source does not exist for any purpose
	//	discovery.enabled: false   the source exists and can be transferred
	//	                           from on request, but is not polled
	//
	// Use this one when a vendor relationship is paused; use discovery.enabled
	// when you only want to stop the polling - a failover mirror, say, which
	// must stay usable but must not double-discover every tag.
	Enabled *bool `json:"enabled,omitempty"`

	// Repository names a single repository. Equivalent to a one-element
	// Repositories, and kept because the single-repository case is the common
	// one and `repository: platform/suite` reads better than a list of one.
	Repository string `json:"repository,omitempty"`

	// Repositories names several explicitly. Use this when a product ships as
	// separate repositories - platform/core, platform/db, platform/ui.
	//
	// LEAVING BOTH EMPTY IS THE INTERESTING CASE: it means "every repository on
	// this registry", found from the catalog and narrowed by
	// discovery.repositoryFilters. That is what a product needs when a new
	// component ships as a new repository and nobody edits the ConfigMap in
	// time - which is the normal way this goes wrong.
	Repositories []string `json:"repositories,omitempty"`

	Type           RegistryType    `json:"type,omitempty"`
	Anonymous      bool            `json:"anonymous,omitempty"`
	CredentialsRef *CredentialsRef `json:"credentialsRef,omitempty"`
	Discovery      Discovery       `json:"discovery,omitempty"`

	// XrayEnabled switches on the JFrog Xray integration for this repository,
	// which then reuses this source's own registry, credential, CA bundle,
	// proxy and timeouts. Valid only on a JFrog type. See xray.go for why it
	// is one field rather than a block.
	XrayEnabled *bool `json:"xrayEnabled,omitempty"`
	// XrayEndpoint overrides the JFrog PLATFORM base URL, needed only where
	// the docker host is a subdomain and the platform is not.
	XrayEndpoint string `json:"xrayEndpoint,omitempty"`

	// Vendor names the PUBLISHING CONVENTION this source follows: `near` for a
	// Nokia NEAR registry, empty (or `auto`) for anything conformant.
	//
	// It is the switch for every vendor-specific behaviour there is, and it is
	// opt-in per source. Without it a NEAR registry is read as an ordinary one -
	// three packages per release rather than one, no signature grouping, no
	// shortening - and, just as importantly, a registry that is NOT NEAR gets
	// none of NEAR's rewriting. That second half is why this field exists:
	// `orbs/` was being trimmed off repository paths and `orb_` off tags for
	// every source, on the strength of what a page of results happened to look
	// like rather than on a statement about the vendor.
	//
	// Deliberately separate from Type, which says how to SPEAK to the registry -
	// and for every vendor met so far, including NEAR, that is plain OCI
	// Distribution v2. Protocol and publishing convention vary independently, so
	// they are two fields.
	//
	// Supersedes `signatures.layout`, which is still accepted and means the same
	// thing; see VendorLayout.
	Vendor string `json:"vendor,omitempty"`

	// Concurrency overrides the application-level limit for this one registry.
	// Almost always absent - see the Concurrency type.
	Concurrency Concurrency `json:"concurrency,omitempty"`

	// Signatures describes how this vendor lays out and formats signatures.
	Signatures Signatures `json:"signatures,omitempty"`

	// RateLimits is the superseded block. Accepted and folded into Concurrency;
	// see LegacyRateLimits.
	RateLimits LegacyRateLimits `json:"rateLimits,omitempty"`

	// Network overrides the product's TLS trust, proxy and timeouts for this
	// source. Set fields win; unset ones inherit.
	Network *Network `json:"network,omitempty"`

	// Verification overrides the product's signing trust for this source.
	//
	// Two vendors do not share a signing identity, and a product that pulls
	// from both cannot express that with one product-level block. The scalar
	// settings inherit; `cosign` replaces wholesale - see Product.VerificationFor.
	Verification *Verification `json:"verification,omitempty"`
}

// VendorLayout is the layout name this source resolves to.
//
// `vendor` wins; `signatures.layout` is the older spelling and is still
// honoured, so documents written before the field moved keep working unchanged.
// The two are the same setting: `layout` was nested under `signatures` when
// grouping tags was the only thing it controlled, and it now also decides how a
// package is NAMED - which is not a signature concern and does not read like
// one at the top of a document.
//
// Both set and disagreeing is a validation error rather than a precedence rule;
// see validateVendor.
func (s Source) VendorLayout() string {
	if v := strings.TrimSpace(s.Vendor); v != "" {
		return v
	}
	return strings.TrimSpace(s.Signatures.Layout)
}

// EnumeratesRepositories reports whether this source finds its repositories
// from the registry rather than being told them.
//
// There is deliberately no `repositoryDiscovery.enabled` flag. Naming no
// repositories IS the statement "I do not know them yet, find them" - a
// separate switch would let configuration say one thing and mean another
// (repositories listed AND discovery off, or none listed AND discovery off,
// which scans nothing while looking configured).
func (s Source) EnumeratesRepositories() bool {
	return len(s.DeclaredRepositories()) == 0
}

// DeclaredRepositories returns the repositories named in configuration.
//
// Repository and Repositories are merged and de-duplicated, so a document may
// use either or both without surprising overlap.
func (s Source) DeclaredRepositories() []string {
	out := make([]string, 0, len(s.Repositories)+1)
	seen := map[string]bool{}

	add := func(r string) {
		r = strings.Trim(strings.TrimSpace(r), "/")
		if r == "" || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}

	add(s.Repository)
	for _, r := range s.Repositories {
		add(r)
	}
	return out
}

// Filters is an include/exclude pair of RE2 patterns.
//
// Shared by tag and repository filtering: the semantics are identical - no
// include patterns admits everything, and exclude always wins over include.
type Filters struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// Signatures describes how a vendor publishes signatures.
//
// TWO independent axes, because they genuinely vary independently: a vendor may
// pair the referrers API with PKCS#7, or a wrapper index with a cosign bundle.
// One combined setting would need a new value for every pairing.
//
//	layout   WHERE the signature is:  auto | none | standard | <vendor>
//	format   HOW it is checked:       auto | cosign | pkcs7
//
// Note that `layout` is deliberately NOT the source's `type`. `type` says how to
// speak to the registry, and for every vendor we have met that is plain OCI
// Distribution v2 - including Nokia's NEAR, whose protocol is entirely
// standard. What differs is the publishing convention, which is this.
//
// Verification itself lands in M5. These settings exist now so configuration
// written today stays valid, and so discovery can already report WHETHER a
// package is signed - which it cannot do without knowing where to look.
type Signatures struct {
	// Layout selects the discovery mechanism. Empty means auto, which is
	// standard behaviour and correct for any conformant registry.
	Layout string `json:"layout,omitempty"`

	// Format selects the verifier. Empty means auto: infer from the
	// signature's own media type, which is reliable because the media type is
	// exactly what distinguishes application/pkcs7-signature from a Sigstore
	// bundle.
	Format string `json:"format,omitempty"`

	// TrustBundleRef names the Secret holding the trust material - CA roots for
	// PKCS#7, a public key or Fulcio root for cosign.
	//
	// A reference, never inline: per the standing constraint, every credential
	// and key is managed by VSO and read from a projected Secret. Read by M5.
	TrustBundleRef *SecretRef `json:"trustBundleRef,omitempty"`
}

// Target is an internal, read-write repository: a replication destination and
// a promotion endpoint in both directions.
type Target struct {
	Name     string `json:"name"`
	Registry string `json:"registry"`

	// Repository is the ONE destination path for this target.
	//
	// A target is a single repository - `internal.example.com/nokia/lab` - and
	// everything replicated to it lands under that path. What follows is the
	// registry's own addressing: tags and digests.
	//
	// One path rather than a mapping from the source's, because a target IS a
	// place: lab is one repository, production is another, and a deployment
	// points at one of them. Deriving the destination from the source would
	// mean the destination path changed whenever a vendor reorganised theirs.
	Repository string `json:"repository"`

	// Enabled turns this target off without deleting it. Defaults to true.
	//
	// A destination being decommissioned, or one whose registry is down for
	// maintenance, should stop receiving transfers without its configuration -
	// and its history - being thrown away.
	Enabled        *bool           `json:"enabled,omitempty"`
	Type           RegistryType    `json:"type,omitempty"`
	Anonymous      bool            `json:"anonymous,omitempty"`
	CredentialsRef *CredentialsRef `json:"credentialsRef,omitempty"`

	// Concurrency overrides the application-level limit for this one registry.
	Concurrency Concurrency `json:"concurrency,omitempty"`

	// RateLimits is the superseded block. Accepted and folded into Concurrency.
	RateLimits LegacyRateLimits `json:"rateLimits,omitempty"`

	// Network overrides the product's TLS trust, proxy and timeouts for this
	// target. A destination inside the datacentre and a vendor outside it
	// routinely need different routes.
	Network *Network `json:"network,omitempty"`

	// Verification overrides the product's signing trust for this target,
	// used for destination-side checks after a push.
	Verification *Verification `json:"verification,omitempty"`

	// Environment is which stage this target represents: `lab`, `production`,
	// whatever a site calls them.
	//
	// SEVERAL TARGETS MAY SHARE ONE. That is the point rather than an
	// oversight: `lab-eu` and `lab-us` are both the lab environment, and a
	// promotion between regions has to be expressible.
	//
	// Free text, because the stages a site runs are its own business. It is
	// what `transferctl transfers promote` resolves against when no explicit
	// --from/--to is given, and it carries no meaning otherwise - a product
	// that never promotes can leave it unset.
	Environment string `json:"environment,omitempty"`

	// Default marks the target used when a request names none.
	Default bool `json:"default,omitempty"`
	// PromotionOnly rejects direct replication, so a production registry can
	// be reachable only by promotion from another target.
	PromotionOnly bool `json:"promotionOnly,omitempty"`

	// Replication says HOW content gets into this target - whether our workers
	// push it, or the registry fetches it for itself. Absent means `copy`,
	// which is what every target meant before this field existed. See
	// replication.go and docs/design/18.
	Replication *Replication `json:"replication,omitempty"`

	// Quay holds the credential for this registry's CONTROL api, which is a
	// different endpoint taking a different credential from the one in
	// credentialsRef. Only needed by a non-copy mode.
	Quay *QuaySettings `json:"quay,omitempty"`

	// XrayEnabled switches on the JFrog Xray integration for this repository,
	// which then reuses this target's own registry, credential, CA bundle,
	// proxy and timeouts. Valid only on a JFrog type. See xray.go for why it is
	// one field rather than a block.
	XrayEnabled *bool `json:"xrayEnabled,omitempty"`
	// XrayEndpoint overrides the JFrog PLATFORM base URL, needed only where the
	// docker host is a subdomain and the platform is not.
	XrayEndpoint string `json:"xrayEndpoint,omitempty"`

	// JFrogEndpoint overrides the JFrog PLATFORM base URL for everything that
	// is not Xray - today, native promotion.
	//
	// Separate from XrayEndpoint only because the two can genuinely differ on
	// a split deployment, and it FALLS BACK to it: they are the same host
	// reached for the same reason in every ordinary estate, and a second field
	// that had to be filled in would be a second field to forget. Leaving both
	// empty derives the host from `registry`, which is right for a
	// repository-path deployment and wrong for a subdomain one - see
	// internal/promote/jfrog for why that cannot be told apart automatically.
	JFrogEndpoint string `json:"jfrogEndpoint,omitempty"`

	// JFrogRepositoryKey names the Artifactory repository this target lives
	// in, when it cannot be derived from `repository`.
	//
	// Needed only on a SUBDOMAIN deployment, where the repository key is in
	// the hostname (`acme-docker-prod.jfrog.io/nokia/orbs`) and the path has
	// none. On the repository-path deployment - `acme.jfrog.io/docker-prod/
	// nokia/orbs` - the first path segment IS the key and nothing has to be
	// written here.
	//
	// Only native promotion reads it, and only a JFrog target may set it.
	JFrogRepositoryKey string `json:"jfrogRepositoryKey,omitempty"`
}

// Promotion declares the hop `transfers promote` takes by default.
//
// Declared rather than hard-coded because the names of a site's stages are the
// site's. A deployment running dev -> qa -> production should not have to fight
// a tool that believes in "lab".
//
// Omitting the block entirely keeps the common default, lab -> production. It
// only has to be written when those words are wrong.
type Promotion struct {
	// From and To are ENVIRONMENT names, matched against Target.Environment.
	// Each may resolve to several targets; `promote` then refuses to guess and
	// asks for --from or --to.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// The default promotion hop, used when a product declares no `promotion`.
const (
	DefaultPromotionFrom = "lab"
	DefaultPromotionTo   = "production"
)

// PromotionPath returns the environments `promote` moves between.
func (p *Product) PromotionPath() (from, to string) {
	from, to = DefaultPromotionFrom, DefaultPromotionTo
	if p.Spec.Promotion == nil {
		return from, to
	}
	if p.Spec.Promotion.From != "" {
		from = p.Spec.Promotion.From
	}
	if p.Spec.Promotion.To != "" {
		to = p.Spec.Promotion.To
	}
	return from, to
}

// RegistryType selects the implementation. `generic` is the expected path for
// all registries; the others exist only for genuine deviations.
type RegistryType string

const (
	RegistryGeneric     RegistryType = "generic"
	RegistryACR         RegistryType = "acr"
	RegistryArtifactory RegistryType = "artifactory"
	// RegistryJFrog is the same backend as RegistryArtifactory, under the name
	// operators actually write. Accepted rather than rejected because
	// `type: jfrog` means something obvious, and a validation error over the
	// company's own name teaches nobody anything. `artifactory` stays
	// canonical; anything asking "is this JFrog" calls RegistryType.IsJFrog
	// so the two can never drift apart.
	RegistryJFrog RegistryType = "jfrog"
	RegistryQuay  RegistryType = "quay"
)

// ValidRegistryTypes is the closed set. Must match the CHECK constraint on
// repositories.registry_type in db/migrations.
var ValidRegistryTypes = []RegistryType{
	RegistryGeneric, RegistryACR, RegistryArtifactory, RegistryJFrog, RegistryQuay,
}

// Discovery governs what a source looks for, and how often.
//
// Both filters live here because discovery is what they govern: a scan finds
// REPOSITORIES and then TAGS, and each step gets a filter. Splitting them
// across the document would make it read as though they were different kinds of
// thing.
type Discovery struct {
	// Enabled defaults to true for sources; use the accessor rather than the
	// field so the nil/zero case is handled once.
	Enabled  *bool    `json:"enabled,omitempty"`
	Interval Duration `json:"interval,omitempty"`

	// RepositoryFilters narrows which repositories are scanned.
	//
	// Its main use is a source that names no repositories and therefore finds
	// them all: on a registry shared with other teams, this is what keeps the
	// scope to yours.
	RepositoryFilters Filters `json:"repositoryFilters,omitempty"`

	// TagFilters narrows which tags within those repositories are recorded.
	TagFilters TagFilters `json:"tagFilters,omitempty"`

	// MaxRepositories bounds what one scan will adopt when enumerating.
	//
	// A catalog that suddenly returns thousands of repositories is far more
	// likely to be a misconfiguration than a real change, and adopting them all
	// would point thousands of scanners at one registry.
	MaxRepositories int `json:"maxRepositories,omitempty"`

	// Concurrency is the superseded per-scan block. Accepted and folded into the
	// source's Concurrency; see LegacyScanConcurrency.
	Concurrency LegacyScanConcurrency `json:"concurrency,omitempty"`
}

// DefaultMaxRepositories caps catalog adoption when unset.
const DefaultMaxRepositories = 200

// Concurrency bounds how hard softwareGateway works ONE REGISTRY.
//
// It replaced seven interacting numbers with two, and the argument for that is
// worth keeping. A source used to carry `rateLimits.maxConcurrentDownloads`,
// `maxConcurrentUploads`, `maxConnections`, `requestsPerSecond` and `burst`,
// plus `discovery.concurrency.repositories` and `.tags` - set per source, per
// target, in every product document. Nobody could predict what a change to one
// of them would do, because the answer depended on the other six.
//
// Worse, they were not independent. Every request a scan makes goes through one
// connection pool, so THE POOL IS THE CONCURRENCY LIMIT: point more goroutines
// at a pool of 32 and you get 32 in-flight requests and a queue. The old
// defaults hid this by agreeing with each other - 4 repositories × 8 tags = 32
// = maxConnections - an agreement nobody wrote down and any edit would break.
// A pool sized above the worker count is idle sockets; below it is goroutines
// blocked on a semaphore they cannot see.
//
// So there is one number. PerRegistry is the connection pool size AND the
// number of in-flight requests, because those are the same thing.
//
// It belongs at the APPLICATION level: `concurrency.perRegistry` in system
// configuration is what a product inherits, and the per-source block below
// exists for the case that actually comes up - one fragile vendor that needs a
// smaller number than the rest of the fleet.
type Concurrency struct {
	// PerRegistry is the number of requests in flight against one registry, and
	// the size of the connection pool serving them. Zero inherits the
	// application-level value.
	PerRegistry int `json:"perRegistry,omitempty"`

	// RequestsPerSecond is a politeness ceiling ON TOP of PerRegistry, for a
	// vendor that rate-limits by rate rather than by connection count. Zero -
	// the usual case - means no artificial limit, and PerRegistry alone bounds
	// the load.
	//
	// Burst is deliberately not configurable. It was a third number whose only
	// sensible value is a small multiple of the rate, so it is derived.
	RequestsPerSecond int `json:"requestsPerSecond,omitempty"`
}

const (
	// DefaultPerRegistry is what the fleet uses unless system configuration says
	// otherwise.
	//
	// 32 rather than a rounder, more cautious number because it is what the
	// system already did: the old defaults multiplied out to exactly 32 in-flight
	// requests against a source, and this change is meant to alter the SHAPE of
	// the configuration, not the load it produces. Lower it for a vendor that
	// complains; raise it for a registry you own.
	DefaultPerRegistry = 32

	// maxPerRegistry caps whatever configuration asks for. A typo of 1000 must
	// not become a thousand concurrent connections to someone else's registry.
	maxPerRegistry = 128
)

// Resolve fills unset fields from the application-level defaults and clamps the
// result, so every consumer reads a number that is already correct.
func (c Concurrency) Resolve(app Concurrency) Concurrency {
	if c.PerRegistry <= 0 {
		c.PerRegistry = app.PerRegistry
	}
	if c.PerRegistry <= 0 {
		c.PerRegistry = DefaultPerRegistry
	}
	if c.PerRegistry > maxPerRegistry {
		c.PerRegistry = maxPerRegistry
	}
	if c.RequestsPerSecond <= 0 {
		c.RequestsPerSecond = app.RequestsPerSecond
	}
	if c.RequestsPerSecond < 0 {
		c.RequestsPerSecond = 0
	}
	return c
}

// Burst is the token-bucket burst for RequestsPerSecond.
//
// Derived rather than configured: twice the sustained rate absorbs the natural
// clumping of a scan without letting a long idle period bank enough tokens to
// arrive as a flood. Zero when there is no rate limit, which the limiter reads
// as "unlimited".
func (c Concurrency) Burst() int {
	if c.RequestsPerSecond <= 0 {
		return 0
	}
	return c.RequestsPerSecond * 2
}

// LegacyRateLimits is the superseded `rateLimits` block.
//
// Still parsed, because operators have these in live ConfigMaps and silently
// ignoring a number someone deliberately set is worse than either honouring it
// or rejecting it. Folded into Concurrency by Source.resolveConcurrency, which
// also records a deprecation so `transferctl config check` can say so.
type LegacyRateLimits struct {
	MaxConcurrentDownloads int `json:"maxConcurrentDownloads,omitempty"`
	MaxConcurrentUploads   int `json:"maxConcurrentUploads,omitempty"`
	MaxConnections         int `json:"maxConnections,omitempty"`
	RequestsPerSecond      int `json:"requestsPerSecond,omitempty"`
	Burst                  int `json:"burst,omitempty"`
}

// set reports whether any field was configured.
func (r LegacyRateLimits) set() bool { return r != LegacyRateLimits{} }

// LegacyScanConcurrency is the superseded `discovery.concurrency` block.
type LegacyScanConcurrency struct {
	Repositories int `json:"repositories,omitempty"`
	Tags         int `json:"tags,omitempty"`
}

func (c LegacyScanConcurrency) set() bool { return c != LegacyScanConcurrency{} }

// foldLegacy merges superseded blocks into an explicit Concurrency and reports
// what it did, for the deprecation notice.
//
// Explicit `concurrency` always wins: a document that has been migrated must not
// have a stale `rateLimits` block quietly override the new one.
func foldLegacy(c Concurrency, r LegacyRateLimits, scan LegacyScanConcurrency, where string) (Concurrency, []string) {
	var notes []string

	if c.PerRegistry <= 0 {
		switch {
		case r.MaxConnections > 0:
			// The closest analogue by construction: maxConnections WAS the pool,
			// and the pool was always the real ceiling.
			c.PerRegistry = r.MaxConnections
		case r.MaxConcurrentDownloads > 0:
			c.PerRegistry = r.MaxConcurrentDownloads
		case r.MaxConcurrentUploads > 0:
			c.PerRegistry = r.MaxConcurrentUploads
		case scan.Repositories > 0 || scan.Tags > 0:
			// The old pair multiplied: R repositories each running T tags meant
			// up to R×T requests in flight. Carrying the product forward keeps
			// the load the operator actually tuned for.
			c.PerRegistry = max(scan.Repositories, 1) * max(scan.Tags, 1)
		}
	}
	if c.RequestsPerSecond <= 0 && r.RequestsPerSecond > 0 {
		c.RequestsPerSecond = r.RequestsPerSecond
	}

	if r.set() {
		notes = append(notes, where+".rateLimits is superseded by "+where+".concurrency")
	}
	if scan.set() {
		notes = append(notes, where+".discovery.concurrency is superseded by "+where+".concurrency")
	}
	return c, notes
}

// EffectiveMaxRepositories returns the configured cap or the default.
func (d Discovery) EffectiveMaxRepositories() int {
	if d.MaxRepositories <= 0 {
		return DefaultMaxRepositories
	}
	return d.MaxRepositories
}

// IsEnabled reports whether discovery should poll this source.
func (d Discovery) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// TagFilters narrows what discovery records at all. Applied before
// auto-download rules, so filtering also bounds scan cost.
//
// An alias rather than its own type: tag and repository filtering have
// identical semantics, and two structurally identical types would invite two
// slightly different implementations.
type TagFilters = Filters

// Defaults from docs/design/02 section 5.3.
const (
	DefaultDiscoveryInterval = 15 * time.Minute
	DefaultRulePriority      = 50
)

type Verification struct {
	Enabled            bool               `json:"enabled,omitempty"`
	Policy             VerificationPolicy `json:"policy,omitempty"`
	AtSource           bool               `json:"atSource,omitempty"`
	AtDestination      bool               `json:"atDestination,omitempty"`
	TransferSignatures *bool              `json:"transferSignatures,omitempty"`
	Cosign             Cosign             `json:"cosign,omitempty"`
}

// ShouldTransferSignatures defaults to true: without it the destination holds
// unsigned copies and the chain of custody ends at our boundary.
func (v Verification) ShouldTransferSignatures() bool {
	return v.TransferSignatures == nil || *v.TransferSignatures
}

// VerificationPolicy decides what a failure does.
type VerificationPolicy string

const (
	// PolicyEnforce blocks or fails the transfer on verification failure.
	PolicyEnforce VerificationPolicy = "enforce"
	// PolicyWarn records the failure, notifies, and proceeds. Appropriate
	// while onboarding a vendor whose signing setup is not yet understood.
	PolicyWarn VerificationPolicy = "warn"
)

type Cosign struct {
	Mode    CosignMode     `json:"mode,omitempty"`
	Keyless *CosignKeyless `json:"keyless,omitempty"`
	Key     *CosignKey     `json:"key,omitempty"`
}

type CosignMode string

const (
	CosignKeylessMode CosignMode = "keyless"
	CosignKeyMode     CosignMode = "key"
)

type CosignKeyless struct {
	// CertificateIdentity and CertificateOidcIssuer are both REQUIRED.
	// Keyless verification without an identity constraint accepts any valid
	// Sigstore signature from anyone: it proves someone signed the artifact,
	// not that the vendor did. Validation rejects it rather than allowing a
	// trust configuration that looks secure and is not.
	CertificateIdentity   string     `json:"certificateIdentity,omitempty"`
	CertificateOidcIssuer string     `json:"certificateOidcIssuer,omitempty"`
	RekorPublicKeysRef    *SecretRef `json:"rekorPublicKeysRef,omitempty"`
	FulcioCertsRef        *SecretRef `json:"fulcioCertsRef,omitempty"`
}

type CosignKey struct {
	PublicKeyRef *SecretRef `json:"publicKeyRef,omitempty"`
}

type Notifications struct {
	Enabled       bool           `json:"enabled,omitempty"`
	Channels      []Channel      `json:"channels,omitempty"`
	Subscriptions []Subscription `json:"subscriptions,omitempty"`
}

type Channel struct {
	Name  string        `json:"name"`
	Type  ChannelType   `json:"type"`
	Email *EmailChannel `json:"email,omitempty"`
	Teams *TeamsChannel `json:"teams,omitempty"`
}

type ChannelType string

const (
	ChannelEmail ChannelType = "email"
	ChannelTeams ChannelType = "teams"
)

type EmailChannel struct {
	Recipients []string `json:"recipients,omitempty"`
}

type TeamsChannel struct {
	// WebhookURLRef must hold a Power Automate WORKFLOW URL. Legacy O365
	// connector webhooks (outlook.office.com/webhook/...) are retired and no
	// longer work in current tenants. See docs/design/16 section 3.
	WebhookURLRef *SecretRef `json:"webhookUrlRef,omitempty"`
}

type Subscription struct {
	Events   []string `json:"events"`
	Channels []string `json:"channels"`
}

// KnownEvents is the closed set of notifiable events.
// See docs/design/12 section 5.
var KnownEvents = []string{
	"PackageDiscovered",
	"TransferCompleted",
	"PromotionCompleted",
	"TransferFailed",
	"VerificationFailed",
	"DiscoveryFailed",
}

type Network struct {
	CABundleRef *SecretRef `json:"caBundleRef,omitempty"`
	Proxy       *Proxy     `json:"proxy,omitempty"`
	TLS         *TLS       `json:"tls,omitempty"`
	Timeouts    Timeouts   `json:"timeouts,omitempty"`
}

// TLS overrides certificate handling for one repository.
type TLS struct {
	// InsecureSkipVerify disables certificate verification entirely: the
	// chain, the expiry and the hostname all stop being checked.
	//
	// An earlier revision of this file said this option would never exist,
	// because supplying a CA bundle is the right fix. That was too strong.
	// caBundleRef fixes an UNTRUSTED chain; it does nothing for a certificate
	// that is expired, carries the wrong hostname, or belongs to a registry
	// being migrated - and an operator who genuinely needs to move bytes past
	// one of those today should not have to patch the binary.
	//
	// It is nonetheless the wrong answer to most problems. In particular it
	// does NOT fix "x509: negative serial number": that failure happens while
	// PARSING the server's certificate, before any verification runs, so
	// skipping verification changes nothing. Measured, not assumed - see
	// tls.allowNegativeSerialNumbers in the system configuration for the fix.
	//
	// Every client built with this set logs a warning naming the repository,
	// and `transferctl products check` reports it. It should be visible in a
	// way that makes leaving it on uncomfortable.
	//
	// A pointer so that silence and `false` are different things. A product-level
	// tls block is INHERITED by every source and target; without the pointer a
	// source could never turn it back off, because an omitted field and an
	// explicit `false` would look identical.
	InsecureSkipVerify *bool `json:"insecureSkipVerify,omitempty"`
}

// SetsSkipVerify reports whether this block states a choice at all.
//
// Nil receiver is safe: `network.tls` is optional at every level, and the
// callers walk a chain of possibly-absent blocks.
func (t *TLS) SetsSkipVerify() bool {
	return t != nil && t.InsecureSkipVerify != nil
}

// SkipsVerify reports whether verification is disabled. Absent means enabled -
// the safe direction, and the only defensible default.
func (t *TLS) SkipsVerify() bool {
	return t != nil && t.InsecureSkipVerify != nil && *t.InsecureSkipVerify
}

// SkipsTLSVerification resolves the effective setting for one repository: the
// product's network block, overridden by the repository's own where it states
// a choice.
//
// One function rather than the rule being re-derived at each call site, because
// this is a security-relevant decision and two call sites disagreeing about it
// is exactly the bug that would not be noticed.
func SkipsTLSVerification(base Network, override *Network) bool {
	skip := base.TLS.SkipsVerify()
	if override != nil && override.TLS.SetsSkipVerify() {
		skip = override.TLS.SkipsVerify()
	}
	return skip
}

type Proxy struct {
	HTTPSProxy string   `json:"httpsProxy,omitempty"`
	NoProxy    []string `json:"noProxy,omitempty"`
	// Direct ignores any inherited proxy and connects straight out -
	// INCLUDING the environment's.
	//
	// Needed for two shapes. The first is "everything goes through the
	// corporate proxy except this one internal registry": a product-level proxy
	// is inherited, and without this the only way to say that is to repeat the
	// registry's own hostname in noProxy at every level, which works and is easy
	// to get subtly wrong when the host has a port or an alias.
	//
	// The second is the one that surprises people. Leaving the proxy block
	// EMPTY does not mean "no proxy" - it falls back to HTTPS_PROXY from the
	// process environment, as curl, docker and kubectl do. That default is
	// right, since a cluster-wide proxy is often the only route out, but it is
	// invisible: a registry we can reach directly gets proxied anyway, and the
	// only symptom is throughput. This field is how a deployment says no and
	// means it. transport.DescribeProxy logs which of the three applies.
	Direct bool `json:"direct,omitempty"`
}

// Timeouts are per request. Blob transfers are governed by IdleStall rather
// than a total deadline - a 40 GB blob is not slow, it is large.
type Timeouts struct {
	Connect        Duration `json:"connect,omitempty"`
	ResponseHeader Duration `json:"responseHeader,omitempty"`
	IdleStall      Duration `json:"idleStall,omitempty"`
}

// DefaultCABundleKey is the key read from a caBundleRef when none is given.
const DefaultCABundleKey = "ca.crt"

// SecretRef names a key inside a projected Kubernetes Secret.
type SecretRef struct {
	SecretName string `json:"secretName"`
	Key        string `json:"key,omitempty"`
}

// CredentialsRef names registry credentials. Defaults to the `username` and
// `password` keys.
type CredentialsRef struct {
	SecretName  string `json:"secretName"`
	UsernameKey string `json:"usernameKey,omitempty"`
	PasswordKey string `json:"passwordKey,omitempty"`
}

// UsernameKeyOrDefault returns the configured key or "username".
func (c CredentialsRef) UsernameKeyOrDefault() string {
	if c.UsernameKey == "" {
		return "username"
	}
	return c.UsernameKey
}

// PasswordKeyOrDefault returns the configured key or "password".
func (c CredentialsRef) PasswordKeyOrDefault() string {
	if c.PasswordKey == "" {
		return "password"
	}
	return c.PasswordKey
}

// Repositories returns every repository in the product, both roles, for
// health checks and startup validation.
func (p Product) Repositories() []RepositoryRef {
	out := make([]RepositoryRef, 0, len(p.Spec.Sources)+len(p.Spec.Targets))
	for _, s := range p.Spec.Sources {
		declared := s.DeclaredRepositories()
		for _, repo := range declared {
			// The row NAME is the source name for a single-repository source,
			// and "<source>/<path>" when the source covers several. The common
			// case therefore reads exactly as it did before sources could span
			// repositories, while the multi-repository case stays unambiguous
			// under the (product, role, name) unique constraint.
			name := s.Name
			if len(declared) > 1 {
				name = s.Name + "/" + repo
			}
			out = append(out, RepositoryRef{
				Role: RoleSource, Name: name, Registry: s.Registry, Repository: repo,
				Type: s.Type, Enabled: p.IsEnabled() && s.IsEnabled(),
			})
		}
	}
	for _, t := range p.Spec.Targets {
		out = append(out, RepositoryRef{
			Role: RoleTarget, Name: t.Name, Registry: t.Registry, Repository: t.Repository,
			Type: t.Type, Enabled: p.IsEnabled() && t.IsEnabled(),
		})
	}
	return out
}

// Role distinguishes a repository's direction of use.
type Role string

const (
	RoleSource Role = "source"
	RoleTarget Role = "target"
)

// RepositoryRef is a flattened repository identity.
type RepositoryRef struct {
	Role       Role
	Name       string
	Registry   string
	Repository string
	Type       RegistryType
	// Enabled is the resolved state: false when the repository itself is
	// disabled, or when the product containing it is.
	Enabled bool
}

// DefaultTarget returns the target marked default, or the sole target when
// there is exactly one. Reports false when a request must name a target.
// DefaultTarget returns the target used when a request names none.
//
// Disabled targets are not candidates: silently replicating into a destination
// somebody switched off would defeat the point of the switch.
func (p Product) DefaultTarget() (Target, bool) {
	enabled := p.EnabledTargets()
	for _, t := range enabled {
		if t.Default {
			return t, true
		}
	}
	if len(enabled) == 1 && !enabled[0].PromotionOnly {
		return enabled[0], true
	}
	return Target{}, false
}

// Target looks up a target by name.
func (p Product) Target(name string) (Target, bool) {
	for _, t := range p.Spec.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// IsEnabled reports whether the product participates at all.
//
// Absent means enabled: a document that says nothing about it is on, which is
// what every existing configuration means and what a new one will expect.
func (p Product) IsEnabled() bool {
	return p.Metadata.Enabled == nil || *p.Metadata.Enabled
}

// IsEnabled reports whether this source participates at all.
func (s Source) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// IsEnabled reports whether this target participates at all.
func (t Target) IsEnabled() bool { return t.Enabled == nil || *t.Enabled }

// EnabledTargets returns the targets that are on.
func (p Product) EnabledTargets() []Target {
	out := make([]Target, 0, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		if t.IsEnabled() {
			out = append(out, t)
		}
	}
	return out
}

// EnabledSources returns the sources that are on.
func (p Product) EnabledSources() []Source {
	out := make([]Source, 0, len(p.Spec.Sources))
	for _, s := range p.Spec.Sources {
		if s.IsEnabled() {
			out = append(out, s)
		}
	}
	return out
}

// VerificationFor returns the verification settings in force for one
// repository.
//
// The merge is deliberately asymmetric, because the two halves behave
// differently:
//
//   - The SCALARS (enabled, policy, atSource, atDestination,
//     transferSignatures) inherit from the product and are overridden
//     individually. A product says "enforce, at source" once; a repository
//     that needs `warn` while a vendor's signing is being onboarded overrides
//     that one field.
//
//   - `cosign` REPLACES WHOLESALE. It is one coherent trust decision - a mode
//     plus the identity or key that mode requires - and merging it field by
//     field would silently produce combinations nobody wrote: a product's
//     keyless certificate identity paired with a repository's key mode, or a
//     Fulcio issuer left over from a block that no longer applies. A trust
//     configuration assembled from two documents is one nobody can audit.
func (p Product) VerificationFor(override *Verification) Verification {
	v := p.Spec.Verification
	if override == nil {
		return v
	}

	// Scalars: an explicitly set field wins. Booleans cannot distinguish
	// "false" from "unset", so `enabled` and the two `at*` flags are taken
	// from the override whenever a verification block is present at all -
	// writing one and having it ignored would be worse than the alternative.
	v.Enabled = override.Enabled
	v.AtSource = override.AtSource
	v.AtDestination = override.AtDestination
	if override.Policy != "" {
		v.Policy = override.Policy
	}
	if override.TransferSignatures != nil {
		v.TransferSignatures = override.TransferSignatures
	}

	// Cosign is atomic.
	if override.Cosign.Mode != "" || override.Cosign.Keyless != nil || override.Cosign.Key != nil {
		v.Cosign = override.Cosign
	}
	return v
}

// Source looks up a source by name.
func (p Product) Source(name string) (Source, bool) {
	for _, s := range p.Spec.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}
