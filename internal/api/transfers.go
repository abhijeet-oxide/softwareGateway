package api

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Transfer READ routes. See docs/design/09-api.md §2.
//
// Read only, and that is the honest state of things: pause, resume, cancel,
// retry and setPriority are specified but not built, so they are absent rather
// than present and inert. A route that accepts a pause and does nothing is
// worse than a 404, because the 404 is believed.

// handleListTransfers serves GET /api/v1/transfers.
func (s *Server) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	pageSize, err := parsePageSize(q.Get("pageSize"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}
	offset, err := parseOffset(q.Get("pageToken"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	state, err := parseTransferState(q.Get("state"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	// One row over the page size, so "is there another page" is answered
	// without a second COUNT and without claiming a page that turns out empty.
	rows, err := s.deps.Packages.ListTransfers(r.Context(), store.ListTransfersFilter{
		ProductName: q.Get("product"),
		State:       state,
		Limit:       pageSize + 1,
		Offset:      offset,
	})
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not list transfers: "+err.Error())
		return
	}

	out := v1.ListTransfersResponse{Transfers: make([]v1.Transfer, 0, pageSize)}
	if len(rows) > pageSize {
		out.NextPageToken = strconv.Itoa(offset + pageSize)
		rows = rows[:pageSize]
	}
	for _, t := range rows {
		out.Transfers = append(out.Transfers, transferDTO(t))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// handleGetTransfer serves GET /api/v1/transfers/{transfer}.
func (s *Server) handleGetTransfer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "transfer")

	t, err := s.deps.Packages.GetTransfer(r.Context(), id)
	if err != nil {
		NotFound(w, r, "transfer", id)
		return
	}
	WriteJSON(w, r, http.StatusOK, transferDTO(t))
}

// handleListTransferJobs serves GET /api/v1/transfers/{transfer}/jobs.
//
// This is where an operator looks when a transfer is not moving: which blob is
// stuck, on which attempt, with which error, held by which worker.
func (s *Server) handleListTransferJobs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "transfer")

	if _, err := s.deps.Packages.GetTransfer(r.Context(), id); err != nil {
		NotFound(w, r, "transfer", id)
		return
	}

	pageSize, err := parsePageSize(r.URL.Query().Get("pageSize"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	jobs, err := s.deps.Packages.ListJobs(r.Context(), id, pageSize)
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not list jobs: "+err.Error())
		return
	}

	out := v1.ListJobsResponse{TransferID: id, Jobs: make([]v1.Job, 0, len(jobs))}
	for _, j := range jobs {
		out.Jobs = append(out.Jobs, v1.Job{
			ID:               strconv.FormatInt(j.ID, 10),
			Kind:             j.Kind,
			Digest:           j.Digest,
			SizeBytes:        v1.Int64String(strconv.FormatInt(j.SizeBytes, 10)),
			State:            v1.JobState(strings.ToUpper(j.State)),
			SkipReason:       v1.SkipReason(j.SkipReason),
			Wave:             j.Wave,
			Attempts:         j.Attempts,
			MaxAttempts:      j.MaxAttempts,
			BytesTransferred: v1.Int64String(strconv.FormatInt(j.BytesTransferred, 10)),
			LeaseOwner:       j.LeaseOwner,
			LastError:        j.LastError,
			LastErrorClass:   j.LastErrorClass,
		})
	}
	WriteJSON(w, r, http.StatusOK, out)
}

func transferDTO(t store.TransferSummary) v1.Transfer {
	return v1.Transfer{
		ID:          t.ID,
		RequestID:   t.RequestID,
		Product:     t.ProductName,
		Tag:         t.Tag,
		Source:      t.Source,
		Target:      t.Target,
		State:       v1.TransferState(strings.ToUpper(t.State)),
		Priority:    t.Priority,
		CurrentWave: t.CurrentWave,
		MaxWave:     t.MaxWave,
		Progress: v1.TransferProgress{
			JobsPlanned:        t.PlannedJobs,
			JobsDone:           t.JobsDone,
			JobsFailed:         t.JobsFailed,
			JobsOutstanding:    t.JobsOutstanding,
			PlannedBytes:       v1.Int64String(strconv.FormatInt(t.PlannedBytes, 10)),
			BytesTransferred:   v1.Int64String(strconv.FormatInt(t.BytesTransferred, 10)),
			DedupeSkippedBytes: v1.Int64String(strconv.FormatInt(t.DedupeSkippedBytes, 10)),
		},
		FailureReason: t.FailureReason,
		CreatedAt:     t.CreatedAt,
		CompletedAt:   t.CompletedAt,
	}
}

// parseTransferState validates the state filter against the closed set.
//
// Checked rather than passed through: an unknown state would silently match
// nothing, and an empty listing is indistinguishable from a mistyped filter.
func parseTransferState(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	got := strings.ToLower(s)
	for _, valid := range []string{
		"pending", "planning", "ready", "running", "paused",
		"verifying", "succeeded", "failed", "cancelling", "cancelled",
	} {
		if got == valid {
			return got, nil
		}
	}
	return "", fmt.Errorf(
		"state %q is not a transfer state: expected one of pending, planning, ready, "+
			"running, paused, verifying, succeeded, failed, cancelling, cancelled", s)
}
