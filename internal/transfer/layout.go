package transfer

import (
	"path"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Where each artifact in a bundle must land at the destination.
//
// # The problem this solves
//
// A bundle is not one artifact in one repository. An ORB is an index over
// container images, Helm charts and generic artifacts, and those components
// have their OWN repository paths and their OWN tags in the source registry.
// Pushing everything into a single destination repository preserves the
// CONTENT - every blob is content-addressed, so nothing is lost - while
// destroying the NAMES. A consumer who could pull `orbs/CFX-5000-k8s/nginx:1.2.3`
// from the vendor would be left with a digest.
//
// # How the destination is discovered rather than configured
//
// From the OCI structure itself, in this order:
//
//  1. The artifact's own `org.opencontainers.image.ref.name` annotation. This
//     is the reserved OCI annotation for exactly this - "what this artifact is
//     called" - and an index that bundles components records it on each child
//     descriptor. NEAR writes the fully-qualified form,
//     `orbs/CFX-5000-k8s:signature_orb_23.8.1076`.
//  2. Failing that, the repository its PARENT landed in. A child manifest that
//     does not name itself belongs where the thing referencing it belongs; this
//     is what makes a plain multi-platform image work without annotations.
//  3. The root falls back to the repository the package was discovered in.
//
// Nothing here knows what an ORB is, what `generic_custo` means, or that Nokia
// exists. It reads annotations the OCI specification reserves and a parent
// relationship the manifest structure states. A vendor that bundles differently
// gets the same treatment.
//
// # Why a component lands in TWO places
//
// An OCI index may only reference content the registry will serve from the
// index's OWN repository. Relocating a component to the repository its
// `ref.name` gives would therefore break the bundle: the index would be pushed
// against a repository missing the manifests it names, and the registry
// answers BLOB_UNKNOWN. That is not a policy choice, it is the distribution
// spec, and the in-repo fake enforces it exactly as a real registry does.
//
// So every artifact is placed in its parent's repository - which keeps the
// bundle intact and resolvable - and ADDITIONALLY at the repository it names,
// when that differs. Both addressing forms then work: pulling the ORB gets the
// whole thing, and pulling `orbs/CFX-5000-k8s/nginx:1.2.3` gets the component
// under the name the vendor documented.
//
// The cost is honest and worth stating: a component published in two
// repositories needs its blobs in both, because a registry stores blobs per
// repository. Within one registry that is a cross-repository MOUNT - zero
// bytes over the wire regardless of size - which is the case that matters,
// since both destinations are the same registry by construction.
//
// # This decides the DESTINATION only, never where to read from
//
// Everything is read from the repository the package was discovered in, and
// that is not a simplification - it is what OCI requires. An index can only
// reference children the registry will serve from the index's own repository,
// so a bundle whose components were not co-located could not have been walked
// in the first place. The walk therefore proves co-location, and a component
// that is genuinely elsewhere fails at fetch time with a plain 404 rather than
// being copied to the wrong place.
//
// `ref.name` says what an artifact is CALLED. It does not say where its bytes
// live, and reading it as if it did would turn a naming hint into a fetch.

// LayoutOptions describe the destination a bundle is being copied into.
type LayoutOptions struct {
	// BasePath is the target's configured repository, used as a PREFIX.
	//
	// Empty means mirror the source path at the destination registry's root,
	// so coordinates are byte-identical to the source and a consumer only
	// swaps the hostname. Set means nest the whole source structure beneath
	// it, which is what keeps two vendors from colliding in one registry.
	BasePath string

	// SourceRepository is where the package itself was discovered - the
	// fallback for the root, and therefore for anything that inherits.
	SourceRepository string

	// RootTags are the tags the package's own root manifest must carry.
	// Usually one: the tag a person asked for.
	RootTags []string
}

// Site is one place an artifact lands.
type Site struct {
	// Repository is the path within the DESTINATION registry.
	Repository string
	// Tags are the tags to apply once the manifest is committed. Empty means
	// the artifact is addressed by digest alone, which is correct and common:
	// a child manifest of an index needs no tag of its own for the index to
	// resolve it.
	Tags []string
}

// Placement is everywhere one artifact lands.
//
// The FIRST site is always the one that keeps the bundle resolvable - the
// artifact's parent repository. Any others are the names the vendor gave it.
type Placement struct {
	Sites []Site
	// SourceRepository is the path this artifact is read from. Always the
	// package's own repository: an index's children are necessarily
	// co-located with it.
	SourceRepository string
}

// Primary is the site that keeps the bundle resolvable.
func (p Placement) Primary() Site {
	if len(p.Sites) == 0 {
		return Site{}
	}
	return p.Sites[0]
}

// ResolveLayout computes a placement for every artifact in a tree.
//
// Returned keyed by artifact digest, which is identity: the same digest is the
// same artifact however many times a bundle references it.
//
// The tree is ordered parents-before-children (both the walker and
// ReadExpandedTree guarantee it), so a single forward pass can inherit.
func ResolveLayout(tree store.ExpandedTree, opts LayoutOptions) map[string]Placement {
	out := make(map[string]Placement, len(tree.Artifacts))

	// containerOf is the repository each artifact must live in for the bundle
	// to resolve - its parent's, by position, so a child inherits in one pass.
	containerOf := make([]string, len(tree.Artifacts))

	for i, a := range tree.Artifacts {
		ref := parseRefName(a.Row.Annotations[registry.AnnotationRefName])

		// The container: where this artifact HAS to be. The root sits in the
		// package's own repository; everything else sits with its parent,
		// because that is the only place an index may reference it from.
		container := opts.SourceRepository
		if a.Parent >= 0 {
			container = inheritedRepository(containerOf, a.Parent, opts.SourceRepository)
		}
		containerOf[i] = container

		sites := []Site{{Repository: destinationPath(opts.BasePath, container)}}

		switch {
		case a.Parent < 0:
			// The root carries what the package is called, plus anything it
			// named itself. Both, not either: a vendor that annotates the root
			// with its own reference has not thereby renamed the release.
			sites[0].Tags = mergeTags(opts.RootTags, ref.tags())

		case ref.repository == "" || sameRepository(ref.repository, container):
			// Named within its own container, or not named at all. The tag —
			// where there is one — belongs right here, unless the root already
			// claims that exact name: a wrapper's payload keeps the same
			// ref.name as the release tag the wrapper itself is tagged with,
			// and the two must not compete to own it.
			sites[0].Tags = withoutRootTags(ref.tags(), opts.RootTags)

		default:
			// Named elsewhere. It stays in the container so the bundle still
			// resolves, and is ALSO published under the name the vendor gave
			// it, untagged in the container because the name is not this
			// repository's to claim. A different repository cannot collide
			// with the root's tags — those only ever name something in the
			// root's own container — so the vendor's own name is kept as is,
			// even where it happens to reuse the release's version string.
			sites = append(sites, Site{
				Repository: destinationPath(opts.BasePath, ref.repository),
				Tags:       ref.tags(),
			})
		}

		// An artifact reached twice - a base image shared by two components -
		// keeps the union of everywhere it was placed. Dropping one would
		// leave a name at the source with no counterpart at the destination.
		if existing, seen := out[a.Row.Digest]; seen {
			sites = mergeSites(existing.Sites, sites)
		}

		out[a.Row.Digest] = Placement{Sites: sites, SourceRepository: container}
	}
	return out
}

// mergeSites unions two placements, keeping the first list's order and
// therefore its primary site.
func mergeSites(a, b []Site) []Site {
	out := append([]Site(nil), a...)

	for _, site := range b {
		found := false
		for i := range out {
			// Case-insensitively, so an artifact reached twice under two
			// spellings of one repository keeps ONE site rather than growing a
			// second that differs only in capitals. See sameRepository.
			if sameRepository(out[i].Repository, site.Repository) {
				out[i].Tags = mergeTags(out[i].Tags, site.Tags)
				found = true
				break
			}
		}
		if !found {
			out = append(out, site)
		}
	}
	return out
}

// inheritedRepository returns the repository an artifact's parent lives in.
//
// One step, not a walk: the tree is ordered parents-before-children, so by the
// time a child is reached its parent's container is already resolved - and a
// parent's container is never empty, because it either inherited one or fell
// back to the package's own repository.
func inheritedRepository(containerOf []string, parent int, fallback string) string {
	if parent < 0 || parent >= len(containerOf) || containerOf[parent] == "" {
		return fallback
	}
	return containerOf[parent]
}

// destinationPath maps a source repository path onto the destination.
//
// With a base, the source structure is nested underneath it verbatim. Without
// one, it is mirrored at the registry root. Either way the source's own shape
// is preserved - the base only decides what sits above it.
// DestinationPath is where content from one source repository lands.
//
// Exported because calibration has to probe the SAME path a transfer writes
// to. Probing the target's configured base directly does not work and is not a
// near miss: a base path is a prefix, not an image repository, so an upload
// session opened against it comes back 404 from a registry that is working
// perfectly. One function, so a probe cannot measure a path no transfer uses.
func DestinationPath(base, sourceRepo string) string {
	return destinationPath(base, sourceRepo)
}

func destinationPath(base, sourceRepo string) string {
	base = strings.Trim(strings.TrimSpace(base), "/")
	sourceRepo = strings.Trim(strings.TrimSpace(sourceRepo), "/")

	switch {
	case base == "":
		return sourceRepo
	case sourceRepo == "":
		return base
	default:
		return path.Join(base, sourceRepo)
	}
}

// sameRepository compares two repository paths as the REGISTRY will.
//
// # The bug this exists for
//
// A vendor's `org.opencontainers.image.ref.name` names the artifact, and there
// is nothing making it agree letter for letter with the repository the artifact
// actually lives in. Observed: content in `orbs/cfx-5000-k8s-215952-edgenac-…`
// annotated `orbs/CFX-5000-k8s-215952-edgeNAC-…:orb_25.7_…`. The same
// repository, spelled twice.
//
// Compared byte for byte, those are two repositories, so the layout published
// the component into its container AND "elsewhere" - and the destination grew
// two sibling folders with the same name and different capitals, half the
// bundle in each.
//
// The second one does not entirely work, either. The OCI grammar for a
// repository name is lowercase-only, and Artifactory accepts a push to a
// mixed-case path while issuing a token scoped to the name it normalised - so a
// later request against the name we sent falls outside that scope and comes
// back 401 unauthorized, on a repository the same credential has been writing
// to for thirty hours. It surfaces at the tag, which is the last request of the
// whole transfer.
//
// Case-insensitive, therefore, and only here. Reading is still verbatim: a
// vendor that genuinely serves `orbs/CFX-5000-k8s` is served from exactly that
// path, and mirroring it to a destination of the same name is still exactly
// what happens. What no longer happens is one repository becoming two because
// an annotation disagreed with itself about capitals.
func sameRepository(a, b string) bool {
	return strings.EqualFold(strings.Trim(a, "/"), strings.Trim(b, "/"))
}

// refName is a parsed org.opencontainers.image.ref.name.
type refName struct {
	repository string
	tag        string
}

func (r refName) tags() []string {
	if r.tag == "" {
		return nil
	}
	return []string{r.tag}
}

// parseRefName reads the reserved OCI reference annotation.
//
// Three shapes appear in the wild, and the OCI grammar tells them apart
// unambiguously - a tag may contain neither `/` nor `:`, and a repository path
// may contain `/` but never `:`:
//
//	orbs/CFX-5000-k8s:orb_23.8.1076   repository and tag
//	orbs/CFX-5000-k8s                 repository only
//	orb_23.8.1076                     tag only
//
// Split at the LAST colon, because a repository path may contain slashes and
// the tag never contains a colon. A value that parses to neither - an empty
// string, a stray colon - yields an empty refName, which the caller reads as
// "this artifact did not name itself" and falls back to inheritance.
func parseRefName(v string) refName {
	v = strings.TrimSpace(v)
	if v == "" {
		return refName{}
	}

	// A digest reference is not a name. `sha256:abcd…` would otherwise parse
	// as repository `sha256` with a very long tag, and that repository would
	// then be created at the destination.
	if strings.HasPrefix(v, "sha256:") || strings.HasPrefix(v, "sha512:") {
		return refName{}
	}

	if i := strings.LastIndex(v, ":"); i >= 0 {
		repo, tag := v[:i], v[i+1:]
		if tag == "" || strings.Contains(tag, "/") {
			// Not a tag: either nothing after the colon, or a path. Treat the
			// whole thing as a repository rather than inventing a tag.
			return refName{repository: strings.Trim(v, "/")}
		}
		return refName{repository: strings.Trim(repo, "/"), tag: tag}
	}

	if strings.Contains(v, "/") {
		return refName{repository: strings.Trim(v, "/")}
	}
	return refName{tag: v}
}

// mergeTags unions two tag lists, preserving order and dropping duplicates.
func mergeTags(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}

	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, t := range list {
			if t == "" || seen[t] {
				continue
			}
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// withoutRootTags drops any tag the root already carries, within the SAME
// repository only — a different repository cannot collide with a tag that
// only ever names something in the root's own container.
//
// # The bug this exists for
//
// A wrapper's payload keeps its own `org.opencontainers.image.ref.name`
// annotation, and a vendor that bundles a release under `signed_orb_X` has no
// reason to change what the payload calls itself: it is still `orb_X`. But
// `orb_X` is also one of the root's RootTags (rootTags in planner.go puts the
// wrapper there precisely so one name reaches both the payload and the
// signature) — so without this, TWO manifests in the same transfer, in the
// same repository, both carried a job to tag `orb_X`: the wrapper, and its own
// payload child. Whichever ran second found the tag already pointing at its
// sibling's digest and was refused as an overwrite, on registries that
// distinguish creating a tag from moving one.
//
// The root is the one place that name resolves correctly, because only the
// wrapper reaches the signature. Dropping the collision here rather than
// deduplicating jobs later keeps the rule where the layout is decided.
func withoutRootTags(tags, rootTags []string) []string {
	if len(rootTags) == 0 || len(tags) == 0 {
		return tags
	}

	claimed := make(map[string]bool, len(rootTags))
	for _, t := range rootTags {
		claimed[t] = true
	}

	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if !claimed[t] {
			out = append(out, t)
		}
	}
	return out
}
