package product

// JFrog Xray, in one field.
//
// See docs/design/21-security-posture.md §3.
//
// # What this deliberately is not
//
// It was a nested block with eight keys - an endpoint, a repository key, a
// watch list, a concurrency, a batch size, a timeout and two retentions. Every
// one of them was either already stated by the repository above it or an
// operational knob that has nothing to do with a product.
//
// A JFrog repository already declares its registry, its repository path, its
// credential, its CA bundle, its proxy and its timeouts. `type: jfrog` already
// says which backend serves it. So the only thing a PRODUCT document has left
// to say about Xray is whether it is on:
//
//	type: jfrog
//	xrayEnabled: true
//
// Everything else moved to where it belongs. The repository key is derived from
// the repository path, because it IS the first segment of it. The concurrency,
// batch size, timeout and retentions are operator tuning and live in the system
// configuration, once, rather than being repeated in every product document and
// drifting between them.
//
// # What is left, and why
//
// One escape hatch survives: XrayEndpoint. JFrog serves Docker two ways, and
// only one of them can be derived. A repository-path deployment puts everything
// on one hostname - `acme.jfrog.io/docker-local/app` - and there the platform
// base URL IS the registry host. A subdomain deployment gives each repository
// its own name - `acme-docker.jfrog.io/app` - and there it is not: Xray lives
// at `acme.jfrog.io`, and asking the docker subdomain returns a 404 that reads
// like a missing artifact rather than a wrong base URL.
//
// There is no reliable way to tell those apart from a hostname, so the
// derivation covers the common case and this field covers the other. It is
// absent from almost every document.

import "strings"

// IsJFrog reports whether a registry type is served by the JFrog plugin.
//
// Two spellings for one backend: `artifactory` is the historical name and stays
// canonical, `jfrog` is accepted because operators write it and rejecting it
// teaches nobody anything. Anything asking "can this repository do Xray" asks
// this, so the two spellings can never drift apart.
func (t RegistryType) IsJFrog() bool {
	return t == RegistryArtifactory || t == RegistryJFrog
}

// Canonical is the spelling that is PERSISTED.
//
// `jfrog` and `artifactory` select one backend, and the database stores one of
// them: `repositories.registry_type` carries a CHECK constraint, and admitting
// a second spelling there would mean rewriting the table on SQLite, which
// cannot alter a constraint in place - carrying every column and index added by
// every migration since. That rebuild is a standing hazard for a purely
// cosmetic gain: one dropped column and the package listing stops working,
// which is precisely what a first attempt at this did.
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

// XrayIsEnabled reports whether Xray is on for a repository.
//
// Nil-safe, and false for a nil: "the document did not mention Xray" and "the
// document switched Xray off" mean the same thing here, which is that no Xray
// request is made.
//
// Absent means OFF, which inverts the convention every other `enabled` in this
// schema follows. Deliberately: the others turn off something the document
// asked for, whereas this one would turn ON traffic to a third system the
// document never mentioned.
func XrayIsEnabled(enabled *bool) bool { return enabled != nil && *enabled }

// XrayRepositoryKey derives the Artifactory repository key from a repository
// path.
//
// It is the first segment, always: Artifactory addresses content as
// `<repoKey>/<path>`, so `docker-local/vendor-a/platform` is the repository
// `docker-local` holding `vendor-a/platform`, and `apm0014228-oci-stage` is a
// repository holding everything at its root.
//
// Derived rather than declared because a declared one is a second place to
// state a fact the document already states, and the second place is the one
// that goes stale - a repository moved to a new key would keep reporting the
// vulnerabilities of the old one, which is worse than reporting none.
func XrayRepositoryKey(repository string) string {
	repository = strings.Trim(strings.TrimSpace(repository), "/")
	if repository == "" {
		return ""
	}
	if i := strings.Index(repository, "/"); i > 0 {
		return repository[:i]
	}
	return repository
}

// XrayFor reports whether one configured repository of a product has Xray on,
// and where its platform lives.
//
// The second return value is not the same as the first: a Quay target with
// `xrayEnabled: true` is a configuration error caught by validation, and a
// JFrog target without it is a perfectly ordinary repository that simply has
// the integration switched off.
func (p Product) XrayFor(role Role, name string) (enabled bool, capable bool) {
	if role == RoleTarget {
		for _, t := range p.Spec.Targets {
			if t.Name == name {
				return XrayIsEnabled(t.XrayEnabled), t.Type.IsJFrog()
			}
		}
		return false, false
	}
	for _, s := range p.Spec.Sources {
		if s.Name == name {
			return XrayIsEnabled(s.XrayEnabled), s.Type.IsJFrog()
		}
	}
	return false, false
}
