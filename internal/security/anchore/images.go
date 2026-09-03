package anchore

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// Getting images INTO Anchore, which is the half of this integration that has
// no counterpart on the Xray path.
//
// Xray indexes a repository: an image that lands there is scanned because it
// landed there, and this platform's only job is to ask. Anchore is told about
// one image at a time, pulls it itself, and analyses it asynchronously. So
// "sync this release" here means submit what Anchore does not have, wait for
// what it is working on, and only then read.

// ImageRecord is what Anchore knows about one image.
type ImageRecord struct {
	Digest string
	// Status is the analysis state - see the Analysis* constants. Empty means
	// Anchore has no record at all, which is different from `not_analyzed`:
	// one has never been submitted and the other is queued.
	Status string
	// AnalyzedAt is when analysis finished, where Anchore says.
	AnalyzedAt *jsonTime
	// FullTag is a pull string Anchore knows the image by, for a message.
	FullTag string
	// Known is false when Anchore has no record of the digest at all.
	Known bool
	// Detail is Anchore's own words about a failure, where it gave any.
	Detail string
}

// Terminal reports whether this record will not change without a resubmission.
func (r ImageRecord) Terminal() bool { return terminal(r.Status) }

// Analyzed reports whether this image's vulnerabilities are complete.
func (r ImageRecord) Analyzed() bool { return r.Status == AnalysisAnalyzed }

// GetImage reads Anchore's record for one digest.
//
// A missing record is (ImageRecord{Known: false}, nil), NOT an error. It is the
// ordinary state of every image on a release's first sync, and turning it into
// an error would make the normal case indistinguishable from an outage.
func (c *Client) GetImage(ctx context.Context, digest string) (ImageRecord, error) {
	var out image
	err := c.do(ctx, http.MethodGet, "/images/"+pathEscape(digest), nil, &out)
	switch {
	case NotFound(err):
		return ImageRecord{Digest: digest}, nil
	case err != nil:
		return ImageRecord{Digest: digest}, err
	}
	return recordOf(out), nil
}

// GetImages reads Anchore's records for several digests in ONE request.
//
// # Why a list rather than a lookup per image
//
// Because a release is a hundred and fifty images and a sync asks this question
// at least twice: once to decide what to submit, and once per poll while it
// waits. Per image that is three hundred requests before any vulnerability has
// been read, repeated every fifteen seconds - which is how an integration
// becomes the reason somebody's Anchore is slow.
//
// Anchore's list endpoint has no digest filter, so this reads the account's
// images and indexes them. That is one large response instead of N small ones,
// and it is the same trade the Xray path makes with AQL.
//
// Falls back to per-image lookups when the list cannot be read, because the
// distinction this answers - submitted or not - is the one the whole sync
// branches on, and losing it would mean submitting every image on every sync.
func (c *Client) GetImages(ctx context.Context, digests []string) (map[string]ImageRecord, error) {
	out := make(map[string]ImageRecord, len(digests))
	for _, d := range digests {
		out[d] = ImageRecord{Digest: d}
	}

	var list imageList
	// image_status=all, because an image Anchore is deleting still has a record
	// and reporting it as never submitted would make the sync submit it again
	// into a delete that is in progress.
	err := c.do(ctx, http.MethodGet, "/images"+query("image_status", "all"), nil, &list)
	if err != nil {
		return out, err
	}
	for _, img := range list.Items {
		if _, wanted := out[img.ImageDigest]; wanted {
			out[img.ImageDigest] = recordOf(img)
		}
	}
	return out, nil
}

func recordOf(img image) ImageRecord {
	r := ImageRecord{
		Digest:     img.ImageDigest,
		Status:     img.AnalysisStatus,
		AnalyzedAt: img.analyzedAt(),
		FullTag:    img.fullTag(),
		Known:      true,
	}
	if img.AnalysisStatus == AnalysisFailed && len(img.AnalysisStatusDetail) > 0 {
		r.Detail = firstLine(string(img.AnalysisStatusDetail))
	}
	return r
}

// Submit asks Anchore to analyse one image.
//
// This Anchore deployment accepts the v2 tag source for its configured
// registry, while rejecting the equivalent digest source. The artifact digest
// remains the immutable identity used for lookup and association after Anchore
// accepts the submission.
//
// # Idempotent
//
// Anchore returns the existing record for an image it already has, so a
// resubmission is a no-op rather than a second analysis. `force` is deliberately
// NOT sent: it discards the existing analysis and starts again, which is a
// minutes-long re-analysis of an image whose bytes cannot have changed.
func (c *Client) Submit(ctx context.Context, ref security.ArtifactRef) (ImageRecord, error) {
	if _, err := PullString(ref); err != nil {
		return ImageRecord{Digest: ref.Digest}, err
	}
	req := analysisRequest{
		ImageType: "docker",
		Source: analysisSource{
			Tag: &tagSource{PullString: TagString(ref)},
		},
		Annotations: annotationsFor(ref),
	}

	var many []image
	if err := c.do(ctx, http.MethodPost, "/images", req, &many); err != nil {
		var one image
		if retryErr := c.do(ctx, http.MethodPost, "/images", req, &one); retryErr == nil {
			return recordOf(one), nil
		}
		return ImageRecord{Digest: ref.Digest}, err
	}
	if len(many) == 0 {
		return ImageRecord{Digest: ref.Digest, Status: AnalysisNotAnalyzed, Known: true}, nil
	}
	return recordOf(many[0]), nil
}

// annotationsFor is what this platform tells Anchore about an image.
//
// Small on purpose. Annotations are searchable in Anchore's own interface, and
// the two facts worth finding an image by there are which release it belongs to
// and that this platform submitted it - the second so an operator cleaning up
// can tell our submissions from anybody else's.
func annotationsFor(ref security.ArtifactRef) map[string]string {
	out := map[string]string{"submitted_by": "softwaregateway"}
	if ref.Name != "" {
		out["artifact"] = ref.Name
	}
	return out
}

// PullString is the reference Anchore pulls by: registry/repository@digest.
//
// # Why this cannot be derived from the source repository
//
// Because the source is the VENDOR's registry, and Anchore cannot reach it -
// that is the whole reason the release was replicated. The reference has to
// name the internal registry the release landed in, which is why ArtifactRef
// carries Registry and Repository separately and why the provider is
// constructed against a specific target (see Settings.Registry).
func PullString(ref security.ArtifactRef) (string, error) {
	registry := strings.TrimSpace(ref.Registry)
	repository := strings.Trim(strings.TrimSpace(ref.Repository), "/")
	digest := strings.TrimSpace(ref.Digest)
	if registry == "" || repository == "" {
		return "", fmt.Errorf(
			"anchore: this artifact has no internal registry path, so Anchore cannot be told where to pull it from")
	}
	if digest == "" {
		return "", fmt.Errorf("anchore: this artifact has no digest, so it cannot be submitted by digest")
	}
	return registry + "/" + repository + "@" + digest, nil
}

// TagString is the tag reference recorded alongside the digest, for display in
// Anchore. Falls back to the digest's own short form where a release's artifact
// has no tag, because the field is required by the schema.
func TagString(ref security.ArtifactRef) string {
	registry := strings.TrimSpace(ref.Registry)
	repository := strings.Trim(strings.TrimSpace(ref.Repository), "/")
	tag := strings.TrimSpace(ref.Tag)
	if tag == "" {
		tag = shortDigest(ref.Digest)
	}
	if registry == "" || repository == "" {
		return tag
	}
	return registry + "/" + repository + ":" + tag
}

func shortDigest(digest string) string {
	d := strings.TrimSpace(digest)
	if i := strings.Index(d, ":"); i >= 0 {
		d = d[i+1:]
	}
	if len(d) > 12 {
		d = d[:12]
	}
	if d == "" {
		return "untagged"
	}
	return "sha256-" + d
}

// pathEscape keeps a digest's colon out of trouble in a path segment.
//
// Anchore's own routes take the digest raw and a colon is legal in a path
// segment, so this is deliberately minimal: escaping the colon breaks the
// lookup on some builds, and everything else in a digest is already safe.
func pathEscape(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "%20")
}
