package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Availability is the service's own record of when it was serving.
//
// # What a row is
//
// A RUN: one replica, one status, serving continuously from `began_at` until
// `until_at`. A beat extends the run it belongs to; a beat that arrives too
// late to be continuous with the last one starts a new run, and the space
// between them is an outage nobody had to write down. See migration 00056 for
// why this is intervals rather than heartbeats.
//
// # What it is not
//
// Not a metrics store, and not a replacement for one. A dashboard answers "how
// slow was it at 14:05 last Tuesday" over a hundred series; this answers "was
// it up, and how often has it not been" - the question somebody has on the
// Overview page, in the product, without knowing that a metrics stack exists
// or being able to reach it from where they are sitting.
type Availability struct {
	db      *sql.DB
	dialect Dialect
}

// NewAvailability builds the recorder and reader over a store.
func NewAvailability(s Store) *Availability {
	return &Availability{db: s.DB(), dialect: DialectFor(s.Driver())}
}

// AvailabilityStatus is what a replica was able to do while a run lasted.
type AvailabilityStatus string

const (
	// AvailabilityHealthy: serving, with every dependency it needs.
	AvailabilityHealthy AvailabilityStatus = "healthy"
	// AvailabilityDegraded: serving, with something wrong that a reader should
	// be told about - a product that would not load, a scanner that is not
	// answering. Deliberately NOT an outage: the API is up, and a page that
	// called this down would be wrong in the direction that gets an operator
	// woken for nothing.
	AvailabilityDegraded AvailabilityStatus = "degraded"
)

// DefaultAvailabilityInterval is how often a replica records that it is there.
//
// Fifteen seconds, which is the resolution of the answer: an outage shorter
// than a beat cannot be seen by anything that samples, here or in a metrics
// stack. Shorter would buy precision nobody reads a dashboard for; much longer
// and a restart stops being distinguishable from a blip.
const DefaultAvailabilityInterval = 15 * time.Second

// AvailabilityContinuity is how late a beat may be and still belong to the run
// before it.
//
// Three intervals. One would make every garbage collection, slow query or
// scheduling delay look like a restart, and a record full of phantom outages is
// one nobody trusts on the day there is a real one. Three is long enough to
// absorb that and short enough that a real stop is visible within a minute.
const AvailabilityContinuity = 3 * DefaultAvailabilityInterval

// DefaultAvailabilityRetention is how much history is kept.
//
// A month, matching what the metrics stack keeps (deploy/observability), so the
// two tell the same story about the same period rather than disagreeing at the
// edge.
const DefaultAvailabilityRetention = 31 * 24 * time.Hour

// AvailabilityRun is one continuous stretch of one replica serving.
type AvailabilityRun struct {
	Instance string
	Status   AvailabilityStatus
	Version  string
	Began    time.Time
	Until    time.Time
}

// Beat records that this instance is serving, now.
//
// `continuity` is how long a run may go unextended and still be treated as the
// same run. It is the caller's beat interval with room to spare: a beat that
// was late because a garbage collection or a slow query got in the way must
// not be filed as a restart, and one that is late because the process was not
// running must be. Everything between those two is the tolerance, and it
// belongs to whoever chose the interval rather than to this file.
//
// Returns whether a NEW run was started, which is what a caller logs: it is
// either this process starting up or this process having been away.
func (a *Availability) Beat(
	ctx context.Context,
	component, instance, version string,
	status AvailabilityStatus,
	continuity time.Duration,
) (started bool, err error) {
	now := time.Now().UTC()

	// The run this instance was last extending, if any.
	var (
		id        int64
		untilText string
		holding   string
	)
	err = a.db.QueryRowContext(ctx, a.dialect.Rewrite(`
		SELECT id, `+a.dialect.TimestampText("until_at")+`, status
		  FROM service_availability
		 WHERE component = ? AND instance_id = ?
		 ORDER BY until_at DESC
		 LIMIT 1`), component, instance).Scan(&id, &untilText, &holding)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing recorded for this instance: first start, or the first start
		// since the table was pruned.
	case err != nil:
		return false, fmt.Errorf("read the availability run of %s: %w", instance, err)
	default:
		// SAME RUN only if it is both continuous and the same status. A status
		// change ends a run and starts another, so a reader can see when the
		// degradation began rather than being told the whole day was degraded
		// because the last five minutes were.
		until, ok := parseStoredTime(untilText)
		if ok && AvailabilityStatus(holding) == status && now.Sub(until) <= continuity {
			if _, err := a.db.ExecContext(ctx, a.dialect.Rewrite(`
				UPDATE service_availability SET until_at = `+a.dialect.Now()+`
				 WHERE id = ?`), id); err != nil {
				return false, fmt.Errorf("extend the availability run of %s: %w", instance, err)
			}
			return false, nil
		}
	}

	// A new run. `began_at` is now rather than the last beat: the time between
	// them is time this process cannot account for, and claiming it would be
	// the record telling a comfortable lie about exactly the moment somebody
	// is looking into.
	if _, err := a.db.ExecContext(ctx, a.dialect.Rewrite(`
		INSERT INTO service_availability
		       (component, instance_id, status, version, began_at, until_at)
		VALUES (?, ?, ?, ?, `+a.dialect.Now()+`, `+a.dialect.Now()+`)`),
		component, instance, string(status), version); err != nil {
		return false, fmt.Errorf("start an availability run for %s: %w", instance, err)
	}
	return true, nil
}

// Runs returns every recorded run of a component that touches the last
// `window`, oldest first.
//
// A run that STARTED before the window is included and returned with its real
// start: the caller clips it. Dropping it would make a service that has been
// up for a fortnight look like a service with no history at all, which is the
// opposite of the truth and the one answer a reader must never be given.
//
// The cutoff is computed BY THE DATABASE, through the dialect, rather than
// bound as a timestamp from here. SQLite compares these columns as text, so a
// parameter in a different spelling than the one `Now()` writes - a
// nanosecond-precision RFC3339, say - compares wrong rather than failing, and
// silently returns the wrong half of the history.
func (a *Availability) Runs(ctx context.Context, component string, window time.Duration) ([]AvailabilityRun, error) {
	rows, err := a.db.QueryContext(ctx, a.dialect.Rewrite(`
		SELECT instance_id, status, version,
		       `+a.dialect.TimestampText("began_at")+`,
		       `+a.dialect.TimestampText("until_at")+`
		  FROM service_availability
		 WHERE component = ? AND until_at >= `+a.dialect.TimeAgo("?")+`
		 ORDER BY began_at`), component, int64(window.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("list availability runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []AvailabilityRun
	for rows.Next() {
		var run AvailabilityRun
		var status, began, until string
		if err := rows.Scan(&run.Instance, &status, &run.Version, &began, &until); err != nil {
			return nil, fmt.Errorf("scan an availability run: %w", err)
		}
		run.Status = AvailabilityStatus(status)
		// A row whose timestamps cannot be read is dropped rather than
		// defaulted: a zero time here would read as an outage since the epoch.
		b, okBegan := parseStoredTime(began)
		u, okUntil := parseStoredTime(until)
		if !okBegan || !okUntil {
			continue
		}
		run.Began, run.Until = b.UTC(), u.UTC()
		out = append(out, run)
	}
	return out, rows.Err()
}

// FirstRecorded is when this component first wrote anything here.
//
// The zero time means never, and that is a fact the interface has to state
// rather than paper over: a window reaching back further than this is a window
// this service knows nothing about, and reporting the unknown part as an
// outage would invent one every time the feature was deployed.
func (a *Availability) FirstRecorded(ctx context.Context, component string) (time.Time, error) {
	var at string
	err := a.db.QueryRowContext(ctx, a.dialect.Rewrite(`
		SELECT `+a.dialect.TimestampText("began_at")+`
		  FROM service_availability
		 WHERE component = ?
		 ORDER BY began_at
		 LIMIT 1`), component).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("read the start of the availability record: %w", err)
	}
	first, ok := parseStoredTime(at)
	if !ok {
		return time.Time{}, nil
	}
	return first.UTC(), nil
}

// Prune drops runs that ended before a cutoff.
//
// Cheap by construction - there is one row per restart rather than one per
// beat - but unbounded is unbounded, and a deployment that has been restarting
// every ten minutes for a year is exactly the deployment whose history nobody
// wants to page through.
func (a *Availability) Prune(ctx context.Context, component string, keep time.Duration) (int64, error) {
	res, err := a.db.ExecContext(ctx, a.dialect.Rewrite(`
		DELETE FROM service_availability
		 WHERE component = ? AND until_at < `+a.dialect.TimeAgo("?")),
		component, int64(keep.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("prune availability runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// Two statuses that are REPORTED AND NEVER STORED. A process cannot write down
// that it was not running, which is the whole reason the gaps carry meaning.
const (
	// AvailabilityDown: nothing has recorded a beat recently enough to count.
	// The service is not serving, or is serving without being able to reach
	// the database - which, to anybody using it, is the same day.
	AvailabilityDown AvailabilityStatus = "down"
	// AvailabilityUnknown: the record says nothing about this period. A
	// deployment upgraded into this feature yesterday cannot speak for last
	// week, and "unknown" is the only honest word for it. It is not "down",
	// and a screen that blurred the two would invent an outage for every new
	// deployment.
	AvailabilityUnknown AvailabilityStatus = "unknown"
)

// AvailabilityOutage is a stretch in which no replica was serving.
type AvailabilityOutage struct {
	Began time.Time
	Ended time.Time
	// Ongoing: nothing has beaten since, so this outage has no end yet.
	// `Ended` holds the moment the summary was taken.
	Ongoing bool
}

// Seconds is how long the outage lasted.
func (o AvailabilityOutage) Seconds() float64 { return o.Ended.Sub(o.Began).Seconds() }

// AvailabilitySummary is the answer to "was it up?", for one window.
type AvailabilitySummary struct {
	// Since is the start of the window the numbers describe, ALREADY CLIPPED
	// to the start of the record: a window reaching further back than the
	// service has been recording covers a period nobody can speak for, and
	// reporting it would put a fictional outage on the screen.
	Since time.Time
	Now   time.Time
	// RecordedFrom is when this component first recorded anything. Zero means
	// never, which is what a fresh deployment looks like.
	RecordedFrom time.Time

	// Status is what it is doing NOW, and CurrentSince is when that began -
	// the uptime a reader quotes, or the moment the outage started.
	Status       AvailabilityStatus
	CurrentSince time.Time

	// The window accounted for in full: Up + Degraded + Down is its length.
	UpSeconds       float64
	DegradedSeconds float64
	DownSeconds     float64

	Outages []AvailabilityOutage
	// Starts is how many times a replica began serving inside the window. A
	// rolling deployment is starts WITHOUT an outage, which is the distinction
	// worth having: "we shipped" and "it fell over" look the same in a restart
	// count and nothing like each other here.
	Starts int
}

// WindowSeconds is the period the summary accounts for.
func (s AvailabilitySummary) WindowSeconds() float64 { return s.Now.Sub(s.Since).Seconds() }

// Uptime is the fraction of the window the service was serving, degraded
// included: a degraded service answered every request it was asked.
//
// A window with no length reports 1 rather than dividing by nothing - a
// deployment whose record starts this second has not been down.
func (s AvailabilitySummary) Uptime() float64 {
	total := s.WindowSeconds()
	if total <= 0 {
		return 1
	}
	up := (s.UpSeconds + s.DegradedSeconds) / total
	if up > 1 {
		return 1
	}
	return up
}

// interval is a half-open stretch of time, used while folding runs together.
type interval struct{ from, to time.Time }

// SummariseAvailability folds the recorded runs into one answer.
//
// # Why the union, and why it is the whole point
//
// Each replica records its own runs, so availability is the UNION of them: the
// service was there whenever any replica was serving. Summing them instead
// would report two replicas as two hundred per cent uptime and - far worse - a
// rolling restart in which one replica always held the traffic would show a
// gap in each individual record and an outage the service never had.
//
// `continuity` is the same tolerance the recorder beats with. A run whose last
// beat is older than that is a run that stopped; anything inside it is a beat
// that has not landed yet, and calling that an outage would raise one every
// time a beat was a second late.
func SummariseAvailability(
	runs []AvailabilityRun,
	window, continuity time.Duration,
	recordedFrom, now time.Time,
) AvailabilitySummary {
	now = now.UTC()
	recordedFrom = recordedFrom.UTC()
	since := now.Add(-window)
	// NOTHING IS REPORTED BEFORE THE RECORD BEGINS. The alternative is a
	// service that has been recording for an hour reporting twenty-three hours
	// of outage on its first day: a false alarm the product raises about
	// itself, on the page somebody checks first.
	if !recordedFrom.IsZero() && recordedFrom.After(since) {
		since = recordedFrom
	}

	out := AvailabilitySummary{
		Since:        since,
		Now:          now,
		RecordedFrom: recordedFrom,
		Status:       AvailabilityUnknown,
		CurrentSince: since,
	}
	if recordedFrom.IsZero() || !now.After(since) {
		return out
	}

	var all, healthy []interval
	for _, r := range runs {
		from, to := r.Began.UTC(), r.Until.UTC()
		// A run that began inside the window is a replica that started here -
		// counted before the clipping below, which would hide the start of a
		// run that began exactly at the boundary.
		if !from.Before(since) && !from.After(now) {
			out.Starts++
		}
		if from.Before(since) {
			from = since
		}
		if to.After(now) {
			to = now
		}
		// A ZERO-LENGTH RUN IS KEPT. A replica that has beaten exactly once -
		// a Coordinator that started ten seconds ago - has `began` equal to
		// `until`, and dropping it because it has no width reported a service
		// that is demonstrably serving as zero per cent available. It covers
		// the instant it recorded, and the tail is carried to now below on the
		// same terms as any other run.
		if to.Before(from) {
			continue
		}
		all = append(all, interval{from, to})
		if r.Status == AvailabilityHealthy {
			healthy = append(healthy, interval{from, to})
		}
	}

	covered := mergeIntervals(all)
	healthyCovered := mergeIntervals(healthy)
	// A RECORD THAT REACHES NOW COVERS NOW. The newest run ends at its last
	// beat, which is always a little way in the past, and leaving it there
	// makes a service that has not missed a beat all week report 99.8% uptime
	// for ever and 100% never - a number that is wrong in the direction that
	// teaches a reader to ignore it. The tail is only extended when it is
	// inside the tolerance, which is the same test that decides there is no
	// outage; beyond it, the gap is the outage and stays that way.
	extendTail(covered, now, continuity)
	extendTail(healthyCovered, now, continuity)

	out.UpSeconds = totalSeconds(healthyCovered)
	// Degraded is what was covered and not healthy - a difference rather than
	// a sum of degraded runs, so a degraded replica running beside a healthy
	// one does not subtract from an uptime that never suffered.
	out.DegradedSeconds = totalSeconds(covered) - out.UpSeconds
	if out.DegradedSeconds < 0 {
		out.DegradedSeconds = 0
	}

	// The gaps between what was covered are the outages.
	cursor := since
	for _, c := range covered {
		if c.from.After(cursor) {
			out.Outages = append(out.Outages, AvailabilityOutage{Began: cursor, Ended: c.from})
		}
		if c.to.After(cursor) {
			cursor = c.to
		}
	}
	// And the tail: a record that stops short of now is either a beat in
	// flight or a service that is gone, and `continuity` is the line between.
	ongoing := now.Sub(cursor) > continuity
	if ongoing {
		out.Outages = append(out.Outages, AvailabilityOutage{Began: cursor, Ended: now, Ongoing: true})
	}
	for _, o := range out.Outages {
		out.DownSeconds += o.Seconds()
	}

	if ongoing {
		out.Status = AvailabilityDown
		out.CurrentSince = cursor
		return out
	}

	// Serving. The status is the newest run's, because that is the one still
	// being extended; the uptime is the start of the unbroken stretch it
	// belongs to, not the start of that run - a replica replaced without an
	// outage did not reset anything a reader cares about.
	out.Status = AvailabilityHealthy
	newest := time.Time{}
	for _, r := range runs {
		if at := r.Until.UTC(); at.After(newest) {
			newest = at
			out.Status = r.Status
		}
	}
	if n := len(covered); n > 0 {
		out.CurrentSince = covered[n-1].from
	}
	return out
}

// mergeIntervals unions overlapping and touching intervals into ordered,
// disjoint ones.
func mergeIntervals(in []interval) []interval {
	if len(in) == 0 {
		return nil
	}
	sorted := append([]interval(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].from.Before(sorted[j].from) })

	out := []interval{sorted[0]}
	for _, next := range sorted[1:] {
		last := &out[len(out)-1]
		// `After` rather than a gap tolerance: two runs that touch exactly are
		// one stretch, and two that do not are separated by a gap the caller
		// has already decided how to read.
		if next.from.After(last.to) {
			out = append(out, next)
			continue
		}
		if next.to.After(last.to) {
			last.to = next.to
		}
	}
	return out
}

// extendTail runs the last interval up to `now` when the gap is only a beat
// that has not landed yet. In place: the slice is the caller's own.
func extendTail(in []interval, now time.Time, continuity time.Duration) {
	if len(in) == 0 {
		return
	}
	last := &in[len(in)-1]
	if gap := now.Sub(last.to); gap > 0 && gap <= continuity {
		last.to = now
	}
}

func totalSeconds(in []interval) float64 {
	var n float64
	for _, i := range in {
		n += i.to.Sub(i.from).Seconds()
	}
	return n
}
