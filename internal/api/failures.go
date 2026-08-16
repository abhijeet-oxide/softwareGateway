package api

import (
	"net/http"
	"strconv"
	"strings"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
	"github.com/go-chi/chi/v5"
)

// handleListTransferFailures serves GET /api/v1/transfers/{transfer}/failures.
//
// A sibling of the job listing and a different question. The job listing
// answers "which jobs are failing" — and when five hundred manifests are
// rejected by the destination, the honest answer to that is five hundred rows
// that differ only in the digest and the path. This answers "why", which
// usually has two or three answers, each one actionable.
func (s *Server) handleListTransferFailures(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "transfer")

	id, err := s.deps.Packages.ResolveTransferID(r.Context(), ref)
	if err != nil {
		NotFound(w, r, "transfer", ref)
		return
	}

	groups, err := s.deps.Packages.FailureGroups(r.Context(), id)
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not summarise the failures: "+err.Error())
		return
	}

	out := v1.ListFailuresResponse{
		TransferID: id,
		Failures:   make([]v1.FailureGroup, 0, len(groups)),
	}

	// The transfer's OWN state and reason, alongside its jobs'. A transfer that
	// failed before planning has no failing jobs and a perfectly good reason,
	// and this is the command whose whole purpose is to report it.
	if t, err := s.deps.Packages.GetTransfer(r.Context(), id); err == nil {
		out.State = v1.TransferState(strings.ToUpper(t.State))
		out.FailureReason = t.FailureReason
	}
	for _, g := range groups {
		out.Failures = append(out.Failures, v1.FailureGroup{
			Class:             g.Class,
			Message:           g.Message,
			Failed:            g.Failed,
			Retrying:          g.Retrying,
			Kinds:             g.Kinds,
			Waves:             g.Waves,
			ExampleJobID:      strconv.FormatInt(g.ExampleJobID, 10),
			ExampleDigest:     g.ExampleDigest,
			ExampleRepository: g.ExampleRepository,
			ExampleTags:       g.ExampleTags,
			ExampleError:      g.ExampleError,
			Retryable:         g.Retryable,
		})
	}
	WriteJSON(w, r, http.StatusOK, out)
}
