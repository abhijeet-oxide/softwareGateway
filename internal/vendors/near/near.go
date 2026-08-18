// Package near implements the layout used by Nokia's NEAR registries.
//
// EVERYTHING NOKIA-SPECIFIC IN THIS SYSTEM LIVES HERE. The core depends on
// vendors.Layout and is forbidden by depguard from importing this package;
// deleting this directory must leave the rest building and passing. If that
// ever stops being true, something has leaked.
//
// # What NEAR does
//
// Three tags per release, taken from live manifests:
//
//	orb_23.8.1076              an index of Helm charts and images (~109 KB)
//	signature_orb_23.8.1076    one layer, application/pkcs7-signature (3051 B)
//	signed_orb_23.8.1076       an index referencing both of the above (875 B)
//
// The wrapper is SELF-DESCRIBING, which is what makes this cheap. Its two
// children carry, in their descriptor annotations:
//
//	org.opencontainers.image.ref.name  "orbs/CFX-5000-k8s:signature_orb_23.8.1076"
//	com.nokia.ncd.orb.type             "generic_signature"
//
// So the grouping is derivable from bytes discovery has already fetched — no
// extra requests, and no reliance on tag naming as the primary signal.
//
// # Why the naming convention is still used, but only as a hint
//
// Prefix matching alone would be brittle: a repository containing a tag that
// merely begins with "signed_" would be misread. The annotations are
// authoritative and the prefixes only narrow the candidate set, so a rename by
// the vendor degrades to "grouping stops working" rather than "wrong tags get
// linked".
//
// # This is not where the ecosystem is
//
// A wrapper index is a pre-OCI-1.1 pattern: it bundles rather than
// back-references. The standard answer is the referrers API, where the
// signature carries a `subject` pointing at what it signs. NEAR predates that
// being widely available. Recorded here so nobody mistakes this for a model to
// copy — when NEAR moves to referrers, this file is what changes.
package near

import (
	"context"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// Name is the value that selects this layout: `signatures.layout: near`.
const Name = "near"

// Tag prefixes NEAR uses. Hints for narrowing candidates, not proof — see the
// package comment.
const (
	prefixSigned    = "signed_"
	prefixSignature = "signature_"
	// prefixOrb marks a payload — NEAR's word for "release".
	prefixOrb = "orb_"
)

// repositoryNamespace is the path segment NEAR puts every product under:
// `orbs/cfx-5000-k8s`, `orbs/cfx-5000-db`. Structural, not informative — it is
// on every repository of every NEAR registry, so a listing repeats it on every
// row and it distinguishes nothing.
const repositoryNamespace = "orbs/"

// Annotations NEAR sets. The first two are OCI-reserved and mean what the spec
// says; the third is Nokia's own and is why this file exists.
const (
	annRefName = "org.opencontainers.image.ref.name"
	annOrbType = "com.nokia.ncd.orb.type"
)

// Values NEAR puts in `com.nokia.ncd.orb.type`, on the descriptor by which an
// orb's index references each of its children.
//
// This is the only place in the system that knows an orb's parts are described
// this way, and it is what makes a release's composition readable BEFORE
// anything is fetched beyond the index itself.
const (
	// orbTypeSignature marks a wrapper child as the signature.
	orbTypeSignature = "generic_signature"
	// orbTypeHelmChart is a Helm chart. It is served as an ordinary image
	// manifest with no artifactType, so without this annotation nothing
	// distinguishes it from a container image until its config is fetched.
	orbTypeHelmChart = "helmchart"
	// orbTypeCNFImage is a container image — a Cloud-native Network Function.
	orbTypeCNFImage = "cnfimage"
	// orbTypeGenericPrefix covers NEAR's remaining `generic_*` values —
	// `generic_custo` and friends — which are configuration and data rather
	// than either of the above.
	orbTypeGenericPrefix = "generic"
)

// Layout groups NEAR's three tags into one package.
type Layout struct{}

// ClassifyArtifact reads `com.nokia.ncd.orb.type` off an orb child.
//
// # Why this is needed and what it fixes
//
// An orb's index lists its parts as `application/vnd.oci.image.manifest.v1+json`
// with no `artifactType`. Discovery records what the index says without
// fetching each child, so the config media type — the field that normally
// tells a Helm chart from an image — is not available. Classified on the OCI
// fields alone, every part of every orb reads as an image: a release of 157
// images and 97 charts reports 254 images, and Helm charts become a category
// that cannot be seen at all.
//
// NEAR states what each part is on the referencing descriptor, which discovery
// already holds. Reading it costs nothing and is correct before any deeper
// walk has happened.
//
// Returns "" for anything it does not recognise, so an unfamiliar value falls
// through to the OCI rules rather than being forced into a bucket.
func (Layout) ClassifyArtifact(annotations map[string]string) string {
	switch orbType := annotations[annOrbType]; {
	case orbType == orbTypeHelmChart:
		return oci.KindChart
	case orbType == orbTypeCNFImage:
		return oci.KindImage
	case orbType == orbTypeSignature:
		return oci.KindSignature
	case strings.HasPrefix(orbType, orbTypeGenericPrefix):
		// Everything else NEAR calls generic is configuration and data. The
		// FILES inside it are layers and are not visible until the manifest is
		// fetched and its blobs walked.
		return oci.KindFile
	default:
		return ""
	}
}

func (Layout) Name() string { return Name }

// Vocabulary: NEAR's users have orbs and orb versions, not repositories and
// tags. Nobody operating this reads "42 repositories scanned" and thinks about
// repositories — they think about orbs, and having to translate every line of a
// summary is the same tax as reading `orbs/` on every row.
func (Layout) Vocabulary() vendors.Vocabulary {
	return vendors.Vocabulary{
		Unit: "orb", Units: "orbs",
		Version: "orb version", Versions: "orb versions",
	}
}

// LooksForSignatures is true: this layout genuinely checks, so a package with
// no signature is reported as `unsigned` rather than `unknown`.
func (Layout) LooksForSignatures() bool { return true }

// DisplayRepository removes the `orbs/` NEAR puts in front of every product.
//
// Only the leading segment, and only when something is left behind: `orbs` on
// its own is a repository whose whole name is that word, and shortening it to
// nothing would put a blank in a listing.
//
// This is the one place the convention is known. It used to be inferred in the
// CLI from the prefix a page of rows happened to share — which shortened paths
// on registries that have no such convention, and which is exactly the kind of
// vendor knowledge this package exists to contain.
func (Layout) DisplayRepository(path string) string {
	rest, ok := strings.CutPrefix(strings.Trim(strings.TrimSpace(path), "/"), repositoryNamespace)
	if !ok || rest == "" {
		return ""
	}
	return rest
}

// Group collapses orb_X, signed_orb_X and signature_orb_X into one package.
//
// The result names `orb_X` — what a person says — while the transfer root is
// `signed_orb_X`, because only the wrapper reaches both the payload and the
// signature. Transferring the payload alone would leave the signature behind
// and make destination-side verification impossible for good.
func (Layout) Group(
	_ context.Context, _ registry.Source, scanned []vendors.ScannedTag,
) ([]vendors.Package, error) {
	byTag := make(map[string]vendors.ScannedTag, len(scanned))
	for _, s := range scanned {
		byTag[s.Tag] = s
	}

	// accessory records a tag that belongs to another package and must not
	// become a package of its own.
	accessory := map[string]bool{}
	// roots and signatures are keyed by the PAYLOAD tag they belong to.
	roots := map[string]vendors.ScannedTag{}
	signatures := map[string]vendors.Related{}

	for _, s := range scanned {
		if !strings.HasPrefix(s.Tag, prefixSigned) {
			continue
		}
		payloadTag, sig, ok := readWrapper(s)
		if !ok {
			// Begins with signed_ but is not a wrapper we recognise. Left
			// alone to become an ordinary package: silently dropping a tag
			// because it failed a vendor heuristic would hide real content.
			continue
		}

		accessory[s.Tag] = true
		roots[payloadTag] = s
		if sig.Descriptor.Digest != "" {
			accessory[sig.Tag] = true
			signatures[payloadTag] = sig
		}
	}

	out := make([]vendors.Package, 0, len(scanned))
	for _, s := range scanned {
		if accessory[s.Tag] {
			continue
		}

		pkg := vendors.Package{
			Tag: s.Tag, Descriptor: s.Descriptor, DisplayTag: Layout{}.DisplayTag(s.Tag),
		}

		if root, ok := roots[s.Tag]; ok {
			pkg.Root = root.Descriptor
			pkg.RootTag = root.Tag
			// The wrapper is recorded as related so the destination can be
			// tagged identically to the source. A consumer expecting NEAR's
			// layout must still find it after replication.
			pkg.Related = append(pkg.Related, vendors.Related{
				Role: vendors.RoleWrapper, Tag: root.Tag, Descriptor: root.Descriptor,
			})
		}
		if sig, ok := signatures[s.Tag]; ok {
			pkg.Related = append(pkg.Related, sig)
		}

		out = append(out, pkg)
	}
	return out, nil
}

// readWrapper interprets a `signed_*` index.
//
// Returns the payload tag it wraps and the signature it carries. The
// annotations are authoritative; the tag prefix only got us here.
func readWrapper(s vendors.ScannedTag) (payloadTag string, sig vendors.Related, ok bool) {
	if len(s.Children) == 0 {
		return "", vendors.Related{}, false
	}

	for _, child := range s.Children {
		tag := tagFromRefName(child.Annotations[annRefName])
		if tag == "" {
			continue
		}

		if child.Annotations[annOrbType] == orbTypeSignature {
			sig = vendors.Related{Role: vendors.RoleSignature, Tag: tag, Descriptor: child}
			continue
		}
		payloadTag = tag
	}

	// A wrapper must name a payload. Without one there is nothing to attach to,
	// and guessing by stripping the prefix would be the brittle behaviour this
	// design avoids.
	if payloadTag == "" {
		return "", vendors.Related{}, false
	}
	return payloadTag, sig, true
}

// AccessoryTags names the two tags NEAR publishes alongside a release.
//
// For `orb_23.8.1076` those are `signed_orb_23.8.1076` — the index binding the
// release to its signature, and the digest a transfer must walk — and
// `signature_orb_23.8.1076`, the manifest whose single layer is the PKCS#7 blob.
//
// This is the difference between a tag FILTER and the vendor's mechanism. An
// operator writing `tagFilters.include: ['^orb_']` is saying "track the
// releases", which is right; they are not saying "ignore the signatures", and
// before this method that is what it meant. Every package came out `unsigned`,
// with nothing in any output to suggest the filter was the reason.
//
// Derived from the tag string, so it costs nothing: the scanner intersects the
// result with the repository's real tag list, and a release NEAR did not sign
// simply has no such tag.
//
// Only a payload tag has accessories. A tag that is itself an accessory returns
// none, so nothing recurses.
func (Layout) AccessoryTags(tag string) []string {
	if strings.HasPrefix(tag, prefixSigned) || strings.HasPrefix(tag, prefixSignature) {
		return nil
	}
	if !strings.HasPrefix(tag, prefixOrb) {
		return nil
	}
	return []string{prefixSigned + tag, prefixSignature + tag}
}

// DisplayTag removes the `orb_` NEAR puts in front of every version.
//
// Derivable from the tag string alone, with no manifest and no registry call,
// which is what lets the scanner reconcile the stored display names of packages
// discovered BEFORE this source declared its vendor. Without that, turning
// `vendor: near` on would only affect tags discovered afterwards — discovery
// skips a tag it already holds — and every existing package would keep reading
// `orb_23.8.1076` forever.
func (Layout) DisplayTag(tag string) string { return displayTag(tag) }

// displayTag removes the `orb_` the vendor puts in front of every version.
//
// `orb_23.8.1076` is `23.8.1076` with four characters of noise, repeated on
// every row of every listing. Removing it is purely cosmetic — the real tag is
// what is stored and transferred, and both spellings resolve as input.
//
// Returns "" when there is nothing to remove, which the core reads as "no
// shortening" rather than as an empty name.
func displayTag(tag string) string {
	// A wrapper or signature tag keeps its marker: `signed_` says WHICH
	// artifact this is, while `orb_` is only the vendor's word for "release".
	for _, marker := range []string{prefixSigned, prefixSignature} {
		if rest, ok := strings.CutPrefix(tag, marker); ok {
			if short := displayTag(rest); short != "" {
				return marker + short
			}
			return ""
		}
	}
	if rest, ok := strings.CutPrefix(tag, prefixOrb); ok && rest != "" {
		return rest
	}
	return ""
}

// tagFromRefName extracts the tag from an image.ref.name annotation.
//
// NEAR writes the fully-qualified form:
//
//	orbs/CFX-5000-k8s:signature_orb_23.8.1076
//
// Split at the LAST colon: a repository path may contain slashes but a tag may
// not contain a colon. A value with no colon at all is treated as a bare tag,
// which is what a registry writing the short form would produce.
func tagFromRefName(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		if i == len(ref)-1 {
			return ""
		}
		return ref[i+1:]
	}
	return ref
}
