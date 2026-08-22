package product

import "time"

// Xray configures the JFrog Xray integration for one JFrog repository.
//
// See docs/design/21-security-posture.md §3.
//
// # What is deliberately absent
//
// An endpoint credential. A username. A password. A secret reference. A
// timeout for the credential exchange. None of them are here, and none of them
// will be added, because JFrog repository credentials ALREADY EXIST - JFrog is
// a supported repository type and every JFrog repository in every product
// already declares a `credentialsRef`. Xray sits on the same platform, takes
// the same credential, and is reached over the same network path with the same
// CA bundle and the same proxy.
//
// A second credential model for the same host is not a feature. It is a second
// place for one password to be rotated, and the second place is the one that is
// missed - the failure arriving weeks later as an Xray integration that quietly
// stopped answering while the repository kept working.
//
// So this block holds only what is genuinely about Xray: whether it is on, what
// to ask it, and how hard to ask.
type Xray struct {
	// Enabled switches the integration on for this repository.
	//
	// Absent means OFF. That is the opposite of the convention used by
	// `enabled` elsewhere in this schema, and it is deliberate: every other
	// `enabled` field turns off something the document explicitly asked for,
	// whereas this one would turn ON traffic to a third system that the
	// document never mentioned. A repository that says nothing about Xray makes
	// no Xray requests and exposes no Xray data.
	Enabled *bool `json:"enabled,omitempty"`

	// Endpoint is the JFrog PLATFORM base URL - `https://acme.jfrog.io`.
	//
	// Absent derives it from the repository's registry host, which is correct
	// for a repository-path deployment (`acme.jfrog.io/docker-local/app`) and
	// wrong for a subdomain one (`acme-docker.jfrog.io/app`), where Xray lives
	// on the platform hostname and the docker subdomain answers 404. There is
	// no way to tell the two apart from the hostname, so the derivation covers
	// the common case and this field covers the other.
	Endpoint string `json:"endpoint,omitempty"`

	// RepositoryKey is the Artifactory repository key - `docker-local`.
	// Used for the path-based lookup that covers artifacts indexed before Xray
	// recorded a checksum we would recognise.
	RepositoryKey string `json:"repositoryKey,omitempty"`

	// Watches scopes results to named Xray watches, so a repository can report
	// what its own policy cares about rather than everything Xray knows.
	// Empty means everything.
	Watches []string `json:"watches,omitempty"`

	// Concurrency caps Xray requests in flight for this repository.
	//
	// Small by default and worth keeping small. Xray's summary endpoint is
	// expensive server-side and rate-limited on hosted JFrog; sixty parallel
	// requests is not ten times faster than six, it is a 429 storm and a slower
	// answer.
	Concurrency int `json:"concurrency,omitempty"`

	// BatchSize is how many artifacts one summary request asks about. It bounds
	// the blast radius of a failed call: one failure costs this many artifacts'
	// results rather than a whole release's.
	BatchSize int `json:"batchSize,omitempty"`

	// Timeout bounds one Xray call end to end.
	Timeout Duration `json:"timeout,omitempty"`

	// DetailTTL is how long a COMPLETE vulnerability response may be cached.
	//
	// Short, and capped by validation, because Xray is the source of truth for
	// detailed findings and this platform is explicitly not a system of record
	// for them. The cache exists so that reopening a finding, repeating a
	// comparison or generating an export does not re-query Xray - not so that
	// the platform can answer without Xray at all.
	DetailTTL Duration `json:"detailTtl,omitempty"`

	// SummaryTTL is how long the LIGHTWEIGHT summary - counts, severities,
	// status - is served before it is refreshed.
	//
	// Much longer than DetailTTL, because it is a different kind of data with a
	// different cost of being stale. A package listing showing counts from an
	// hour ago is useful; a listing that queries Xray for 157 artifacts on
	// every page render is not a listing.
	SummaryTTL Duration `json:"summaryTtl,omitempty"`
}

// Defaults for the Xray block. Stated here rather than in the plugin so that
// `transferctl config check` can print what a document will actually do.
const (
	DefaultXrayConcurrency = 6
	DefaultXrayBatchSize   = 50
	DefaultXrayTimeout     = 60 * time.Second

	// DefaultXrayDetailTTL keeps a complete response long enough to reopen a
	// finding, repeat a comparison and export it - minutes of one person's
	// work - and not long enough to become a stale second copy of Xray.
	DefaultXrayDetailTTL = 15 * time.Minute
	// MaxXrayDetailTTL is the ceiling validation enforces. Past this the cache
	// stops being a cache and starts being an unsynchronised replica of a
	// system that updates its findings continuously.
	MaxXrayDetailTTL = 24 * time.Hour

	// DefaultXraySummaryTTL is how long counts are good for.
	DefaultXraySummaryTTL = 6 * time.Hour

	// MaxXrayBatchSize bounds one request. Xray accepts more; a request this
	// large already takes long enough that its failure is expensive.
	MaxXrayBatchSize = 200
	// MaxXrayConcurrency bounds what one repository may do to one Xray.
	MaxXrayConcurrency = 32
)

// IsEnabled reports whether Xray is on for this repository.
//
// Nil-safe, and false for a nil block: "the document did not mention Xray"
// and "the document switched Xray off" mean the same thing here, which is that
// no Xray request is made.
func (x *Xray) IsEnabled() bool {
	return x != nil && x.Enabled != nil && *x.Enabled
}

// ConcurrencyOrDefault, BatchSizeOrDefault and the TTL accessors resolve one
// field each, so that no caller re-implements a default and gets it subtly
// different. Every one of them is nil-safe.
func (x *Xray) ConcurrencyOrDefault() int {
	if x == nil || x.Concurrency <= 0 {
		return DefaultXrayConcurrency
	}
	return x.Concurrency
}

func (x *Xray) BatchSizeOrDefault() int {
	if x == nil || x.BatchSize <= 0 {
		return DefaultXrayBatchSize
	}
	return x.BatchSize
}

func (x *Xray) TimeoutOrDefault() time.Duration {
	if x == nil {
		return DefaultXrayTimeout
	}
	return x.Timeout.Or(DefaultXrayTimeout)
}

func (x *Xray) DetailTTLOrDefault() time.Duration {
	if x == nil {
		return DefaultXrayDetailTTL
	}
	return x.DetailTTL.Or(DefaultXrayDetailTTL)
}

func (x *Xray) SummaryTTLOrDefault() time.Duration {
	if x == nil {
		return DefaultXraySummaryTTL
	}
	return x.SummaryTTL.Or(DefaultXraySummaryTTL)
}

// IsJFrog reports whether a registry type is served by the JFrog plugin.
//
// Two spellings for one backend: `artifactory` is the historical name and stays
// canonical, `jfrog` is accepted because operators write it and rejecting it
// teaches nobody anything. Anything that asks "can this repository do Xray"
// asks this, so the two spellings can never drift apart.
func (t RegistryType) IsJFrog() bool {
	return t == RegistryArtifactory || t == RegistryJFrog
}

// Canonical is the spelling that is PERSISTED.
//
// `jfrog` and `artifactory` select one backend, and the database stores one of
// them: `repositories.registry_type` carries a CHECK constraint, and admitting
// a second spelling there would mean rewriting the table on SQLite - which
// cannot alter a constraint in place, so the table has to be recreated,
// carrying every column and index added by every migration since. That rebuild
// is a standing hazard for a purely cosmetic gain: one dropped column and the
// package listing stops working, which is precisely what a first attempt at
// this did.
//
// So the document accepts both and the schema keeps knowing one. Nothing above
// the catalog notices, because everything that asks "is this JFrog" calls
// IsJFrog.
func (t RegistryType) Canonical() RegistryType {
	if t == RegistryJFrog {
		return RegistryArtifactory
	}
	return t
}

// XrayFor returns the Xray configuration of one configured repository, by name
// and role, and whether that repository can do Xray at all.
//
// The second return value is not the same as `x != nil`: a Quay source with an
// xray block would be a configuration error caught by validation, and a JFrog
// source with none is a perfectly ordinary repository that simply has the
// integration switched off.
func (p Product) XrayFor(role Role, name string) (*Xray, bool) {
	if role == RoleTarget {
		for _, t := range p.Spec.Targets {
			if t.Name == name {
				return t.Xray, t.Type.IsJFrog()
			}
		}
		return nil, false
	}
	for _, s := range p.Spec.Sources {
		if s.Name == name {
			return s.Xray, s.Type.IsJFrog()
		}
	}
	return nil, false
}
