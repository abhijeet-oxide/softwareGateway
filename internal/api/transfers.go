package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Transfer routes. See docs/design/09-api.md §2.
//
// Create and the read routes. Pause, resume, cancel, retry and setPriority are
// specified but not built, so they are absent rather than present and inert. A
// route that accepts a pause and does nothing is worse than a 404, because the
// 404 is believed.

// Requests creates transfer requests.
//
// A consumer-defined interface: the API needs the one call, not the resolver,
// the catalog and the planner behind it (docs/design/15 §6).
type Requests interface {
	Create(ctx context.Context, req transfer.CreateRequest) (transfer.CreateResult, error)
}

// handleCreateTransfer serves POST /api/v1/transfers.
func (s *Server) handleCreateTransfer(w http.ResponseWriter, r *http.Request) {
	var req v1.CreateTransferRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Product == "" || req.Package == "" {
		Error(w, r, v1.CodeInvalidArgument, "product and package are both required")
		return
	}
	if len(req.To) > 0 && req.ToEnvironment != "" {
		Error(w, r, v1.CodeInvalidArgument,
			"to and toEnvironment name destinations two different ways; use one")
		return
	}

	// The package is resolved HERE so a bad reference fails as NOT_FOUND
	// naming what was looked for, rather than as a resolution error from
	// deeper down that names a row ID nobody typed.
	pkg, err := s.deps.Packages.GetPackage(r.Context(), req.Product, req.Package)
	if err != nil {
		NotFound(w, r, "package", req.Package)
		return
	}

	create := transfer.CreateRequest{
		Product:       req.Product,
		Package:       req.Package,
		Row:           pkg,
		From:          req.From,
		To:            req.To,
		ToEnvironment: req.ToEnvironment,
		Promote:       req.Promote,
		Priority:      req.Priority,
		RequestedBy:   middleware.IdentityFrom(r.Context()).Subject,
		Origin:        "api",
		ValidateOnly:  req.ValidateOnly,
	}

	res, err := s.deps.Requests.Create(r.Context(), create)
	if err != nil {
		requestError(w, r, err)
		return
	}

	// 201 for a new request, 200 for a replay of one that already existed —
	// the distinction docs/design/04 §7 asks for, so a caller can tell "I made
	// this" from "this was already asked for". A dry run creates nothing, so
	// it is always 200.
	status := http.StatusCreated
	if !res.Created || req.ValidateOnly {
		status = http.StatusOK
	}
	WriteJSON(w, r, status, createTransferDTO(res))
}

// requestError maps a resolution failure onto the error model.
//
// Nearly every failure here is the caller naming something that does not
// exist or cannot be used, which is INVALID_ARGUMENT rather than a server
// fault — and the messages already say which flag settles it, so they are
// passed through rather than replaced.
func requestError(w http.ResponseWriter, r *http.Request, err error) {
	Error(w, r, v1.CodeInvalidArgument, err.Error())
}

func createTransferDTO(res transfer.CreateResult) v1.CreateTransferResponse {
	out := v1.CreateTransferResponse{
		RequestID:   res.RequestID,
		Created:     res.Created,
		Operation:   strings.ToUpper(res.Operation),
		From:        endpointViewDTO(res.Origin),
		TransferIDs: res.TransferIDs,
	}
	for _, t := range res.Targets {
		out.To = append(out.To, endpointViewDTO(t))
	}
	return out
}

func endpointViewDTO(v transfer.RepoView) v1.TransferEndpoint {
	return v1.TransferEndpoint{
		Name:        v.Name,
		Role:        v.Role,
		Environment: v.Environment,
		Registry:    v.Registry,
		Repository:  v.Repository,
	}
}

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

	state, err := parseJobState(r.URL.Query().Get("state"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	jobs, err := s.deps.Packages.ListJobs(r.Context(), id, state, pageSize)
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not list jobs: "+err.Error())
		return
	}

	out := v1.ListJobsResponse{TransferID: id, Jobs: make([]v1.Job, 0, len(jobs))}
	for _, j := range jobs {
		var parent *v1.JobParent
		if j.ParentDigest != "" {
			parent = &v1.JobParent{
				Digest: j.ParentDigest, MediaType: j.ParentMediaType,
				Ref: j.ParentRef, Shared: j.ParentShared,
			}
		}
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
			SourceRepository: j.SourceRepository,
			TargetRepository: j.TargetRepository,
			TargetTags:       j.TargetTags,
			Parent:           parent,
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
		DisplayTag:  t.DisplayTag,
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
			JobsInFlight:       t.JobsInFlight,
			Workers:            t.Workers,
			JobsWaiting:        t.JobsWaiting,
			PlannedBytes:       v1.Int64String(strconv.FormatInt(t.PlannedBytes, 10)),
			BytesTransferred:   v1.Int64String(strconv.FormatInt(t.BytesTransferred, 10)),
			DedupeSkippedBytes: v1.Int64String(strconv.FormatInt(t.DedupeSkippedBytes, 10)),
		},
		FailureReason: t.FailureReason,
		CreatedAt:     t.CreatedAt,
		StartedAt:     t.StartedAt,
		CompletedAt:   t.CompletedAt,
	}
}

// parseJobState validates the job state filter against the closed set.
//
// Checked rather than passed through, for the same reason as the transfer
// state: an unknown value would match nothing, and an empty listing is
// indistinguishable from a mistyped filter.
func parseJobState(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	got := strings.ToLower(s)
	for _, valid := range []string{
		"blocked", "pending", "leased", "succeeded", "skipped", "failed", "cancelled",
	} {
		if got == valid {
			return got, nil
		}
	}
	return "", fmt.Errorf(
		"state %q is not a job state: expected one of blocked, pending, leased, "+
			"succeeded, skipped, failed, cancelled", s)
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
