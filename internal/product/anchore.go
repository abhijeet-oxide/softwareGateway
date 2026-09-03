package product

import "strings"

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

// Anchore overrides which Anchore a product's releases are registered with.
//
// # Why an override exists at all, when the deployment already has one
//
// Because "one Anchore per estate" is true right up until it is not. A product
// under a customer's own contract goes to that customer's Anchore; a product
// under evaluation goes to a staging one; a product owned by another business
// unit goes to the same Anchore under a different account, so its findings are
// visible to them and not to everybody. None of those is exotic and all of them
// are impossible with a single deployment-wide address.
//
// # Why it is the SAME mechanism as a source or a target
//
// `credentialsRef` naming a projected secret, resolved by the same
// SecretResolver, with the same `usernameKey` / `passwordKey` defaults. A
// second credential model for a third system is a second thing to rotate, a
// second thing to get wrong, and a second thing an operator has to learn - and
// the one they already know works.
//
// # Every field is optional, and absent means "the deployment's"
//
// A product that only needs a different ACCOUNT on the same Anchore writes one
// line and inherits the endpoint and the credential. A product that needs a
// different Anchore entirely writes an endpoint and a credentialsRef. Nothing
// has to be restated to change one thing, which is the difference between an
// override and a second copy of the configuration.
type Anchore struct {
	// Endpoint is the Anchore API base URL for this product's releases. Empty
	// uses the deployment's.
	Endpoint string `json:"endpoint,omitempty"`

	// CredentialsRef names the projected secret holding the username and
	// password (or an API key in the password key) for that Anchore.
	//
	// Empty uses the deployment's credential - which is right when this
	// override exists only to change the account, and wrong the moment the
	// endpoint changes. See validateAnchore: an endpoint of its own with no
	// credential of its own is refused, because it would send this
	// deployment's credential to somebody else's Anchore.
	CredentialsRef *CredentialsRef `json:"credentialsRef,omitempty"`

	// Account scopes this product's requests to one Anchore account, through
	// the `x-anchore-account` header. Admin-only in Anchore.
	//
	// The lightest override there is, and the common one: one Anchore, one
	// credential, and each business unit's products landing in their own
	// account rather than in everybody's.
	Account string `json:"account,omitempty"`
}

// AnchoreEndpoint is this product's Anchore address, or the deployment's.
func (p Product) AnchoreEndpoint(deploymentDefault string) string {
	if p.Spec.Anchore != nil && strings.TrimSpace(p.Spec.Anchore.Endpoint) != "" {
		return strings.TrimSpace(p.Spec.Anchore.Endpoint)
	}
	return deploymentDefault
}

// AnchoreAccount is this product's Anchore account, or the deployment's.
func (p Product) AnchoreAccount(deploymentDefault string) string {
	if p.Spec.Anchore != nil && strings.TrimSpace(p.Spec.Anchore.Account) != "" {
		return strings.TrimSpace(p.Spec.Anchore.Account)
	}
	return deploymentDefault
}

// AnchoreCredentials is this product's own Anchore credential, if it has one.
//
// Returns false when the product inherits the deployment's, which is the common
// case and is NOT an error - the caller falls back rather than failing.
func (p Product) AnchoreCredentials() (CredentialsRef, bool) {
	if p.Spec.Anchore == nil || p.Spec.Anchore.CredentialsRef == nil {
		return CredentialsRef{}, false
	}
	return *p.Spec.Anchore.CredentialsRef, true
}
