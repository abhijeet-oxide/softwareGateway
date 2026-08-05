package discovery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Bounds. A registry we do not control decides how wide an index is, so the
// listing is capped: one hostile or broken index must not become a package with
// a million rows.
const (
	// maxListedArtifacts caps the children recorded from one index. A
	// multi-platform image has under ten; a large product bundle might have
	// several hundred.
	maxListedArtifacts = 512
	// maxTreeDepth is kept for the transfer-time walk, which does traverse.
	// Discovery no longer recurses at all, so it records exactly two levels:
	// the index and what it lists.
	maxTreeDepth = 4
)

// artifact is one manifest in a package's tree, with its raw bytes.
type artifact struct {
	Descriptor registry.Descriptor
	// Raw is the manifest exactly as the registry served it. Kept verbatim
	// because the digest — and every signature over it — is the hash of these
	// exact bytes.
	Raw []byte
	// Parent indexes into the tree slice; -1 for the root.
	Parent int
	Depth  int
	// Blobs are the config and layer descriptors this manifest references.
	// Empty for an index, whose children are manifests rather than blobs.
	Blobs []blobRef
}

type blobRef struct {
	Descriptor registry.Descriptor
	// Kind is "config" or "layer".
	Kind    string
	Ordinal int
}

// tree is a flattened artifact tree, parents before children.
type tree struct {
	Artifacts []artifact
	// TotalBytes is the transfer cost, counting each distinct digest ONCE.
	//
	// Deduplicated deliberately: a fat index whose platforms share a base layer
	// does not transfer that layer per platform, so summing naively would
	// overstate the cost — sometimes by several times — and make every size
	// shown to an operator a lie.
	//
	// NIL when it cannot be known, which is the case for a package whose root
	// is an INDEX: the index states the size of each child manifest, not of the
	// layers underneath it, and discovery no longer fetches those children. Nil
	// rather than zero, because "we have not measured this" and "this is empty"
	// are different facts and only one of them is true.
	TotalBytes *int64
	// BlobCount is distinct blob digests, on the same basis, and nil on the
	// same condition.
	BlobCount *int
}

// manifestBody is the subset of a manifest or index we parse.
//
// Deliberately partial. We do not deserialize a manifest into a full typed
// model and we never re-serialize one: the raw bytes are what gets stored,
// pushed and verified. This struct exists only to WALK the tree.
type manifestBody struct {
	MediaType    string                `json:"mediaType"`
	ArtifactType string                `json:"artifactType"`
	Config       *registry.Descriptor  `json:"config"`
	Layers       []registry.Descriptor `json:"layers"`
	Manifests    []registry.Descriptor `json:"manifests"`
	Subject      *registry.Descriptor  `json:"subject"`
}

// fetchPackage fetches the tag's OWN manifest, and nothing else.
//
// This used to walk the whole tree, fetching every child of every index. It no
// longer does, and the argument for deferring is that NOTHING IS LOST: the root
// digest immutably determines the entire tree, so it can be walked exactly, at
// any time, from one digest we already hold. The traversal was a cache, and it
// was paid for on every newly discovered tag — a bundle whose index references
// sixty artifacts cost sixty extra round trips, and a first scan of a real
// vendor catalogue ran into five figures of requests.
//
// An index already carries what a listing needs. Each child descriptor states
// its digest, media type, size and platform, so "this package contains three
// images, a chart and two files" is answerable from the bytes we just fetched.
// Those children are recorded WITHOUT raw bytes, and that distinction is the
// point: a row with bytes was fetched and verified against its digest, a row
// without has the vendor's word for it.
//
// The cost of deferring is that a bundle's transfer size is not known at
// discovery — the index states the size of each child MANIFEST, not of the
// layers underneath it. That is reported as unknown rather than guessed. See
// tree.TotalBytes.
func fetchPackage(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor,
) (tree, error) {
	var t tree

	desc, raw, err := src.FetchManifest(ctx, root.Digest.String())
	if err != nil {
		return tree{}, fmt.Errorf("fetch manifest %s: %w", root.Digest.Short(), err)
	}

	children, err := appendArtifact(&t, root, desc, raw, -1, 0)
	if err != nil {
		return tree{}, err
	}

	if len(children) > maxListedArtifacts {
		return tree{}, fmt.Errorf("index lists %d artifacts, more than the %d cap",
			len(children), maxListedArtifacts)
	}

	// Recorded from the index's own descriptors: no request each, and the
	// listing an operator sees is exactly what the vendor published.
	seen := map[registry.Digest]bool{t.Artifacts[0].Descriptor.Digest: true}
	for _, c := range children {
		if seen[c.desc.Digest] {
			continue
		}
		seen[c.desc.Digest] = true
		t.Artifacts = append(t.Artifacts, artifact{
			Descriptor: c.desc,
			Parent:     c.parent,
			Depth:      c.depth,
			// Raw is deliberately nil: listed, not fetched.
		})
	}

	t.TotalBytes, t.BlobCount = measure(t.Artifacts)
	return t, nil
}

// queuedChild is a manifest referenced by an index, waiting to be walked.
type queuedChild struct {
	desc   registry.Descriptor
	parent int
	depth  int
}

// appendArtifact records one fetched manifest and returns the children to walk.
//
// Split out of fetchTree so the parallel fetch and the strictly-ordered
// bookkeeping stay separate. Everything here mutates the tree and must run on
// one goroutine, in level order, or the parent indices would point at the
// wrong artifacts.
func appendArtifact(
	t *tree, from, desc registry.Descriptor, raw []byte, parent, depth int,
) ([]queuedChild, error) {
	// Carry the media type and platform from the referencing descriptor when
	// the response omits them: an index states its children's platforms, and
	// the child manifest itself does not.
	if desc.MediaType == "" {
		desc.MediaType = from.MediaType
	}
	if desc.Platform == nil {
		desc.Platform = from.Platform
	}

	var body manifestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w: %w",
			desc.Digest.Short(), registry.ErrMalformedResponse, err)
	}
	// The body's own mediaType is authoritative where present — some registries
	// return a generic Content-Type.
	if body.MediaType != "" {
		desc.MediaType = body.MediaType
	}
	if body.ArtifactType != "" {
		desc.ArtifactType = body.ArtifactType
	}

	a := artifact{Descriptor: desc, Raw: raw, Parent: parent, Depth: depth}

	var children []queuedChild

	switch {
	case registry.IsIndex(desc.MediaType):
		if depth >= maxTreeDepth {
			return nil, fmt.Errorf("manifest tree deeper than %d levels", maxTreeDepth)
		}
		idx := len(t.Artifacts)
		for _, child := range body.Manifests {
			if err := child.Digest.Validate(); err != nil {
				return nil, fmt.Errorf("index %s: %w", desc.Digest.Short(), err)
			}
			children = append(children, queuedChild{desc: child, parent: idx, depth: depth + 1})
		}

	default:
		// An image manifest, a Helm chart, or any other single artifact.
		// Anything that is not an index is treated as a leaf carrying blobs,
		// which is what the OCI artifact model asks for — new artifact types
		// arrive constantly and none of them should need a code change here.
		if body.Config != nil && body.Config.Digest != "" {
			if err := body.Config.Digest.Validate(); err != nil {
				return nil, fmt.Errorf("manifest %s config: %w", desc.Digest.Short(), err)
			}
			a.Blobs = append(a.Blobs, blobRef{Descriptor: *body.Config, Kind: "config"})
		}
		for i, layer := range body.Layers {
			if err := layer.Digest.Validate(); err != nil {
				return nil, fmt.Errorf("manifest %s layer %d: %w", desc.Digest.Short(), i, err)
			}
			a.Blobs = append(a.Blobs, blobRef{Descriptor: layer, Kind: "layer", Ordinal: i})
		}
	}

	t.Artifacts = append(t.Artifacts, a)
	return children, nil
}

// measure sums distinct content, counting each digest once.
func measure(artifacts []artifact) (*int64, *int) {
	// An artifact we listed but did not fetch has no blob list, so its bytes
	// are unaccounted for. Reporting a total that omits them would understate a
	// bundle's transfer cost — by nearly all of it — and a wrong size is worse
	// than an absent one, because nobody questions a number.
	for _, a := range artifacts {
		if a.Raw == nil {
			return nil, nil
		}
	}

	var totalBytes int64
	var blobCount int
	counted := map[registry.Digest]bool{}

	for _, a := range artifacts {
		if !counted[a.Descriptor.Digest] {
			counted[a.Descriptor.Digest] = true
			totalBytes += int64(len(a.Raw))
		}
		for _, b := range a.Blobs {
			if counted[b.Descriptor.Digest] {
				continue
			}
			counted[b.Descriptor.Digest] = true
			totalBytes += b.Descriptor.Size
			blobCount++
		}
	}
	return &totalBytes, &blobCount
}
