package discovery

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Tree bounds. A registry we do not control decides how deep and how wide the
// walk goes, so both are capped. Without these, one hostile or broken index
// could turn a routine scan into thousands of requests.
const (
	// maxTreeDepth allows index -> manifest, plus a margin for the nested
	// indexes some vendors publish. Four is well past anything seen in
	// practice.
	maxTreeDepth = 4
	// maxTreeArtifacts caps total manifests fetched for one package. A
	// multi-platform image has under ten; a very large fat index might have
	// fifty.
	maxTreeArtifacts = 512
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
	TotalBytes int64
	// BlobCount is distinct blob digests, on the same basis.
	BlobCount int
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

// fetchTree walks a package's manifest tree breadth-first.
//
// Called only for genuinely NEW packages. A scan where nothing changed costs
// one HEAD per tag and fetches no manifest bodies at all (docs/design/07 §2).
func fetchTree(ctx context.Context, src registry.ManifestReader, root registry.Descriptor) (tree, error) {
	var t tree

	// Breadth-first, so parents are always recorded before their children and
	// the parent index is valid by construction.
	type queued struct {
		desc   registry.Descriptor
		parent int
		depth  int
	}
	queue := []queued{{desc: root, parent: -1, depth: 0}}

	// Guards against an index that references itself, directly or through a
	// cycle. A registry should not serve one; a walk that assumes it will not
	// is a walk that hangs.
	seen := map[registry.Digest]bool{}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if seen[cur.desc.Digest] {
			continue
		}
		seen[cur.desc.Digest] = true

		if len(t.Artifacts) >= maxTreeArtifacts {
			return tree{}, fmt.Errorf("manifest tree exceeds %d artifacts", maxTreeArtifacts)
		}

		desc, raw, err := src.FetchManifest(ctx, cur.desc.Digest.String())
		if err != nil {
			return tree{}, fmt.Errorf("fetch manifest %s: %w", cur.desc.Digest.Short(), err)
		}

		// Carry the media type and platform from the referencing descriptor when
		// the response omits them: an index states its children's platforms, and
		// the child manifest itself does not.
		if desc.MediaType == "" {
			desc.MediaType = cur.desc.MediaType
		}
		if desc.Platform == nil {
			desc.Platform = cur.desc.Platform
		}

		var body manifestBody
		if err := json.Unmarshal(raw, &body); err != nil {
			return tree{}, fmt.Errorf("parse manifest %s: %w: %w",
				desc.Digest.Short(), registry.ErrMalformedResponse, err)
		}
		// The body's own mediaType is authoritative where present — some
		// registries return a generic Content-Type.
		if body.MediaType != "" {
			desc.MediaType = body.MediaType
		}
		if body.ArtifactType != "" {
			desc.ArtifactType = body.ArtifactType
		}

		a := artifact{Descriptor: desc, Raw: raw, Parent: cur.parent, Depth: cur.depth}

		switch {
		case registry.IsIndex(desc.MediaType):
			if cur.depth >= maxTreeDepth {
				return tree{}, fmt.Errorf("manifest tree deeper than %d levels", maxTreeDepth)
			}
			idx := len(t.Artifacts)
			for _, child := range body.Manifests {
				if err := child.Digest.Validate(); err != nil {
					return tree{}, fmt.Errorf("index %s: %w", desc.Digest.Short(), err)
				}
				queue = append(queue, queued{desc: child, parent: idx, depth: cur.depth + 1})
			}

		default:
			// An image manifest, a Helm chart, or any other single artifact.
			// Anything that is not an index is treated as a leaf carrying blobs,
			// which is what the OCI artifact model asks for — new artifact types
			// arrive constantly and none of them should need a code change here.
			if body.Config != nil && body.Config.Digest != "" {
				if err := body.Config.Digest.Validate(); err != nil {
					return tree{}, fmt.Errorf("manifest %s config: %w", desc.Digest.Short(), err)
				}
				a.Blobs = append(a.Blobs, blobRef{Descriptor: *body.Config, Kind: "config"})
			}
			for i, layer := range body.Layers {
				if err := layer.Digest.Validate(); err != nil {
					return tree{}, fmt.Errorf("manifest %s layer %d: %w", desc.Digest.Short(), i, err)
				}
				a.Blobs = append(a.Blobs, blobRef{Descriptor: layer, Kind: "layer", Ordinal: i})
			}
		}

		t.Artifacts = append(t.Artifacts, a)
	}

	t.TotalBytes, t.BlobCount = measure(t.Artifacts)
	return t, nil
}

// measure sums distinct content, counting each digest once.
func measure(artifacts []artifact) (totalBytes int64, blobCount int) {
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
	return totalBytes, blobCount
}
