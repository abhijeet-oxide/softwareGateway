package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// maxManifestBytes bounds a manifest body.
//
// The OCI spec recommends registries reject manifests above 4 MiB. Bounding it
// here means a hostile or broken registry cannot exhaust memory during a
// routine scan — discovery fetches a manifest for every newly discovered tag.
const maxManifestBytes = 8 << 20 // 8 MiB

// ResolveTag returns the descriptor a tag points at, without fetching the body.
//
// A HEAD reading Docker-Content-Digest. Discovery calls this for EVERY tag on
// EVERY scan, so it must stay cheap: the common case — a scan where nothing
// changed — costs one small request per tag and transfers no manifest bodies.
func (r *Repository) ResolveTag(ctx context.Context, tag string) (registry.Descriptor, error) {
	if tag == "" {
		return registry.Descriptor{}, fmt.Errorf("tag is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, r.url("/manifests/%s", tag), nil)
	if err != nil {
		return registry.Descriptor{}, err
	}
	req.Header.Set("Accept", strings.Join(registry.ManifestAcceptTypes, ", "))

	resp, err := r.client.Do(req)
	if err != nil {
		return registry.Descriptor{}, r.wrap("resolve tag", tag, 0, nil, transportError(err))
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return registry.Descriptor{}, r.wrap("resolve tag", tag, resp.StatusCode, resp, registry.ClassifyStatus(resp.StatusCode))
	}

	desc, err := descriptorFromHeaders(resp)
	if err != nil {
		return registry.Descriptor{}, r.wrap("resolve tag", tag, resp.StatusCode, nil, err)
	}
	return desc, nil
}

// FetchManifest returns manifest bytes VERBATIM.
//
// The bytes are returned exactly as received and are never re-serialized: the
// digest — and every signature over it — is the hash of these exact bytes.
// Round-tripping through a struct would change whitespace or key order and
// produce a different digest, silently breaking verification later.
//
// ref may be a tag or a digest.
func (r *Repository) FetchManifest(ctx context.Context, ref string) (registry.Descriptor, []byte, error) {
	if ref == "" {
		return registry.Descriptor{}, nil, fmt.Errorf("reference is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url("/manifests/%s", ref), nil)
	if err != nil {
		return registry.Descriptor{}, nil, err
	}
	req.Header.Set("Accept", strings.Join(registry.ManifestAcceptTypes, ", "))

	resp, err := r.client.Do(req)
	if err != nil {
		return registry.Descriptor{}, nil, r.wrap("fetch manifest", ref, 0, nil, transportError(err))
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		return registry.Descriptor{}, nil, r.wrap("fetch manifest", ref, resp.StatusCode, resp, registry.ClassifyStatus(resp.StatusCode))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return registry.Descriptor{}, nil, r.wrap("fetch manifest", ref, resp.StatusCode, nil, transportError(err))
	}
	if len(raw) > maxManifestBytes {
		return registry.Descriptor{}, nil, r.wrap("fetch manifest", ref, resp.StatusCode, nil,
			fmt.Errorf("%w: manifest exceeds %d bytes", registry.ErrMalformedResponse, maxManifestBytes))
	}

	// Verify the content against its address. The registry's own
	// Docker-Content-Digest header is a claim; the bytes are the fact. A
	// mismatch means a corrupting proxy or a misbehaving registry, and
	// accepting it would poison every downstream dedupe decision that trusts
	// the digest.
	actual := registry.NewDigestFromBytes(raw)
	if claimed := registry.Digest(resp.Header.Get("Docker-Content-Digest")); claimed != "" && claimed != actual {
		return registry.Descriptor{}, nil, r.wrap("fetch manifest", ref, resp.StatusCode, nil,
			fmt.Errorf("%w: registry claimed %s, content hashes to %s",
				registry.ErrDigestMismatch, claimed.Short(), actual.Short()))
	}
	// When the reference IS a digest, it must match too.
	if strings.Contains(ref, ":") {
		if requested := registry.Digest(ref); requested.Validate() == nil && requested != actual {
			return registry.Descriptor{}, nil, r.wrap("fetch manifest", ref, resp.StatusCode, nil,
				fmt.Errorf("%w: requested %s, content hashes to %s",
					registry.ErrDigestMismatch, requested.Short(), actual.Short()))
		}
	}

	desc := registry.Descriptor{
		MediaType: resp.Header.Get("Content-Type"),
		Digest:    actual,
		Size:      int64(len(raw)),
	}
	if desc.MediaType == "" {
		desc.MediaType = registry.MediaTypeOCIManifest
	}

	return desc, raw, nil
}

// descriptorFromHeaders builds a descriptor from a HEAD response.
func descriptorFromHeaders(resp *http.Response) (registry.Descriptor, error) {
	dgst := registry.Digest(resp.Header.Get("Docker-Content-Digest"))
	if dgst == "" {
		// Without this header we cannot pin content, and a tag alone is not an
		// identity. Better to fail than to record a package we cannot address.
		return registry.Descriptor{}, fmt.Errorf(
			"%w: response has no Docker-Content-Digest header", registry.ErrMalformedResponse)
	}
	if err := dgst.Validate(); err != nil {
		return registry.Descriptor{}, fmt.Errorf("%w: %w", registry.ErrMalformedResponse, err)
	}

	desc := registry.Descriptor{
		MediaType: resp.Header.Get("Content-Type"),
		Digest:    dgst,
	}
	if desc.MediaType == "" {
		desc.MediaType = registry.MediaTypeOCIManifest
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			desc.Size = n
		}
	}
	return desc, nil
}

// retryAfter reads the Retry-After header.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// transportError classifies a client-side failure.
//
// A timeout is retryable and a DNS failure usually is not, but distinguishing
// them reliably is not worth the fragility — an unclassified error is treated
// as retryable and bounded by the attempt cap, which is the safer default.
func transportError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	// A failure the auth layer already classified — missing credentials, say —
	// must keep that classification. Re-labelling it ErrUnavailable would make
	// a terminal problem look retryable and reintroduce the backoff hang.
	if registry.ClassOf(err) != registry.ClassUnclassified {
		return err
	}

	// Both errors are wrapped, so the sentinel drives classification while the
	// underlying cause stays reachable through errors.As — a caller wanting the
	// *net.OpError for diagnostics can still get at it.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// The knob is named in the message because the stdlib's wording —
		// "net/http: timeout awaiting response headers" — describes the symptom
		// and not one thing you can do about it. A registry that is slow rather
		// than broken is a configuration problem, and the configuration is
		// per repository.
		return fmt.Errorf("%w: %w (no response headers within the deadline; if this "+
			"registry is simply slow, raise network.timeouts.responseHeader — it "+
			"defaults to %s)", registry.ErrTimeout, err, transport.DefaultResponseHeaderTimeout)
	}
	return fmt.Errorf("%w: %w", registry.ErrUnavailable, err)
}
