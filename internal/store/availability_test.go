package store

import (
	"testing"
	"time"
)

// The summary is what the Overview page states as fact about the service's own
// availability, so each of these is a sentence it must never get wrong.

const beat = 15 * time.Second

// continuity is what the recorder uses: three beats, so a late one is not a
// restart.
const continuity = 3 * beat

func at(base time.Time, minutes float64) time.Time {
	return base.Add(time.Duration(minutes * float64(time.Minute)))
}

// A SERVICE THAT NEVER STOPPED HAS NO OUTAGES, and the commonest way to get
// this wrong is to report the seconds since the last beat as one.
func TestAnUnbrokenRunReportsNoOutage(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	began := at(now, -120)

	s := SummariseAvailability([]AvailabilityRun{
		{Instance: "a", Status: AvailabilityHealthy, Began: began, Until: at(now, -0.1)},
	}, time.Hour, continuity, began, now)

	if len(s.Outages) != 0 {
		t.Fatalf("outages %+v, want none", s.Outages)
	}
	if s.Status != AvailabilityHealthy {
		t.Fatalf("status %q, want healthy", s.Status)
	}
	if s.Uptime() < 0.999 {
		t.Fatalf("uptime %.4f, want ~1", s.Uptime())
	}
	// The window is clipped to the hour asked for, not to the run.
	if got := s.WindowSeconds(); got < 3590 || got > 3610 {
		t.Fatalf("window %.0fs, want an hour", got)
	}
}

// A GAP IS AN OUTAGE, and it is the only thing that is: the record cannot hold
// a row saying "not running", because writing one requires running.
func TestAGapBetweenRunsIsAnOutage(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	start := at(now, -60)

	s := SummariseAvailability([]AvailabilityRun{
		{Instance: "a", Status: AvailabilityHealthy, Began: start, Until: at(now, -40)},
		// Twelve minutes with nothing recorded, then it came back.
		{Instance: "a", Status: AvailabilityHealthy, Began: at(now, -28), Until: at(now, -0.1)},
	}, time.Hour, continuity, start, now)

	if len(s.Outages) != 1 {
		t.Fatalf("outages %+v, want one", s.Outages)
	}
	if got := s.Outages[0].Seconds(); got < 700 || got > 740 {
		t.Fatalf("outage lasted %.0fs, want about 720", got)
	}
	if s.Outages[0].Ongoing {
		t.Error("an outage that ended is not ongoing")
	}
	if s.Status != AvailabilityHealthy {
		t.Fatalf("status %q, want healthy - it came back", s.Status)
	}
	// Twelve minutes down in an hour.
	if s.Uptime() > 0.81 || s.Uptime() < 0.79 {
		t.Fatalf("uptime %.4f, want about 0.80", s.Uptime())
	}
	// Two: the run the record opens with, and the one after the gap. The
	// window is clipped to the start of the record, so the first run began
	// inside it - which is the truth, and is why the record begins there.
	if s.Starts != 2 {
		t.Fatalf("starts %d, want 2", s.Starts)
	}
}

// THE ROLLING RESTART. Two replicas, each with a gap of its own, and the
// service never stopped answering. Summing the records would report an outage
// that nobody experienced - and a deployment that pages somebody every time it
// ships is a deployment nobody ships.
func TestOverlappingReplicasCoverEachOther(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	start := at(now, -60)

	s := SummariseAvailability([]AvailabilityRun{
		// a serves the first half, and stops.
		{Instance: "a", Status: AvailabilityHealthy, Began: start, Until: at(now, -25)},
		// b started before a stopped and is still going.
		{Instance: "b", Status: AvailabilityHealthy, Began: at(now, -30), Until: at(now, -0.1)},
	}, time.Hour, continuity, start, now)

	if len(s.Outages) != 0 {
		t.Fatalf("outages %+v, want none: one replica always held the traffic", s.Outages)
	}
	if s.Uptime() < 0.999 {
		t.Fatalf("uptime %.4f, want ~1", s.Uptime())
	}
	// a opens the record and b joined it: two starts, and no outage between
	// them. That pair is what a healthy rolling restart looks like.
	if s.Starts != 2 {
		t.Fatalf("starts %d, want 2", s.Starts)
	}
	// Uptime is the unbroken stretch, not the newest run: a replica replaced
	// without an outage did not reset anything a reader cares about.
	if !s.CurrentSince.Equal(start) {
		t.Fatalf("serving since %s, want the start of the unbroken stretch %s",
			s.CurrentSince, start)
	}
}

// A SERVICE THAT IS DOWN RIGHT NOW says so, and dates the outage from the last
// beat rather than from the moment somebody looked.
func TestAStaleRecordIsAnOngoingOutage(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	start := at(now, -60)
	stopped := at(now, -9)

	s := SummariseAvailability([]AvailabilityRun{
		{Instance: "a", Status: AvailabilityHealthy, Began: start, Until: stopped},
	}, time.Hour, continuity, start, now)

	if s.Status != AvailabilityDown {
		t.Fatalf("status %q, want down", s.Status)
	}
	if len(s.Outages) != 1 || !s.Outages[len(s.Outages)-1].Ongoing {
		t.Fatalf("outages %+v, want one ongoing", s.Outages)
	}
	if !s.CurrentSince.Equal(stopped) {
		t.Fatalf("down since %s, want the last beat %s", s.CurrentSince, stopped)
	}
}

// A BEAT THAT IS MERELY LATE IS NOT AN OUTAGE. Without the tolerance, every
// summary taken between two beats would report the service as down for as long
// as the beat interval, which is an outage banner that arrives on a timer.
func TestALateBeatIsNotAnOutage(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	start := at(now, -60)

	s := SummariseAvailability([]AvailabilityRun{
		// Last beat 20 seconds ago: later than the 15-second interval, well
		// inside the 45-second tolerance.
		{Instance: "a", Status: AvailabilityHealthy, Began: start, Until: now.Add(-20 * time.Second)},
	}, time.Hour, continuity, start, now)

	if len(s.Outages) != 0 {
		t.Fatalf("outages %+v, want none - the beat is late, not missing", s.Outages)
	}
	if s.Status != AvailabilityHealthy {
		t.Fatalf("status %q, want healthy", s.Status)
	}
}

// NOTHING IS CLAIMED ABOUT TIME THE RECORD DOES NOT COVER. A deployment
// upgraded into this feature an hour ago must not report twenty-three hours of
// outage - the product raising a false alarm about itself, on the page
// somebody checks first.
func TestTheWindowIsClippedToTheStartOfTheRecord(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	start := at(now, -45)

	s := SummariseAvailability([]AvailabilityRun{
		{Instance: "a", Status: AvailabilityHealthy, Began: start, Until: at(now, -0.1)},
	}, 24*time.Hour, continuity, start, now)

	if !s.Since.Equal(start) {
		t.Fatalf("window starts %s, want the first record %s", s.Since, start)
	}
	if len(s.Outages) != 0 {
		t.Fatalf("outages %+v, want none - nothing is known about before the record", s.Outages)
	}
	if got := s.WindowSeconds(); got > 2760 {
		t.Fatalf("window %.0fs, want about 45 minutes", got)
	}
}

// An empty record says UNKNOWN, which is not DOWN. They look identical in the
// data and mean opposite things to whoever is reading.
func TestNoRecordIsUnknownRatherThanDown(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)

	s := SummariseAvailability(nil, time.Hour, continuity, time.Time{}, now)

	if s.Status != AvailabilityUnknown {
		t.Fatalf("status %q, want unknown", s.Status)
	}
	if len(s.Outages) != 0 {
		t.Fatalf("outages %+v, want none", s.Outages)
	}
	if s.DownSeconds != 0 {
		t.Fatalf("down %.0fs, want 0 - nothing is known, which is not an outage", s.DownSeconds)
	}
}

// DEGRADED IS NOT DOWN, and it is not counted twice. A degraded replica beside
// a healthy one must not subtract from an uptime that never suffered.
func TestDegradedTimeIsCountedOnceAndStillCountsAsServing(t *testing.T) {
	now := time.Date(2026, 9, 17, 16, 0, 0, 0, time.UTC)
	start := at(now, -60)

	s := SummariseAvailability([]AvailabilityRun{
		{Instance: "a", Status: AvailabilityHealthy, Began: start, Until: at(now, -30)},
		{Instance: "a", Status: AvailabilityDegraded, Began: at(now, -30), Until: at(now, -0.1)},
	}, time.Hour, continuity, start, now)

	if s.Status != AvailabilityDegraded {
		t.Fatalf("status %q, want degraded - that is the run still being extended", s.Status)
	}
	if len(s.Outages) != 0 {
		t.Fatalf("outages %+v, want none - degraded is still serving", s.Outages)
	}
	if s.Uptime() < 0.999 {
		t.Fatalf("uptime %.4f, want ~1: a degraded service answered every request", s.Uptime())
	}
	if s.DegradedSeconds < 1700 || s.DegradedSeconds > 1900 {
		t.Fatalf("degraded %.0fs, want about 1800", s.DegradedSeconds)
	}
	// Up + degraded + down accounts for the whole window.
	total := s.UpSeconds + s.DegradedSeconds + s.DownSeconds
	if diff := total - s.WindowSeconds(); diff > 1 || diff < -1 {
		t.Fatalf("up+degraded+down = %.0fs, window = %.0fs", total, s.WindowSeconds())
	}
}

// The round trip through the database, on the dialect `task run` uses: a beat
// extends its run, a late one starts a new one, and the gap between them is
// what the summary reads as an outage.
func TestBeatExtendsARunAndAGapStartsANewOne(t *testing.T) {
	s := openTestStore(t)
	av := NewAvailability(s)
	ctx := t.Context()

	if _, err := av.Beat(ctx, "coordinator", "one", "1.0.0", AvailabilityHealthy, continuity); err != nil {
		t.Fatal(err)
	}
	// A second beat inside the tolerance must not produce a second row - the
	// whole reason this is intervals rather than heartbeats.
	started, err := av.Beat(ctx, "coordinator", "one", "1.0.0", AvailabilityHealthy, continuity)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("a beat inside the tolerance started a new run")
	}

	runs, err := av.Runs(ctx, "coordinator", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}

	// A beat that could not be continuous with the last one - zero tolerance -
	// is a process that has been away.
	started, err = av.Beat(ctx, "coordinator", "one", "1.0.0", AvailabilityHealthy, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !started {
		t.Fatal("a beat outside the tolerance must start a new run")
	}
	runs, err = av.Runs(ctx, "coordinator", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("%d runs, want 2", len(runs))
	}

	// A status change also ends a run, so a reader can see when the
	// degradation began rather than being told the whole day was degraded.
	if _, err := av.Beat(ctx, "coordinator", "one", "1.0.0", AvailabilityDegraded, time.Hour); err != nil {
		t.Fatal(err)
	}
	runs, err = av.Runs(ctx, "coordinator", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("%d runs after a status change, want 3", len(runs))
	}
	if runs[len(runs)-1].Status != AvailabilityDegraded {
		t.Fatalf("newest run is %q, want degraded", runs[len(runs)-1].Status)
	}

	first, err := av.FirstRecorded(ctx, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	if first.IsZero() {
		t.Fatal("the record has rows but reports no start")
	}
	if none, err := av.FirstRecorded(ctx, "worker"); err != nil || !none.IsZero() {
		t.Fatalf("a component that never recorded: %v, %v", none, err)
	}
}
