package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/calibrate"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// handleCalibrate serves POST /api/v1/products/{product}:calibrate.
//
// An AIP-136 custom method, and one with real side effects: it reads blobs from
// the source and streams bytes into cancelled upload sessions on the target.
// Nothing is committed anywhere (see internal/calibrate), but both registries
// see genuine load, which is why this is a deliberate POST to a verb rather
// than anything a client could reach by accident.
//
// # Why it is a synchronous request
//
// It takes minutes, which normally argues for a long-running operation with a
// handle to poll. It is synchronous anyway because the result is a REPORT for a
// person, not a resource for a system: nobody calibrates in a script, nobody
// needs the run to survive the client hanging up, and a stored calibration
// would be a measurement of a network as it was on Tuesday, which is exactly
// the sort of stale number this feature exists to replace.
func (s *Server) handleCalibrate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Products == nil || s.deps.Calibrator == nil {
		Error(w, r, v1.CodeUnavailable, "calibration is not configured")
		return
	}

	name := chi.URLParam(r, "product")
	p, ok := s.deps.Products.Get(name)
	if !ok {
		NotFound(w, r, "product", name)
		return
	}

	var req v1.CalibrateRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		Error(w, r, v1.CodeInvalidArgument, "malformed request body: "+err.Error())
		return
	}

	opts := calibrate.Options{
		Source:           req.Source,
		Target:           req.Target,
		SourceRepository: req.SourceRepository,
		Levels:           req.Levels,
		// Absent means run the write probe. A calibration that measured only
		// the read half would recommend a concurrency for the wrong end of the
		// path, and the write probe leaves nothing behind.
		Write: req.Write == nil || *req.Write,
	}
	if req.BudgetSeconds > 0 {
		opts.Budget = time.Duration(req.BudgetSeconds * float64(time.Second))
	}
	if req.BundleBytes != "" {
		n, err := strconv.ParseInt(string(req.BundleBytes), 10, 64)
		if err != nil {
			Error(w, r, v1.CodeInvalidArgument, "bundleBytes: "+err.Error())
			return
		}
		opts.BundleBytes = n
	}

	rep, err := s.deps.Calibrator.Run(r.Context(), p, opts)
	if err != nil {
		// A run that could not START is the caller's problem to fix - an
		// unknown source, a target the product does not declare - so it is an
		// argument error rather than a server fault.
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	WriteJSON(w, r, http.StatusOK, toAPICalibration(rep))
}

func toAPICalibration(rep calibrate.Report) v1.CalibrateResponse {
	out := v1.CalibrateResponse{
		Product:      rep.Product,
		MeasuredFrom: rep.MeasuredFrom,
		StartedAt:    rep.StartedAt.UTC().Format(time.RFC3339Nano),
		DurationSec:  rep.Duration.Seconds(),
		Source:       toAPICalibrationSide(rep.Source),
		Target:       toAPICalibrationSide(rep.Target),
		Suggestions:  []v1.CalibrationSuggestion{},
		Notes:        rep.Notes,
	}
	for _, sg := range rep.Suggestions {
		out.Suggestions = append(out.Suggestions, v1.CalibrationSuggestion{
			Severity:  string(sg.Severity),
			Setting:   sg.Setting,
			Scope:     sg.Scope,
			Current:   sg.Current,
			Suggested: sg.Suggested,
			Evidence:  sg.Evidence,
		})
	}
	return out
}

func toAPICalibrationSide(s calibrate.SideReport) v1.CalibrationSide {
	out := v1.CalibrationSide{
		Role: s.Role, Name: s.Name, Registry: s.Registry, Repository: s.Repository,
		Route: v1.CalibrationRoute{
			Configured:      s.Route.Configured,
			ProxyInUse:      s.Route.ProxyInUse,
			DirectTested:    s.Route.DirectTested,
			DirectReachable: s.Route.DirectReachable,
			DirectDetail:    s.Route.DirectDetail,
			ProxiedRate:     s.Route.ProxiedRate,
			DirectRate:      s.Route.DirectRate,
		},
		RTTMs:         float64(s.RTT.Microseconds()) / 1000,
		Samples:       s.Samples,
		Knee:          s.Knee,
		StillClimbing: s.StillClimbing,
		Skipped:       s.Skipped,
	}
	if s.LargestSample > 0 {
		out.LargestSampleBytes = v1.Int64String(strconv.FormatInt(s.LargestSample, 10))
	}
	for _, l := range s.Levels {
		out.Levels = append(out.Levels, v1.CalibrationLevel{
			Concurrency: l.Concurrency,
			Bytes:       v1.Int64String(strconv.FormatInt(l.Bytes, 10)),
			Seconds:     l.Duration.Seconds(),
			Rate:        l.Rate,
			PerStream:   l.PerStream,
			Requests:    l.Requests,
			Errors:      l.Errors,
			Throttled:   l.Throttled,
			TTFBMs:      float64(l.TTFB.Microseconds()) / 1000,
			FirstError:  l.FirstError,
		})
	}
	return out
}
