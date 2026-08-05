// Package product owns product configuration: the schema, loading, validation
// and hot reload.
//
// See docs/design/02-configuration.md.
//
// A Product is the root aggregate — the unit of configuration, ownership and
// blast radius. One product is one ConfigMap, one YAML document; everything
// about that product lives in that one place.
package product

import "time"

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
}

type Metadata struct {
	// Name is the resource ID: lowercase alphanumeric and hyphens, <=63 chars
	// (AIP-122). Immutable; appears in API paths, metric labels and audit.
	Name        string            `json:"name"`
	DisplayName string            `json:"displayName,omitempty"`
	Description string            `json:"description,omitempty"`
	Owner       string            `json:"owner,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

type Spec struct {
	Sources       []Source            `json:"sources"`
	Targets       []Target            `json:"targets"`
	AutoDownload  AutoDownload        `json:"autoDownload,omitempty"`
	Verification  Verification        `json:"verification,omitempty"`
	Notifications Notifications       `json:"notifications,omitempty"`
	Network       Network             `json:"network,omitempty"`
	Retention     map[string]Duration `json:"retention,omitempty"`
}

// Source is a vendor-side, read-only repository. Polled by discovery.
type Source struct {
	Name           string          `json:"name"`
	Registry       string          `json:"registry"`
	Repository     string          `json:"repository"`
	Type           RegistryType    `json:"type,omitempty"`
	Anonymous      bool            `json:"anonymous,omitempty"`
	CredentialsRef *CredentialsRef `json:"credentialsRef,omitempty"`
	Discovery      Discovery       `json:"discovery,omitempty"`
	RateLimits     RateLimits      `json:"rateLimits,omitempty"`
	Network        *Network        `json:"network,omitempty"`
}

// Target is an internal, read-write repository: a replication destination and
// a promotion endpoint in both directions.
type Target struct {
	Name           string          `json:"name"`
	Registry       string          `json:"registry"`
	Repository     string          `json:"repository"`
	Type           RegistryType    `json:"type,omitempty"`
	Anonymous      bool            `json:"anonymous,omitempty"`
	CredentialsRef *CredentialsRef `json:"credentialsRef,omitempty"`
	RateLimits     RateLimits      `json:"rateLimits,omitempty"`
	Network        *Network        `json:"network,omitempty"`
	// Default marks the target used when a request names none.
	Default bool `json:"default,omitempty"`
	// PromotionOnly rejects direct replication, so a production registry can
	// be reachable only by promotion from another target.
	PromotionOnly bool `json:"promotionOnly,omitempty"`
}

// RegistryType selects the implementation. `generic` is the expected path for
// all registries; the others exist only for genuine deviations.
type RegistryType string

const (
	RegistryGeneric     RegistryType = "generic"
	RegistryACR         RegistryType = "acr"
	RegistryArtifactory RegistryType = "artifactory"
	RegistryQuay        RegistryType = "quay"
)

// ValidRegistryTypes is the closed set. Must match the CHECK constraint on
// repositories.registry_type in db/migrations.
var ValidRegistryTypes = []RegistryType{
	RegistryGeneric, RegistryACR, RegistryArtifactory, RegistryQuay,
}

type Discovery struct {
	// Enabled defaults to true for sources; use the accessor rather than the
	// field so the nil/zero case is handled once.
	Enabled    *bool      `json:"enabled,omitempty"`
	Interval   Duration   `json:"interval,omitempty"`
	TagFilters TagFilters `json:"tagFilters,omitempty"`
}

// IsEnabled reports whether discovery should poll this source.
func (d Discovery) IsEnabled() bool { return d.Enabled == nil || *d.Enabled }

// TagFilters narrows what discovery records at all. Applied before
// auto-download rules, so filtering also bounds scan cost.
type TagFilters struct {
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
}

// RateLimits are per repository and fleet-wide, not per worker.
//
// Fleet-wide matters: a per-worker limit would silently multiply by the
// replica count and flatten a vendor registry the moment HPA scaled out.
// The Coordinator divides these across active workers.
type RateLimits struct {
	MaxConcurrentDownloads int `json:"maxConcurrentDownloads,omitempty"`
	MaxConcurrentUploads   int `json:"maxConcurrentUploads,omitempty"`
	MaxConnections         int `json:"maxConnections,omitempty"`
	RequestsPerSecond      int `json:"requestsPerSecond,omitempty"`
	Burst                  int `json:"burst,omitempty"`
}

// Defaults from docs/design/02 section 5.3.
const (
	DefaultMaxConcurrentDownloads = 8
	DefaultMaxConcurrentUploads   = 8
	DefaultMaxConnections         = 32
	DefaultDiscoveryInterval      = 15 * time.Minute
	DefaultRulePriority           = 50
)

// WithDefaults fills unset limits.
func (r RateLimits) WithDefaults() RateLimits {
	if r.MaxConcurrentDownloads == 0 {
		r.MaxConcurrentDownloads = DefaultMaxConcurrentDownloads
	}
	if r.MaxConcurrentUploads == 0 {
		r.MaxConcurrentUploads = DefaultMaxConcurrentUploads
	}
	if r.MaxConnections == 0 {
		r.MaxConnections = DefaultMaxConnections
	}
	if r.Burst == 0 && r.RequestsPerSecond > 0 {
		r.Burst = r.RequestsPerSecond * 2
	}
	return r
}

type AutoDownload struct {
	Enabled bool   `json:"enabled,omitempty"`
	Rules   []Rule `json:"rules,omitempty"`
}

// Rule matches a discovered tag and creates a transfer request.
//
// Rules are evaluated in order and the FIRST MATCH WINS — not all-match,
// because two rules matching one tag with different priorities has no sensible
// interpretation.
type Rule struct {
	Name string `json:"name"`
	// TagPattern is RE2 (Go regexp): linear time, no backtracking. Stated
	// explicitly because a user-supplied pattern evaluated inside a polling
	// loop would, under a backtracking engine, be a denial-of-service vector.
	TagPattern           string   `json:"tagPattern"`
	Targets              []string `json:"targets,omitempty"`
	Priority             *int     `json:"priority,omitempty"`
	VerifyBeforeTransfer *bool    `json:"verifyBeforeTransfer,omitempty"`
}

// EffectivePriority returns the rule's priority or the default.
func (r Rule) EffectivePriority() int {
	if r.Priority == nil {
		return DefaultRulePriority
	}
	return *r.Priority
}

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
	Timeouts    Timeouts   `json:"timeouts,omitempty"`
}

type Proxy struct {
	HTTPSProxy string   `json:"httpsProxy,omitempty"`
	NoProxy    []string `json:"noProxy,omitempty"`
}

// Timeouts are per request. Blob transfers are governed by IdleStall rather
// than a total deadline — a 40 GB blob is not slow, it is large.
type Timeouts struct {
	Connect        Duration `json:"connect,omitempty"`
	ResponseHeader Duration `json:"responseHeader,omitempty"`
	IdleStall      Duration `json:"idleStall,omitempty"`
}

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
		out = append(out, RepositoryRef{Role: RoleSource, Name: s.Name, Registry: s.Registry, Repository: s.Repository, Type: s.Type})
	}
	for _, t := range p.Spec.Targets {
		out = append(out, RepositoryRef{Role: RoleTarget, Name: t.Name, Registry: t.Registry, Repository: t.Repository, Type: t.Type})
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
}

// DefaultTarget returns the target marked default, or the sole target when
// there is exactly one. Reports false when a request must name a target.
func (p Product) DefaultTarget() (Target, bool) {
	for _, t := range p.Spec.Targets {
		if t.Default {
			return t, true
		}
	}
	if len(p.Spec.Targets) == 1 && !p.Spec.Targets[0].PromotionOnly {
		return p.Spec.Targets[0], true
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

// Source looks up a source by name.
func (p Product) Source(name string) (Source, bool) {
	for _, s := range p.Spec.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return Source{}, false
}
