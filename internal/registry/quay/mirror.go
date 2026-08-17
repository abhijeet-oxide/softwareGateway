package quay

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// RepositoryState is Quay's repository state, which mirroring depends on.
//
// MIRROR is not a flag: it makes the repository READ-ONLY to everyone but the
// configured robot and converges its content to the tag rule, deleting what
// does not match. That is why applying one is an explicit act in this system
// and never a side effect of a configuration reload.
type RepositoryState string

const (
	StateNormal   RepositoryState = "NORMAL"
	StateReadOnly RepositoryState = "READ_ONLY"
	StateMirror   RepositoryState = "MIRROR"
)

// SyncStatus is Quay's mirror sync status.
//
// StatusUnknown is a real member rather than an error case: Quay's enum has
// grown across versions, and a value we do not recognise must be reported as
// unrecognised rather than silently mapped onto SUCCESS or FAIL. Open question
// Q7 in the delivery plan is precisely whether this is observable enough to
// report honestly, and quietly guessing would answer it dishonestly.
type SyncStatus string

const (
	SyncNeverRun SyncStatus = "NEVER_RUN"
	SyncNow      SyncStatus = "SYNC_NOW"
	SyncSyncing  SyncStatus = "SYNCING"
	SyncSuccess  SyncStatus = "SUCCESS"
	SyncFail     SyncStatus = "FAIL"
	SyncUnknown  SyncStatus = "UNKNOWN"
)

// Terminal reports whether a status will not change without another trigger.
func (s SyncStatus) Terminal() bool {
	return s == SyncSuccess || s == SyncFail || s == SyncNeverRun
}

// Running reports whether Quay is working on it now, or is about to.
func (s SyncStatus) Running() bool {
	return s == SyncSyncing || s == SyncNow
}

func normalizeStatus(raw string) SyncStatus {
	switch SyncStatus(strings.ToUpper(strings.TrimSpace(raw))) {
	case SyncNeverRun:
		return SyncNeverRun
	case SyncNow:
		return SyncNow
	case SyncSyncing:
		return SyncSyncing
	case SyncSuccess:
		return SyncSuccess
	case SyncFail:
		return SyncFail
	default:
		return SyncUnknown
	}
}

// RootRule is Quay's tag selector. `tag_glob_csv` is the only kind Quay has,
// and its values are SHELL GLOBS, not regular expressions.
type RootRule struct {
	RuleKind  string   `json:"rule_kind"`
	RuleValue []string `json:"rule_value"`
}

// RuleKindTagGlobCSV is Quay's only rule kind.
const RuleKindTagGlobCSV = "tag_glob_csv"

// ExternalRegistryConfig is how Quay reaches the upstream. Quay's egress, not
// ours: nothing here affects our own transport.
type ExternalRegistryConfig struct {
	VerifyTLS      *bool        `json:"verify_tls,omitempty"`
	UnsignedImages *bool        `json:"unsigned_images,omitempty"`
	Proxy          *ProxyConfig `json:"proxy,omitempty"`
}

// ProxyConfig is Quay's egress proxy. `no_proxy` is a comma-joined string on
// the wire, which is why it is not a slice here.
type ProxyConfig struct {
	HTTPProxy  string `json:"http_proxy,omitempty"`
	HTTPSProxy string `json:"https_proxy,omitempty"`
	NoProxy    string `json:"no_proxy,omitempty"`
}

// MirrorSpec is what we WRITE. Separate from MirrorState, what we READ,
// because they are genuinely different documents: Quay never returns the
// credentials it stores, and a single struct used for both would make a
// redacted field look like a cleared one.
type MirrorSpec struct {
	IsEnabled bool   `json:"is_enabled"`
	ExtRef    string `json:"external_reference"`

	ExternalRegistryUsername string `json:"external_registry_username,omitempty"`
	ExternalRegistryPassword string `json:"external_registry_password,omitempty"`

	// SyncStartDate is RFC 3339. Quay requires one; the caller supplies the
	// apply time when the configuration does not say.
	SyncStartDate string `json:"sync_start_date"`
	// SyncInterval is SECONDS.
	SyncInterval int64 `json:"sync_interval"`

	RobotUsername string `json:"robot_username"`

	RootRule               RootRule                `json:"root_rule"`
	ExternalRegistryConfig *ExternalRegistryConfig `json:"external_registry_config,omitempty"`

	// SkopeoTimeoutInterval is seconds. Named as Quay names it.
	SkopeoTimeoutInterval int64 `json:"skopeo_timeout_interval,omitempty"`
}

// MirrorState is what Quay REPORTS. The write fields plus the observation
// fields, with credentials redacted by Quay itself.
type MirrorState struct {
	IsEnabled bool   `json:"is_enabled"`
	ExtRef    string `json:"external_reference"`

	ExternalRegistryUsername string `json:"external_registry_username"`

	SyncStartDate string `json:"sync_start_date"`
	SyncInterval  int64  `json:"sync_interval"`
	RobotUsername string `json:"robot_username"`

	RootRule               RootRule                `json:"root_rule"`
	ExternalRegistryConfig *ExternalRegistryConfig `json:"external_registry_config"`
	SkopeoTimeoutInterval  int64                   `json:"skopeo_timeout_interval"`

	// Observation. RawSyncStatus is kept beside the normalised value so an
	// unrecognised status can be reported verbatim instead of disappearing
	// into UNKNOWN.
	RawSyncStatus       string `json:"sync_status"`
	SyncExpirationDate  string `json:"sync_expiration_date"`
	SyncRetriesRemainng int    `json:"sync_retries_remaining"`
	MirrorType          string `json:"mirror_type"`
}

// Status returns the normalised sync status.
func (m *MirrorState) Status() SyncStatus { return normalizeStatus(m.RawSyncStatus) }

// Repository is the subset of Quay's repository resource we need: its state,
// which decides whether mirroring is even in play.
type Repository struct {
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Description string `json:"description"`
	IsPublic    bool   `json:"is_public"`
}

// GetRepository reads a repository, chiefly for its state.
func (c *Client) GetRepository(ctx context.Context, namespace, name string) (*Repository, error) {
	var out Repository
	if err := c.do(ctx, http.MethodGet, "/api/v1/repository/"+repoPath(namespace, name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetRepositoryState changes a repository's state.
//
// Setting MIRROR is the destructive step: from that moment the repository is
// read-only to everyone but the mirror robot, and its content converges to the
// tag rule. Callers must have shown a human what this will do first.
func (c *Client) SetRepositoryState(ctx context.Context, namespace, name string, state RepositoryState) error {
	body := struct {
		State string `json:"state"`
	}{State: string(state)}
	return c.do(ctx, http.MethodPut, "/api/v1/repository/"+repoPath(namespace, name)+"/changestate", body, nil)
}

// GetMirror reads a repository's mirror configuration.
//
// A repository with no mirror configured answers 404, which is a normal
// negative answer rather than a failure — the caller distinguishes them with
// errors.Is(err, registry.ErrNotFound).
func (c *Client) GetMirror(ctx context.Context, namespace, name string) (*MirrorState, error) {
	var out MirrorState
	if err := c.do(ctx, http.MethodGet, "/api/v1/repository/"+repoPath(namespace, name)+"/mirror", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutMirror writes a repository's mirror configuration, creating it if absent.
//
// Quay splits create (POST) and update (PUT) and answers 4xx for the wrong
// one. Callers should not have to know which, so this reads first and picks —
// one round trip more, and one whole class of "409 on a config that was
// already right" removed.
func (c *Client) PutMirror(ctx context.Context, namespace, name string, spec MirrorSpec) error {
	path := "/api/v1/repository/" + repoPath(namespace, name) + "/mirror"

	_, err := c.GetMirror(ctx, namespace, name)
	switch {
	case err == nil:
		return c.do(ctx, http.MethodPut, path, spec, nil)
	case errors.Is(err, registry.ErrNotFound):
		return c.do(ctx, http.MethodPost, path, spec, nil)
	default:
		return err
	}
}

// SyncNow asks Quay to run a sync immediately.
//
// Quay answers 409 when a sync is already running. That is not a failure —
// the caller asked for a sync and a sync is happening — so it is reported as
// such rather than as an error the operator has to interpret.
func (c *Client) SyncNow(ctx context.Context, namespace, name string) (alreadyRunning bool, err error) {
	err = c.do(ctx, http.MethodPost, "/api/v1/repository/"+repoPath(namespace, name)+"/mirror/sync-now", nil, nil)
	var rerr *registry.Error
	if errors.As(err, &rerr) && rerr.StatusCode == http.StatusConflict {
		return true, nil
	}
	return false, err
}

// CancelSync stops an in-progress sync. A 409 here means there was nothing
// running, which is the state the caller wanted.
func (c *Client) CancelSync(ctx context.Context, namespace, name string) (wasRunning bool, err error) {
	err = c.do(ctx, http.MethodPost, "/api/v1/repository/"+repoPath(namespace, name)+"/mirror/sync-cancel", nil, nil)
	var rerr *registry.Error
	if errors.As(err, &rerr) && rerr.StatusCode == http.StatusConflict {
		return false, nil
	}
	return err == nil, err
}

// ProxyCache is Quay's organization-level pull-through cache. One struct for
// read and write: unlike the mirror, Quay's proxy-cache resource has no
// observation fields, and the password is write-only in both directions.
type ProxyCache struct {
	UpstreamRegistry string `json:"upstream_registry"`
	// Expiration is SECONDS. Quay's field name; kept as Quay names it.
	ExpirationS int64 `json:"expiration_s,omitempty"`
	Insecure    bool  `json:"insecure,omitempty"`

	UpstreamRegistryUsername string `json:"upstream_registry_username,omitempty"`
	UpstreamRegistryPassword string `json:"upstream_registry_password,omitempty"`

	// OrgName is returned by Quay on a read and ignored on a write.
	OrgName string `json:"org_name,omitempty"`
}

// GetProxyCache reads an organization's proxy-cache configuration. Absent
// answers 404.
func (c *Client) GetProxyCache(ctx context.Context, org string) (*ProxyCache, error) {
	var out ProxyCache
	if err := c.do(ctx, http.MethodGet, "/api/v1/organization/"+url.PathEscape(org)+"/proxycache", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ValidateProxyCache asks Quay whether it can reach the upstream with these
// settings, WITHOUT storing them.
//
// Worth its own call: a proxy cache that cannot reach its upstream fails at
// the moment a pod pulls, which is both the worst time to find out and the
// hardest place to trace it back from.
func (c *Client) ValidateProxyCache(ctx context.Context, org string, spec ProxyCache) error {
	return c.do(ctx, http.MethodPost, "/api/v1/organization/"+url.PathEscape(org)+"/validateproxycache", spec, nil)
}

// CreateProxyCache stores an organization's proxy-cache configuration.
func (c *Client) CreateProxyCache(ctx context.Context, org string, spec ProxyCache) error {
	return c.do(ctx, http.MethodPost, "/api/v1/organization/"+url.PathEscape(org)+"/proxycache", spec, nil)
}

// DeleteProxyCache removes it.
func (c *Client) DeleteProxyCache(ctx context.Context, org string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/organization/"+url.PathEscape(org)+"/proxycache", nil, nil)
}

// SecondsOf renders a duration as the whole seconds Quay's fields take,
// rounding up so a sub-second value never becomes zero — Quay reads 0 as
// "unset" on some fields and as "immediately" on others, and neither is what a
// caller passing 800ms meant.
func SecondsOf(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	s := int64(d / time.Second)
	if d%time.Second != 0 {
		s++
	}
	return s
}

// FormatTime renders Quay's expected timestamp form.
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ParseTime reads one back, tolerating the two forms Quay emits across
// versions: RFC 3339 with an offset, and a naive UTC timestamp with none.
func ParseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
