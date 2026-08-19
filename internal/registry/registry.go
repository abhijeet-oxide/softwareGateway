// Package registry owns all OCI registry I/O behind a narrow interface.
//
// See docs/design/06-registry-abstraction.md.
//
// The interface is deliberately split into small capability interfaces rather
// than being one wide type. Two reasons:
//
//  1. Honesty. M2 implements reading - tag listing and manifest resolution -
//     and nothing else. A single wide interface would force stub methods
//     returning "not implemented", which is exactly the fabricated-behaviour
//     pattern the router already avoids. A type that does not satisfy
//     Repository is a clearer statement than one that satisfies it and lies.
//  2. Consumers depend on what they use. Discovery needs TagLister and
//     ManifestReader, not blob upload. Go idiom, and it keeps fakes small.
//
// Nothing here references an OCI client library, and that has outlived the
// reason it started. ADR-001 is now CLOSED on oras-go/v2 (docs/design/16
// §1.1), used for the write path inside internal/registry/generic and nowhere
// else. This file staying library-free is what keeps that decision reversible:
// swapping the backend touches one directory, which is the property
// docs/design/15 §4 asks for.
package registry

import (
	"context"
	"io"
)

// ---------------------------------------------------------------------------
// Capability interfaces
// ---------------------------------------------------------------------------

// Identity names a repository.
type Identity interface {
	// Name is the full reference, "registry.example.com/vendor-a/platform".
	Name() string
	// Registry is the host, "registry.example.com".
	Registry() string
	// Path is the repository path, "vendor-a/platform".
	Path() string
}

// TagLister enumerates tags. Implemented in M2.
type TagLister interface {
	// ListTags returns one page of tags in registry order, plus the token for
	// the next page. `last` resumes from a previous page; "" starts.
	//
	// There is no ordering guarantee across calls and no change feed, which is
	// why discovery does a full scan rather than tracking a cursor
	// (docs/design/07 §3).
	ListTags(ctx context.Context, last string, limit int) (tags []string, next string, err error)
}

// ManifestReader resolves and fetches manifests. Implemented in M2.
type ManifestReader interface {
	// ResolveTag returns the descriptor a tag currently points at, WITHOUT
	// fetching the body. Discovery calls this for every tag on every scan, so
	// it must stay cheap: a HEAD reading Docker-Content-Digest.
	ResolveTag(ctx context.Context, tag string) (Descriptor, error)

	// FetchManifest returns manifest bytes VERBATIM.
	//
	// Implementations must not re-serialize: the digest - and every signature
	// over it - is the hash of these exact bytes. Round-tripping through a
	// struct would change whitespace or key order and produce a different
	// digest, silently breaking verification.
	FetchManifest(ctx context.Context, ref string) (Descriptor, []byte, error)
}

// Prober reports reachability and capabilities. Implemented in M2.
type Prober interface {
	// Ping verifies the registry answers and credentials work. Backs
	// `transferctl health`.
	Ping(ctx context.Context) error
	// Capabilities reports what this registry supports (§3).
	Capabilities(ctx context.Context) Capabilities
}

// BlobReader reads blobs. M3.
type BlobReader interface {
	StatBlob(ctx context.Context, dgst Digest) (Descriptor, error)
	FetchBlob(ctx context.Context, dgst Digest) (io.ReadCloser, error)
}

// BlobWriter writes blobs. M3.
type BlobWriter interface {
	PushBlob(ctx context.Context, dgst Digest, size int64, r io.Reader) error
	MountBlob(ctx context.Context, dgst Digest, fromRepo string) error
	ResumeUpload(ctx context.Context, state UploadState, r io.Reader) error
}

// ManifestWriter writes manifests and tags. M3.
type ManifestWriter interface {
	PushManifest(ctx context.Context, ref, mediaType string, raw []byte) (Descriptor, error)
	Tag(ctx context.Context, dgst Digest, tag string) error
}

// ReferrerLister discovers artifacts referring to a subject - cosign
// signatures, SBOMs, attestations. M5.
type ReferrerLister interface {
	Referrers(ctx context.Context, subject Digest, artifactType string) ([]Descriptor, error)
}

// ---------------------------------------------------------------------------
// Composed interfaces
// ---------------------------------------------------------------------------

// Source is what discovery needs: identify, list, resolve, fetch, probe.
//
// This is the whole contract for M2, and the generic implementation satisfies
// it completely - no stubs.
type Source interface {
	Identity
	TagLister
	ManifestReader
	Prober
}

// Repository is the full contract from docs/design/06 §2, reached in M3.
//
// Satisfied by generic.Repository, which asserts it at compile time. The one
// method that is present but not implemented is ResumeUpload: it returns
// ErrUnsupported, so an interrupted blob restarts rather than resuming. That
// is a deliberate deferral with a stated reason (docs/design/05 §4.6, Q3), not
// a stub standing in for missing work - restarting a content-addressed blob is
// always correct.
type Repository interface {
	Source
	BlobReader
	BlobWriter
	ManifestWriter
	ReferrerLister
}

// Compile-time assertion that Source is a strict subset of Repository, so the
// two cannot drift apart as methods are added.
var _ = func(r Repository) Source { return r }
