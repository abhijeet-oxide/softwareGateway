// Package artifactory is the JFrog repository plugin.
//
// See docs/design/06-registry-abstraction.md §6.3 and
// docs/design/21-security-posture.md.
//
// # Two protocols, one host, one credential
//
// JFrog's artifact path is plain OCI Distribution v2 and is served by
// internal/registry/generic without a single deviation - the Repository here
// embeds it and overrides nothing, which is exactly what §6.5 predicts for a
// registry that does not actually differ.
//
// What this package adds is the OTHER endpoint on the same host: JFrog Xray, at
// `/xray/api`, which reports what is wrong with the artifacts rather than
// serving them. That is why Xray lives here rather than in a top-level plugin
// of its own:
//
//   - It is not a registry. It cannot list tags or serve a blob, so it does not
//     implement, and must not be forced to implement, registry.Source.
//   - It takes THE SAME CREDENTIAL as the repository. A separate plugin would
//     need its own endpoint, its own credential reference and its own secret
//     mount, all of them duplicates of ones this product already declares - and
//     the duplicate is the one that goes stale.
//   - It is scoped by the repository. "Is Xray on" is a property of a
//     configured JFrog repository, not of the estate, and a repository-scoped
//     switch belongs with the repository's own configuration.
//
// The core platform sees none of this. It receives internal/security's
// normalized model through the Provider boundary, and nothing above this
// directory imports an Xray type.
package artifactory

import (
	"log/slog"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
)

// TypeArtifactory and TypeJFrog are the configured names for this backend.
//
// Two names for one backend, because the product and the company are called
// different things and operators write both. `artifactory` is the historical
// spelling and stays the canonical one; `jfrog` is accepted because a document
// that says `type: jfrog` means something obvious and failing it teaches
// nobody anything.
const (
	TypeArtifactory = "artifactory"
	TypeJFrog       = "jfrog"
)

// Repository is a JFrog repository over OCI Distribution.
//
// It embeds the generic implementation and overrides nothing. That is not a
// placeholder: JFrog's distribution endpoint is conformant, and the deviations
// documented in §6.3 - repository-key path prefixes, tag pagination style,
// referrers availability - are handled as configuration and as probed
// capabilities rather than as code here. A file that overrode methods
// identically would be pure risk.
//
// The type exists so that `type: jfrog` selects a real thing, and so that the
// Xray attachment below has somewhere to hang.
type Repository struct {
	*generic.Repository
}

func init() {
	registry.Register(TypeArtifactory, New)
	registry.Register(TypeJFrog, New)
}

// New builds a JFrog repository client.
func New(c registry.ClientConfig) (registry.Source, error) {
	base, err := generic.New(generic.FromClientConfig(c))
	if err != nil {
		return nil, err
	}
	return &Repository{Repository: base}, nil
}

// XrayConfigFor derives the Xray client configuration from a repository's
// EXISTING JFrog configuration.
//
// Every transport-shaped field here is copied, never re-declared: the CA
// bundle, the proxy, the timeouts and the credential are the ones already
// configured for reaching this JFrog, because they describe reaching this
// JFrog. A separate credential model for Xray would be a second place for the
// same password to be rotated, and the second place is the one that is missed.
//
// Only the settings that are genuinely about Xray - whether it is on, which
// watches to scope to, how many requests may be in flight - come from the
// caller's XraySettings.
func XrayConfigFor(c registry.ClientConfig, s XraySettings) XrayConfig {
	log, _ := c.Logger.(*slog.Logger)
	return XrayConfig{
		Endpoint:      s.Endpoint,
		Registry:      c.Registry,
		RepositoryKey: s.RepositoryKey,
		Watches:       s.Watches,

		Username: c.Username,
		Password: c.Password,

		CABundle:           c.CABundle,
		HTTPSProxy:         c.HTTPSProxy,
		NoProxy:            c.NoProxy,
		DirectConnect:      c.DirectConnect,
		InsecureSkipVerify: c.InsecureSkipVerify,

		ConnectTimeout:        c.ConnectTimeout,
		ResponseHeaderTimeout: c.ResponseHeaderTimeout,
		RequestTimeout:        s.RequestTimeout,

		UserAgent:   c.UserAgent,
		Logger:      log,
		Concurrency: s.Concurrency,
		BatchSize:   s.BatchSize,
		PlainHTTP:   c.PlainHTTP,
	}
}

// XraySettings is the Xray-specific half of a JFrog repository's configuration:
// everything that is NOT already stated by the repository itself.
//
// Deliberately small. Anything that could be answered by "the same as the
// repository" is not here, and adding a field to this struct should require
// arguing that Xray genuinely differs from the registry it sits in front of.
type XraySettings struct {
	// Enabled switches the integration on for this repository. Off by default:
	// a repository whose document says nothing about Xray makes no Xray
	// requests and exposes no Xray data.
	Enabled bool
	// Endpoint overrides the JFrog platform base URL. Empty derives it from the
	// registry host, which is right for a platform served at one hostname and
	// wrong for a docker subdomain - see resolveEndpoint.
	Endpoint string
	// RepositoryKey is the Artifactory repository key ("docker-local"), used
	// for the path-based lookup that covers artifacts Xray indexed before it
	// knew their checksum.
	RepositoryKey string
	// Watches scopes results to named Xray watches. Empty means everything
	// Xray knows about the artifact.
	Watches []string
	// Concurrency caps in-flight Xray requests for this repository.
	Concurrency int
	// BatchSize is how many artifacts one summary request asks about.
	BatchSize int
	// RequestTimeout bounds a single Xray call end to end.
	RequestTimeout time.Duration
}

var _ registry.Source = (*Repository)(nil)
