// Package config loads deployment-scoped system configuration.
//
// See docs/design/02-configuration.md section 8. Precedence is
// flag -> SWGW_ env -> file -> default.
//
// This is distinct from product configuration (internal/product), which is
// GitOps-managed data. System config is an operator concern: addresses,
// database DSN, tick intervals, log level.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// EnvPrefix namespaces environment overrides: SWGW_DATABASE_DSN maps to
// database.dsn.
const EnvPrefix = "SWGW_"

// SystemConfig is the deployment-scoped configuration document.
type SystemConfig struct {
	ConfigDir string `koanf:"configDir"`

	// Stage is the task vocabulary: what a release can be moved through, and
	// what each move checks. See stage.go - this is the block that makes
	// download, onboard and promote configuration rather than code.
	Stage StageConfig `koanf:"stage"`

	Server        ServerConfig        `koanf:"server"`
	Database      DatabaseConfig      `koanf:"database"`
	Coordinator   CoordinatorConfig   `koanf:"coordinator"`
	Worker        WorkerConfig        `koanf:"worker"`
	Observability ObservabilityConfig `koanf:"observability"`
	Retention     RetentionConfig     `koanf:"retention"`
	TLS           TLSConfig           `koanf:"tls"`
	Concurrency   ConcurrencyConfig   `koanf:"concurrency"`
}

// TLSConfig relaxes certificate handling for the WHOLE PROCESS.
//
// Deliberately here and not in product configuration, unlike
// network.tls.insecureSkipVerify. These settings are implemented by Go's
// GODEBUG mechanism, which is per process and cannot be scoped to one
// connection - so pretending they were per repository would be a lie about
// their blast radius. An operator concern, in the operator's config file.
type TLSConfig struct {
	// AllowNegativeSerialNumbers accepts certificates whose serial number is
	// negative, which crypto/x509 has rejected since Go 1.23.
	//
	// This is the fix for:
	//
	//	tls: failed to parse certificate from server: x509: negative serial number
	//
	// And it is the ONLY fix. That error happens while PARSING the server's
	// certificate - before any verification runs - so
	// network.tls.insecureSkipVerify does not help, and neither does a CA
	// bundle. Measured on Go 1.25.7, not reasoned about: with
	// insecureSkipVerify alone the handshake fails with the identical message.
	//
	// RFC 5280 §4.1.2.2 requires a positive serial number, so a certificate
	// with a negative one is malformed. Some appliance and enterprise CAs emit
	// them anyway by encoding a random 20-byte value without clearing the high
	// bit. The certificate is otherwise fine; the standard library is simply
	// stricter than the estate.
	//
	// Process-wide, and logged as such at startup.
	AllowNegativeSerialNumbers bool `koanf:"allowNegativeSerialNumbers"`
}

// ConcurrencyConfig is how hard this installation works any one registry.
//
// It lives here, at the application level, because it is an operational
// property of the DEPLOYMENT - the bandwidth it has, the proxy it sits behind,
// the politeness its vendors expect - and not of any one product. Every product
// inherits it; a product may override it per source or per target for the case
// that genuinely differs, which is one fragile vendor rather than the rule.
//
// See product.Concurrency for why this is one number rather than seven.
type ConcurrencyConfig struct {
	// PerRegistry is the number of requests in flight against one registry, and
	// the size of the connection pool serving them.
	PerRegistry int `koanf:"perRegistry"`

	// RequestsPerSecond is an optional politeness ceiling on top of it. Zero,
	// the default, means no artificial limit.
	RequestsPerSecond int `koanf:"requestsPerSecond"`
}

type ServerConfig struct {
	Address             string        `koanf:"address"`
	ShutdownGracePeriod time.Duration `koanf:"shutdownGracePeriod"`
}

type DatabaseConfig struct {
	Driver          string        `koanf:"driver"` // postgres | sqlite
	DSN             string        `koanf:"dsn"`
	MaxOpenConns    int           `koanf:"maxOpenConns"`
	MaxIdleConns    int           `koanf:"maxIdleConns"`
	ConnMaxLifetime time.Duration `koanf:"connMaxLifetime"`
}

type CoordinatorConfig struct {
	LeaderElection LeaderElectionConfig `koanf:"leaderElection"`
	Scheduler      TickConfig           `koanf:"scheduler"`
	Reaper         ReaperConfig         `koanf:"reaper"`
	Queue          QueueConfig          `koanf:"queue"`
	GC             GCConfig             `koanf:"gc"`
	ManifestCache  ManifestCacheConfig  `koanf:"manifestCache"`
	Files          FilesConfig          `koanf:"files"`
	Security       SecurityConfig       `koanf:"security"`
	Compliance     ComplianceConfig     `koanf:"compliance"`
}

// ComplianceConfig tunes checking a release against the organization's own
// Kubernetes and CNF standards.
//
// Here rather than in a product document, and the placement is the argument.
// None of it is a property of a PRODUCT: which Kubernetes version to render
// against is a property of the estate, which registries are approved is a
// property of the organization, and where the policy packs are mounted is a
// property of this deployment. Stated per product they would be repeated in
// every document and drift between them, and the drift would show up as one
// vendor mysteriously failing a check another passes.
type ComplianceConfig struct {
	// Enabled registers the compliance routes and the runner. Off leaves the
	// feature absent rather than present and failing.
	Enabled bool `koanf:"enabled"`

	// PolicyPaths are directories of policy packs, discovered on start and
	// re-read on change. The built-in baseline is always loaded and cannot be
	// removed by emptying this - which is what stops a misconfigured mount
	// turning every release green.
	PolicyPaths []string `koanf:"policyPaths"`

	// HelmBinary is the renderer. Looked up on PATH when empty. A Coordinator
	// without it still serves stored results and reports every rendered check
	// as undecided, never as a pass.
	HelmBinary string `koanf:"helmBinary"`

	// KubeVersion and APIVersions are PINNED render inputs. A chart branching
	// on the cluster version must render the same way twice, or no finding
	// derived from it is reproducible.
	KubeVersion string   `koanf:"kubeVersion"`
	APIVersions []string `koanf:"apiVersions"`

	// ApprovedRegistries is what SUP-02 accepts. Configuration rather than a
	// policy constant, because the registries an organization runs are a fact
	// about the organization and change when a datacentre opens.
	ApprovedRegistries []string `koanf:"approvedRegistries"`

	// Determinacy runs the second, perturbed render that distinguishes a value
	// the chart fixes from one a values file can override. It doubles the
	// rendering cost and it is what lets a tier-1 finding block without lying,
	// so it defaults on.
	Determinacy *bool `koanf:"determinacy"`

	// MaxChartBytes and MaxReleaseBytes bound what one run reads out of a
	// registry. Two numbers because one mislabelled 500 MB artifact and four
	// hundred ordinary charts are different problems: without the first, the
	// bad artifact consumes the whole budget and every chart after it is
	// skipped for an unrelated reason.
	MaxChartBytes   int64 `koanf:"maxChartBytes"`
	MaxReleaseBytes int64 `koanf:"maxReleaseBytes"`

	// MaxResults truncates a report rather than exhausting memory on a
	// pathological release. A truncated run says so: a silently shortened
	// report is worse than a failed one, because it looks complete.
	MaxResults int `koanf:"maxResults"`

	// EvidencePerDocument and EvidencePerRelease bound the rendered manifests a
	// run keeps so a finding can be SHOWN against the text it came from.
	//
	// Two numbers for the same reason MaxChartBytes and MaxReleaseBytes are
	// two: one pathological chart and four hundred ordinary ones are different
	// problems, and without the per-document cap the pathological one spends
	// the whole budget and every chart after it loses its evidence for an
	// unrelated reason. Over the cap a document is kept TRUNCATED and says so.
	//
	// Negative turns the keeping off entirely, for a deployment that will not
	// hold vendor manifests in its database. Findings are unaffected - the
	// manifests are what a finding is displayed against, never what it is
	// derived from - and the interface says the evidence was not kept rather
	// than showing an empty document.
	EvidencePerDocument int64 `koanf:"evidencePerDocument"`
	EvidencePerRelease  int64 `koanf:"evidencePerRelease"`

	// RenderCacheTTL and RenderCacheBytes bound the cache of rendered charts.
	//
	// A chart's rendered output is a pure function of its bytes and the pinned
	// render inputs, so the same chart never has to be rendered twice - and,
	// because the cache is keyed by the chart's LAYER DIGEST, a hit also means
	// it never has to be downloaded. Most charts are unchanged between two
	// releases of a product, so this is most of the second check of an orb.
	//
	// Evictable, and safe to evict: a missing entry costs one render and can
	// never be wrong. The TTL removes charts a vendor has stopped shipping,
	// which no byte budget would ever reach; the budget removes the tail when a
	// large estate keeps everything warm. Zero on either turns that bound off;
	// RenderCacheBytes below zero turns the cache off entirely.
	RenderCacheTTL   time.Duration `koanf:"renderCacheTTL"`
	RenderCacheBytes int64         `koanf:"renderCacheBytes"`
	// RenderCacheSweep is how often the cache is measured against those bounds.
	// A cheap query, and a cache a little over its budget for an hour is not a
	// problem worth a tighter loop.
	RenderCacheSweep time.Duration `koanf:"renderCacheSweep"`

	// FetchConcurrency and RenderConcurrency are how many charts a run pulls
	// and renders at once.
	//
	// Two numbers because the two stages are bound by different things. Pulling
	// a chart layer is almost all round trip against SOMEBODY ELSE'S registry,
	// so the limit is politeness: thirty parallel requests is a rate limiter
	// and a slower answer, not a faster one. Rendering is `helm template`,
	// which is CPU-bound and local, so its limit is this machine's cores.
	//
	// Zero picks a default (6 fetching; 4-8 rendering, from the CPU count).
	// One does that stage in sequence, which is what a Coordinator sharing a
	// small node with everything else may want. Neither can change a result:
	// what a run produces is assembled in chart order regardless of which
	// worker finished first.
	FetchConcurrency  int `koanf:"fetchConcurrency"`
	RenderConcurrency int `koanf:"renderConcurrency"`

	// RenderTimeout bounds one chart's render, so a template loop that does
	// not terminate cannot take the Coordinator with it.
	RenderTimeout time.Duration `koanf:"renderTimeout"`

	// StaleAfter is how long a run's claim survives without a heartbeat.
	// Past it the sweeper releases the claim, so a release whose Coordinator
	// died does not stay uncheckable forever.
	StaleAfter time.Duration `koanf:"staleAfter"`
}

// ProbeDeterminacy reports whether the second render runs, defaulting on.
func (c ComplianceConfig) ProbeDeterminacy() bool {
	return c.Determinacy == nil || *c.Determinacy
}

// SecurityConfig tunes how vulnerability syncs reach a scanner and how long
// what they record is kept.
//
// Here rather than in a product document, and that placement is the whole point.
// None of it is a property of a PRODUCT: how hard to push a scanner is a
// property of the scanner and the network to it, and how long to keep an index
// is a property of this deployment's disk. Stated per product they would be
// repeated in every document and drift between them, and the drift would show
// up as one product mysteriously slower than another.
//
// A product document says one thing about Xray: whether it is on.
type SecurityConfig struct {
	// Concurrency caps scanner requests in flight for one sync.
	//
	// Bounded, and worth keeping bounded. Xray's summary endpoint is expensive
	// server-side and rate-limited on hosted JFrog; sixty parallel requests is
	// not six times faster than ten, it is a 429 storm and a slower answer.
	//
	// PER SYNC, not per Coordinator: two releases syncing at once are two
	// budgets. Raise it for a self-hosted Xray with headroom; leave it alone on
	// hosted JFrog.
	Concurrency int `koanf:"concurrency"`
	// BatchSize is how many artifacts one scanner request asks about. It bounds
	// the blast radius of a failed call: one failure costs this many artifacts'
	// results rather than a whole release's.
	BatchSize int `koanf:"batchSize"`
	// RequestTimeout bounds a single scanner call end to end.
	RequestTimeout time.Duration `koanf:"requestTimeout"`

	// The three retentions are how long a row is PINNED, not how long it lives.
	//
	// That changed, and the change is the point. They used to be lifetimes: a
	// row past its retention was invisible to every read and deleted by the
	// next sweep, which is a correct cache and the wrong policy for a security
	// index. It produced releases with counts and no findings behind them - the
	// summary lives in `package_security` and never expired, the rows did - and
	// the only way back was re-running a twenty-minute sync against somebody
	// else's scanner.
	//
	// Now a row past its retention becomes EVICTABLE. It is still read, still
	// exported, still counted, and it is removed only when the store is over
	// CacheBudgetBytes and it is the least recently read thing in it. Nothing is
	// deleted until deleting is required.

	// IndexRetention pins the LIGHTWEIGHT half of a sync: statuses, counts, and
	// the identifiers that make a finding findable - CVE, component, severity,
	// fixed version. Long, because this is the durable result of a sync and
	// what every listing, comparison and search reads.
	IndexRetention time.Duration `koanf:"indexRetention"`
	// DetailRetention pins the prose half: descriptions, references, CVSS
	// vectors. Shorter, because it is the part that would otherwise make this
	// platform a second copy of a vulnerability database that re-grades itself
	// continuously - and because when it does go, the findings are still
	// complete enough to list, filter, compare and export.
	DetailRetention time.Duration `koanf:"detailRetention"`
	// DocumentRetention pins the RAW scanner bodies: the vulnerability
	// response as it arrived, the SBOM, the policy violations, the malware
	// verdict. These are the largest thing the feature stores and the only
	// thing an export can hand to a customer unaltered, which is why they are
	// kept at all and why they are the first tier evicted.
	DocumentRetention time.Duration `koanf:"documentRetention"`

	// CacheBudgetBytes is the ceiling for the two regenerable tiers - the
	// stored (compressed) detail payloads and raw documents.
	//
	// ZERO MEANS NO CEILING, and that is the default. Forgetting a security
	// answer is the surprising behaviour and should have to be asked for; a
	// deployment that has not thought about disk gets a store that keeps
	// everything, and one that has sets a number here.
	//
	// The index tier is deliberately not in the budget. It is bytes per
	// artifact and rebuilding it is minutes of somebody else's scanner, so a
	// budget that could evict it would be a budget somebody set by mistake.
	CacheBudgetBytes int64 `koanf:"cacheBudgetBytes"`

	// SweepInterval is how often the store is measured against the budget.
	SweepInterval time.Duration `koanf:"sweepInterval"`

	// MaxAge is how old a vulnerability answer may be before the interface
	// says so and offers to fetch it again.
	//
	// # Why this is not a retention, and not a refetch either
	//
	// Retention decides when a row may be DELETED. This decides when a row
	// stops being presented as current - which is a different question with a
	// different answer, because a three-week-old answer is still the best
	// answer anybody has and deleting it would leave nothing. So nothing
	// expires here: past this age a release's counts carry their age in words
	// beside them and a Refresh sits next to it.
	//
	// Nothing refetches on its own either. A sync is minutes against somebody
	// else's scanner, and a Coordinator that quietly started one because a
	// timer went off is a Coordinator that hammers Xray at 3am for a page
	// nobody has open. Scheduling belongs to whoever schedules the rest of
	// this estate's work; this is the number that tells them, and the reader,
	// when it is due.
	//
	// Zero means never say stale.
	//
	// Seven days by default: long enough that a release synced on Monday is not
	// nagging by Wednesday, short enough that a CVE published mid-week is not
	// missed for a month.
	MaxAge time.Duration `koanf:"maxAge"`

	// SBOMMaxAge is the same for the component inventory, and it is DIFFERENT
	// on purpose.
	//
	// An SBOM describes what is inside one immutable set of bytes. A digest's
	// component list cannot change, because changing it would produce another
	// digest - so an SBOM fetched once is correct for ever and refetching it is
	// tens of megabytes and minutes of a scanner's time spent proving that.
	//
	// Zero, the default, means never stale. It is configurable only because a
	// scanner that improves its own analysis may produce a better inventory of
	// the same bytes, and a deployment that cares can ask for one.
	SBOMMaxAge time.Duration `koanf:"sbomMaxAge"`

	// Documents names the extra scanner bodies a sync retrieves per artifact,
	// beyond the vulnerability response.
	//
	// Valid values: policy, malware, sbom. The vulnerability response is always
	// kept and never needs naming - it is captured from the request the scan
	// was making anyway, and costs nothing.
	//
	// The others cost a REQUEST PER IMAGE, which on a 157-image release is 157
	// more round trips against the scanner somebody is already waiting on. So:
	//
	//   - policy and malware are on by default. They are one request between
	//     them (malware is the malicious subset of the policy verdict), they
	//     are small, and a malware hit is the one finding a release manager
	//     must not have to go looking for.
	//   - sbom is OFF by default. It is minutes and tens of megabytes per
	//     image, it is wanted for an export rather than for a page, and it is
	//     fetched on demand when somebody presses the button.
	Documents []string `koanf:"documents"`

	// Anchore is where the deployment's Anchore Enterprise lives, and how hard
	// to push it. See AnchoreConfig.
	Anchore AnchoreConfig `koanf:"anchore"`
}

// AnchoreConfig is the deployment's Anchore Enterprise: where it is, which
// credential reaches it, and how hard to push it.
//
// # Why the address is here and Xray's is not
//
// Because Xray is not a separate system. It is a second endpoint on a JFrog
// platform this codebase already speaks to, reached with the credential the
// repository already holds - so a product document says one thing about it
// ("on") and everything else is derived. There is nothing to derive an Anchore
// endpoint from: it is its own host with its own credential, and one stanza
// here is the only alternative to repeating that host in every product
// document and watching the copies drift.
//
// A product document still says exactly one thing about Anchore, and it is the
// same thing it says about Xray: whether it is on for a repository.
//
//	targets:
//	  - name: internal-jfrog
//	    type: jfrog
//	    xrayEnabled: true
//	    anchoreEnabled: true
//
// Everything here is a DEFAULT rather than the only answer. A product that has
// to reach a different Anchore, or the same one under a different account,
// overrides the endpoint, the credential and the account in `spec.anchore` (see
// product.Anchore) and inherits the rest of this stanza - the concurrency, the
// timeouts, the document kinds - which are operator tuning rather than a
// property of the product.
type AnchoreConfig struct {
	// Endpoint is the Anchore API base URL as an operator has it in a browser -
	// "https://anchore.example.com". The `/v2` prefix is appended if absent, so
	// a URL copied from the API documentation works unchanged.
	//
	// EMPTY MEANS ANCHORE IS NOT AVAILABLE in this deployment, whatever a
	// product document says. That direction is deliberate: a product asking for
	// a scanner the deployment does not have should report "Anchore is not
	// configured on this Coordinator" rather than fail every sync against a
	// URL nobody set.
	Endpoint string `koanf:"endpoint"`

	// InsecureSkipVerify disables certificate verification for the Anchore endpoint.
	// It defaults to false; true is intended only for deployments whose
	// Anchore certificate is signed by a private or otherwise untrusted CA.
	InsecureSkipVerify *bool `koanf:"insecureSkipVerify"`

	// SecretName is the projected secret holding the Anchore credential, read
	// the same way every registry credential is: <secretsDir>/<name>/<key>.
	//
	// UsernameKey and PasswordKey default to "username" and "password". An API
	// key goes in the password key, which is what a service account should be
	// using.
	SecretName  string `koanf:"secretName"`
	UsernameKey string `koanf:"usernameKey"`
	PasswordKey string `koanf:"passwordKey"`

	// Account scopes every request to one Anchore account, through the
	// `x-anchore-account` header. Admin-only in Anchore, and left empty in
	// almost every deployment: the credential's own account is what a service
	// account should be scoped to already.
	Account string `koanf:"account"`

	// Concurrency caps Anchore requests in flight for one sync.
	//
	// Higher than the Xray equivalent by default, and for a structural reason
	// rather than a preference: Anchore answers per IMAGE where Xray answers
	// per batch of fifty, so a release of a hundred and fifty images is a
	// hundred and fifty requests here. A concurrency of six would make a first
	// sync twenty-five serial minutes.
	Concurrency int `koanf:"concurrency"`
	// RequestTimeout bounds one Anchore call end to end.
	RequestTimeout time.Duration `koanf:"requestTimeout"`

	// NOTHING WAITS FOR ANALYSIS, and there is deliberately no knob for how
	// long it would.
	//
	// There was one. A sync submitted every image and waited up to ten minutes
	// for Anchore to finish, and the design was wrong however the number was
	// set: Anchore analyses on its own schedule and nobody can promise a bound,
	// so a sync that waited had its duration decided by somebody else's queue,
	// held a claim on the release throughout, and reported the release as
	// unscanned every time the wait ran out.
	//
	// Registering a release with Anchore and reading its results are now
	// separate acts - `:replicateSecurity` and `:syncSecurity`. Neither blocks
	// on the other.

	// Submit says whether this Coordinator may register images with Anchore.
	//
	// True by default, because an image Anchore has never been told about
	// produces no findings and a platform that could only read would report
	// every new release as unscanned forever. Worth being able to turn off: an
	// estate whose own pipeline registers images wants this platform to read
	// Anchore rather than add to it, and a sync that submitted anyway would
	// duplicate their registration under our annotations.
	Submit *bool `koanf:"submit"`

	// SBOMFormat is which flavour of SBOM to fetch: spdx-json, cyclonedx-json
	// or native-json. Empty uses SPDX, which is what the person pressing
	// "download SBOM" is overwhelmingly about to send to somebody else.
	SBOMFormat string `koanf:"sbomFormat"`

	// Application overrides how a release is named in Anchore's own
	// Application/Version model. Two templates, and both default to the
	// obvious: the product's name and the release's version.
	//
	// Configurable only because an estate that already has an application
	// taxonomy in Anchore needs its releases to land inside it rather than
	// beside it. Setting either to "-" switches the grouping off entirely, and
	// this platform then reads per-image results only.
	ApplicationName string `koanf:"applicationName"`
	VersionName     string `koanf:"versionName"`
}

// Available reports whether this deployment can reach an Anchore at all.
//
// The endpoint alone. A credential that is missing is a failure worth
// reporting when somebody switches Anchore on for a repository; an endpoint
// that is missing means the deployment never had one, and every product
// document mentioning Anchore should say so quietly rather than fail.
func (c AnchoreConfig) Available() bool { return strings.TrimSpace(c.Endpoint) != "" }

// SkipsTLSVerification reports whether Anchore certificate verification is disabled.
func (c AnchoreConfig) SkipsTLSVerification() bool {
	return c.InsecureSkipVerify != nil && *c.InsecureSkipVerify
}

// SubmitImages reports whether syncs may register images with Anchore.
func (c AnchoreConfig) SubmitImages() bool { return c.Submit == nil || *c.Submit }

// Grouping reports whether releases should be grouped into Anchore
// Applications and Versions. "-" in either template switches it off.
func (c AnchoreConfig) Grouping() bool {
	return strings.TrimSpace(c.ApplicationName) != "-" && strings.TrimSpace(c.VersionName) != "-"
}

// SecurityDocumentKinds is the configured list, defaulted and validated.
//
// Unknown values are dropped rather than refused. A typo in a list of optional
// extras must not stop a Coordinator starting - the failure it would cause is
// an outage, and the failure it prevents is a missing tab.
func (c SecurityConfig) SecurityDocumentKinds() []string {
	if c.Documents == nil {
		return []string{"policy", "malware"}
	}
	out := make([]string, 0, len(c.Documents))
	for _, raw := range c.Documents {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "policy", "malware", "sbom", "vulnerabilities":
			out = append(out, strings.ToLower(strings.TrimSpace(raw)))
		}
	}
	return out
}

// FilesConfig governs the file-content routes - looking inside a release at
// what a vendor shipped, one named layer at a time.
type FilesConfig struct {
	// DownloadEnabled registers the raw-bytes download route. Off by default
	// is not the shape: a reader who cannot view a signature or an archive
	// inline still has a legitimate reason to save it, so this defaults on and
	// exists so a deployment that added it for one investigation can turn it
	// back off without a rebuild.
	DownloadEnabled bool `koanf:"downloadEnabled"`
}

// ManifestCacheConfig bounds the cached manifest bodies.
//
// A package's manifest BODIES are the only thing this system records that grows
// without limit and can be discarded without losing a fact. Everything else it
// knows about a package - the artifacts, their digests and sizes, the blobs
// they reference, the totals - is a few kilobytes and is kept forever.
//
// The bodies are large, are read only when a manifest is PUSHED, and are
// exactly recoverable, because a manifest is addressed by the hash of its own
// bytes. So they are a cache, and this is its size. Over a vendor catalogue
// accumulated across years, an unbounded one would be the largest thing in the
// database by a wide margin.
//
// See internal/store/manifestcache.go.
type ManifestCacheConfig struct {
	// BudgetBytes is the ceiling. Zero disables the budget entirely, for a
	// deployment that would rather spend disk than ever re-fetch.
	BudgetBytes int64 `koanf:"budgetBytes"`
	// TTL evicts bodies untouched for longer than this, whatever the budget
	// says. It is what keeps a deployment that discovers far more than it
	// transfers from carrying a full budget of manifests nobody will push.
	// Zero disables the age pass.
	TTL time.Duration `koanf:"ttl"`
	// SweepInterval is how often the sweeper runs. It is a cheap query against
	// a partial index, so this is minutes rather than hours; the point of
	// sweeping often is that the cache stays near its budget instead of sawing
	// between empty and over.
	SweepInterval time.Duration `koanf:"sweepInterval"`
}

type LeaderElectionConfig struct {
	Enabled bool  `koanf:"enabled"`
	LockID  int64 `koanf:"lockID"`
	// RetryInterval is how often a follower attempts to acquire leadership.
	RetryInterval time.Duration `koanf:"retryInterval"`
}

type TickConfig struct {
	TickInterval time.Duration `koanf:"tickInterval"`
}

type ReaperConfig struct {
	TickInterval  time.Duration `koanf:"tickInterval"`
	LeaseDuration time.Duration `koanf:"leaseDuration"`
}

type QueueConfig struct {
	MaxLeaseBatchSize int `koanf:"maxLeaseBatchSize"`
}

// GCConfig bounds how much HISTORY the database keeps.
//
// Three tables grow with USE rather than with the size of the catalogue, and
// they are the whole problem: `jobs` at roughly 2500 rows per transfer, and one
// transfer per release per target; `worker_logs`, a row per interesting thing a
// worker did; `audit_events`, a row per state transition. Everything else grows
// with the CATALOGUE and is the answer to "what does this vendor publish",
// which is the point of the system - none of it expires.
//
// Every duration is zero-means-keep-forever, so a deployment that wants an
// unbounded audit trail gets one by leaving the field unset rather than by
// finding a switch to turn something off.
//
// See internal/store/retention.go for what each sweep may and may not touch.
type GCConfig struct {
	TickInterval time.Duration `koanf:"tickInterval"`
	// BatchSize bounds one pass, so the first sweep of a database that has run
	// unbounded for a year does not hold a lock for minutes.
	BatchSize int `koanf:"batchSize"`

	// Transfers deletes SETTLED transfers, and their jobs with them, once they
	// have been finished for this long.
	//
	// Settled rather than merely old: a transfer running for a month is one
	// somebody is watching, and deleting it out from under them would look
	// exactly like the data loss this sweep exists to avoid being blamed for.
	Transfers time.Duration `koanf:"transfers"`
	// WorkerLogs expires the convenience tail. It is not a log store - cluster
	// log aggregation remains the system of record.
	WorkerLogs time.Duration `koanf:"workerLogs"`
	// AuditEvents expires the audit trail. Longest of the three by default, and
	// reasonably set to zero: an audit trail with a short retention is not an
	// audit trail.
	AuditEvents time.Duration `koanf:"auditEvents"`
	// Placements expires blob placement records not confirmed in this long.
	//
	// Zero, and worth leaving zero. This table is the memory that makes a
	// second transfer of a product line nearly free, losing a row costs a HEAD
	// per blob on the next transfer, and the whole table is measured in tens of
	// thousands of rows.
	Placements time.Duration `koanf:"placements"`

	// ComplianceRuns keeps the N most recent compliance runs of each release
	// and deletes the rest, with their charts, results and rendered manifests.
	//
	// A COUNT, and the only bound here that is not a duration. What a release's
	// compliance history is FOR is "what did this look like the last few times
	// we checked": a release checked once eight months ago must keep that run,
	// because it is the only answer anybody has about it, while a release
	// checked hourly by a schedule must not keep six thousand.
	//
	// The one-row-per-release summary the Software listing reads is never
	// swept. Zero keeps every run forever.
	ComplianceRuns int `koanf:"complianceRuns"`
}

type WorkerConfig struct {
	CoordinatorEndpoint string        `koanf:"coordinatorEndpoint"`
	WorkerID            string        `koanf:"workerID"`
	Address             string        `koanf:"address"`
	MaxConcurrentJobs   int           `koanf:"maxConcurrentJobs"`
	CopyBufferSize      int64         `koanf:"copyBufferSize"`
	HeartbeatInterval   time.Duration `koanf:"heartbeatInterval"`
	// StallTimeout is how long one job may make no progress before this worker
	// abandons it so another attempt can run. Zero uses the default; negative
	// disables the check.
	//
	// It is not a deadline on the job: a job that is transferring resets the
	// clock on every progress report, so a large blob may legitimately run for
	// hours. It bounds only silence.
	StallTimeout time.Duration `koanf:"stallTimeout"`
}

type ObservabilityConfig struct {
	Log     LogConfig     `koanf:"log"`
	Metrics MetricsConfig `koanf:"metrics"`
	Tracing TracingConfig `koanf:"tracing"`
}

type LogConfig struct {
	Level  string `koanf:"level"`
	Format string `koanf:"format"`
}

type MetricsConfig struct {
	Enabled bool   `koanf:"enabled"`
	Path    string `koanf:"path"`
}

type TracingConfig struct {
	Enabled     bool    `koanf:"enabled"`
	Endpoint    string  `koanf:"endpoint"`
	SampleRatio float64 `koanf:"sampleRatio"`
}

type RetentionConfig struct {
	CompletedJobs       time.Duration `koanf:"completedJobs"`
	QueueHistory        time.Duration `koanf:"queueHistory"`
	DiscoveryHistory    time.Duration `koanf:"discoveryHistory"`
	NotificationHistory time.Duration `koanf:"notificationHistory"`
	AuditHistory        time.Duration `koanf:"auditHistory"`
}

// Defaults returns the shipped defaults.
//
// SQLite is the development default so `go run ./cmd/coordinator` works with
// no setup at all - see docs/design/14 section 5.1. It is explicitly not
// supported in production, and the Coordinator warns at startup.
func Defaults() SystemConfig {
	return SystemConfig{
		ConfigDir: "/etc/softwaregateway",
		Server: ServerConfig{
			Address:             ":8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Database: DatabaseConfig{
			Driver:          "sqlite",
			DSN:             "./dev/swgw.db",
			MaxOpenConns:    25,
			MaxIdleConns:    10,
			ConnMaxLifetime: time.Hour,
		},
		Coordinator: CoordinatorConfig{
			LeaderElection: LeaderElectionConfig{
				Enabled:       true,
				LockID:        1,
				RetryInterval: 10 * time.Second,
			},
			Scheduler: TickConfig{TickInterval: 10 * time.Second},
			Reaper: ReaperConfig{
				TickInterval:  30 * time.Second,
				LeaseDuration: 2 * time.Minute,
			},
			Queue: QueueConfig{MaxLeaseBatchSize: 32},
			GC: GCConfig{
				TickInterval: time.Hour,
				BatchSize:    5000,
				// Ten runs per release. Enough to see a trend across a
				// fortnight of scheduled checks and to compare a re-check
				// against what it replaced; short of the unbounded growth a
				// nightly schedule would otherwise produce, and it is the
				// rendered manifests each run keeps that make that growth
				// matter rather than the result rows.
				ComplianceRuns: 10,
				// Ninety days of transfer history and thirty of worker logs.
				//
				// Chosen against what each is FOR. A settled transfer's rows
				// answer "what did that run do", which is asked in the days
				// after it and effectively never a quarter later; the content
				// it moved is at the destination and what we know about the
				// source is in the catalogue, so nothing recoverable is lost.
				// Worker logs answer "why did that job fail", which is asked
				// the same week.
				//
				// The audit trail and the placement cache are left unbounded on
				// purpose: an audit trail with a short retention is not an audit
				// trail, and the placements are what make a re-transfer nearly
				// free.
				Transfers:  90 * 24 * time.Hour,
				WorkerLogs: 30 * 24 * time.Hour,
			},
			ManifestCache: ManifestCacheConfig{
				// 512 MiB and a week.
				//
				// Sized against what it is FOR rather than against available
				// disk. A manifest body is a few kilobytes, so this holds on
				// the order of a hundred thousand of them - far more than any
				// plausible working set of packages being replicated in a
				// week, and a small fraction of the volume a database this
				// system runs on would be given. The TTL is the bound that
				// usually bites; the budget is the one that stops a bulk
				// inspection of an entire catalogue from being unbounded.
				//
				// Both are safe to lower aggressively: the cost of a miss is
				// re-fetching a few kilobytes from the source registry at
				// transfer time, and nothing else.
				BudgetBytes:   512 << 20,
				TTL:           7 * 24 * time.Hour,
				SweepInterval: 15 * time.Minute,
			},
			Files: FilesConfig{
				DownloadEnabled: true,
			},
			Compliance: ComplianceConfig{
				// On by default. The built-in baseline is compiled in and
				// needs nothing configured, so the feature works out of the
				// box - and a Coordinator without helm degrades to "could not
				// be checked" rather than to a wrong answer.
				Enabled: true,

				// Pinned, so a chart branching on the cluster version renders
				// the same way twice. A number rather than "whatever is
				// current", because a finding that changes when this binary is
				// rebuilt is not reproducible.
				KubeVersion: "1.30.0",

				// A large chart is a few megabytes; a release of a hundred is
				// comfortably inside the total. Both are refusals rather than
				// truncations: what they skip is named on the run.
				MaxChartBytes:   64 << 20,
				MaxReleaseBytes: 512 << 20,

				// Fifteen thousand rows is a large release checked in full.
				// The limit is well above that so it is reached only by
				// something pathological, and a run that reaches it says so.
				MaxResults: 200_000,

				// Sized against what charts actually render to: a large one is
				// a few hundred kilobytes of YAML, so a release of a hundred
				// fits inside the total with room to spare.
				EvidencePerDocument: 4 << 20,
				EvidencePerRelease:  24 << 20,

				// Thirty days and 512 MB. A chart renders to tens of kilobytes,
				// so the budget holds an estate of several thousand distinct
				// charts - and the TTL is what actually reclaims, because a
				// chart nobody ships any more is never asked for again.
				RenderCacheTTL:   30 * 24 * time.Hour,
				RenderCacheBytes: 512 << 20,
				RenderCacheSweep: time.Hour,

				// Ninety seconds per chart. A chart that takes longer than
				// that to render is not a chart that is nearly done.
				RenderTimeout: 90 * time.Second,

				// Five minutes without a heartbeat and the claim is released.
				// Long enough that a slow render is never mistaken for a dead
				// Coordinator, short enough that a release is checkable again
				// within a coffee break.
				StaleAfter: 5 * time.Minute,
			},
			Security: SecurityConfig{
				// Ten in flight, fifty per request, a minute each.
				//
				// Sized against the scanner rather than against this process: a
				// release of a few hundred artifacts is a handful of requests at
				// this batch size, and ten of them in flight is polite to a
				// hosted JFrog while still finishing a large release in tens of
				// seconds rather than minutes.
				//
				// It was six, and two things since have paid for the rest. The
				// probe that follows a scan no longer spends this budget one
				// image at a time - it asks about a hundred at once - so the
				// number now governs only the summary calls it was written for.
				// And the transport retries a 429 on the scanner's own
				// Retry-After, so a burst that trips a rate limit costs a pause
				// rather than a release's worth of unavailable artifacts.
				Concurrency:    10,
				BatchSize:      50,
				RequestTimeout: 60 * time.Second,

				// Thirty days, seven days, thirty days - and none of them a
				// deadline. Past its retention a row is evictable, not gone,
				// and it goes only when the budget below says something has to.
				//
				// The detail tier is pinned for a week rather than a day
				// because a day was chosen when expiry meant deletion, and the
				// cost of a re-fetch is a scanner round trip per image. It is
				// the first thing to give when disk is short and there is no
				// reason for it to be the first thing to give when it is not.
				IndexRetention:    30 * 24 * time.Hour,
				DetailRetention:   7 * 24 * time.Hour,
				DocumentRetention: 30 * 24 * time.Hour,
				MaxAge:            7 * 24 * time.Hour,
				// No ceiling by default. See the field comment: a deployment
				// that has not thought about this gets a store that keeps its
				// answers.
				CacheBudgetBytes: 0,
				SweepInterval:    15 * time.Minute,

				// The gate and the malware verdict, which are one request
				// between them. Not the SBOM: see the field comment.
				Documents: []string{"policy", "malware"},

				// Anchore, with NO ENDPOINT. That is the switch: a deployment
				// that has not configured one has no Anchore, and a product
				// asking for it is told so rather than failing every sync
				// against a URL nobody set.
				Anchore: AnchoreConfig{
					Concurrency:    12,
					RequestTimeout: 60 * time.Second,
					SBOMFormat:     "spdx-json",
				},
			},
		},
		Worker: WorkerConfig{
			CoordinatorEndpoint: "http://localhost:8080",
			Address:             ":8081",
			MaxConcurrentJobs:   16,
			CopyBufferSize:      1 << 20, // 1 MiB
			HeartbeatInterval:   20 * time.Second,
			StallTimeout:        15 * time.Minute,
		},
		Observability: ObservabilityConfig{
			Log:     LogConfig{Level: "info", Format: "json"},
			Metrics: MetricsConfig{Enabled: true, Path: "/metrics"},
			Tracing: TracingConfig{Enabled: false, SampleRatio: 0.05},
		},
		Concurrency: ConcurrencyConfig{
			// Matches what the previous seven knobs multiplied out to, so this
			// simplification changes the shape of the configuration and not the
			// load it produces.
			PerRegistry:       32,
			RequestsPerSecond: 0,
		},
		Retention: RetentionConfig{
			CompletedJobs:       7 * 24 * time.Hour,
			QueueHistory:        7 * 24 * time.Hour,
			DiscoveryHistory:    90 * 24 * time.Hour,
			NotificationHistory: 30 * 24 * time.Hour,
			AuditHistory:        365 * 24 * time.Hour,
		},
	}
}

// Load resolves configuration in precedence order: defaults, then the file if
// present, then SWGW_ environment variables.
//
// A missing file is not an error - the defaults plus environment must be
// enough to start, which is what makes the zero-setup development path work.
func Load(path string) (SystemConfig, error) {
	k := koanf.New(".")

	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return SystemConfig{}, fmt.Errorf("load defaults: %w", err)
	}

	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
				return SystemConfig{}, fmt.Errorf("load config file %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return SystemConfig{}, fmt.Errorf("stat config file %s: %w", path, err)
		}
	}

	// SWGW_DATABASE_DSN -> database.dsn, SWGW_DATABASE_MAXOPENCONNS ->
	// database.maxOpenConns.
	//
	// The second form is why this needs a lookup table rather than a string
	// transform. An environment variable cannot carry case, so the naive
	// mapping produces `database.maxopenconns` - which is a DIFFERENT koanf key
	// from `database.maxOpenConns` and therefore binds to nothing. The override
	// was silently ignored, which is the worst possible failure for a
	// configuration mechanism: the operator sets the variable, sees no error,
	// and gets the default.
	//
	// Resolving against the canonical keys makes every setting reachable and
	// makes an unknown variable detectable.
	canonical := canonicalKeys(k.Keys())
	var unknown []string

	err := k.Load(env.Provider(EnvPrefix, ".", func(s string) string {
		flat := strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(s, EnvPrefix)), "_", ".")
		if key, ok := canonical[flat]; ok {
			return key
		}
		unknown = append(unknown, s)
		// Returning "" tells koanf to skip the variable. Passing the unmatched
		// key through would reintroduce the silent-no-op this exists to fix.
		return ""
	}), nil)
	if err != nil {
		return SystemConfig{}, fmt.Errorf("load environment: %w", err)
	}
	if len(unknown) > 0 {
		// Fail rather than warn. A typo'd SWGW_ variable means the operator
		// believes they have changed something they have not, and finding that
		// out during an incident is far more expensive than at startup.
		sort.Strings(unknown)
		return SystemConfig{}, fmt.Errorf(
			"unknown environment variable(s): %s (no such configuration key; "+
				"names are SWGW_ plus the config path with dots as underscores, "+
				"e.g. SWGW_DATABASE_MAXOPENCONNS for database.maxOpenConns)",
			strings.Join(unknown, ", "))
	}

	var cfg SystemConfig
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf"}); err != nil {
		return SystemConfig{}, fmt.Errorf("unmarshal config: %w", err)
	}

	// Expand ${VAR} in the DSN so a manifest can reference a secret env var
	// without the literal ever appearing in a config file.
	cfg.Database.DSN = os.ExpandEnv(cfg.Database.DSN)

	if err := cfg.Validate(); err != nil {
		return SystemConfig{}, err
	}
	return cfg, nil
}

// canonicalKeys maps each lowercased config path to its real, cased form.
//
// Built from the defaults, which by construction contain every key the struct
// defines - so a key that exists in the schema is reachable from the
// environment, and one that does not is detected as a typo.
func canonicalKeys(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[strings.ToLower(k)] = k
	}
	return out
}

// Validate rejects configurations that cannot work.
func (c SystemConfig) Validate() error {
	switch c.Database.Driver {
	case "postgres", "sqlite":
	default:
		return fmt.Errorf("database.driver: %q is not one of postgres, sqlite", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn: required")
	}
	if c.Server.Address == "" {
		return fmt.Errorf("server.address: required")
	}
	if r := c.Observability.Tracing.SampleRatio; r < 0 || r > 1 {
		return fmt.Errorf("observability.tracing.sampleRatio: %v is outside [0,1]", r)
	}
	if c.Coordinator.ManifestCache.BudgetBytes < 0 {
		return fmt.Errorf("coordinator.manifestCache.budgetBytes: must not be negative (0 disables the budget)")
	}
	if c.Coordinator.ManifestCache.TTL < 0 {
		return fmt.Errorf("coordinator.manifestCache.ttl: must not be negative (0 disables expiry)")
	}
	if c.Coordinator.Reaper.LeaseDuration <= c.Coordinator.Reaper.TickInterval {
		return fmt.Errorf(
			"coordinator.reaper.leaseDuration (%v) must exceed tickInterval (%v), "+
				"or leases expire faster than the reaper can observe them",
			c.Coordinator.Reaper.LeaseDuration, c.Coordinator.Reaper.TickInterval)
	}
	return nil
}

// ProductsDir is where per-product ConfigMaps are projected.
func (c SystemConfig) ProductsDir() string { return c.ConfigDir + "/products" }

// SecretsDir is where VSO-managed Secrets are projected.
func (c SystemConfig) SecretsDir() string { return c.ConfigDir + "/secrets" }

// IsProduction reports whether the store is production-grade. Used to warn at
// startup that SQLite is a development convenience only.
func (c SystemConfig) IsProduction() bool { return c.Database.Driver == "postgres" }
