// Package vendor holds the seam between what OCI standardises and what an
// individual vendor actually does.
//
// The problem it solves, stated concretely. Our first real vendor publishes
// THREE tags per release:
//
//	orb_23.8.1076              the payload - an index of Helm charts and images
//	signature_orb_23.8.1076    a manifest whose only layer is a PKCS#7 blob
//	signed_orb_23.8.1076       an index referencing both of the above
//
// Left alone, discovery records those as three unrelated packages: one release
// becomes three rows, forty-eight repositories become three times the noise,
// and - the part that actually matters - asking to transfer `orb_23.8.1076`
// moves the payload and LEAVES THE SIGNATURE BEHIND, after which nobody can
// ever verify what landed.
//
// # The concept is standard; only the mechanism is not
//
// "A package has artifacts that belong to it but sit outside its own manifest
// tree" is exactly what OCI 1.1 formalises with `subject` and the referrers
// API. Signatures, SBOMs and attestations are all that shape. What differs
// between vendors is only HOW THE RELATIONSHIP IS DISCOVERED:
//
//	referrers      the signature carries `subject`; ask the registry (the standard)
//	cosign tag     rewrite the digest as sha256-<hex>.sig and fetch it
//	wrapper index  a third tag bundles both as siblings (pre-1.1; our vendor)
//
// So this package models the CONCEPT, and a Layout supplies the mechanism.
// Everything downstream - storage, listing, planning, transfer - sees one
// shape and cannot tell which mechanism produced it. A vendor that already
// uses referrers needs no plugin at all, and when a vendor migrates to
// referrers we change one file.
//
// # What a Layout may and may not do
//
// A Layout CLASSIFIES AND RELATES. It does not fetch beyond what it is handed,
// does not push, and does not decide transfer policy. That bound is deliberate:
// a broken Layout can mislabel a tag, but it cannot corrupt a transfer.
//
// # Keeping the core generic
//
// internal/discovery and internal/transfer depend on this package's interface
// and are forbidden by depguard from importing any implementation. The check is
// mechanical rather than aspirational: DELETE internal/vendors/near AND
// EVERYTHING STILL BUILDS AND PASSES.
package vendors

import (
	"context"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Role is what a related artifact is FOR.
//
// Vendor-neutral by construction: the database stores `signature`, never
// `nokia_signature`. A vendor's naming lives in the plugin that produced the
// row, not in the schema that holds it.
type Role string

const (
	// RoleSignature is a detached signature over the package - cosign bundle,
	// PKCS#7, or whatever the vendor emits. The format is a separate axis from
	// the role, because a vendor may pair either discovery mechanism with
	// either format.
	RoleSignature Role = "signature"
	// RoleSBOM is a software bill of materials.
	RoleSBOM Role = "sbom"
	// RoleAttestation is an in-toto or similar provenance attestation.
	RoleAttestation Role = "attestation"
	// RoleWrapper is an artifact that exists only to bundle a package with its
	// related artifacts - our vendor's `signed_orb_*`.
	//
	// Recorded rather than discarded because it is the transfer root: pushing
	// it at the destination is what makes the destination a faithful copy, so
	// a consumer expecting the vendor's layout still finds it.
	RoleWrapper Role = "wrapper"
)

// ScannedTag is one tag discovery has resolved, handed to a Layout for
// grouping.
//
// Raw is the manifest EXACTLY as the registry returned it. A Layout may parse
// it but must never re-serialize it: the digest is the hash of these bytes and
// every signature is over that digest.
type ScannedTag struct {
	Tag        string
	Descriptor registry.Descriptor
	Raw        []byte
	// Annotations are the manifest's own, already parsed by discovery. Most
	// Layouts need only these and never touch Raw.
	Annotations map[string]string
	// Children are the descriptors an index lists. Empty for a plain manifest.
	Children []registry.Descriptor
}

// Related is one artifact that belongs to a package but lives outside its tree.
type Related struct {
	Role Role
	// Tag is where the artifact lives, when it has one. A referrers-discovered
	// signature usually has no tag and is addressed by digest alone.
	Tag        string
	Descriptor registry.Descriptor
}

// Package is one release as a person thinks of it.
//
// The distinction between Tag and Root is the whole point of this type. Tag is
// IDENTITY - what appears in a listing and what someone types. Root is what the
// planner walks. For our vendor they differ: you name `orb_23.8.1076` and the
// transfer walks `signed_orb_23.8.1076`, because only the wrapper reaches both
// the payload and the signature. For a standard source they are the same.
type Package struct {
	Tag        string
	Descriptor registry.Descriptor

	// DisplayTag is the tag with this vendor's structural noise removed -
	// `23.8.1076` for a vendor whose real tag is `orb_23.8.1076`.
	//
	// Set by the Layout, because the convention being removed is the vendor's
	// and nothing else in the system may know it. Empty means "no shortening",
	// which is what any conformant registry gets.
	//
	// Cosmetic ONLY: the real Tag is what is stored, transferred and returned
	// by `-o json`, and BOTH spellings resolve as input - an abbreviation you
	// cannot type back is a trap, not a convenience.
	DisplayTag string

	// Root is the descriptor a transfer plans from. Zero means "use
	// Descriptor" - the ordinary case, and the reason a standard Layout can
	// leave it alone.
	Root registry.Descriptor
	// RootTag names Root when it has a tag of its own, so the destination can
	// be tagged identically to the source.
	RootTag string

	Related []Related
}

// EffectiveRoot returns the descriptor a transfer should walk.
func (p Package) EffectiveRoot() registry.Descriptor {
	if p.Root.Digest != "" {
		return p.Root
	}
	return p.Descriptor
}

// SignatureStatus is a THREE-state answer, and the third state is the point.
//
// "We looked and found nothing" and "we never looked" are the same value in a
// boolean and completely different facts when someone is deciding whether to
// trust a package. A vendor does not sign everything - older releases and
// hotfixes routinely go unsigned - so the distinction shows up in real data,
// not just in theory.
type SignatureStatus string

const (
	// SignatureSigned means a signature artifact was found. It says nothing
	// about whether that signature is VALID; verification is a separate step.
	SignatureSigned SignatureStatus = "signed"
	// SignatureUnsigned means we looked for one and there was none.
	SignatureUnsigned SignatureStatus = "unsigned"
	// SignatureUnknown means nobody has looked - the layout is `none`, or the
	// row predates signature discovery.
	SignatureUnknown SignatureStatus = "unknown"
)

// Status reports whether this package carries a signature.
func (p Package) Status(looked bool) SignatureStatus {
	if !looked {
		return SignatureUnknown
	}
	for _, r := range p.Related {
		if r.Role == RoleSignature {
			return SignatureSigned
		}
	}
	return SignatureUnsigned
}

// Vocabulary is what a vendor's users call the things a scan counts.
//
// The counterpart of DisplayRepository and DisplayTag, one level up: those
// shorten a NAME, this one names the KIND. A NEAR operator does not have
// repositories and tags, they have orbs and orb versions, and a summary reading
//
//	Repositories scanned   42
//	Tags listed            16921
//
// makes them translate every line before they can read it. Nothing outside a
// Layout may know the mapping, for exactly the reason nothing outside a Layout
// may know that `orbs/` is a prefix worth removing.
//
// Empty fields mean "no special word", and the caller falls back to the
// standard OCI nouns - which is what any conformant source gets.
type Vocabulary struct {
	// Unit and Units name what holds versions: "repository" / "orb".
	Unit  string
	Units string
	// Version and Versions name what a repository holds: "tag" / "orb version".
	Version  string
	Versions string
}

// Or fills in whatever this Vocabulary leaves blank from another.
func (v Vocabulary) Or(fallback Vocabulary) Vocabulary {
	if v.Unit == "" {
		v.Unit = fallback.Unit
	}
	if v.Units == "" {
		v.Units = fallback.Units
	}
	if v.Version == "" {
		v.Version = fallback.Version
	}
	if v.Versions == "" {
		v.Versions = fallback.Versions
	}
	return v
}

// StandardVocabulary is the OCI wording, used wherever a vendor supplies none.
func StandardVocabulary() Vocabulary {
	return Vocabulary{
		Unit: "repository", Units: "repositories",
		Version: "tag", Versions: "tags",
	}
}

// Layout is how one vendor lays packages out in a repository.
//
// One method. Everything a Layout does - collapsing several tags into one
// release, naming the transfer root, locating signatures - is expressible as
// "turn the tags I scanned into the packages they actually represent".
type Layout interface {
	// Name is the value that selects this Layout in configuration.
	Name() string

	// Vocabulary is what this vendor's users call a repository and a tag. The
	// zero value means the standard OCI nouns.
	Vocabulary() Vocabulary

	// Group turns a repository's scanned tags into packages.
	//
	// It receives EVERY admitted tag of one repository at once, because a
	// vendor's relationships are between tags and cannot be resolved one at a
	// time - discovery has no ordering guarantee, so seeing `orb_X` alone tells
	// you nothing about whether a `signed_orb_X` exists.
	//
	// src is available for Layouts that must ask the registry (referrers).
	// A Layout that can answer from the scanned set alone should not use it:
	// discovery already paid for those bytes.
	Group(ctx context.Context, src registry.Source, scanned []ScannedTag) ([]Package, error)

	// LooksForSignatures reports whether this Layout actually attempts
	// signature discovery, which is what separates "unsigned" from "unknown".
	LooksForSignatures() bool

	// DisplayRepository is the repository path with this vendor's structural
	// noise removed - `cfx-5000-k8s` for a vendor who puts every product under
	// `orbs/`.
	//
	// The counterpart of Package.DisplayTag, and it exists for the same reason:
	// the transform is the VENDOR's, and nothing outside the plugin may know it.
	// This used to be done in the CLI by dropping the prefix every row in view
	// happened to share, which needed no vendor knowledge and was wrong for
	// exactly that reason - it shortened paths on registries that have no such
	// convention, and it changed what a row said depending on which other rows
	// were on screen.
	//
	// Empty means "no shortening", which is what any conformant registry gets.
	// Cosmetic ONLY: the real path is what is stored and transferred, and BOTH
	// spellings resolve as input.
	DisplayRepository(path string) string

	// DisplayTag is a tag with this vendor's structural noise removed -
	// `23.8.1076` for a vendor whose real tag is `orb_23.8.1076`.
	//
	// The same transform Group applies when it sets Package.DisplayTag, exposed
	// separately because it must be answerable FROM THE TAG STRING ALONE, with
	// no manifest and no registry call.
	//
	// That requirement is not decoration. Discovery skips a tag it has already
	// recorded - one HEAD, no fetch, no grouping - so a source that gains
	// `vendor: near` after its packages were discovered would otherwise keep
	// their unshortened names forever, and no amount of re-scanning would fix
	// it. The scanner reconciles the stored display names against this method on
	// every pass, which costs one query per repository and no registry traffic.
	//
	// Empty means "no shortening". A Layout whose shortening genuinely cannot be
	// derived from the tag alone should return empty here and set DisplayTag in
	// Group; it then forgoes reconciliation, which is the honest trade.
	DisplayTag(tag string) string

	// AccessoryTags names the OTHER tags this Layout needs in order to classify
	// one admitted tag - NEAR's `signed_orb_X` and `signature_orb_X` for a
	// payload `orb_X`.
	//
	// It exists because tag filters and vendor mechanism are different things,
	// and conflating them made signatures invisible. `discovery.tagFilters`
	// is how an operator says WHICH RELEASES to track: `include: ['^orb_']` is
	// a completely reasonable way to say "the release tags, not the noise".
	// But under a Layout, `signed_orb_X` is not noise and is not a release -
	// it is the vendor's internal plumbing for the release that WAS admitted,
	// and it is where the signature lives.
	//
	// With the two conflated, that filter silently produced a catalogue in which
	// every package read `unsigned`: the accessory tags never entered the work
	// list, so Group never saw them, so no signature was ever found. Nothing was
	// wrong with the filter, and nothing in any output pointed at it.
	//
	// So the filter selects PACKAGES, and a Layout may pull in the tags those
	// packages are made of. Returning a tag that does not exist is free - the
	// scanner intersects this with the repository's real tag list.
	//
	// Called only for tags the filters ADMITTED, so an excluded release does not
	// drag its accessories in behind it.
	AccessoryTags(tag string) []string

	// ClassifyArtifact names what ONE artifact of a package is, from the
	// annotations the vendor put on it. It returns an oci.Kind* value, or ""
	// to defer to the OCI rules.
	//
	// # Why a Layout gets a say at all
	//
	// The OCI classification reads media type, artifact type and config media
	// type. For an index's children, discovery records what the index LISTED
	// and does not fetch each child - so the config is unknown, and a vendor
	// whose charts and images are all `image.manifest.v1+json` with no
	// artifactType classifies as image, every one of them. That is not a
	// rounding error: a release of 157 images and 97 charts reads as 254
	// images, and the charts become a category nobody can see.
	//
	// Such a vendor usually says what each child is on the referencing
	// descriptor, which discovery already has in hand and stores. This is the
	// hook that reads it - the vendor's key stays inside the vendor's package,
	// and the core keeps handling a bounded, neutral set of kinds.
	//
	// Advisory, and bounded like everything else a Layout does: it may
	// mislabel a row, and it cannot affect what is transferred.
	ClassifyArtifact(annotations map[string]string) string
}

// Format is how a signature is verified once it has been found.
//
// A SEPARATE axis from the Layout, because the two genuinely vary
// independently: a vendor may pair the referrers API with PKCS#7, or a wrapper
// index with a cosign bundle. Collapsing them into one setting would force a
// new combined value for every pairing.
//
// Verification itself is M5. The type exists now so configuration written today
// stays valid, and so the value survives into storage where M5 will read it.
type Format string

const (
	// FormatAuto infers the format from the signature's own media type -
	// application/pkcs7-signature, a cosign bundle, and so on.
	FormatAuto Format = "auto"
	// FormatCosign is a Sigstore bundle, keyed or keyless.
	FormatCosign Format = "cosign"
	// FormatPKCS7 is CMS (RFC 5652), verified against a trusted CA root. This
	// is what our first vendor emits, and it is NOT Sigstore - sigstore-go
	// cannot verify it and cosign has no part in it.
	FormatPKCS7 Format = "pkcs7"
)
