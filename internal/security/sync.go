package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// PackageResult is one release's finished sync, in this package's own terms.
//
// Defined here rather than in the store so the sync can be written without
// importing persistence - the dependency points the other way, and the store
// adapts this into its row.
type PackageResult struct {
	PackageID int64

	// Provider is the scanner whose name goes on the release row, for the
	// single-scanner case and for a listing with one column.
	Provider string
	// Providers is every scanner that actually answered, sorted. The
	// per-scanner breakdown is keyed by these.
	Providers  []string
	Repository string
	Role       string

	Posture     Posture
	Fingerprint string
	// Log is the run's transcript, stored with the result so the question
	// "what happened during that sync" survives the process that answered it.
	Log []SyncLogEntry
}

// SyncLogEntry is one line of a sync's transcript.
//
// Levels rather than prose, because the reader of a two-hundred-line log is
// looking for the lines that matter and a transcript that cannot be skimmed is
// one nobody opens twice.
type SyncLogEntry struct {
	At time.Time `json:"at"`
	// Level is info | warning | error.
	Level   string `json:"level"`
	Message string `json:"message"`
	// Repeat counts identical consecutive lines. A release whose scanner is
	// timing out produces the same sentence forty times, and forty copies of it
	// push the line that says what started out of a bounded log.
	Repeat int `json:"repeat,omitempty"`
}

// Log levels.
//
// Aliases of the constants in provider.go, which is where a provider reaches
// them from. Four rather than three: a transcript whose only positive outcome
// is the absence of red cannot say "this worked", and "Sync finished. 157 of
// 157 images scanned" is the line a reader is looking for.
const (
	LogInfo    = LevelInfo
	LogSuccess = LevelSuccess
	LogWarning = LevelWarning
	LogError   = LevelError
)

// maxLogEntries bounds one sync's transcript. Generous enough to hold a run's
// worth of distinct events, small enough that it is kilobytes in a row that is
// read whole on every page render.
const maxLogEntries = 200

// Recorder is where a sync writes what it found.
//
// Each method is a state transition, not a setter. That shape is deliberate:
// the claim has to be atomic against a second caller, and the failure path has
// to be distinguishable from the success path by a reader who only ever sees
// the row.
type Recorder interface {
	// Claim marks a release as syncing, on behalf of the named process.
	// Returns an error the caller reports as "already running" when somebody
	// else holds it.
	Claim(ctx context.Context, packageID int64, owner string, staleAfter time.Duration) error
	// Heartbeat renews a claim and reports whether it is still held by
	// `owner`. False means the claim went away - somebody stopped the sync, or
	// it was taken after this process stopped beating - and the run must stand
	// down.
	Heartbeat(ctx context.Context, packageID int64, owner string) (bool, error)
	// Stop releases a running claim, leaving the last stored result in place.
	// Reports whether a claim was actually held.
	Stop(ctx context.Context, packageID int64) (bool, error)
	// Record stores a finished sync.
	Record(ctx context.Context, res PackageResult) error
	// Fail records a sync that gave up, keeping the last good counts. The log
	// is what makes the one-sentence reason answerable: it is the failure that
	// most needs a transcript and the one that has nothing else to leave.
	Fail(ctx context.Context, packageID int64, reason string, log []SyncLogEntry) error
}

// SyncRequest is one release's sync.
type SyncRequest struct {
	PackageID int64
	// Label is what to call the release in progress messages.
	Label string
	Scope Scope
	// Providers names the scanners to ask, in the order to ask them.
	//
	// Empty asks whatever the scope names, which is the single-scanner path
	// and stays exactly as it was. Named, each scanner answers into its own
	// scope and the release records the merged result plus a row per scanner.
	Providers []string
	// Release names the release for the scanners that group by one - Anchore's
	// Application and Version. See ScanOptions.Release.
	Release ReleaseRef
	// Artifacts is what to ask about, assembled by the caller from the
	// release's own tree.
	Artifacts []ArtifactRef
	TTL       CacheTTL
	// Documents are the extra scanner bodies to retrieve alongside the
	// findings. See Request.Documents: each named kind is a request per
	// artifact, so this is the deployment's choice and not the provider's.
	Documents []DocumentKind
	// MaxAge is how old a stored answer may be before this sync asks about
	// that image again. Zero reuses anything held.
	//
	// This is what makes a sync cheap for a release whose images have already
	// been answered for by a neighbouring release - which is most releases of
	// most products. See Request.MaxAge.
	MaxAge time.Duration
	// Force asks the scanner about every artifact regardless of what is held
	// and how old it is. A person who pressed "re-fetch everything", and
	// nothing else.
	Force bool
}

// SyncStatus is what a caller is told when it asks for a sync.
type SyncStatus string

const (
	// SyncStarted means this call claimed the release and work is under way.
	SyncStarted SyncStatus = "started"
	// SyncAlreadyRunning means somebody else holds it. Not a failure - the
	// thing the caller wanted is happening.
	SyncAlreadyRunning SyncStatus = "already_running"
)

// StaleClaimAfter is how long a claim is honoured before it is treated as
// abandoned, for a sync that is still beating.
//
// Generous, because a release of several hundred artifacts against a busy
// scanner is genuinely slow, and stealing a live claim costs a duplicate query.
// Finite, because a sync that has been going for longer than any sync takes is
// not going.
const StaleClaimAfter = 30 * time.Minute

// The heartbeat, which is what tells a running sync from an abandoned one.
//
// # Why a claim needs one at all
//
// `state = syncing` says a sync was STARTED, and nothing else. A Coordinator
// killed mid-sync leaves that row exactly as a healthy one leaves it, so every
// reader was told the sync was running on another Coordinator - for the half
// hour it took the stale window to expire, on a deployment that has one
// Coordinator and had just restarted it.
//
// A run that is alive says so every HeartbeatInterval. A claim that has not
// been renewed within HeartbeatTimeout is held by nothing: the interface says
// the sync was interrupted, and the next claim takes the row.
//
// The timeout is several intervals, so a busy process or a slow database costs
// a late beat rather than a stolen claim.
const (
	HeartbeatInterval = 15 * time.Second
	HeartbeatTimeout  = 90 * time.Second
)

// Syncer runs vulnerability syncs in the background.
//
// # Why a job rather than a read
//
// Retrieving a release's posture is a scanner query per batch across a few
// hundred artifacts - tens of seconds, sometimes minutes. Doing that on a page
// render means a page that takes minutes, twenty of them behind a listing, and
// a scanner rate-limiting the whole estate because somebody scrolled.
//
// So it is an explicit act with a durable result. Somebody asks, the release
// says it is syncing, the answer is stored, and from then on every listing,
// comparison and search reads what is stored and troubles no scanner at all.
// That is also what makes search answerable at all: there is a table to search.
type Syncer struct {
	service  *Service
	recorder Recorder
	log      *slog.Logger
	// owner names THIS process in every claim it takes. A restarted
	// Coordinator is a different owner, which is what lets it tell its
	// predecessor's abandoned claims from a live one held elsewhere.
	owner string
	// beatEvery is how often a running sync renews its claim. A field rather
	// than the constant so a test can watch a beat without waiting for one.
	beatEvery time.Duration

	// documents are the extra scanner bodies every sync retrieves. A property
	// of the deployment rather than of a request, because "how hard do we push
	// this scanner" is an operator's decision and a per-request switch would
	// let a page turn a two-minute sync into a twenty-minute one.
	documents []DocumentKind

	mu       sync.Mutex
	inFlight map[int64]*runningSync
}

// WithDocuments sets the extra bodies every sync retrieves.
func (s *Syncer) WithDocuments(kinds []DocumentKind) *Syncer {
	s.documents = kinds
	return s
}

// Documents is what a sync will retrieve beyond the findings.
func (s *Syncer) Documents() []DocumentKind { return s.documents }

// runningSync is one sync this replica is running: what it will say about
// itself, and the handle that stops it.
type runningSync struct {
	progress *SyncProgress
	cancel   context.CancelFunc
}

// NewSyncer builds the syncer.
func NewSyncer(service *Service, recorder Recorder, log *slog.Logger) *Syncer {
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{
		service:   service,
		recorder:  recorder,
		log:       log,
		owner:     processOwner(),
		beatEvery: HeartbeatInterval,
		inFlight:  map[int64]*runningSync{},
	}
}

// processOwner names this process: host, pid and a random suffix.
//
// The suffix matters. Two Coordinators in one pod-per-host deployment restart
// onto the same host with the same pid often enough that host and pid alone
// would let a restarted process mistake its predecessor's claim for its own.
func processOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "coordinator"
	}
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.NewString()[:8])
}

// SyncProgress is what one running sync will say about itself.
type SyncProgress struct {
	mu        sync.Mutex
	stages    map[string]stagePosition
	order     []string
	notes     []progressNote
	log       []SyncLogEntry
	startedAt time.Time
	done      bool

	// active is what is in a worker's hands RIGHT NOW, in the order it was
	// picked up. The single most useful thing on a progress panel: a list that
	// changes every few seconds is a run that is working whatever the bar is
	// doing, and it is the only thing that tells a slow scanner from a wedged
	// one. A slice rather than a set because the order is what makes it read as
	// a queue draining rather than a list reshuffling.
	active []string
	// concurrency is how many may be in flight at once, so a watcher can tell
	// "one at a time against a slow registry" from "sixteen at a time".
	concurrency int
	statuses    map[string]int
	providers   []string
}

type stagePosition struct{ done, total int }

// NewProgress builds a progress tracker whose clock starts now.
//
// Exported because a replication builds one too, and two literals of the same
// struct with the same three fields is how the two drift apart.
func NewProgress() *SyncProgress {
	return &SyncProgress{stages: map[string]stagePosition{}, startedAt: time.Now()}
}

// SetStatuses records the latest completed-item counts by provider state.
func (p *SyncProgress) SetStatuses(statuses map[string]int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.statuses = make(map[string]int, len(statuses))
	for status, count := range statuses {
		p.statuses[status] = count
	}
}

// SetProviders records the scanners this run is asking, in request order.
func (p *SyncProgress) SetProviders(providers []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.providers = append([]string(nil), providers...)
}

// Begin records that something is now being worked on.
func (p *SyncProgress) Begin(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, existing := range p.active {
		if existing == name {
			return
		}
	}
	p.active = append(p.active, name)
}

// End records that something is no longer being worked on.
func (p *SyncProgress) End(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.active {
		if existing == name {
			p.active = append(p.active[:i], p.active[i+1:]...)
			return
		}
	}
}

// SetConcurrency records how many things may be in flight at once.
func (p *SyncProgress) SetConcurrency(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.concurrency = n
}

// progressNote is one thing the sync has to say, and how many times it has had
// to say it.
type progressNote struct {
	// shape is the sentence with its numbers removed, which is what makes two
	// tellings of the same news the same note.
	shape string
	text  string
	count int
}

// digits matches the numbers that make one recurring sentence look like several.
var digits = regexp.MustCompile(`\d+`)

// noteShape is a note with its numbers taken out.
//
// # The line this exists for
//
// "JFrog Xray timed out on 50 artifacts. Retrying as two smaller requests."
// then "... on 25 artifacts ..." then "... on 7 artifacts ...". Three
// tellings of one piece of news - the scanner is struggling and the batch is
// being split - which arrived as three lines, then five, then nine, and read
// as nine separate problems. They are one problem whose number is going down.
//
// So a note replaces the note it is a re-telling of, and carries how many
// times it has been said. What a watcher sees is one line that updates.
func noteShape(s string) string {
	return digits.ReplaceAllString(strings.ToLower(strings.Join(strings.Fields(s), " ")), "#")
}

// Stage implements Progress.
func (p *SyncProgress) Stage(name string, done, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cur, seen := p.stages[name]
	if !seen {
		p.order = append(p.order, name)
	}
	// Never backwards: batches land concurrently and an earlier number
	// arriving late would make the bar stutter.
	if done >= cur.done {
		cur.done = done
	}
	// A stage can be reported by more than one provider or request. Its initial
	// position is authoritative for the denominator; a later lower position is
	// not retry work and must not leave the UI showing another stage's total.
	if total > 0 && (!seen || (cur.done == 0 && done == 0)) {
		cur.total = total
	}
	p.stages[name] = cur
}

// Note implements Progress: a warning that is not a position.
func (p *SyncProgress) Note(s string) { p.Record(LogWarning, false, s) }

// Record implements Detailed.
//
// A note that re-tells one already here REPLACES it and moves to the end,
// rather than being appended beside it. See noteShape: the sentence that keeps
// arriving with a smaller number in it is one situation, and stacking its
// tellings turns a scanner backing off into what reads as a growing list of
// failures.
//
// `replace` goes further and suppresses the COUNT as well, for a line whose
// number is the point. "Retrieved scan results for 96 of 157 images (x30)" puts
// a repeat count beside a figure that was never repeated; it is one line whose
// value moved thirty times.
func (p *SyncProgress) Record(level string, replace bool, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// The live notes panel carries what is going WRONG. A progress line belongs
	// in the transcript and not beside the bar, which already says 96 of 157.
	if level != LogInfo && level != LogSuccess {
		shape := noteShape(s)
		replaced := false
		for i, existing := range p.notes {
			if existing.shape != shape {
				continue
			}
			count := existing.count + 1
			if replace {
				count = existing.count
			}
			note := progressNote{shape: shape, text: s, count: count}
			p.notes = append(append(p.notes[:i:i], p.notes[i+1:]...), note)
			replaced = true
			break
		}
		if !replaced {
			if len(p.notes) >= 20 {
				p.notes = p.notes[1:]
			}
			p.notes = append(p.notes, progressNote{shape: shape, text: s, count: 1})
		}
	}

	p.appendLocked(level, s, replace)
}

// Log adds one line to the transcript.
func (p *SyncProgress) Log(level, message string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.appendLocked(level, message, false)
}

func (p *SyncProgress) appendLocked(level, message string, replace bool) {
	message = strings.TrimSpace(message)
	if message == "" {
		return
	}
	if len(message) > 500 {
		message = message[:497] + "..."
	}
	// Collapsed on the SHAPE, not on the exact sentence. A batch that keeps
	// timing out says the same thing about a different number of artifacts
	// every time, so an equality test left forty lines in a bounded transcript
	// and pushed out the ones that said what the sync was doing. The latest
	// telling wins, because the number in it is the current one.
	if n := len(p.log); n > 0 && p.log[n-1].Level == level && noteShape(p.log[n-1].Message) == noteShape(message) {
		if !replace {
			p.log[n-1].Repeat++
		}
		p.log[n-1].Message = message
		p.log[n-1].At = time.Now().UTC()
		return
	}
	if len(p.log) >= maxLogEntries {
		// The oldest line goes, never the first: "this sync started against X
		// with N artifacts" is the line every other line is read against.
		p.log = append(p.log[:1], p.log[2:]...)
	}
	p.log = append(p.log, SyncLogEntry{At: time.Now().UTC(), Level: level, Message: message})
}

// Entries returns a copy of the transcript so far.
func (p *SyncProgress) Entries() []SyncLogEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]SyncLogEntry(nil), p.log...)
}

// Snapshot returns a copy safe to serialize.
func (p *SyncProgress) Snapshot() (stages []SyncStage, notes []string, startedAt time.Time, done bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range p.order {
		pos := p.stages[name]
		stages = append(stages, SyncStage{Name: name, Done: pos.done, Total: pos.total})
	}
	for _, n := range p.notes {
		// The count travels with the line rather than as a second one. "…
		// (3 times)" says both that this has been going on and that it is one
		// thing going on, which two copies of the sentence do not.
		if n.count > 1 {
			notes = append(notes, fmt.Sprintf("%s (%d times)", n.text, n.count))
			continue
		}
		notes = append(notes, n.text)
	}
	return stages, notes, p.startedAt, p.done
}

// SyncStage is one phase's position.
type SyncStage struct {
	Name  string `json:"name"`
	Done  int    `json:"done,omitempty"`
	Total int    `json:"total,omitempty"`
}

// ProgressSnapshot is a run's whole live position in a shape that can be
// STORED, which is the difference between a panel that survives a refresh and
// one that does not.
//
// # Why this is written to the database rather than only served from memory
//
// Because the memory it used to live in belongs to whichever replica happens to
// be running the work, and a browser reload is a new request that may not reach
// that replica - and a Coordinator restart reaches no replica at all. The row
// said "running" and could say nothing else, which is the state a reader is
// least able to act on: it looks identical to a job that has hung.
//
// Rewritten on each heartbeat rather than on each event. A person watching a
// bar cannot use finer resolution than seconds, and an UPDATE per submitted
// image would put a hundred and fifty writes into an operation whose whole
// merit is that it is quick.
type ProgressSnapshot struct {
	Stages      []SyncStage    `json:"stages,omitempty"`
	Notes       []string       `json:"notes,omitempty"`
	Active      []string       `json:"active,omitempty"`
	Concurrency int            `json:"concurrency,omitempty"`
	Statuses    map[string]int `json:"statuses,omitempty"`
	Providers   []string       `json:"providers,omitempty"`
	StartedAt   time.Time      `json:"startedAt,omitempty"`
	Log         []SyncLogEntry `json:"log,omitempty"`
}

// Snapshot copies the whole live position, safe to serialize.
func (p *SyncProgress) SnapshotFull() ProgressSnapshot {
	stages, notes, startedAt, _ := p.Snapshot()
	p.mu.Lock()
	defer p.mu.Unlock()
	return ProgressSnapshot{
		Stages:      stages,
		Notes:       notes,
		Active:      append([]string(nil), p.active...),
		Concurrency: p.concurrency,
		Statuses:    maps.Clone(p.statuses),
		Providers:   append([]string(nil), p.providers...),
		StartedAt:   startedAt,
		Log:         append([]SyncLogEntry(nil), p.log...),
	}
}

// Start claims a release and syncs it in the background.
//
// Returns as soon as the claim is decided, never when the work is done. The
// caller is answering an HTTP request and the work is minutes; holding the
// request open would put every intermediary's idle timeout into the control
// plane, which is the same argument discovery's `wait: false` makes.
//
// The background context is deliberately detached from the request's. A user
// who navigates away must not cancel a sync half way through and leave the row
// claimed - the whole point of making this a job is that it outlives the page.
func (s *Syncer) Start(ctx context.Context, req SyncRequest) (SyncStatus, error) {
	if err := s.recorder.Claim(ctx, req.PackageID, s.owner, StaleClaimAfter); err != nil {
		return SyncAlreadyRunning, err
	}

	progress := &SyncProgress{stages: map[string]stagePosition{}, startedAt: time.Now()}
	// Bounded, because an unbounded background job against an unresponsive
	// scanner is a goroutine that never ends and a row that stays claimed until
	// the stale sweep notices. Detached from the request's context: a user who
	// navigates away must not cancel a sync half way through.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), StaleClaimAfter)

	s.mu.Lock()
	s.inFlight[req.PackageID] = &runningSync{progress: progress, cancel: cancel}
	s.mu.Unlock()

	go s.run(runCtx, cancel, req, progress)
	return SyncStarted, nil
}

// Progress returns the live progress of a running sync, if this replica is the
// one running it.
//
// A miss is a normal answer: the sync may be on another replica, or may have
// finished. The caller falls back to the stored state, which is durable.
func (s *Syncer) Progress(packageID int64) (*SyncProgress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.inFlight[packageID]
	if !ok {
		return nil, false
	}
	return r.progress, true
}

// Running reports whether this replica is syncing the release right now.
func (s *Syncer) Running(packageID int64) bool {
	_, ok := s.Progress(packageID)
	return ok
}

// Cancel stops a running sync, wherever it is running.
//
// Two halves, because a sync is two things: a goroutine here and a claim in the
// database. The claim is released either way - that is what makes a sync
// stoppable from a Coordinator that is not the one running it, because the run
// checks on every heartbeat whether it still holds one and stands down when it
// does not. Cancelling the local context as well is what makes it immediate for
// the common case where the reader is talking to the replica doing the work.
//
// Reports whether anything was actually stopped: pressing Stop on a sync that
// finished a second ago should say so rather than claim a success.
func (s *Syncer) Cancel(ctx context.Context, packageID int64) (bool, error) {
	stopped, err := s.recorder.Stop(ctx, packageID)
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	run, here := s.inFlight[packageID]
	s.mu.Unlock()
	if here {
		run.progress.Log(LogWarning, "The sync was stopped before it finished.")
		run.cancel()
	}
	return stopped || here, nil
}

// beat renews the claim until the run ends, and cancels the run if the claim
// has gone.
func (s *Syncer) beat(ctx context.Context, cancel context.CancelFunc, packageID int64) {
	every := s.beatEvery
	if every <= 0 {
		every = HeartbeatInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			held, err := s.recorder.Heartbeat(ctx, packageID, s.owner)
			if err != nil {
				// A database that cannot be reached is not a claim that was
				// taken away. Losing a beat is what the timeout has slack for,
				// and abandoning a sync of several hundred artifacts over one
				// failed UPDATE would be the worse trade.
				s.log.Warn("could not renew the security sync claim",
					"package", packageID, "error", err)
				continue
			}
			if !held {
				s.log.Info("security sync claim released elsewhere; stopping",
					"package", packageID)
				cancel()
				return
			}
		}
	}
}

func (s *Syncer) run(
	ctx context.Context, cancel context.CancelFunc, req SyncRequest, progress *SyncProgress,
) {
	defer cancel()

	defer func() {
		s.mu.Lock()
		delete(s.inFlight, req.PackageID)
		s.mu.Unlock()
	}()

	// The claim says this process is alive while the work runs. Started before
	// the work so a scanner that hangs on the first request still keeps the
	// claim honest.
	go s.beat(ctx, cancel, req.PackageID)

	scanners := req.Providers
	if len(scanners) == 0 {
		scanners = []string{req.Scope.Provider}
	}
	progress.SetProviders(scanners)
	progress.Log(LogInfo, fmt.Sprintf(
		"Sync started for %s. %d artifacts to consider, using %s against repository %s.",
		labelOr(req.Label, "this release"), len(req.Artifacts),
		DescribeAll(scanners), req.Scope.Repository))
	if len(s.documents) > 0 {
		// What ELSE this sync is going to ask for, because each named kind is a
		// request per image and a reader watching a slow sync deserves to know
		// what the requests are for.
		names := make([]string, 0, len(s.documents))
		for _, kind := range s.documents {
			names = append(names, strings.ToLower(kind.Label()))
		}
		progress.Log(LogInfo, "Retrieving "+strings.Join(names, " and ")+" for each scanned image.")
	}

	res, err := s.service.Postures(ctx, MultiRequest{
		Providers: req.Providers,
		Request: Request{
			Scope:     req.Scope,
			Artifacts: req.Artifacts,
			Release:   req.Release,
			// Detail, always. A sync exists to fill the index that search and
			// comparison read; counts alone would leave both of them empty and
			// force the scanner query back onto the page that reads them.
			Detail: true,
			// NOT Refresh. A sync reuses a stored answer that is inside the age
			// limit and asks about the rest - which for a release sharing its
			// images with one synced this morning is most of the release. Force
			// is the escape hatch for somebody who wants every image re-asked.
			Refresh:   req.Force,
			MaxAge:    req.MaxAge,
			TTL:       req.TTL,
			Documents: documentsOr(req.Documents, s.documents),
			Progress:  progress,
		},
	})
	if err != nil {
		// A sync somebody STOPPED is not a sync that failed, and must not
		// overwrite the state Stop already put back. The release keeps whatever
		// it knew before the run started, which is the truth: this run did not
		// happen. A run that hit its own deadline is a failure and falls
		// through.
		if errors.Is(ctx.Err(), context.Canceled) {
			s.log.Info("security sync stopped before it finished",
				"package", req.PackageID, "label", req.Label)
			return
		}
		s.log.Error("security sync failed", "package", req.PackageID, "label", req.Label, "error", err)
		progress.Log(LogError, "The sync stopped before it finished: "+err.Error())
		s.fail(ctx, req.PackageID, err.Error(), progress.Entries())
		return
	}

	// STOPPED, but with an answer in hand.
	//
	// A provider that turns a cancelled request into "unavailable" reports
	// rather than an error hands back a complete-looking result full of
	// nothing - and recording that would mark the release failed, or worse
	// overwrite good stored counts with a half-finished run's, on the strength
	// of a stop somebody asked for. The context is the authority on whether
	// this run is still wanted.
	if errors.Is(ctx.Err(), context.Canceled) {
		s.log.Info("security sync stopped before it finished",
			"package", req.PackageID, "label", req.Label)
		return
	}

	// Every distinct thing the scanners said, with how many artifacts they said
	// it about. This is the whole answer to "why is my release only 4%
	// scanned", and until it was written down the interface could say the
	// number and never the reason.
	for _, group := range groupMessages(res.Posture.Reports) {
		progress.Log(group.level, group.line())
	}

	// A scanner that could not be asked at all, named - and NOT fatal.
	//
	// One of two scanners refusing a credential is a degraded sync, not a
	// failed one: the release has Xray's ninety thousand findings and does not
	// have Anchore's, and recording that as a failure would throw away the
	// answer somebody is waiting for to report a problem they can read in the
	// transcript.
	for _, provider := range sortedProviders(res.Failures) {
		progress.Log(LogError, fmt.Sprintf("%s could not be asked about this release: %s",
			ProviderLabel(provider), res.Failures[provider].Error()))
	}

	// A sync that reached the scanners and was told "off" by every one of them
	// is not a success with zero findings. Recording it as one would put a
	// clean-looking row in a listing for a release nobody scanned.
	if !res.Enabled() {
		reason := fmt.Sprintf("No scanner is enabled for the repository holding this release (%s).",
			req.Scope.Repository)
		if len(res.Posture.Reports) > 0 && res.Posture.Reports[0].Message != "" {
			reason = res.Posture.Reports[0].Message
		}
		if len(res.Failures) > 0 {
			reason = firstFailure(res.Failures)
		}
		progress.Log(LogError, reason)
		s.fail(ctx, req.PackageID, reason, progress.Entries())
		return
	}

	// A sync that reached the scanner and got results for NOTHING is not a
	// success with zero findings. It is the failure this whole feature exists
	// to keep visible: recorded as a success it would put a clean-looking row -
	// "0 vulnerabilities" - in a listing for a release nobody scanned.
	//
	// Distinct from a release with nothing scannable in it, which is an honest
	// zero and stays a success.
	if cov := res.Posture.Coverage; cov.Scannable() > 0 && cov.Scanned == 0 {
		reason := fmt.Sprintf(
			"The scanner returned no results for any of the %d artifacts in this release.",
			cov.Scannable())
		if msg := firstMessage(res.Posture.Reports); msg != "" {
			reason += " " + msg
		}
		// A FIRST SYNC AGAINST AN ASYNCHRONOUS SCANNER, told apart from a
		// scanner that is broken.
		//
		// # The failure this sentence exists to prevent
		//
		// Anchore is told about an image and analyses it in its own time, so a
		// release synced within minutes of being transferred has every image
		// submitted, none analysed, and no results at all. That is the ordinary
		// first sync of every release on an Anchore-only deployment - and the
		// sentence above sends the reader to look for a broken scanner, which
		// is the one thing that is not wrong.
		//
		// The STATE stays failed, deliberately. These numbers are not a clean
		// result and must not be recorded as one; the rule above is the whole
		// reason this feature can be trusted. What changes is the sentence,
		// which now names the remedy - and the remedy is one button.
		if n := stillAnalysing(res.Posture.Reports); n > 0 && n == cov.Scannable() {
			reason = fmt.Sprintf(
				"The scanner has not finished analysing any of the %d images in this release yet. "+
					"This is the ordinary state of a release that has just been submitted - "+
					"sync again in a few minutes to pick up the results.", n)
		}
		s.log.Warn("security sync produced no results",
			"package", req.PackageID, "label", req.Label, "artifacts", cov.Scannable())
		progress.Log(LogError, reason)
		s.fail(ctx, req.PackageID, reason, progress.Entries())
		return
	}

	ReportStage(progress, StageCorrelating, len(res.Posture.Reports), len(res.Posture.Reports))
	cov := res.Posture.Coverage

	// The closing line, and it is allowed to say the work SUCCEEDED.
	//
	// A transcript whose best outcome is the absence of red cannot tell a
	// reader that a sync did what it was asked; "Sync finished" in the same
	// grey as every other line is the difference between a log somebody trusts
	// and one they scroll to the bottom of looking for something worse.
	level := LogSuccess
	if !cov.Complete() {
		level = LogWarning
	}
	progress.Log(level, fmt.Sprintf(
		"Sync finished. %d of %d images scanned, %d vulnerabilities across %d distinct advisories.",
		cov.Scanned, cov.Scannable(), res.Posture.Counts.Total, res.Posture.UniqueCVEs))
	// The KEVs, on their own line and above the per-scanner breakdown. Every
	// other number in this transcript is a backlog; this one is a list of
	// vulnerabilities somebody is already being attacked through, and it must
	// not arrive as a clause inside a sentence about totals.
	if res.Posture.KEVs > 0 {
		progress.Log(LogError, fmt.Sprintf(
			"%d known-exploited %s in this release, %d of them fixable.",
			res.Posture.KEVs, plural(res.Posture.KEVs, "vulnerability", "vulnerabilities"),
			res.Posture.KEVFixable))
	}
	// What each scanner contributed, once there is more than one - and
	// specifically what only it found, because that is the number that says
	// whether running two was worth it.
	for _, sc := range res.Posture.BySource {
		line := fmt.Sprintf("%s reported %d vulnerabilities across %d advisories",
			ProviderLabel(sc.Provider), sc.Counts.Total, sc.UniqueCVEs)
		if sc.OnlyHere > 0 {
			line += fmt.Sprintf(", %d of which no other scanner reported", sc.OnlyHere)
		}
		progress.Log(LogInfo, line+".")
	}
	if malware := countMalware(res.Posture.Reports); malware > 0 {
		// Never buried in the totals. Everything else in this transcript is
		// something to schedule; this is something to stop.
		progress.Log(LogError, fmt.Sprintf(
			"%d malicious %s found in this release.",
			malware, plural(malware, "package", "packages")))
	}

	writeCtx, done := writeContext(ctx)
	defer done()
	if err := s.recorder.Record(writeCtx, PackageResult{
		PackageID: req.PackageID,
		// The scanners that ANSWERED, not the ones that were asked. A release
		// whose Anchore refused a credential is a release Xray scanned, and
		// recording both would put a row in the per-source breakdown for a
		// scanner that contributed nothing.
		Provider:    primaryProvider(res),
		Providers:   res.ProviderNames(),
		Repository:  req.Scope.Repository,
		Role:        req.Scope.Role,
		Posture:     res.Posture,
		Fingerprint: res.Fingerprint,
		Log:         progress.Entries(),
	}); err != nil {
		s.log.Error("could not record security sync", "package", req.PackageID, "error", err)
		return
	}

	s.log.Info("security sync complete",
		"package", req.PackageID, "label", req.Label,
		"providers", res.ProviderNames(),
		"artifacts", res.Posture.Coverage.Artifacts,
		"scanned", res.Posture.Coverage.Scanned,
		"vulnerabilities", res.Posture.Counts.Total,
		"kevs", res.Posture.KEVs,
		"fromCache", res.FromCache, "fetched", res.Fetched)
}

// primaryProvider is the scanner whose name goes on the release row.
//
// The first that answered, in the order they were asked - which puts Xray
// there on a deployment running both, because that is the order ProvidersFor
// returns and because it is the one a reader of a single-column listing expects
// to see. The full list travels beside it; nothing reads this to decide what
// was scanned.
func primaryProvider(res MultiResult) string {
	if len(res.Sources) > 0 {
		return res.Sources[0].Provider
	}
	return ""
}

// sortedProviders is the keys of a failure map, in a stable order, so a
// transcript reads the same on two runs of the same broken configuration.
func sortedProviders(failures map[string]error) []string {
	out := make([]string, 0, len(failures))
	for provider := range failures {
		out = append(out, provider)
	}
	sort.Strings(out)
	return out
}

// firstFailure is one scanner's refusal, as the reason a sync had nothing.
func firstFailure(failures map[string]error) string {
	for _, provider := range sortedProviders(failures) {
		return fmt.Sprintf("%s could not be asked about this release: %s",
			ProviderLabel(provider), failures[provider].Error())
	}
	return ""
}

// documentsOr prefers the request's own list, falling back to the syncer's.
//
// A request that names nothing means "whatever this deployment does", not
// "nothing" - the alternative is every caller having to remember to pass the
// configured list through, and the one that forgets ships a release with an
// empty malware tab and no indication why.
func documentsOr(request, configured []DocumentKind) []DocumentKind {
	if len(request) > 0 {
		return request
	}
	return configured
}

// fail records a run that gave up, on a context of its own.
func (s *Syncer) fail(ctx context.Context, packageID int64, reason string, log []SyncLogEntry) {
	writeCtx, done := writeContext(ctx)
	defer done()
	if err := s.recorder.Fail(writeCtx, packageID, reason, log); err != nil {
		s.log.Error("could not record security sync failure", "package", packageID, "error", err)
	}
}

// writeContext detaches the write that records an outcome from the run's own
// deadline.
//
// A sync that ran out of time has an expired context, and recording the failure
// through it would fail too - leaving the row claimed and the reader watching a
// sync that stopped half an hour ago. The outcome is the one thing that must
// still be written when the work has been abandoned.
func writeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// messageGroup is one thing the scanner said, and how many artifacts it said it
// about.
type messageGroup struct {
	status  Status
	message string
	count   int
	level   string
}

func (g messageGroup) line() string {
	noun := "artifacts"
	if g.count == 1 {
		noun = "artifact"
	}
	return fmt.Sprintf("%d %s. %s", g.count, noun, g.message)
}

// groupMessages collapses a release's reports into the distinct things that
// went wrong, worst first and largest first.
//
// # Why grouped
//
// A release of 261 artifacts against a struggling scanner produces 261
// messages and about three sentences. Logging them one per artifact is a
// transcript nobody reads and a page that cannot render; logging the three
// sentences with their counts is the same information in three lines, and it is
// the form somebody can act on: "132 artifacts: Xray could not be reached" is a
// network problem, "150 artifacts: not indexed in Xray" is an indexing one, and
// they have nothing to do with each other.
func groupMessages(reports []Report) []messageGroup {
	type key struct {
		status  Status
		message string
	}
	counts := map[key]int{}
	for _, r := range reports {
		if r.Status == StatusScanned || strings.TrimSpace(r.Message) == "" {
			continue
		}
		counts[key{r.Status, r.Message}]++
	}

	out := make([]messageGroup, 0, len(counts))
	for k, n := range counts {
		level := LogWarning
		if k.status == StatusUnavailable {
			level = LogError
		}
		if k.status == StatusUnsupported {
			level = LogInfo
		}
		out = append(out, messageGroup{status: k.status, message: k.message, count: n, level: level})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].level != out[j].level {
			return logRank(out[i].level) > logRank(out[j].level)
		}
		if out[i].count != out[j].count {
			return out[i].count > out[j].count
		}
		return out[i].message < out[j].message
	})
	return out
}

func logRank(level string) int {
	switch level {
	case LogError:
		return 2
	case LogWarning:
		return 1
	default:
		return 0
	}
}

// stillAnalysing is how many artifacts a scanner has accepted and not finished.
//
// # Why it reads the message rather than the status
//
// Because the status cannot tell them apart. "Not scanned" covers an image
// Anchore is actively analysing, one Xray has not got round to indexing, and
// one that was submitted and rejected - and only the provider knows which. The
// providers phrase the in-progress case consistently and this recognises that
// phrasing, which is a weaker coupling than a status of its own and does not
// require every provider to learn a state most of them do not have.
//
// A provider whose phrasing this does not recognise simply falls through to the
// original sentence, which is the correct failure and never a wrong one.
func stillAnalysing(reports []Report) int {
	n := 0
	for _, r := range reports {
		if r.Status != StatusNotScanned || r.Missing {
			continue
		}
		msg := strings.ToLower(r.Message)
		if strings.Contains(msg, "not finished analysing") || strings.Contains(msg, "analysing") {
			n++
		}
	}
	return n
}

// countMalware is how many malicious packages a release turned out to hold.
func countMalware(reports []Report) int {
	n := 0
	for _, r := range reports {
		n += len(r.Malware)
	}
	return n
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// labelOr names the release, falling back where the caller gave no label.
func labelOr(label, fallback string) string {
	if strings.TrimSpace(label) == "" {
		return fallback
	}
	return label
}

// firstMessage is whatever the provider said about the first artifact it could
// not answer for.
// One message rather than all of them: a release of three hundred artifacts
// against an unreachable scanner produces three hundred identical sentences,
// and the useful part is the sentence, not the count.
func firstMessage(reports []Report) string {
	for _, r := range reports {
		if r.Status == StatusUnavailable && r.Message != "" {
			return r.Message
		}
	}
	for _, r := range reports {
		if r.Message != "" {
			return r.Message
		}
	}
	return ""
}

// Describe renders a sync's position as a sentence, for a caller that has a
// stored state and no live progress.
func Describe(stages []SyncStage) string {
	if len(stages) == 0 {
		return "Starting"
	}
	last := stages[len(stages)-1]
	if last.Total > 0 {
		return fmt.Sprintf("%s %d of %d", last.Name, last.Done, last.Total)
	}
	return last.Name
}
