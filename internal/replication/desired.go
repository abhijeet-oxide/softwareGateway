// Package replication turns a target's configured replication block into the
// document Quay's management API expects, and says what differs between the
// two.
//
// It sits between internal/product, which knows about ConfigMaps and Secrets,
// and internal/registry/quay, which knows about HTTP — and it belongs to
// neither. The translation is where every unit conversion, default and naming
// difference lives, so that neither side has to know about the other's
// vocabulary: durations become seconds here, `noProxy` becomes a comma-joined
// string here, and Quay's field names appear nowhere in a product document.
//
// See docs/design/18 sections 5 and 8.
package replication

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
)

// Credentials is what Quay will use to reach its upstream. Resolved by the
// caller from the Secret the configuration references, because reading Secrets
// is the composition root's job and not this package's.
type Credentials struct {
	Username string
	Password string
}

// Desired is the fully resolved state a target's configuration asks for:
// everything needed to write it, with nothing left to look up.
type Desired struct {
	Target string
	Mode   product.ReplicationMode

	// Namespace and Name are the Quay coordinates of the repository, split
	// from the target's configured path.
	Namespace string
	Name      string
	// Organization is the Quay org, for proxy-cache calls which are org-level.
	Organization string

	Mirror *quay.MirrorSpec
	Proxy  *quay.ProxyCache

	// SecretHash covers the credential VALUES this desired state carries.
	//
	// Separate from ConfigHash because Quay never returns a stored password:
	// comparing against the API response would report permanent, unfixable
	// drift on every credential field, which is the classic reconciler bug
	// that trains operators to ignore the drift signal entirely. Hashing the
	// value we sent means a ROTATED credential shows as drift and a redacted
	// response does not.
	SecretHash string
}

// Resolve builds the desired state for one target.
//
// now supplies the mirror's start date when the configuration does not, which
// it usually does not: Quay requires one, and "when we first applied it" is
// the only defensible default.
func Resolve(t product.Target, upstream string, creds Credentials, now time.Time) (*Desired, error) {
	namespace, name, ok := quay.SplitRepository(t.Repository)
	if !ok {
		return nil, fmt.Errorf("target %q: repository %q is not in Quay's organization/name form",
			t.Name, t.Repository)
	}

	d := &Desired{
		Target:       t.Name,
		Mode:         t.ReplicationMode(),
		Namespace:    namespace,
		Name:         name,
		Organization: namespace,
		SecretHash:   hashCredentials(creds),
	}

	switch d.Mode {
	case product.ReplicationMirror:
		m, ok := t.MirrorSettings()
		if !ok {
			return nil, fmt.Errorf("target %q: mode is mirror but no mirror block is configured", t.Name)
		}
		if upstream == "" {
			return nil, fmt.Errorf("target %q: the mirror upstream could not be resolved", t.Name)
		}
		d.Mirror = buildMirrorSpec(m, upstream, creds, now)
	case product.ReplicationProxy:
		c, ok := t.ProxySettings()
		if !ok {
			return nil, fmt.Errorf("target %q: mode is proxy but no proxy block is configured", t.Name)
		}
		d.Organization = c.Organization
		d.Proxy = buildProxyCache(c, creds)
	}
	return d, nil
}

func buildMirrorSpec(m *product.MirrorConfig, upstream string, creds Credentials, now time.Time) *quay.MirrorSpec {
	start := m.StartAt
	if start == "" {
		start = quay.FormatTime(now)
	}

	spec := &quay.MirrorSpec{
		IsEnabled:                m.IsEnabled(),
		ExtRef:                   upstream,
		ExternalRegistryUsername: creds.Username,
		ExternalRegistryPassword: creds.Password,
		SyncStartDate:            start,
		SyncInterval:             quay.SecondsOf(m.EffectiveInterval()),
		RobotUsername:            m.Robot,
		RootRule: quay.RootRule{
			RuleKind:  quay.RuleKindTagGlobCSV,
			RuleValue: append([]string(nil), m.Tags...),
		},
		SkopeoTimeoutInterval: quay.SecondsOf(m.EffectiveSkopeoTimeout()),
	}

	verify := m.VerifiesTLS()
	unsigned := m.AcceptUnsignedImages
	cfg := &quay.ExternalRegistryConfig{VerifyTLS: &verify, UnsignedImages: &unsigned}
	if m.Network != nil && m.Network.Proxy != nil {
		p := m.Network.Proxy
		cfg.Proxy = &quay.ProxyConfig{
			HTTPProxy:  p.HTTPProxy,
			HTTPSProxy: p.HTTPSProxy,
			// Quay takes one comma-joined string where our schema takes a list,
			// because a list is what a person can read and review.
			NoProxy: strings.Join(p.NoProxy, ","),
		}
	}
	spec.ExternalRegistryConfig = cfg
	return spec
}

func buildProxyCache(c *product.ProxyCacheConfig, creds Credentials) *quay.ProxyCache {
	return &quay.ProxyCache{
		UpstreamRegistry:         c.UpstreamRegistry,
		ExpirationS:              quay.SecondsOf(c.EffectiveExpiration()),
		Insecure:                 c.Insecure,
		UpstreamRegistryUsername: creds.Username,
		UpstreamRegistryPassword: creds.Password,
	}
}

// UpstreamReference renders what Quay should be told to pull from.
//
// `mirror.from` names another target, so the reference is that target's
// registry and repository — declared once, on the target, rather than repeated
// as a literal in every mirror that pulls from it.
func UpstreamReference(t product.Target) string {
	if t.Repository == "" {
		return t.Registry
	}
	return t.Registry + "/" + strings.Trim(t.Repository, "/")
}

// ResolveUpstream returns what a mirror target should pull from, following
// `from` to another target or taking `externalReference` as written.
func ResolveUpstream(p *product.Product, t product.Target) (string, error) {
	m, ok := t.MirrorSettings()
	if !ok {
		return "", nil
	}
	if m.ExternalReference != "" {
		return m.ExternalReference, nil
	}
	src, ok := p.Target(m.From)
	if !ok {
		return "", fmt.Errorf("target %q mirrors from %q, which is not a declared target", t.Name, m.From)
	}
	return UpstreamReference(src), nil
}

// ConfigHash is the fingerprint of the NON-SECRET desired configuration.
//
// Stored beside a successful apply as `applied_config_hash`, so a later reload
// can answer "is what Git says still what we wrote?" without a network call and
// without needing Quay to return anything it will not return.
func (d *Desired) ConfigHash() string {
	// Marshalled from an explicit ordered shape rather than from the spec
	// struct, so that adding a field to the wire type does not silently
	// invalidate every stored hash in the estate on the next deploy.
	payload := map[string]any{"mode": string(d.Mode), "namespace": d.Namespace, "name": d.Name}
	if d.Mirror != nil {
		payload["mirror"] = mirrorFingerprint(d.Mirror)
	}
	if d.Proxy != nil {
		payload["proxy"] = map[string]any{
			"organization":      d.Organization,
			"upstream_registry": d.Proxy.UpstreamRegistry,
			"expiration_s":      d.Proxy.ExpirationS,
			"insecure":          d.Proxy.Insecure,
			"username":          d.Proxy.UpstreamRegistryUsername,
		}
	}
	return hashJSON(payload)
}

func mirrorFingerprint(m *quay.MirrorSpec) map[string]any {
	out := map[string]any{
		"is_enabled":         m.IsEnabled,
		"external_reference": m.ExtRef,
		"sync_interval":      m.SyncInterval,
		"robot_username":     m.RobotUsername,
		"tags":               m.RootRule.RuleValue,
		"skopeo_timeout":     m.SkopeoTimeoutInterval,
		"username":           m.ExternalRegistryUsername,
	}
	// sync_start_date is deliberately EXCLUDED.
	//
	// It defaults to the moment of the apply, so including it would make every
	// hash differ from the last one and report permanent drift on a
	// configuration nobody had touched.
	if c := m.ExternalRegistryConfig; c != nil {
		cfg := map[string]any{}
		if c.VerifyTLS != nil {
			cfg["verify_tls"] = *c.VerifyTLS
		}
		if c.UnsignedImages != nil {
			cfg["unsigned_images"] = *c.UnsignedImages
		}
		if c.Proxy != nil {
			cfg["http_proxy"] = c.Proxy.HTTPProxy
			cfg["https_proxy"] = c.Proxy.HTTPSProxy
			cfg["no_proxy"] = c.Proxy.NoProxy
		}
		out["external_registry_config"] = cfg
	}
	return out
}

// hashCredentials fingerprints credential values so a rotation is detectable
// without the value ever being stored or logged.
func hashCredentials(c Credentials) string {
	if c.Username == "" && c.Password == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(c.Username + "\x00" + c.Password))
	return hex.EncodeToString(sum[:])
}

func hashJSON(v any) string {
	// json.Marshal orders map keys, so the encoding is stable.
	b, err := json.Marshal(v)
	if err != nil {
		// Unreachable for the shapes above; hashing the error keeps the
		// function total rather than growing a second return value that every
		// caller would have to ignore.
		b = []byte("unhashable:" + err.Error())
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
