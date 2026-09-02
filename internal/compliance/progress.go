package compliance

import (
	"fmt"
	"sync"
	"time"
)

// What a run says about itself while it is running.
//
// # Why this is more than a percentage
//
// A check of a real orb is four to fifteen minutes: ninety-five charts pulled
// out of a vendor registry, each rendered twice, seventy-three checks evaluated
// against several thousand objects. For most of that, a bar at 0% and the words
// "Fetching charts" are indistinguishable from a hang - and the honest question
// somebody asks in front of one is not "how far along is it" but "is this
// working at all".
//
// So a run reports what it has DONE, not only where it is: how long it has been
// going, how many charts it has pulled and how many bytes that was, which ones
// it is working on right now, how many workers are doing that, and a running
// list of what has happened - the chart that failed to render, the artifact the
// budget refused, the moment the last stage ended. Those are answers to "is
// this working". A percentage is not.

// Stage is what a run is doing, in the words the interface shows.
type Stage string

const (
	StageResolving  Stage = "resolving"
	StageFetching   Stage = "fetching"
	StageRendering  Stage = "rendering"
	StageEvaluating Stage = "evaluating"
	StageRecording  Stage = "recording"
)

// Stages is the pipeline in order, so an interface can draw the whole route and
// show where the run has got to rather than only the step it is on.
var Stages = []Stage{
	StageResolving, StageFetching, StageRendering, StageEvaluating, StageRecording,
}

// Label is the stage as a sentence, so the interface does not have to hold a
// second copy of this vocabulary.
func (s Stage) Label() string {
	switch s {
	case StageResolving:
		return "Discovering charts"
	case StageFetching:
		return "Downloading charts"
	case StageRendering:
		return "Rendering charts"
	case StageEvaluating:
		return "Evaluating checks"
	case StageRecording:
		return "Recording results"
	default:
		return "Working"
	}
}

// Detail is the one-line explanation of what a stage actually does, shown
// beside it. Written for somebody who has never seen this run before and is
// deciding whether four minutes is reasonable.
func (s Stage) Detail() string {
	switch s {
	case StageResolving:
		return "Reading the release's recorded contents to identify which artifacts are Helm charts"
	case StageFetching:
		return "Pulling each chart's layer from the vendor registry and unpacking it"
	case StageRendering:
		return "Running helm template on each chart twice - once at its defaults and once " +
			"perturbed - which distinguishes a value the chart fixes from one a site can override"
	case StageEvaluating:
		return "Compiling every check and evaluating it against every object the charts produced"
	case StageRecording:
		return "Writing the results, the coverage and the rendered manifests"
	}
	return ""
}

// EventKind colours one line of the log without the interface parsing its text.
type EventKind string

const (
	EventInfo EventKind = "info"
	EventOK   EventKind = "ok"
	EventWarn EventKind = "warn"
	EventFail EventKind = "fail"
)

// ProgressEvent is one thing that happened, in the order it happened.
type ProgressEvent struct {
	At   time.Time `json:"at"`
	Sec  float64   `json:"sec"`
	Kind EventKind `json:"kind"`
	Text string    `json:"text"`
}

// maxEvents bounds the log a run carries.
//
// A ring, not a transcript: ninety-five charts is ninety-five lines and nobody
// reads more than the last screenful, but a run that reported nothing until it
// finished would be exactly the screen this replaces. The interesting lines -
// failures and refusals - are kept whatever else is dropped, because those are
// the ones somebody scrolls back for.
const maxEvents = 60

// MaxLogEvents is maxEvents, for a reader of a stored run.
//
// Exported so the interface can say that a log AT the cap has lines missing
// from the front. A transcript that silently begins in the middle of a run is a
// transcript somebody reads as the whole run, and then wonders why it does not
// start with the run starting.
const MaxLogEvents = maxEvents

// ProgressCounts is what a run has actually got through.
//
// Every field is a number somebody can act on. "12 of 95" says how far; "3
// refused, 2 would not render" says what the answer is going to be missing, and
// says it while there is still time to stop and fix the cause.
type ProgressCounts struct {
	// Charts, from the release's contents to a rendered manifest.
	ChartsFound int `json:"chartsFound"`
	// ChartsReused is charts served from the render cache: neither downloaded
	// nor rendered, because identical bytes were rendered before under
	// identical inputs. On screen because it is the difference between a check
	// that takes four minutes and one that takes twelve seconds, and somebody
	// watching either deserves to know which they are getting.
	ChartsReused   int `json:"chartsReused"`
	ChartsFetched  int `json:"chartsFetched"`
	ChartsSkipped  int `json:"chartsSkipped"`
	ChartsRendered int `json:"chartsRendered"`
	ChartsFailed   int `json:"chartsFailed"`

	// Bytes is what has been read out of the vendor registry so far. On screen
	// because this is the one part of a run that touches the vendor's
	// bandwidth, and because a release whose charts are 400 MB is a release
	// somebody wants to know about before the budget refuses the rest.
	Bytes int64 `json:"bytes"`

	// Objects is Kubernetes objects parsed out of the rendered manifests, and
	// Checks is how many are in the rulebook. Their product is roughly the
	// number of judgements the evaluation stage will make.
	Objects int `json:"objects"`
	Checks  int `json:"checks"`

	// Results and Findings are filled in as the evaluation runs, so the
	// interface can show something accumulating rather than a bar.
	Results  int `json:"results"`
	Findings int `json:"findings"`
}

// Progress is what a run reports while it is running.
//
// Done and Total are counts of the CURRENT stage, not of the whole run: "12 of
// 95 charts rendered" is a number somebody can reason about, and a single
// percentage across five stages of wildly different cost is not. The route
// through the stages is in Stages, and Counts says what each of them achieved.
type Progress struct {
	RunID string `json:"runId"`
	Stage Stage  `json:"stage"`
	Label string `json:"label"`
	// Detail explains the stage for somebody deciding whether the wait is
	// reasonable. Carried rather than duplicated in the interface, so the two
	// cannot drift.
	Detail string `json:"detail,omitempty"`

	Done  int    `json:"done"`
	Total int    `json:"total"`
	Note  string `json:"note,omitempty"`

	// Active names what is being worked on RIGHT NOW - several things, when
	// several workers are. This is the field that answers "is it stuck": a
	// run whose active list changes is working, whatever the bar says.
	Active []string `json:"active,omitempty"`
	// Concurrency is how many of those may run at once, so a wait that is
	// simply long is not read as a wait that is serialised by mistake.
	Concurrency int `json:"concurrency,omitempty"`

	Started time.Time `json:"started"`
	// Elapsed and Estimate are seconds, computed when the progress is read.
	//
	// Estimate covers THE CURRENT STAGE only and is zero until there is a rate
	// to derive it from. A total-run estimate would have to guess the cost of
	// stages that have not started, whose cost depends on what the ones running
	// now produce - and a made-up number that turns out to be four times wrong
	// is worse than no number.
	Elapsed  float64 `json:"elapsed"`
	Estimate float64 `json:"estimate,omitempty"`

	Counts ProgressCounts `json:"counts"`
	// Done stages, in order, with what each took - so a run at 8 minutes can be
	// read as "the download was 6 of those" rather than as 8 minutes of unknown.
	Completed []StageTiming `json:"completed,omitempty"`
	// Events is what has happened, newest LAST.
	Events []ProgressEvent `json:"events,omitempty"`
}

// StageTiming is one finished stage and what it cost.
type StageTiming struct {
	Stage   Stage   `json:"stage"`
	Label   string  `json:"label"`
	Seconds float64 `json:"seconds"`
	Note    string  `json:"note,omitempty"`
}

// tracker is the writable side of a run's progress.
//
// Its own type, with its own lock, so the fast inner loops - a chart fetched, a
// chart rendered - never contend with the runner's map of live runs. Every
// method is safe from several workers at once, which is what the concurrent
// fetch and render need.
type tracker struct {
	mu sync.Mutex

	runID       string
	started     time.Time
	stageAt     time.Time
	stage       Stage
	done, total int
	note        string
	concurrency int

	active    map[string]struct{}
	counts    ProgressCounts
	completed []StageTiming
	events    []ProgressEvent
}

func newTracker(runID string, at time.Time) *tracker {
	return &tracker{
		runID: runID, started: at, stageAt: at,
		stage: StageResolving, active: map[string]struct{}{},
	}
}

// Stage moves the run on, timing the stage it leaves.
func (t *tracker) Stage(s Stage, done, total int, note string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if s != t.stage {
		if t.stage != "" {
			t.completed = append(t.completed, StageTiming{
				Stage: t.stage, Label: t.stage.Label(),
				Seconds: now.Sub(t.stageAt).Seconds(), Note: t.note,
			})
		}
		t.stage, t.stageAt = s, now
		t.active = map[string]struct{}{}
	}
	t.done, t.total, t.note = done, total, note
}

// Advance bumps the current stage's counter without restating the stage, for a
// worker that has finished one item and knows nothing else about the run.
func (t *tracker) Advance(delta int) {
	t.mu.Lock()
	t.done += delta
	t.mu.Unlock()
}

// Concurrency records how many workers the current stage is using.
func (t *tracker) Concurrency(n int) {
	t.mu.Lock()
	t.concurrency = n
	t.mu.Unlock()
}

// Begin and End bracket one item of work, so the interface can name what is in
// flight. A set rather than a list, because workers finish out of order.
func (t *tracker) Begin(what string) {
	if what == "" {
		return
	}
	t.mu.Lock()
	t.active[what] = struct{}{}
	t.mu.Unlock()
}

func (t *tracker) End(what string) {
	t.mu.Lock()
	delete(t.active, what)
	t.mu.Unlock()
}

// Count mutates the counters under the lock.
func (t *tracker) Count(mutate func(*ProgressCounts)) {
	if mutate == nil {
		return
	}
	t.mu.Lock()
	mutate(&t.counts)
	t.mu.Unlock()
}

// Event appends one line to the log.
//
// Failures and refusals survive the ring; ordinary progress lines are dropped
// oldest-first once it is full. A log that discarded the one line explaining
// why a chart is missing, in order to keep ninety-four lines saying charts
// arrived, would be a log worth nothing.
func (t *tracker) Event(kind EventKind, format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	t.events = append(t.events, ProgressEvent{
		At: now.UTC(), Sec: now.Sub(t.started).Seconds(),
		Kind: kind, Text: fmt.Sprintf(format, args...),
	})
	if len(t.events) <= maxEvents {
		return
	}
	for i, e := range t.events {
		if e.Kind == EventInfo || e.Kind == EventOK {
			t.events = append(t.events[:i], t.events[i+1:]...)
			return
		}
	}
	t.events = t.events[1:]
}

// snapshot is what a poller reads. Every slice is copied: the caller renders it
// while the run keeps writing.
func (t *tracker) snapshot() Progress {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	p := Progress{
		RunID: t.runID, Stage: t.stage, Label: t.stage.Label(), Detail: t.stage.Detail(),
		Done: t.done, Total: t.total, Note: t.note,
		Concurrency: t.concurrency,
		Started:     t.started.UTC(),
		Elapsed:     now.Sub(t.started).Seconds(),
		Counts:      t.counts,
	}

	// The estimate, from THIS stage's own rate. Nothing before the second item:
	// one sample of a stage whose first item paid for a connection and a
	// credential is not a rate, and the number it produces is wildly high at
	// exactly the moment somebody is deciding whether to wait.
	if t.done > 1 && t.total > t.done {
		per := now.Sub(t.stageAt).Seconds() / float64(t.done)
		p.Estimate = per * float64(t.total-t.done)
	}

	for a := range t.active {
		p.Active = append(p.Active, a)
	}
	sortStrings(p.Active)

	p.Completed = append([]StageTiming(nil), t.completed...)
	p.Events = append([]ProgressEvent(nil), t.events...)
	return p
}

// sortStrings keeps the active list stable between polls, so an interface is
// not redrawing the same three names in a different order every two seconds.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Reporter is how the work tells the run what is happening.
//
// An interface, and the reason is the same one that makes Source an interface:
// the fetching and rendering must be drivable from a test with no runner at
// all. NopReporter is what those tests pass, and it is also what makes every
// call site safe without a nil check.
type Reporter interface {
	// Stage moves the run to a stage and sets that stage's counters.
	Stage(s Stage, done, total int, note string)
	// Advance adds to the current stage's counter.
	Advance(delta int)
	// Concurrency records how many workers this stage is running.
	Concurrency(n int)
	// Begin and End bracket one named item of work.
	Begin(what string)
	End(what string)
	// Count mutates the run's counters.
	Count(mutate func(*ProgressCounts))
	// Event records one line of what happened.
	Event(kind EventKind, format string, args ...any)
}

// NopReporter discards everything, for a caller that is not a run.
type NopReporter struct{}

func (NopReporter) Stage(Stage, int, int, string)   {}
func (NopReporter) Advance(int)                     {}
func (NopReporter) Concurrency(int)                 {}
func (NopReporter) Begin(string)                    {}
func (NopReporter) End(string)                      {}
func (NopReporter) Count(func(*ProgressCounts))     {}
func (NopReporter) Event(EventKind, string, ...any) {}

var _ Reporter = NopReporter{}
var _ Reporter = (*tracker)(nil)
