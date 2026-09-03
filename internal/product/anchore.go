package product

// Anchore, in one field.
//
// See docs/design/21-security-posture.md and docs/security/Anchore.md.
//
// # Why it is one field, like xrayEnabled and unlike everything else
//
// Because everything else about reaching Anchore is either already stated by
// the repository above it or is operator tuning that has nothing to do with a
// product.
//
// A repository already declares its registry host and its path, which is what
// Anchore has to be told to pull. The Anchore endpoint, the credential, the
// concurrency, the timeouts and how long a sync waits for analysis are one
// stanza in the SYSTEM configuration (config.AnchoreConfig), stated once for
// the deployment - because there is one Anchore in an estate, and repeating its
// host in every product document is a set of copies that drift.
//
// So a product document says exactly one thing about Anchore, and it is the
// same thing it says about Xray:
//
//	targets:
//	  - name: internal-jfrog
//	    type: jfrog
//	    xrayEnabled: true
//	    anchoreEnabled: true
//
// # Why it is on a repository rather than on the product
//
// Because "which images does Anchore analyse" is answered by "the ones in this
// repository". A product replicating to a lab and a production registry may
// legitimately want Anchore looking at production only - and the alternative,
// a product-level switch plus a separate field naming which repository it
// means, is two fields that can disagree.
//
// # Why it is valid on any registry type, where xrayEnabled is not
//
// Xray is a second endpoint on a JFrog platform, so asking for it on a Quay
// repository is a request that cannot be served and validation says so. Anchore
// PULLS an image over the registry API, so it works against any registry it can
// reach and has credentials for - and refusing `anchoreEnabled` on a Quay
// target would be this schema inventing a limitation Anchore does not have.
//
// What it cannot do is pull from a registry it cannot reach, and that is not a
// fact this document knows. It is reported by the sync, per image, with the
// sentence that names the fix.

// AnchoreIsEnabled reports whether Anchore is on for a repository.
//
// Nil-safe, and false for a nil, on the same argument as XrayIsEnabled: absent
// means OFF, which inverts the convention every other `enabled` in this schema
// follows, because the others turn off something the document asked for and
// this one would turn ON traffic to a third system the document never
// mentioned.
func AnchoreIsEnabled(enabled *bool) bool { return enabled != nil && *enabled }

// AnchoreFor reports whether one configured repository of a product has Anchore
// on, and where its images live.
//
// The registry and repository come back with it because they are what Anchore
// is told to pull, and the caller would otherwise have to walk the same lists
// again to find them.
func (p Product) AnchoreFor(role Role, name string) (enabled bool, registry, repository string) {
	if role == RoleTarget {
		for _, t := range p.Spec.Targets {
			if t.Name == name {
				return AnchoreIsEnabled(t.AnchoreEnabled), t.Registry, t.Repository
			}
		}
		return false, "", ""
	}
	for _, s := range p.Spec.Sources {
		if s.Name != name {
			continue
		}
		repo := s.Repository
		if repo == "" {
			if declared := s.DeclaredRepositories(); len(declared) > 0 {
				repo = declared[0]
			}
		}
		return AnchoreIsEnabled(s.AnchoreEnabled), s.Registry, repo
	}
	return false, "", ""
}

// AnchoreEnabledAnywhere reports whether a product has Anchore switched on for
// any repository.
//
// Used by the interface and the deep health check, which need to tell "this
// product has no Anchore" apart from "this release has not been analysed"
// without building a provider for every repository first.
func AnchoreEnabledAnywhere(p *Product) bool {
	for _, t := range p.Spec.Targets {
		if AnchoreIsEnabled(t.AnchoreEnabled) {
			return true
		}
	}
	for _, s := range p.Spec.Sources {
		if AnchoreIsEnabled(s.AnchoreEnabled) {
			return true
		}
	}
	return false
}
