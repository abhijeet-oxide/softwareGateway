package product

import (
	"fmt"
	"strings"
	"time"
)

// Replication says HOW content gets into a target, as opposed to what goes
// there. Absent means `copy`, which is what every document written before this
// block existed already means — so no configuration in any estate changes
// meaning by this type being added.
//
// See docs/design/18 section 5. The three modes differ in who moves the bytes
// and who owns the schedule:
//
//	copy    our workers pull from the origin and push here (today's behaviour)
//	mirror  Quay pulls from an upstream on its own schedule
//	proxy   a Quay organization fronts an upstream and fills when a pod pulls
type Replication struct {
	Mode ReplicationMode `json:"mode,omitempty"`

	// Mirror configures Quay's repository mirroring. Required when
	// Mode is mirror, rejected otherwise — a block that is silently
	// ignored is worse than an error.
	Mirror *MirrorConfig `json:"mirror,omitempty"`

	// Proxy configures a Quay proxy-cache organization. Required when
	// Mode is proxy, rejected otherwise.
	Proxy *ProxyCacheConfig `json:"proxy,omitempty"`
}

// ReplicationMode is the closed set of mechanisms.
type ReplicationMode string

const (
	ReplicationCopy   ReplicationMode = "copy"
	ReplicationMirror ReplicationMode = "mirror"
	ReplicationProxy  ReplicationMode = "proxy"
)

// ValidReplicationModes is the closed set. Must match the CHECK constraint on
// target_replication.mode in db/migrations.
var ValidReplicationModes = []ReplicationMode{
	ReplicationCopy, ReplicationMirror, ReplicationProxy,
}

// ManageMode says what we do about the registry-side configuration.
//
// `apply` writes it, but only on an explicit `targets apply` — never as a side
// effect of a config reload, because the write is destructive (docs/design/18
// section 8). `detect` never writes and only reports drift.
type ManageMode string

const (
	ManageApply  ManageMode = "apply"
	ManageDetect ManageMode = "detect"
)

// ValidManageModes is the closed set.
var ValidManageModes = []ManageMode{ManageApply, ManageDetect}

// Defaults for the fields Quay itself defaults, restated here so that what we
// send is what this document says rather than what a Quay version happens to
// do. DefaultSkopeoTimeout is Quay's own default; MaxSkopeoTimeout is its
// documented ceiling.
const (
	DefaultMirrorInterval    = 24 * time.Hour
	DefaultSkopeoTimeout     = 300 * time.Second
	MaxSkopeoTimeout         = 43200 * time.Second
	DefaultProxyCacheExpiry  = 24 * time.Hour
	MinMirrorInterval        = time.Minute
	DefaultMirrorSyncTimeout = 6 * time.Hour
)

// MirrorConfig is Quay's per-repository mirroring, one field per Quay field.
//
// Every value here is written INTO Quay and describes Quay's behaviour, not
// ours. That is the whole reason the block is nested rather than reusing the
// target's own `network` and `credentialsRef`: those describe how WE reach a
// registry, and confusing the two produces a mirror that works from our pods
// and fails from Quay's, or the reverse.
type MirrorConfig struct {
	// From names another target in this product as the upstream, so the chain
	// jfrog-store -> ocp-prod is declared once and stays consistent when the
	// JFrog path changes. It is also the edge a download rule's chain is
	// derived from (docs/design/20 section 4).
	From string `json:"from,omitempty"`

	// ExternalReference is the escape hatch for an upstream this product does
	// not otherwise declare: `host/path`, no tag and no digest. Exactly one of
	// From and ExternalReference is required.
	ExternalReference string `json:"externalReference,omitempty"`

	// Tags is Quay's only selector, and it is SHELL GLOBS -- not the RE2 used
	// by tagFilters and download rules. There is no faithful translation in
	// either direction, so the field is named `tags` rather than `tagPattern`
	// and validation rejects RE2 syntax outright (docs/design/18 section 5.3).
	Tags []string `json:"tags,omitempty"`

	// Interval maps to Quay's sync_interval. StartAt maps to sync_start_date
	// and defaults to the moment the configuration is first applied.
	Interval Duration `json:"interval,omitempty"`
	StartAt  string   `json:"startAt,omitempty"`

	// Enabled maps to is_enabled: pause syncing while keeping the
	// configuration. Defaults to true.
	Enabled *bool `json:"enabled,omitempty"`

	// Robot is the Quay robot that performs the push, `org+name`. Required by
	// Quay, and there is no default we could invent.
	Robot string `json:"robot,omitempty"`

	// SourceCredentialsRef is what QUAY will use to reach the upstream. These
	// leave our custody -- see docs/design/18 section 11. Omit for an
	// anonymous upstream.
	SourceCredentialsRef *CredentialsRef `json:"sourceCredentialsRef,omitempty"`

	// Network is Quay's egress, not ours.
	Network *MirrorNetwork `json:"network,omitempty"`

	// AcceptUnsignedImages maps to external_registry_config.unsigned_images.
	AcceptUnsignedImages bool `json:"acceptUnsignedImages,omitempty"`

	// SkopeoTimeout bounds one skopeo invocation inside Quay. Quay's default
	// is 300s and its ceiling 43200s.
	SkopeoTimeout Duration `json:"skopeoTimeout,omitempty"`

	// Manage says whether we ever write this configuration. Defaults to apply.
	Manage ManageMode `json:"manage,omitempty"`

	// SyncOnRequest makes a download against this target a sync-now plus a
	// wait, rather than an error. Defaults to true.
	SyncOnRequest *bool `json:"syncOnRequest,omitempty"`
}

// MirrorNetwork is the TLS and proxy posture Quay uses to reach the upstream.
type MirrorNetwork struct {
	// VerifyTLS maps to external_registry_config.verify_tls. Defaults to true.
	VerifyTLS *bool        `json:"verifyTLS,omitempty"`
	Proxy     *MirrorProxy `json:"proxy,omitempty"`
}

// MirrorProxy is Quay's egress proxy. Named separately from the product's own
// Proxy type because it has no `direct` field: we cannot tell Quay to ignore
// its own environment, so offering the switch would be a lie.
type MirrorProxy struct {
	HTTPProxy  string   `json:"httpProxy,omitempty"`
	HTTPSProxy string   `json:"httpsProxy,omitempty"`
	NoProxy    []string `json:"noProxy,omitempty"`
}

// ProxyCacheConfig is Quay's organization-level pull-through cache.
//
// Note what is NOT here: anything about what to cache. A proxy cache is lazy
// by construction -- content enters on a pull and never before -- which is
// exactly why a proxy target cannot be a transfer destination.
type ProxyCacheConfig struct {
	// Organization is the Quay org that IS the cache. It must agree with the
	// first path segment of the target's repository, because that path is the
	// proxy layout and two spellings that disagree cannot both be right.
	Organization string `json:"organization,omitempty"`

	// UpstreamRegistry is a registry host, optionally with a namespace:
	// `artifactory.example.com/docker-local`.
	UpstreamRegistry       string          `json:"upstreamRegistry,omitempty"`
	UpstreamCredentialsRef *CredentialsRef `json:"upstreamCredentialsRef,omitempty"`

	// Expiration maps to expiration_s. Quay's default is 86400s.
	Expiration Duration `json:"expiration,omitempty"`

	// Insecure disables upstream TLS validation inside Quay.
	Insecure bool `json:"insecure,omitempty"`

	// Manage says whether we ever write this configuration.
	Manage ManageMode `json:"manage,omitempty"`

	// Prewarm pulls every blob of a discovered package THROUGH the proxy so
	// the cache is populated before anyone needs it. Off by default because it
	// costs a full pull whose bytes are discarded (docs/design/18 section 6.3).
	Prewarm bool `json:"prewarm,omitempty"`
}

// QuaySettings is how we speak to a Quay registry's CONTROL API, which is not
// its registry API and does not take the same credential.
//
//	/v2/...     artifacts       robot account, `org+robot` as the username
//	/api/v1/... configuration   OAuth 2 bearer with repo:admin or org:admin
//
// Declared beside credentialsRef rather than inside replication, because it is
// a property of speaking to this registry's control API in the same way
// credentialsRef is a property of speaking to its data API -- and because a
// future Quay capability (quota, notifications) will want the same token
// without belonging to replication at all.
//
// A copy-mode Quay target needs none of this.
type QuaySettings struct {
	APITokenRef *SecretRef `json:"apiTokenRef,omitempty"`

	// APIEndpoint defaults to https:// plus the target's registry host.
	APIEndpoint string `json:"apiEndpoint,omitempty"`
}

// ReplicationMode returns the target's effective mode. Absent is copy.
func (t Target) ReplicationMode() ReplicationMode {
	if t.Replication == nil || t.Replication.Mode == "" {
		return ReplicationCopy
	}
	return t.Replication.Mode
}

// Delegated reports whether the registry, rather than our fleet, moves the
// bytes into this target. It is the predicate the transfer path branches on,
// and the predicate every "no byte progress" rule is written against.
func (t Target) Delegated() bool {
	return t.ReplicationMode() != ReplicationCopy
}

// MirrorSettings returns the mirror block when the target is in mirror mode.
func (t Target) MirrorSettings() (*MirrorConfig, bool) {
	if t.ReplicationMode() != ReplicationMirror || t.Replication.Mirror == nil {
		return nil, false
	}
	return t.Replication.Mirror, true
}

// ProxySettings returns the proxy block when the target is in proxy mode.
func (t Target) ProxySettings() (*ProxyCacheConfig, bool) {
	if t.ReplicationMode() != ReplicationProxy || t.Replication.Proxy == nil {
		return nil, false
	}
	return t.Replication.Proxy, true
}

// EffectiveInterval is the sync period sent to Quay.
func (m *MirrorConfig) EffectiveInterval() time.Duration {
	return m.Interval.Or(DefaultMirrorInterval)
}

// EffectiveSkopeoTimeout is the per-invocation timeout sent to Quay, clamped
// to Quay's ceiling. Clamping rather than erroring at this point because
// validation has already rejected an over-large value; this is the belt to
// that braces, for a value that arrived from somewhere other than a document.
func (m *MirrorConfig) EffectiveSkopeoTimeout() time.Duration {
	d := m.SkopeoTimeout.Or(DefaultSkopeoTimeout)
	if d > MaxSkopeoTimeout {
		return MaxSkopeoTimeout
	}
	return d
}

// IsEnabled reports Quay's is_enabled. Defaults to true.
func (m *MirrorConfig) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

// SyncsOnRequest reports whether a download becomes a sync-now. Defaults true.
func (m *MirrorConfig) SyncsOnRequest() bool {
	return m.SyncOnRequest == nil || *m.SyncOnRequest
}

// VerifiesTLS reports Quay's TLS posture towards the upstream. Defaults true.
func (m *MirrorConfig) VerifiesTLS() bool {
	if m.Network == nil || m.Network.VerifyTLS == nil {
		return true
	}
	return *m.Network.VerifyTLS
}

// ManageMode is the effective management mode. Defaults to apply.
func (m *MirrorConfig) ManageMode() ManageMode {
	if m.Manage == "" {
		return ManageApply
	}
	return m.Manage
}

// EffectiveExpiration is the cache staleness window sent to Quay.
func (p *ProxyCacheConfig) EffectiveExpiration() time.Duration {
	return p.Expiration.Or(DefaultProxyCacheExpiry)
}

// ManageMode is the effective management mode. Defaults to apply.
func (p *ProxyCacheConfig) ManageMode() ManageMode {
	if p.Manage == "" {
		return ManageApply
	}
	return p.Manage
}

// Organization returns the Quay organization a target's repository sits in,
// which is its first path segment. Quay's model is org/repository and both the
// robot and the proxy cache are scoped to the org, so this is needed to check
// that a robot belongs where it is being used.
func (t Target) Organization() string {
	repo := strings.TrimPrefix(t.Repository, "/")
	if i := strings.Index(repo, "/"); i >= 0 {
		return repo[:i]
	}
	return repo
}

// RobotOrganization splits `org+name` and returns the org.
func RobotOrganization(robot string) (string, bool) {
	i := strings.Index(robot, "+")
	if i <= 0 || i == len(robot)-1 {
		return "", false
	}
	return robot[:i], true
}

// re2InGlob names the RE2 constructs that make a "glob" silently match nothing.
//
// This is the most expensive mistake available in this block. `^v\d+\.\d+$`
// is a perfectly good tag filter everywhere else in this schema; as a Quay tag
// glob it matches zero tags, the sync succeeds, and the destination stays
// empty forever while reporting success.
var re2InGlob = []struct {
	token string
	what  string
}{
	{`\d`, `\d`},
	{`\w`, `\w`},
	{`\.`, `\.`},
	{`(?`, `(?...)`},
	{`+`, `+`},
	{`|`, `|`},
}

// GlobDialectError returns a message when a tag glob contains RE2 syntax, and
// the empty string when it looks like a glob.
//
// Anchors are checked separately from the escapes because `^` and `$` are the
// giveaway that someone pasted a regex, and they deserve to be named first.
func GlobDialectError(glob string) string {
	switch {
	case strings.HasPrefix(glob, "^"), strings.HasSuffix(glob, "$"):
		return fmt.Sprintf("%q is a regular expression, not a glob: Quay accepts shell wildcards only", glob)
	}
	for _, c := range re2InGlob {
		if strings.Contains(glob, c.token) {
			return fmt.Sprintf("%q contains %s, which is regular-expression syntax: Quay accepts shell wildcards only", glob, c.what)
		}
	}
	return ""
}

// SelectsSignatures reports whether a glob set can match cosign signature
// tags, which are `sha256-<hex>.sig`.
//
// This exists because the obvious rule -- mirror `v3.*` -- excludes every
// signature, so the sync succeeds and destination verification then fails for
// every package with no visible cause (docs/design/18 section 9).
func SelectsSignatures(globs []string) bool {
	for _, g := range globs {
		if g == "*" || strings.HasPrefix(g, "sha256-") {
			return true
		}
	}
	return false
}
