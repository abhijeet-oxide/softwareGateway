package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Downloading one scanner body for one image.
//
// # Why this is a route and not part of the security response
//
// Because an SBOM for a large container image is tens of megabytes, a release
// is 157 images, and a page that carried them would be a four-gigabyte JSON
// document rendered as a table of numbers. The response says WHICH bodies exist
// and how big they are; this serves one when somebody asks for it.
//
// # Why it may fetch
//
// The vulnerability response, the policy verdict and the malware list are
// captured by a sync, because that sync was making the request anyway. The SBOM
// is not: generating one is minutes and megabytes PER IMAGE, and doing it for
// every image on every sync would turn a two-minute job into an hour for a file
// somebody downloads occasionally. So an SBOM that is not held is generated
// here, once, and kept.

// handleSecurityDocument serves
// GET /products/{product}/packages/{package}/security/documents/{kind}?digest=.
func (s *Server) handleSecurityDocument(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.SecurityStore == nil {
		Error(w, r, v1.CodeUnavailable, "security storage is not configured on this Coordinator")
		return
	}

	kind, ok := security.ParseDocumentKind(chi.URLParam(r, "kind"))
	if !ok {
		Error(w, r, v1.CodeInvalidArgument,
			"kind must be one of vulnerabilities, sbom, policy, malware")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	digest := strings.TrimSpace(r.URL.Query().Get("digest"))
	if digest == "" {
		Error(w, r, v1.CodeInvalidArgument,
			"digest is required: a document belongs to one image, not to the release")
		return
	}

	target := s.securityTargetFor(r.Context(), productName, pkg)
	if !target.Available {
		Error(w, r, v1.CodeFailedPrecondition, target.Reason)
		return
	}

	// The artifact as the release knows it, not as the query described it. The
	// scanner is asked about a path and a tag, and taking those from a query
	// parameter would let a caller ask this Coordinator's credential about an
	// image in somebody else's repository.
	artifact, ok := s.artifactByDigest(r.Context(), productName, pkg, digest)
	if !ok {
		Error(w, r, v1.CodeNotFound,
			"this release does not contain an artifact with that digest")
		return
	}

	docs, err := s.deps.SecurityStore.LoadDocuments(
		r.Context(), target.Scope, []security.ArtifactRef{artifact},
		[]security.DocumentKind{kind})
	if err != nil {
		s.internal(w, r, "read security document", err)
		return
	}

	doc, held := docs[artifact.Ref()][kind]
	if (!held || !doc.Available) && s.deps.SecurityDocuments != nil {
		// Not held. Worth generating for an SBOM, which a sync deliberately
		// does not fetch; not worth it for the other three, whose absence means
		// the sync did not retrieve them and whose fix is a sync rather than a
		// download that quietly re-runs part of one.
		if kind != security.DocumentSBOM {
			s.documentMissing(w, r, kind, doc, held)
			return
		}
		fetched, err := s.deps.SecurityDocuments.Documents(r.Context(), security.DocumentRequest{
			Scope:     target.Scope,
			Artifacts: []security.ArtifactRef{artifact},
			Kinds:     []security.DocumentKind{kind},
			TTL:       s.deps.SecurityRetention,
		})
		if err != nil {
			s.internal(w, r, "generate security document", err)
			return
		}
		for _, d := range fetched {
			if d.Kind == kind {
				doc, held = d, true
			}
		}
	}

	if !held || !doc.Available || len(doc.Payload) == 0 {
		s.documentMissing(w, r, kind, doc, held)
		return
	}

	contentType := doc.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	filename := strings.Join([]string{
		bundleSegment(productName), bundleSegment(releaseLabel(pkg)),
		bundleSegment(artifact.ArtifactKey()), bundleSegment(tagOrDigest(artifact)),
		string(kind),
	}, "_") + doc.Extension()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	// The scanner's own body, for one repository's permissions. A shared cache
	// must never hold it.
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc.Payload)
}

// documentMissing explains an absence in words with an action in them.
//
// A 404 with no body would leave a reader who pressed a visible button with no
// idea whether the scanner does not offer this, the sync did not fetch it, or
// something is broken - three different next steps.
func (s *Server) documentMissing(
	w http.ResponseWriter, r *http.Request,
	kind security.DocumentKind, doc security.Document, held bool,
) {
	if held && doc.Message != "" {
		Error(w, r, v1.CodeFailedPrecondition, doc.Message)
		return
	}
	switch kind {
	case security.DocumentSBOM:
		Error(w, r, v1.CodeNotFound,
			"JFrog Xray has no SBOM for this image. It is generated on demand, so this "+
				"means the scanner declined - check that the image is indexed, and that the "+
				"JFrog credential is allowed to export component details.")
	default:
		Error(w, r, v1.CodeNotFound,
			"This release has no stored "+kind.Label()+" for that image. "+
				"Sync it again to retrieve one - and check coordinator.security.documents "+
				"lists "+string(kind)+", because a sync only fetches what it is asked for.")
	}
}

// artifactByDigest finds one of a release's artifacts by digest.
//
// Read from the release's own tree rather than trusted from the request, so a
// caller cannot point this Coordinator's JFrog credential at an image in a
// repository they were never entitled to ask about.
func (s *Server) artifactByDigest(
	ctx context.Context, productName string, pkg store.PackageRow, digest string,
) (security.ArtifactRef, bool) {
	want := strings.ToLower(strings.TrimSpace(digest))
	for _, ref := range s.securityArtifactsFor(productName, pkg, ctx) {
		if strings.ToLower(ref.Digest) == want {
			return ref, true
		}
	}
	return security.ArtifactRef{}, false
}
