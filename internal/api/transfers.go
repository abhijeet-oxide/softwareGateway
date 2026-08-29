package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/api/middleware"
	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Transfer routes. See docs/design/09-api.md §2.
//
// Create and the read routes. The custom methods - retry, pause, resume, stop -
// live in retry.go, which owns the verb split. `setPriority` is specified and
// not built, so it is absent rather than present and inert: a route that accepts
// a request and does nothing is worse than a 404, because the 404 is believed.

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

	// 201 for a new request, 200 for a replay of one that already existed -
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
// fault - and the messages already say which flag settles it, so they are
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
	operation, err := parseOperation(q.Get("operation"))
	if err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	// One row over the page size, so "is there another page" is answered
	// without a second COUNT and without claiming a page that turns out empty.
	rows, err := s.deps.Packages.ListTransfers(r.Context(), store.ListTransfersFilter{
		ProductName: q.Get("product"),
		State:       state,
		Operation:   operation,
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

// handleTransferActivity serves GET /api/v1/transfers:activity.
//
// The shell's one line, answered by the database rather than assembled in the
// browser from a page of transfers it did not want. See
// v1.TransferActivityResponse for the measurement that justifies its existence.
func (s *Server) handleTransferActivity(w http.ResponseWriter, r *http.Request) {
	a, err := s.deps.Packages.Activity(r.Context())
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not summarise activity: "+err.Error())
		return
	}
	WriteJSON(w, r, http.StatusOK, v1.TransferActivityResponse{
		Moving: a.Moving, Held: a.Held, Failed: a.Failed,
	})
}

// handleGetTransfer serves GET /api/v1/transfers/{transfer}.
func (s *Server) handleGetTransfer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "transfer")

	t, err := s.deps.Packages.GetTransfer(r.Context(), id)
	if err != nil {
		NotFound(w, r, "transfer", id)
		return
	}

	dto := transferDTO(t)
	// Only on the single-transfer read. A failure to group the waves does not
	// fail the request: it is a breakdown of numbers already in the response,
	// and losing it must not cost the reader the response.
	if waves, err := s.deps.Packages.WaveProgress(r.Context(), t.ID); err == nil {
		dto.Waves = toAPIWaves(waves)
	}

	if content, err := s.deps.Packages.ContentBreakdown(r.Context(), t.ID); err == nil {
		// The transfer's own product, so its vendor layout gets the same say
		// here that it gets on the release page. Without it a NEAR orb's
		// charts and files are indistinguishable from its images, and this
		// page said `image 260` beside a release page saying 160 images,
		// 97 charts and 2 files.
		dto.Content = toAPIContent(content, s.artifactClassifier(t.ProductName))
	}

	// The byte account over DISTINCT content, which is what a bar is drawn
	// from. See store.TransferContentBytes for why the per-repository figures
	// are the wrong axis for bytes.
	if c, err := s.deps.Packages.TransferContentBytes(r.Context(), t.ID); err == nil {
		if c.Total > 0 {
			dto.Progress.ContentBytes = int64String(c.Total)
		}
		dto.Progress.ContentMovedBytes = int64String(c.Moved)
		dto.Progress.ContentPresentBytes = int64String(c.Present)
	}

	// A promotion the registry carried out itself. Present only on those, and
	// on those it is the only honest progress the transfer has: it moved no
	// bytes, so every byte column above is structurally zero and a client
	// drawing a percentage from them would be inventing one.
	if s.deps.PromotionStore != nil {
		if pm, err := s.deps.PromotionStore.ForTransfer(r.Context(), t.ID); err == nil {
			dto.Promotion = promotionProgressDTO(pm)
		}
	}

	if skips, err := s.deps.Packages.SkipBreakdown(r.Context(), t.ID); err == nil {
		for _, k := range skips {
			dto.Progress.Skips = append(dto.Progress.Skips, v1.SkipBreakdown{
				Reason: k.Reason, Jobs: k.Jobs,
				Bytes:   v1.Int64String(strconv.FormatInt(k.Bytes, 10)),
				Trusted: k.Trusted,
			})
		}
	}
	WriteJSON(w, r, http.StatusOK, dto)
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
		PackageName: t.PackageName,
		PackageID:   strconv.FormatInt(t.PackageID, 10),
		Tag:         t.Tag,
		DisplayTag:  t.DisplayTag,
		Source:      t.Source,
		Target:      t.Target,
		SourceName:  t.SourceName,
		TargetName:  t.TargetName,
		State:       v1.TransferState(strings.ToUpper(t.State)),
		Operation:   strings.ToUpper(t.Operation),
		Strategy:    t.Strategy,
		Priority:    t.Priority,
		CurrentWave: t.CurrentWave,
		MaxWave:     t.MaxWave,
		Progress: v1.TransferProgress{
			JobsPlanned:        t.PlannedJobs,
			JobsDone:           t.JobsDone,
			JobsFailed:         t.JobsFailed,
			JobsBlocked:        t.JobsBlocked,
			JobsRepaired:       t.JobsRepaired,
			OutstandingBytes:   v1.Int64String(strconv.FormatInt(t.OutstandingBytes, 10)),
			QuietestInFlight:   t.QuietestInFlight,
			JobsOutstanding:    t.JobsOutstanding,
			JobsInFlight:       t.JobsInFlight,
			Workers:            t.Workers,
			JobsWaiting:        t.JobsWaiting,
			ContentBytes:       v1.Int64String(strconv.FormatInt(t.ContentBytes, 10)),
			PlannedBytes:       v1.Int64String(strconv.FormatInt(t.PlannedBytes, 10)),
			BytesTransferred:   v1.Int64String(strconv.FormatInt(t.BytesTransferred, 10)),
			DedupeSkippedBytes: v1.Int64String(strconv.FormatInt(t.DedupeSkippedBytes, 10)),
			SkippedBytes:       v1.Int64String(strconv.FormatInt(t.SkippedBytes, 10)),
			SavedBytes:         v1.Int64String(strconv.FormatInt(t.SavedBytes(), 10)),
		},
		FailureReason: t.FailureReason,
		CreatedAt:     t.CreatedAt,
		StartedAt:     t.StartedAt,
		CompletedAt:   t.CompletedAt,
		ActiveSeconds: activeSeconds(t),
	}
}

// activeSeconds is the stored accrual plus whatever this instant is owed.
//
// # Why the remainder is added here rather than accrued more often
//
// The sweep runs on the reaper's interval - half a minute - and a page watching
// a live download polls every two seconds. Serving the stored figure alone
// would show a number that sits still for thirty polls and then jumps, which
// reads as a stuck page rather than as a coarse measurement.
//
// The remainder is only owed when a worker is holding one of this transfer's
// jobs RIGHT NOW. With nothing in flight, no time is being spent and none is
// added - which is what keeps a queued download from accumulating "time spent
// downloading" while it waits.
//
// # Why it is capped
//
// The same reason the sweep refuses a long gap: a stale anchor means the sweep
// has not run, and the honest thing to say about a period nobody measured is
// nothing. Beyond the cap the stored figure is served unchanged and the next
// sweep re-anchors.
func activeSeconds(t store.TransferSummary) float64 {
	seconds := t.ActiveSeconds
	if t.JobsInFlight > 0 && t.LastActiveAt != "" {
		if since, ok := secondsSince(t.LastActiveAt); ok && since <= maxActiveRemainder {
			seconds += since
		}
	}

	// NEVER LARGER THAN THE WALL CLOCK. The two are measured by different
	// mechanisms - a sampling sweep and two timestamps - and a rounding that
	// let the active figure exceed the elapsed one would put "took 4m 02s of
	// the 4m 01s it has existed" on the page, which reads as a bug in
	// everything around it.
	if wall, ok := wallClockSeconds(t); ok && seconds > wall {
		return wall
	}
	return seconds
}

// maxActiveRemainder bounds what one response will add to the stored figure.
//
// Comfortably more than a reap interval so an ordinary sweep delay is invisible,
// and far less than any outage worth excluding.
var maxActiveRemainder = (5 * time.Minute).Seconds()

func secondsSince(ts string) (float64, bool) {
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0, false
	}
	d := time.Since(at).Seconds()
	if d < 0 {
		// The Coordinator's clock and the database's disagree. Adding a
		// negative would take time OFF a measurement, so nothing is added.
		return 0, false
	}
	return d, true
}

// wallClockSeconds is how long the transfer has existed as a running thing -
// the number active time is a subset of.
func wallClockSeconds(t store.TransferSummary) (float64, bool) {
	start, err := time.Parse(time.RFC3339Nano, t.StartedAt)
	if err != nil {
		return 0, false
	}
	end := time.Now()
	if t.CompletedAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, t.CompletedAt); err == nil {
			end = parsed
		}
	}
	d := end.Sub(start).Seconds()
	return d, d >= 0
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
	// Every state the machine defines, not a subset. A filter that cannot
	// express a state the listing RETURNS is a filter with a hole in it, and
	// the holes were the delegated and promoted ones - exactly the transfers
	// somebody is most likely to go looking for on their own.
	valid := []string{
		"waiting", "pending", "planning", "ready", "running", "paused",
		"syncing", "promoting", "verifying", "succeeded", "diverged",
		"skipped", "failed", "cancelling", "cancelled",
	}
	if slices.Contains(valid, got) {
		return got, nil
	}
	return "", fmt.Errorf("state %q is not a transfer state: expected one of %s",
		s, strings.Join(valid, ", "))
}

// parseOperation narrows a listing to downloads or to promotions.
//
// Two values and no third: `verify` is an operation the schema allows and
// nothing creates, so accepting it here would offer a filter that always
// returns nothing.
func parseOperation(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	got := strings.ToLower(s)
	if got == "replicate" || got == "promote" {
		return got, nil
	}
	return "", fmt.Errorf(
		"operation %q is not a transfer operation: expected replicate or promote", s)
}

// handleListPresentComponents serves
// GET /api/v1/transfers/{transfer}/present.
//
// WHAT the destination already held, by name.
//
// Its own route rather than a field on the transfer, because the transfer is
// POLLED - every two seconds while a download runs - and this is two hundred
// and sixty rows that change only when a job settles. A caller fetches it when
// somebody actually asks what the saving was made of.
func (s *Server) handleListPresentComponents(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "transfer")

	id, err := s.deps.Packages.ResolveTransferID(r.Context(), ref)
	if err != nil {
		NotFound(w, r, "transfer", ref)
		return
	}

	rows, err := s.deps.Packages.PresentComponents(r.Context(), id)
	if err != nil {
		Error(w, r, v1.CodeUnavailable, "could not list what was already there: "+err.Error())
		return
	}

	// The transfer's own product, so its vendor layout gets the same say here
	// that it gets everywhere else. Without it a NEAR orb's charts and files
	// are reported as images, and this list would disagree with the Contents
	// table it is explaining.
	classify := vendors.Classifier(vendors.OCIOnly)
	if t, err := s.deps.Packages.GetTransfer(r.Context(), id); err == nil {
		classify = s.artifactClassifier(t.ProductName)
	}

	out := v1.ListPresentComponentsResponse{
		TransferID: id,
		Components: make([]v1.PresentComponent, 0, len(rows)),
	}
	var total int64
	for _, c := range rows {
		total += c.Bytes
		out.Components = append(out.Components, v1.PresentComponent{
			Name:    c.Name,
			Digest:  c.Digest,
			Kind:    classify(c.MediaType, c.ArtifactType, c.ConfigMediaType, c.Annotations),
			Bytes:   int64String(c.Bytes),
			Partial: c.Outstanding > 0,
		})
	}
	out.TotalBytes = int64String(total)

	WriteJSON(w, r, http.StatusOK, out)
}

// toAPIContent folds the store's rows into the kinds a person names things by.
//
// The FOLD is the point. The store returns media types verbatim, and several of
// them are one kind: `application/vnd.oci.image.manifest.v1+json` and
// `application/vnd.docker.distribution.manifest.v2+json` are both an image, and
// a reader asked to add them up has been handed the tool's internals instead of
// an answer. Naming them is protocol knowledge, so it comes from the one place
// that holds it - the same function the comparison classifies with.
//
// classify is passed in rather than being oci.Classify directly, because the
// OCI fields are not always enough: a vendor whose charts are plain image
// manifests has said so in its annotations, and only the product's layout
// plugin can read that. The artifact listing already classifies this way, and
// the two MUST agree - a transfer of a release breaks down into the same
// things the release is made of.
func toAPIContent(rows []store.ContentRow, classify vendors.Classifier) []v1.ContentGroup {
	if classify == nil {
		classify = vendors.OCIOnly
	}

	byKind := map[string]*v1.ContentGroup{}
	// Bytes accumulate as numbers and are rendered once at the end: Int64String
	// is a wire format, not something to do arithmetic in.
	saved, copied := map[string]int64{}, map[string]int64{}

	for _, row := range rows {
		kind := classify(row.MediaType, row.ArtifactType, row.ConfigMediaType, row.Annotations)
		group, ok := byKind[kind]
		if !ok {
			group = &v1.ContentGroup{Kind: kind}
			byKind[kind] = group
		}

		// Summed across every OUTCOME of this kind, not only the skipped one: a
		// component reported as copied may still have had blobs the destination
		// already held, and those bytes were saved just the same.
		saved[kind] += row.SavedBytes
		copied[kind] += row.CopiedBytes

		// The layers, configs and manifests beneath them, summed across every
		// outcome: a component reported as outstanding usually has most of its
		// units already at the destination, and that is exactly the progress
		// the component counts cannot express.
		group.Units += row.Units
		group.UnitsCopied += row.UnitsCopied
		group.UnitsPresent += row.UnitsPresent
		group.UnitsFailed += row.UnitsFailed
		group.UnitsOutstanding += row.UnitsOutstanding

		// FILES, on the kind where a component is not one.
		//
		// Set here rather than on every kind because that is the only place it
		// is a different question: an image's layers are not files anybody
		// names, and reporting a count of them as `files` would put a second
		// unit into a column that already has one. A file bundle IS the case
		// where the two diverge - one component, a hundred and twelve named
		// layers - and the release page has counted the layers since it learnt
		// to list them.
		if kind == oci.KindFile {
			group.Files += row.NamedFiles
		}

		group.Total += row.Count
		switch row.Outcome {
		case store.ContentCopied:
			group.Copied += row.Count
		case store.ContentPresent:
			group.Present += row.Count
		case store.ContentFailed:
			group.Failed += row.Count
		default:
			group.Outstanding += row.Count
		}
	}

	out := make([]v1.ContentGroup, 0, len(byKind))
	for kind, group := range byKind {
		group.SavedBytes = int64String(saved[kind])
		group.CopiedBytes = int64String(copied[kind])
		out = append(out, *group)
	}
	// Structural first, and FIXED rather than by count: a reader comparing two
	// transfers compares rows by position, which a table that reorders when a
	// count changes makes impossible.
	sort.Slice(out, func(i, j int) bool {
		return oci.RankOf(out[i].Kind) < oci.RankOf(out[j].Kind)
	})
	return out
}

// toAPIWaves renders the per-wave breakdown, marking the waves in progress.
//
// PLURAL, and derived from the jobs rather than from transfers.current_wave.
// That column is a watermark - the highest wave the drain check has opened -
// and it stopped being a description of what is happening the moment two other
// things became true: per-artifact readiness lets a manifest run before its
// wave opens (docs/design/04 §3.5), and a repair sends wave-0 blobs back after
// the watermark has moved past them (docs/design/11 §2.5).
//
// So a transfer legitimately has work running in wave 0 and wave 1 at once
// while the watermark reads 1. Marking only the watermark told the reader that
// wave 0 was finished while thirteen of its jobs were visibly running two lines
// below, which is a table disagreeing with itself.
func toAPIWaves(waves []store.WaveSummary) []v1.TransferWave {
	out := make([]v1.TransferWave, 0, len(waves))
	for _, w := range waves {
		out = append(out, v1.TransferWave{
			Wave: w.Wave, Kind: w.Kind,
			// In progress means something in it can move NOW: running, or
			// runnable and waiting for a worker.
			Current: w.Running > 0 || w.Pending > 0,
			Total:   w.Total, Done: w.Done, Running: w.Running,
			Pending: w.Pending, Waiting: w.Waiting,
			Blocked: w.Blocked, Failed: w.Failed,
			PlannedBytes:     v1.Int64String(strconv.FormatInt(w.PlannedBytes, 10)),
			TransferredBytes: v1.Int64String(strconv.FormatInt(w.TransferredBytes, 10)),
		})
	}
	return out
}
