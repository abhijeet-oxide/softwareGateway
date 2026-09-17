package api

import (
	"net/http"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The windows a caller may ask for.
//
// A fixed set rather than a free duration, because every one of these is a
// question somebody actually has - since yesterday, since last week, since
// last month - and an arbitrary window is an invitation to ask for a year from
// a record that keeps a month and be quietly given the month.
var availabilityWindows = map[string]time.Duration{
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

const defaultAvailabilityWindow = "24h"

// handleGetAvailability serves GET /api/v1/system/availability.
//
// # What this answers, and why the product owes an answer
//
// "Was the gateway up, and how often has it not been?" Until this existed the
// product could not say. A metrics stack holds `up` for whoever has a
// dashboard open and knows what to type into it; the page somebody actually
// opens had nothing, and the only availability claim the interface made was a
// browser INFERRING an outage from one endpoint's error - which it did, wrongly
// and repeatedly, because that is what an interface does when it has to guess
// at a fact nobody is recording.
//
// # Read-only, and cheap
//
// One indexed range scan over a table with a row per restart rather than a row
// per beat, folded in memory. It is safe to poll and safe to open on the worst
// day the deployment has, which is the day it matters.
func (s *Server) handleGetAvailability(w http.ResponseWriter, r *http.Request) {
	if s.deps.Availability == nil {
		Error(w, r, v1.CodeUnavailable, "this Coordinator does not record its own availability")
		return
	}

	key := r.URL.Query().Get("window")
	if key == "" {
		key = defaultAvailabilityWindow
	}
	window, ok := availabilityWindows[key]
	if !ok {
		Error(w, r, v1.CodeInvalidArgument,
			"window must be one of 1h, 24h, 7d or 30d")
		return
	}

	component := s.deps.Component
	if component == "" {
		component = "coordinator"
	}

	runs, err := s.deps.Availability.Runs(r.Context(), component, window)
	if err != nil {
		s.fault(w, r, "could not read the availability record", err)
		return
	}
	first, err := s.deps.Availability.FirstRecorded(r.Context(), component)
	if err != nil {
		s.fault(w, r, "could not read the start of the availability record", err)
		return
	}

	summary := store.SummariseAvailability(
		runs, window, store.AvailabilityContinuity, first, time.Now().UTC())

	WriteJSON(w, r, http.StatusOK, availabilityResponse(summary))
}

func availabilityResponse(s store.AvailabilitySummary) v1.AvailabilityResponse {
	out := v1.AvailabilityResponse{
		Since:           s.Since.Format(time.RFC3339),
		Now:             s.Now.Format(time.RFC3339),
		Status:          apiAvailabilityStatus(s.Status),
		StatusSince:     s.CurrentSince.Format(time.RFC3339),
		WindowSeconds:   int64(s.WindowSeconds()),
		UpSeconds:       int64(s.UpSeconds),
		DegradedSeconds: int64(s.DegradedSeconds),
		DownSeconds:     int64(s.DownSeconds),
		Uptime:          s.Uptime(),
		// Never nil: an empty list is "no outages", and a null is a client
		// deciding for itself what that means.
		Outages:     make([]v1.Outage, 0, len(s.Outages)),
		Starts:      s.Starts,
		BeatSeconds: int64(store.DefaultAvailabilityInterval.Seconds()),
	}
	if !s.RecordedFrom.IsZero() {
		out.RecordedFrom = s.RecordedFrom.Format(time.RFC3339)
	}
	for _, o := range s.Outages {
		out.Outages = append(out.Outages, v1.Outage{
			Began:   o.Began.Format(time.RFC3339),
			Ended:   o.Ended.Format(time.RFC3339),
			Seconds: int64(o.Seconds()),
			Ongoing: o.Ongoing,
		})
	}
	return out
}

func apiAvailabilityStatus(s store.AvailabilityStatus) v1.AvailabilityStatus {
	switch s {
	case store.AvailabilityHealthy:
		return v1.AvailabilityHealthy
	case store.AvailabilityDegraded:
		return v1.AvailabilityDegraded
	case store.AvailabilityDown:
		return v1.AvailabilityDown
	default:
		return v1.AvailabilityUnknown
	}
}
