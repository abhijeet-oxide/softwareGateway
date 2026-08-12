package generic

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/errdef"
	orasregistry "oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// The write half of Distribution v2, on oras-go/v2 — and the read half above
// is not, which is a deliberate asymmetry worth explaining.
//
// # Why a library at all
//
// Reading is four straightforward calls. WRITING is where the protocol gets
// fiddly in ways that only show up against a real registry: an upload session
// whose `Location` may be absolute or relative; `PATCH` with `Range` for
// resumption; a cross-repository mount that answers `201 Created` when it works
// and `202 Accepted` — an upload session, not an error — when the registry
// declines. Those are exactly the parts a library has already got wrong once
// and fixed, and hand-rolling them means finding each bug ourselves, in
// production, against someone else's registry.
//
// # Why the read path was NOT converted
//
// It works against a hostile real registry — a corporate proxy, a certificate
// the standard library refuses to parse, a vendor that omits `mediaType` on a
// descriptor. Rewriting proven code to remove an asymmetry would be trading a
// known-good path for a tidier one. The two halves share no logic, so this is
// not the same protocol implemented twice.
//
// # ORAS does no authentication here
//
// `remote.Repository.Client` is an interface with one method, `Do`, which our
// *http.Client satisfies directly. So the whole M2 transport — the rate limiter
// outermost, the retry budget, the single-flight token cache, the CA bundle,
// the proxy, the request tracer — stays in charge, and ORAS never sees a
// credential. Adopting a library thinly is the point: it supplies the upload
// protocol, nothing else.

// orasRepo builds an ORAS repository handle sharing our HTTP client.
//
// # Why the Reference is constructed rather than parsed
//
// `remote.NewRepository` parses a string and REJECTS anything the OCI
// distribution spec's grammar disallows — in particular uppercase, since the
// spec's repository grammar is lowercase-only.
//
// That is correct as a rule and wrong as a gate here, because we do not choose
// these names. A vendor whose repository is `orbs/CFX-5000-k8s` has published
// content at that path and real registries serve it; refusing to speak to it
// would mean this tool cannot copy the very artifacts it exists to copy. The
// failure was also badly placed: discovery uses plain net/http and validates
// nothing, so such a repository scanned perfectly and then failed on the first
// blob, which reads as a transfer bug rather than a naming one.
//
// Constructing the Reference directly skips the parse and sends the path
// exactly as given — which is the whole contract of this tool: copy, do not
// change. A path the destination registry genuinely will not accept still
// fails, with that registry's own error, which is the right place for the
// judgement.
func (r *Repository) orasRepo() (*remote.Repository, error) {
	if r.repoPath == "" {
		return nil, fmt.Errorf("build repository handle for %s: repository path is empty",
			r.registryHost)
	}

	repo := &remote.Repository{
		Reference: orasregistry.Reference{Registry: r.registryHost, Repository: r.repoPath},
		Client:    r.client,
		PlainHTTP: r.scheme == "http",
		// Manifests are pushed and fetched verbatim; ORAS must not decide a
		// size limit for content we already know the size of.
		MaxMetadataBytes: maxManifestBytes,
	}
	return repo, nil
}

// StatBlob reports a blob's presence and size without transferring it.
//
// This is fast path 2 of docs/design/05 §4: one request, zero bytes, and it
// catches the case a stale placement record would otherwise miss.
func (r *Repository) StatBlob(ctx context.Context, dgst registry.Digest) (registry.Descriptor, error) {
	repo, err := r.orasRepo()
	if err != nil {
		return registry.Descriptor{}, err
	}

	desc, err := repo.Blobs().Resolve(ctx, dgst.String())
	if err != nil {
		return registry.Descriptor{}, r.classify(err, "stat blob "+dgst.Short())
	}
	return fromOCI(desc), nil
}

// FetchBlob opens a blob for streaming.
//
// Returns the LIVE response body. Never buffered: invariant I5 is that bytes do
// not touch worker memory in bulk or worker disk at all, and an 8 GB layer read
// into a []byte would OOM the pod. The caller must Close it.
func (r *Repository) FetchBlob(ctx context.Context, dgst registry.Digest) (io.ReadCloser, error) {
	repo, err := r.orasRepo()
	if err != nil {
		return nil, err
	}

	desc, err := repo.Blobs().Resolve(ctx, dgst.String())
	if err != nil {
		return nil, r.classify(err, "resolve blob "+dgst.Short())
	}
	rc, err := repo.Blobs().Fetch(ctx, desc)
	if err != nil {
		return nil, r.classify(err, "fetch blob "+dgst.Short())
	}
	return rc, nil
}

// PushBlob streams a blob to this repository.
//
// The reader is consumed as the request body, so the bytes never accumulate
// anywhere. ORAS verifies the digest as it writes and the registry verifies it
// again on commit — two independent checks, catching corruption on our side and
// in transit respectively.
//
// A push of content already present succeeds without transferring it: ORAS
// returns ErrAlreadyExists, which is not an error to a caller whose goal is
// "this blob is at the destination".
func (r *Repository) PushBlob(
	ctx context.Context, dgst registry.Digest, size int64, src io.Reader,
) error {
	repo, err := r.orasRepo()
	if err != nil {
		return err
	}

	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.Digest(dgst.String()),
		Size:      size,
	}

	err = repo.Blobs().Push(ctx, desc, src)
	if errors.Is(err, errdef.ErrAlreadyExists) {
		return nil
	}
	if err != nil {
		return r.classify(err, "push blob "+dgst.Short())
	}
	return nil
}

// MountBlob asks the registry to relocate a blob from another repository on the
// SAME registry, moving zero bytes.
//
// This is the dominant optimisation for promotion — a 45 GB lab-to-production
// copy can complete in seconds — and applies to replication only when a vendor
// happens to share a registry with us.
//
// Support is uneven, so a registry declining is a NORMAL outcome and not an
// error: it answers 202 with an ordinary upload session instead. That surfaces
// here as ErrMountUnsupported, which the caller treats as "fall through to
// streaming" rather than as a failure.
func (r *Repository) MountBlob(ctx context.Context, dgst registry.Digest, fromRepo string) error {
	repo, err := r.orasRepo()
	if err != nil {
		return err
	}

	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    digest.Digest(dgst.String()),
		// Size is unknown to a mount and unused by it: the registry already
		// holds the content and is only being asked to reference it.
		Size: 0,
	}

	// getContent is nil deliberately. Passing a fallback would let ORAS quietly
	// UPLOAD the blob when the mount is declined — which is the right outcome
	// but the wrong place to decide it: the caller owns the fast-path ladder,
	// counts the bytes, and records why a job was skipped.
	err = repo.Mount(ctx, desc, fromRepo, nil)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errdef.ErrAlreadyExists):
		return nil
	case errors.Is(err, errdef.ErrUnsupported), errors.Is(err, errdef.ErrNotFound):
		return fmt.Errorf("%w: registry declined to mount %s from %s",
			registry.ErrMountUnsupported, dgst.Short(), fromRepo)
	default:
		// A mount that fails for any other reason is still not fatal: streaming
		// is always correct. Reported as unsupported so one ladder handles it.
		return fmt.Errorf("%w: mount %s from %s: %w",
			registry.ErrMountUnsupported, dgst.Short(), fromRepo, err)
	}
}

// PushManifest writes a manifest VERBATIM and points a reference at it.
//
// Invariant I9: the bytes pushed are the bytes fetched, byte for byte. The
// digest is the hash of exactly these bytes and every signature is over that
// digest, so re-serializing — even to identical-looking JSON with different key
// order or whitespace — would change the digest and invalidate the signature.
// That is why this takes raw bytes and not a struct.
//
// ref may be a tag or a digest. A digest push is content-addressed and always
// safe; a tag push is what makes the release visible, and is therefore the LAST
// thing a transfer does (invariant I1).
func (r *Repository) PushManifest(
	ctx context.Context, ref, mediaType string, raw []byte,
) (registry.Descriptor, error) {
	repo, err := r.orasRepo()
	if err != nil {
		return registry.Descriptor{}, err
	}

	if mediaType == "" {
		mediaType = registry.MediaTypeOCIManifest
	}
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.Digest(registry.NewDigestFromBytes(raw).String()),
		Size:      int64(len(raw)),
	}

	if ref == "" {
		ref = desc.Digest.String()
	}

	err = repo.Manifests().PushReference(ctx, desc, newByteReader(raw), ref)
	if errors.Is(err, errdef.ErrAlreadyExists) {
		return fromOCI(desc), nil
	}
	if err != nil {
		return registry.Descriptor{}, r.classify(err, "push manifest "+ref)
	}
	return fromOCI(desc), nil
}

// Tag points a tag at content already present.
//
// Separate from PushManifest because the two happen at different moments: the
// manifest is pushed by digest during the transfer, and the tag applied only
// once everything beneath it exists. A tag that appears early is a consumer
// pulling a half-written release.
func (r *Repository) Tag(ctx context.Context, dgst registry.Digest, tag string) error {
	repo, err := r.orasRepo()
	if err != nil {
		return err
	}

	desc, err := repo.Manifests().Resolve(ctx, dgst.String())
	if err != nil {
		return r.classify(err, "resolve manifest "+dgst.Short())
	}
	if err := repo.Tag(ctx, desc, tag); err != nil {
		return r.classify(err, "tag "+dgst.Short()+" as "+tag)
	}
	return nil
}

// Referrers lists artifacts that reference a subject — signatures, SBOMs,
// attestations.
//
// The OCI 1.1 API where the registry supports it, with ORAS falling back to the
// tag schema where it does not. Both mechanisms are needed in practice: the API
// is the standard and the fallback is what most deployed registries actually
// answer.
func (r *Repository) Referrers(
	ctx context.Context, subject registry.Digest, artifactType string,
) ([]registry.Descriptor, error) {
	repo, err := r.orasRepo()
	if err != nil {
		return nil, err
	}

	desc := ocispec.Descriptor{
		MediaType: registry.MediaTypeOCIManifest,
		Digest:    digest.Digest(subject.String()),
	}

	var out []registry.Descriptor
	err = repo.Referrers(ctx, desc, artifactType, func(refs []ocispec.Descriptor) error {
		for _, d := range refs {
			out = append(out, fromOCI(d))
		}
		return nil
	})
	if err != nil {
		// A registry that supports neither mechanism has no referrers to
		// report, which is a fact rather than a failure.
		if errors.Is(err, errdef.ErrUnsupported) || errors.Is(err, errdef.ErrNotFound) {
			return nil, nil
		}
		return nil, r.classify(err, "list referrers of "+subject.Short())
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// conversion and error classification
// ---------------------------------------------------------------------------

// fromOCI converts an ORAS descriptor to ours.
//
// The conversion is confined to this file. Everything above internal/registry
// speaks our Descriptor, so swapping the library — which ADR-001 requires to
// stay possible — touches these two functions and nothing else.
func fromOCI(d ocispec.Descriptor) registry.Descriptor {
	out := registry.Descriptor{
		MediaType:    d.MediaType,
		ArtifactType: d.ArtifactType,
		Digest:       registry.Digest(d.Digest.String()),
		Size:         d.Size,
		Annotations:  d.Annotations,
	}
	if d.Platform != nil {
		out.Platform = &registry.Platform{
			OS:           d.Platform.OS,
			Architecture: d.Platform.Architecture,
			Variant:      d.Platform.Variant,
		}
	}
	return out
}

// classify maps an ORAS error onto our sentinel classes, so retry policy keys
// off an error class rather than re-inspecting an HTTP status deep in the
// stack.
//
// # Why this reads the OCI error CODE and not only the status
//
// It used to handle four statuses and let everything else through unwrapped.
// Anything the registry rejected with a 400 — which is how several registries
// answer a manifest they will not accept — came out `unclassified`, and
// unclassified is treated as RETRYABLE on the reasonable theory that an
// uncategorised failure is more likely a transient network fault than a
// permanent one. For a manifest the destination will never accept, that theory
// is wrong in the most expensive possible way: eight attempts, each a full
// round trip over a slow link, per manifest, ending exactly where it started.
//
// So the status now goes through registry.ClassifyStatus in full, and the
// registry's own error code is read first where there is one. The code is the
// better signal: `MANIFEST_BLOB_UNKNOWN` means "a blob you referenced is not
// here", which is a self-healing signal the engine routes to placement
// invalidation — and it arrives as a 400 from some registries and a 404 from
// others, so keying off the status alone gets it right half the time.
//
// The result is a *registry.Error, which carries the status and the registry's
// own words as structured fields. That is what makes a failure legible in a
// listing: `HTTP 400: MANIFEST_INVALID: …` reads as a cause, where the ORAS
// string put the same information at the end of a line nobody can see.
func (r *Repository) classify(err error, what string) error {
	if err == nil {
		return nil
	}

	out := &registry.Error{Op: what, Repository: r.registryHost + "/" + r.repoPath}

	var respErr *errcode.ErrorResponse
	if errors.As(err, &respErr) {
		out.StatusCode = respErr.StatusCode
		out.Detail = detailOf(respErr)
		out.Err = sentinelFor(respErr)
		return out
	}

	switch {
	case errors.Is(err, errdef.ErrNotFound):
		out.Err = registry.ErrNotFound
	case errors.Is(err, errdef.ErrUnsupported):
		out.Err = registry.ErrUnsupported
	default:
		// Genuinely uncategorised — a transport failure, most often. Kept
		// verbatim: the string is all there is, and truncating it here would
		// leave the operator with a class and no cause.
		out.Err = err
	}
	return out
}

// sentinelFor picks the sentinel an error response maps onto.
//
// The registry's own error code wins over the HTTP status, because the code
// says WHAT was wrong and the status only says how the registry chose to
// signal it. Where there is no code, the status is all we have.
func sentinelFor(resp *errcode.ErrorResponse) error {
	for _, e := range resp.Errors {
		switch e.Code {
		case errcode.ErrorCodeBlobUnknown, errcode.ErrorCodeManifestBlobUnknown,
			errcode.ErrorCodeManifestUnknown, errcode.ErrorCodeNameUnknown:
			// The destination is missing something we believed was there. On a
			// manifest push this is the placement cache being wrong, which the
			// engine repairs rather than retries (docs/design/11 §2.5).
			return registry.ErrNotFound
		case errcode.ErrorCodeUnauthorized:
			return registry.ErrUnauthorized
		case errcode.ErrorCodeDenied:
			return registry.ErrForbidden
		case errcode.ErrorCodeDigestInvalid, errcode.ErrorCodeSizeInvalid:
			return registry.ErrDigestMismatch
		case errcode.ErrorCodeManifestInvalid, errcode.ErrorCodeNameInvalid,
			errcode.ErrorCodeUnsupported:
			// MANIFEST_INVALID is where one registry hides a missing blob, so
			// the detail decides before the code does.
			if reportsAMissingBlob(e) {
				return registry.ErrNotFound
			}
			// Otherwise not retryable. The registry will reject these bytes
			// every time, and eight attempts over a slow link prove it eight
			// times.
			return registry.ErrUnsupported
		}
	}

	if sentinel := registry.ClassifyStatus(resp.StatusCode); sentinel != nil {
		return sentinel
	}
	return registry.ErrUnsupported
}

// missingBlobPhrases are the ways a registry says "a blob you referenced is not
// in this repository" without using the error code reserved for saying it.
//
// The observed one, from Artifactory, verbatim:
//
//	HTTP 400: manifest invalid: map[description:Failed to copy blob sha256:… to
//	orbs/cfx-5000-…/sha256:…/sha256__dc82908b11cf…]
//
// That is MANIFEST_BLOB_UNKNOWN wearing MANIFEST_INVALID's code. The
// distinction is not cosmetic: MANIFEST_INVALID means "these bytes are wrong
// and always will be", and a missing blob means "put the blob there and this
// works". Reading the first as the second gives up on a transfer that is one
// upload away from finishing.
var missingBlobPhrases = []string{
	"failed to copy blob",
	"blob unknown",
	"unknown blob",
	"missing blob",
	"blob not found",
}

// reportsAMissingBlob reads a registry's own prose for a missing blob.
//
// Matching on prose is exactly as unpleasant as it looks, and it is confined to
// this function and to error codes that are otherwise terminal, so the cost of
// being wrong is a retry rather than a wrong result. The alternative is
// accepting that one registry's bundles can never be replicated, which is a
// worse trade than a string match.
func reportsAMissingBlob(e errcode.Error) bool {
	text := strings.ToLower(e.Message)
	if e.Detail != nil {
		text += " " + strings.ToLower(fmt.Sprint(e.Detail))
	}
	for _, phrase := range missingBlobPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// detailOf renders what the registry actually said, in the order an operator
// reads it: the code first, because that is the part that is actionable, then
// the message.
func detailOf(resp *errcode.ErrorResponse) string {
	if len(resp.Errors) == 0 {
		return http.StatusText(resp.StatusCode)
	}
	return resp.Errors.Error()
}

// newByteReader wraps bytes for a manifest push.
//
// A dedicated helper rather than bytes.NewReader inline, so the intent is
// visible at the call site: manifests are pushed from memory because they are
// small and must be digest-stable, while BLOBS are streamed and never are.
func newByteReader(b []byte) io.Reader { return bytes.NewReader(b) }

// ResumeUpload continues an interrupted chunked upload.
//
// NOT IMPLEMENTED, and returning ErrUnsupported here is a correct
// implementation rather than a stub — which is worth being precise about.
//
// Resumption is an OPTIMISATION, never a correctness requirement
// (docs/design/05 §4.6). Blobs are content-addressed and pushing one twice is
// idempotent, so restarting a blob is always right; resumption only saves
// re-sending bytes. The caller's fallback for any resume failure is therefore
// already "restart the blob", and this simply takes that path every time.
//
// Deferred because the honest weak spot in §4.6 is real: several registries
// accept the PATCH/Range protocol without durably retaining a session across a
// connection drop, so an implementation would need measurement against the
// registries actually in use before it could be trusted. Building it against
// the fake registry would only prove the fake registry resumes.
//
// `softwaregateway_upload_resume_total{result}` exists so that measurement
// arrives from production rather than from documentation. Until it does, an
// interrupted 8 GB upload costs a restart, which is visible in the transfer
// duration and in nothing else.
func (r *Repository) ResumeUpload(_ context.Context, state registry.UploadState, _ io.Reader) error {
	return fmt.Errorf("%w: resuming an upload at offset %d is not implemented; "+
		"restart the blob instead", registry.ErrUnsupported, state.Offset)
}

// The compile-time assertion the M2 code deliberately could not make.
//
// internal/registry.Repository declares the full contract — read, write,
// referrers — and until now nothing satisfied it, which was the type system
// telling the truth about what existed. It is satisfied now.
var _ registry.Repository = (*Repository)(nil)
