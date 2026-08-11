package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The retry custom method: `POST /api/v1/transfers/{transfer}:retry` and the
// fleet-wide `POST /api/v1/transfers:retry`.
//
// # Why the collection form exists
//
// An outage does not fail one transfer. It fails every transfer that was
// running, and the operator's intent afterwards is one thing — "carry on with
// what the network interrupted" — not N things. Making them copy IDs out of a
// listing to express that would be busywork whose only effect is that some get
// missed.
//
// It is scoped rather than open-ended: it acts on transfers that HAVE failed
// jobs, which is the set an outage produces. It cannot start a transfer that
// was cancelled, and it cannot resurrect one that succeeded.

// handleTransferCustomMethod dispatches the verbs on one transfer.
//
// The verb is split here rather than by the router for the reason given on
// handleJobCustomMethod: chi matches a partial segment at the FIRST delimiter,
// and routing that depends on an identifier never containing a colon is a trap
// set for whoever changes the identifier.
func (s *Server) handleTransferCustomMethod(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "transfer")

	id, verb, ok := splitVerb(raw)
	if !ok {
		Error(w, r, v1.CodeInvalidArgument, "expected /transfers/{transfer}:retry")
		return
	}

	switch verb {
	case "retry":
		s.retryTransfer(w, r, id)
	default:
		// Named explicitly, because the specified-but-unbuilt verbs are the
		// ones somebody will reach for first and a bare "unknown method" would
		// read as a typo in the one they typed.
		if isPlannedVerb(verb) {
			Error(w, r, v1.CodeUnavailable,
				":"+verb+" is specified but not built yet; :retry is available")
			return
		}
		Error(w, r, v1.CodeInvalidArgument, "unknown method :"+verb+" on a transfer")
	}
}

func isPlannedVerb(verb string) bool {
	switch verb {
	case "pause", "resume", "cancel", "setPriority":
		return true
	default:
		return false
	}
}

func (s *Server) retryTransfer(w http.ResponseWriter, r *http.Request, ref string) {
	if s.deps.Queue == nil {
		Error(w, r, v1.CodeUnavailable, "this Coordinator has no queue, so it cannot requeue work")
		return
	}

	id, err := s.deps.Packages.ResolveTransferID(r.Context(), ref)
	if err != nil {
		NotFound(w, r, "transfer", ref)
		return
	}

	res, err := s.deps.Queue.Retry(r.Context(), id)
	if err != nil {
		// A retry refused because the transfer is settled is a CONFLICT rather
		// than an error: the caller asked for something the transfer's state
		// cannot satisfy, and the state is the answer (docs/design/09 §5).
		Error(w, r, v1.CodeFailedPrecondition, err.Error())
		return
	}

	out := v1.RetryTransferResponse{
		Transfers: []v1.TransferRetry{retryDTO(res)},
		Requeued:  res.Requeued,
	}
	WriteJSON(w, r, http.StatusOK, out)
}

// handleRetryTransfers serves POST /api/v1/transfers:retry — every transfer
// with failed jobs.
func (s *Server) handleRetryTransfers(w http.ResponseWriter, r *http.Request) {
	if s.deps.Queue == nil {
		Error(w, r, v1.CodeUnavailable, "this Coordinator has no queue, so it cannot requeue work")
		return
	}

	ids, err := s.deps.Packages.RetryableTransfers(r.Context())
	if err != nil {
		Error(w, r, v1.CodeInternal, err.Error())
		return
	}

	out := v1.RetryTransferResponse{Transfers: []v1.TransferRetry{}}
	for _, id := range ids {
		res, err := s.deps.Queue.Retry(r.Context(), id)
		if err != nil {
			// One transfer that cannot be retried must not abandon the rest:
			// after an outage the caller wants everything restartable
			// restarted, and a partial result naming the exception is more
			// useful than a failure naming only the first problem.
			out.Transfers = append(out.Transfers, v1.TransferRetry{
				TransferID: id, Error: err.Error(),
			})
			continue
		}
		out.Transfers = append(out.Transfers, retryDTO(res))
		out.Requeued += res.Requeued
	}
	WriteJSON(w, r, http.StatusOK, out)
}

func retryDTO(res store.RetryResult) v1.TransferRetry {
	out := v1.TransferRetry{
		TransferID: res.TransferID,
		Requeued:   res.Requeued,
		State:      res.State,
	}
	if res.NoJobs {
		// The distinction the caller cannot make from the state alone: this
		// transfer failed before it had any work, so there is nothing to
		// requeue and re-creating it is the only way forward.
		out.Error = "this transfer failed before any job was planned, so there is nothing " +
			"to requeue — create it again"
	}
	return out
}
