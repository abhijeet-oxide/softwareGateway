package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

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
func fetchTree(
	ctx context.Context, src registry.ManifestReader, root registry.Descriptor, concurrency int,
) (tree, error) {
	var t tree

	// Breadth-first, LEVEL BY LEVEL, so parents are always recorded before
	// their children and the parent index is valid by construction.
	//
	// A level's nodes are fetched in PARALLEL. They are the children of one
	// index and are independent of each other; walking them one at a time was
	// the last serial bottleneck in a scan, and the most expensive one — a
	// product bundle with sixty artifacts cost sixty sequential round trips
	// inside a single tag, which at 2.5s each is two and a half minutes during
	// which the tag counter does not move at all. That is what "hundreds of
	// requests succeeding and no progress" looked like from the outside.
	level := []queuedChild{{desc: root, parent: -1, depth: 0}}

	// Guards against an index that references itself, directly or through a
	// cycle. A registry should not serve one; a walk that assumes it will not
	// is a walk that hangs.
	seen := map[registry.Digest]bool{}

	if concurrency <= 0 {
		concurrency = 1
	}

	for len(level) > 0 {
		// De-duplicate within the level before fetching, so a diamond in the
		// graph costs one request rather than two.
		pending := make([]queuedChild, 0, len(level))
		for _, q := range level {
			if seen[q.desc.Digest] {
				continue
			}
			seen[q.desc.Digest] = true
			pending = append(pending, q)
		}
		if len(pending) == 0 {
			break
		}
		if len(t.Artifacts)+len(pending) > maxTreeArtifacts {
			return tree{}, fmt.Errorf("manifest tree exceeds %d artifacts", maxTreeArtifacts)
		}

		type fetched struct {
			q    queuedChild
			desc registry.Descriptor
			raw  []byte
			err  error
		}
		results := make([]fetched, len(pending))

		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for i, q := range pending {
			wg.Add(1)
			go func(i int, q queuedChild) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()

				desc, raw, err := src.FetchManifest(ctx, q.desc.Digest.String())
				results[i] = fetched{q: q, desc: desc, raw: raw, err: err}
			}(i, q)
		}
		wg.Wait()

		var next []queuedChild

		// Processed in level order, so the artifact list and every parent index
		// into it are identical to what the sequential walk produced.
		for _, r := range results {
			if r.err != nil {
				return tree{}, fmt.Errorf("fetch manifest %s: %w", r.q.desc.Digest.Short(), r.err)
			}
			children, err := appendArtifact(&t, r.q.desc, r.desc, r.raw, r.q.parent, r.q.depth)
			if err != nil {
				return tree{}, err
			}
			next = append(next, children...)
		}
		level = next
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
