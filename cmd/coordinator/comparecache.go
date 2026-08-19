package main

import (
	"context"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Reading a comparison's manifests from what analysis already learnt.
//
// # The complaint this answers
//
// "I compared two packages, it walked the whole tree, I closed it, I compared
// them again, and it did the whole thing again." Which is exactly what it did:
// a comparison walks both sides from their roots, and nothing in that path had
// ever heard of the manifests sitting in our own database — fetched, verified
// against their digests, and kept.
//
// # Why serving them is sound
//
// A manifest is addressed by the hash of its own bytes. If we hold bytes that
// hash to the digest a walk asks for, they ARE that manifest; there is no
// version of this where the registry would answer differently, because a
// registry that did would be serving content that fails its own digest check.
//
// # Why only the SOURCE side
//
// The whole point of comparing against a TARGET is to find out what is actually
// there: a transfer can report every job successful and leave a destination
// missing a component, and the walk reporting that component as unreachable is
// the finding. Serving it from our cache would answer the question with our own
// record of what we sent, which is the one answer nobody needs.
//
// The source is different. It is where the release came from and where its
// content is defined; a component under a root digest cannot have changed, and
// asking the vendor to re-serve two hundred manifests we already hold to be
// told what we already know is the cost this removes.
//
// # And why the tag lookup is untouched
//
// `ResolveTag` is not cached and must not be: a tag is a pointer and a
// publisher may move it. Every comparison still resolves its roots live, and
// only what hangs beneath a resolved digest comes from here.

// cachedManifests wraps a repository so manifest reads BY DIGEST are served
// from the store when it has them.
type cachedManifests struct {
	registry.Repository

	packages *store.Packages
}

// FetchManifest serves a by-digest read from the store, and everything else
// from the registry.
//
// A miss is unremarkable — bodies are an evictable cache — and so is an error:
// the registry is the authority and is right there, so a database that cannot
// answer costs a round trip rather than the comparison.
func (c cachedManifests) FetchManifest(
	ctx context.Context, ref string,
) (registry.Descriptor, []byte, error) {
	if !isDigest(ref) {
		return c.Repository.FetchManifest(ctx, ref)
	}

	if m, ok, err := c.packages.ManifestByDigest(ctx, ref); err == nil && ok {
		size := m.SizeBytes
		if size == 0 {
			size = int64(len(m.Raw))
		}
		return registry.Descriptor{
			MediaType: m.MediaType,
			Digest:    registry.Digest(ref),
			Size:      size,
		}, m.Raw, nil
	}

	return c.Repository.FetchManifest(ctx, ref)
}

// isDigest reports whether a reference is content-addressed rather than a tag.
//
// `algorithm:hex`, per the OCI specification. A tag may not contain a colon, so
// the test is exact rather than heuristic.
func isDigest(ref string) bool {
	i := strings.Index(ref, ":")
	return i > 0 && i < len(ref)-1
}
