package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The worker plane: lease, progress, complete, heartbeat.
//
// See docs/design/09-api.md §7. These four routes are the whole of what a
// worker may call, and the reason workers hold no database credentials: every
// query behind them runs here, on the Coordinator, which is the sole writer.
//
// The handlers do what every handler in this package does and nothing more -
// parse, call a domain package, serialize. The queue decides what a lease
// means; this decides what it looks like over HTTP.

// maxLeaseCapacity bounds what one worker may ask for in a single call.
//
// A worker asking for ten thousand jobs is either misconfigured or hostile,
// and either way the answer is the same: the queue would hand them over, the
// leases would all expire together, and the reaper would have a very bad
// thirty seconds. The cap is generous relative to a real worker's concurrency
// (docs/design/05 §8) and small relative to a mistake.
const maxLeaseCapacity = 256

// handleLeaseJobs serves POST /api/v1/jobs:lease.
func (s *Server) handleLeaseJobs(w http.ResponseWriter, r *http.Request) {
	var req v1.LeaseRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.WorkerID == "" {
		Error(w, r, v1.CodeInvalidArgument, "workerId is required")
		return
	}
	if req.Capacity <= 0 {
		Error(w, r, v1.CodeInvalidArgument, "capacity must be greater than zero")
		return
	}
	if req.Capacity > maxLeaseCapacity {
		req.Capacity = maxLeaseCapacity
	}

	// Recorded before the lease, so a worker that asks for work and gets none
	// still appears in the fleet. An idle worker missing from `health` is the
	// case somebody is most likely to be looking for.
	s.deps.Queue.RecordWorker(r.Context(), req.WorkerID, req.Capacity, req.ActiveJobs, req.Version)

	res, err := s.deps.Queue.Lease(r.Context(), req.WorkerID, req.Capacity, req.ActiveJobs)
	if err != nil {
		s.deps.Logger.ErrorContext(r.Context(), "lease failed",
			"worker", req.WorkerID, "error", err)
		Error(w, r, v1.CodeUnavailable, "could not lease work: "+err.Error())
		return
	}

	out := v1.LeaseResponse{
		Jobs:                 make([]v1.LeasedJob, 0, len(res.Jobs)),
		LeaseDurationSeconds: int(res.LeaseDuration.Seconds()),
		NextPollAfterSeconds: int(res.NextPollAfter.Seconds()),
	}
	for _, a := range res.Jobs {
		out.Jobs = append(out.Jobs, leasedJobDTO(a))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

func leasedJobDTO(a queue.Assignment) v1.LeasedJob {
	return v1.LeasedJob{
		JobID:               strconv.FormatInt(a.ID, 10),
		TransferID:          a.TransferID,
		Kind:                a.Kind,
		Digest:              a.Digest,
		SizeBytes:           v1.Int64String(strconv.FormatInt(a.SizeBytes, 10)),
		MediaType:           a.MediaType,
		Source:              endpointDTO(a.Source),
		Target:              endpointDTO(a.Target),
		KnownPlacement:      a.KnownPlacement,
		RepairLevel:         a.RepairLevel,
		MountFromRepository: a.MountFrom,
		Tags:                a.Tags,
		TargetRepository:    a.TargetRepository,
		Attempt:             a.Attempt,
		Wave:                a.Wave,
	}
}

func endpointDTO(e store.Endpoint) v1.JobEndpoint {
	return v1.JobEndpoint{
		Product:    e.Product,
		Name:       e.Name,
		Registry:   e.Registry,
		Repository: e.Repository,
		Type:       e.RegistryType,
		Role:       e.Role,
	}
}

// handleJobCustomMethod dispatches :reportProgress and :complete.
//
// The verb is split off HERE rather than by the router, for the same reason
// the package routes split theirs: chi matches a partial segment at the FIRST
// delimiter, and while a job ID is numeric today, routing that depends on that
// staying true is a trap set for whoever changes it. Splitting at the last
// colon works for any identifier.
func (s *Server) handleJobCustomMethod(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "job")

	idPart, verb, ok := splitVerb(raw)
	if !ok {
		Error(w, r, v1.CodeInvalidArgument,
			"expected /jobs/{job}:reportProgress or /jobs/{job}:complete")
		return
	}

	jobID, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, "job "+idPart+" is not a job ID")
		return
	}

	switch verb {
	case "reportProgress":
		s.reportProgress(w, r, jobID)
	case "complete":
		s.completeJob(w, r, jobID)
	default:
		Error(w, r, v1.CodeInvalidArgument, "unknown method :"+verb+" on a job")
	}
}

// splitVerb separates a resource ID from a trailing :verb.
func splitVerb(raw string) (id, verb string, ok bool) {
	i := strings.LastIndex(raw, ":")
	if i <= 0 || i == len(raw)-1 {
		return "", "", false
	}
	return raw[:i], raw[i+1:], true
}

func (s *Server) reportProgress(w http.ResponseWriter, r *http.Request, jobID int64) {
	var req v1.ProgressRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WorkerID == "" {
		Error(w, r, v1.CodeInvalidArgument, "workerId is required")
		return
	}

	bytes, err := parseInt64String(req.BytesTransferred)
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, "bytesTransferred: "+err.Error())
		return
	}

	if err := s.deps.Queue.Progress(r.Context(), jobID, req.WorkerID, bytes); err != nil {
		// A dropped progress report costs nothing - it is a UI signal, and the
		// worker is mid-transfer. Answering 503 would make a worker retry
		// something not worth retrying, so this is logged and accepted.
		s.deps.Logger.WarnContext(r.Context(), "could not record job progress",
			"job", jobID, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeJob(w http.ResponseWriter, r *http.Request, jobID int64) {
	var req v1.CompleteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WorkerID == "" {
		Error(w, r, v1.CodeInvalidArgument, "workerId is required")
		return
	}

	outcome, err := jobOutcome(req.Outcome)
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	bytes, err := parseInt64String(req.BytesTransferred)
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, "bytesTransferred: "+err.Error())
		return
	}

	res, err := s.deps.Queue.Complete(r.Context(), store.Completion{
		JobID:            jobID,
		Owner:            req.WorkerID,
		Outcome:          outcome,
		BytesTransferred: bytes,
		SkipReason:       string(req.SkipReason),
		ErrorClass:       req.ErrorClass,
		ErrorMsg:         req.ErrorMessage,
		Placed:           req.Placed,
		Attempt:          req.Attempt,
	})
	if err != nil {
		s.deps.Logger.ErrorContext(r.Context(), "could not record job completion",
			"job", jobID, "worker", req.WorkerID, "error", err)
		Error(w, r, v1.CodeUnavailable, "could not record the completion: "+err.Error())
		return
	}

	WriteJSON(w, r, http.StatusOK, v1.CompleteResponse{
		Applied:       res.Applied,
		TransferState: v1.TransferState(strings.ToUpper(res.TransferState)),
		WaveAdvanced:  res.WaveAdvanced,
		CurrentWave:   res.NewWave,
	})
}

// jobOutcome maps the API's outcome vocabulary onto the column's.
//
// A closed set, checked here rather than trusted: the jobs.state CHECK
// constraint would reject an unknown value anyway, but as a 500 naming a
// constraint rather than a 400 naming the field.
func jobOutcome(s v1.JobState) (string, error) {
	switch s {
	case v1.JobSucceeded:
		return "succeeded", nil
	case v1.JobSkipped:
		return "skipped", nil
	case v1.JobFailed:
		return "failed", nil
	case v1.JobCancelled:
		return "cancelled", nil
	default:
		return "", errors.New(
			"outcome must be SUCCEEDED, SKIPPED, FAILED or CANCELLED, not " + string(s))
	}
}

// handleWorkerHeartbeat serves POST /api/v1/workers/{worker}:heartbeat.
func (s *Server) handleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "worker")

	workerID, verb, ok := splitVerb(raw)
	if !ok || verb != "heartbeat" {
		Error(w, r, v1.CodeInvalidArgument, "expected /workers/{worker}:heartbeat")
		return
	}

	var req v1.HeartbeatRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	ids := make([]int64, 0, len(req.ActiveJobIDs))
	for _, s := range req.ActiveJobIDs {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			Error(w, r, v1.CodeInvalidArgument, "activeJobIds: "+s+" is not a job ID")
			return
		}
		ids = append(ids, id)
	}

	renewed, cancelled, err := s.deps.Queue.Heartbeat(r.Context(), workerID, ids)
	if err != nil {
		s.deps.Logger.ErrorContext(r.Context(), "heartbeat failed",
			"worker", workerID, "error", err)
		Error(w, r, v1.CodeUnavailable, "could not renew leases: "+err.Error())
		return
	}

	out := v1.HeartbeatResponse{LeasesRenewed: make([]string, 0, len(renewed))}
	for _, id := range renewed {
		out.LeasesRenewed = append(out.LeasesRenewed, strconv.FormatInt(id, 10))
	}
	// The field was specified and never filled in, so `stop` reached the queue
	// and never reached the worker: a job already in flight kept its lease
	// renewed and ran to completion, and the transfer sat in `cancelling` for
	// however long its largest blob took.
	for _, id := range cancelled {
		out.CancelledJobIDs = append(out.CancelledJobIDs, strconv.FormatInt(id, 10))
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Shared decoding
// ---------------------------------------------------------------------------

// decodeJSON reads a request body, reporting failure to the client itself.
//
// Returns false when it has already written a response, so a handler reads as
// `if !decodeJSON(...) { return }` rather than repeating the error shape at
// every call site.
func decodeJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	// Unknown fields are rejected rather than ignored: a worker sending
	// `bytesTransfered` should be told, not silently reported as having moved
	// nothing.
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		Error(w, r, v1.CodeInvalidArgument, "could not read the request body: "+err.Error())
		return false
	}
	return true
}

// maxRequestBody bounds a worker-plane request. These bodies are a few hundred
// bytes; a megabyte is room for a worker holding hundreds of jobs.
const maxRequestBody = 1 << 20

// parseInt64String reads AIP-141's stringified integer.
//
// Empty is zero rather than an error: a completion that moved no bytes may
// legitimately omit the field, and forcing "0" onto every skip would be
// ceremony.
func parseInt64String(v v1.Int64String) (int64, error) {
	if v == "" {
		return 0, nil
	}
	return strconv.ParseInt(string(v), 10, 64)
}
