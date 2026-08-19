package api

import (
	"net/http"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// GET /api/v1/workers - the fleet.
//
// # Why this is worth its own route rather than a field on health
//
// It is on health TOO, because "are the workers up?" is the first question of
// any incident and the answer belongs where somebody already looks. It is a
// resource as well because the fleet outlives that question: how many workers,
// which build each is running, how much capacity each has and how much of it is
// in use are facts a listing answers and a health verdict does not.
//
// Read-only, and served by any replica: a worker row is observability. Nothing
// in the queue reads it, and crash recovery remains the reaper and a timestamp
// on the job (docs/design/04 §4.3).

func (s *Server) handleListWorkers(w http.ResponseWriter, r *http.Request) {
	if s.deps.Queue == nil {
		Error(w, r, v1.CodeUnavailable, "this Coordinator has no queue, so it has no workers")
		return
	}

	workers, err := s.deps.Queue.Workers(r.Context())
	if err != nil {
		Error(w, r, v1.CodeInternal, err.Error())
		return
	}
	WriteJSON(w, r, http.StatusOK, v1.ListWorkersResponse{Workers: toAPIWorkers(workers)})
}

func toAPIWorkers(workers []store.WorkerSummary) []v1.Worker {
	out := make([]v1.Worker, 0, len(workers))
	for _, w := range workers {
		out = append(out, v1.Worker{
			WorkerID:       w.ID,
			Version:        w.Version,
			MaxConcurrency: w.MaxConcurrency,
			ActiveJobs:     w.ActiveJobs,
			LeasedJobs:     w.LeasedJobs,
			State:          workerState(w),
			LastHeartbeat:  formatTime(w.LastHeartbeat),
		})
	}
	return out
}

// workerState is what the fleet view calls this worker.
//
// STALE beats whatever the row says, because it is derived from a timestamp
// rather than from an announcement: a worker that was killed never got to
// update its own state, and the only evidence is that it stopped talking.
func workerState(w store.WorkerSummary) string {
	if w.Stale {
		return "STALE"
	}
	switch w.State {
	case "draining":
		return "DRAINING"
	default:
		return "ACTIVE"
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
