package security

import (
	"context"
	"errors"
	"time"
)

// Provider is the security-provider boundary: everything the core needs from a
// scanner, and nothing a particular scanner needs from the core.
//
// # Why this exists before there is a second scanner
//
// There is exactly one implementation today - JFrog Xray, inside the JFrog
// repository plugin - and this interface has one method that matters. That is
// not an accident of an unfinished design; it is the design. The cost of the
// boundary is one interface and one translation function per provider. The cost
// of NOT having it is that every handler, every cache row, every export column
// and every React component learns Xray's JSON shape, and the second scanner
// becomes a rewrite rather than a package.
//
// # What it deliberately does not have
//
// No credential methods, no configuration, no repository discovery. A provider
// is HANDED artifacts and answers about them. Where it gets its credentials
// from is the plugin's business - for Xray that is the JFrog repository's
// existing credential, unchanged and not duplicated.
type Provider interface {
	// Name identifies the scanner in every finding, badge and export column:
	// "jfrog-xray". Stable across versions - it is written into cache rows.
	Name() string

	// Enabled reports whether this provider will answer at all.
	//
	// A disabled provider must be asked nothing. It exists rather than being
	// absent so that the interface can say "Xray is switched off for this
	// repository", which is a different sentence from "this repository has no
	// scanner" and a very different one from "no vulnerabilities found".
	Enabled() bool

	// Scan returns one report per requested artifact, in any order.
	//
	// Implementations retrieve in parallel within their own configured
	// concurrency and rate limits - the core asks for a hundred artifacts at
	// once and does not know how many round trips that is.
	//
	// A per-artifact failure is a report with StatusUnavailable, NOT an error:
	// one image the scanner would not answer for must not lose the other
	// ninety-nine. An error return is reserved for a failure that makes the
	// whole request meaningless - a cancelled context, a credential the scanner
	// rejects outright.
	Scan(ctx context.Context, refs []ArtifactRef, opts ScanOptions) ([]Report, error)
}

// ScanOptions modulates one retrieval.
type ScanOptions struct {
	// Refresh bypasses any cache the provider holds and asks the scanner.
	// The user pressing "refresh" is the only thing that should set it.
	Refresh bool
	// Detail asks for full findings. When false the provider may return counts
	// only, which for Xray is one cheap call instead of one expensive one -
	// that is what a package listing's vulnerability column needs.
	Detail bool
	// Progress, when set, is told what is happening. May be called from several
	// goroutines; implementations of Progress must be safe for that.
	Progress Progress
	// Release names the release these artifacts belong to.
	//
	// # Why the core tells a provider what it is scanning
	//
	// Because one scanner groups by it. Xray indexes a repository and has no
	// notion of a release; Anchore has an Application/Version model that is
	// exactly a product and a release, and using it is what makes a hundred and
	// fifty images legible as one thing in Anchore's own interface - and what
	// unlocks its release-level vulnerability report.
	//
	// A provider that has no use for it ignores it, which is the ordinary case
	// and costs nothing. The alternative - a provider built per release rather
	// than per repository - would rebuild a credential and a transport for
	// every release on a listing.
	Release ReleaseRef
	// Sink, when set, receives the scanner's own bodies as they arrive.
	//
	// # Why the raw payload rides out on a callback rather than on the Report
	//
	// Because it is large, it is wanted rarely, and it must not be fetched
	// twice. A Report is serialized into the cache on every sync, so a raw body
	// on it would multiply the stored size of every release by ten to serve a
	// download somebody presses once a month. A second fetch at download time
	// is the fifteen-minute sync all over again, behind a button somebody
	// expects to be instant.
	//
	// A sink is neither: the body is captured on the request that was going to
	// happen anyway, and the caller decides whether to keep it.
	//
	// May be called from several goroutines.
	Sink DocumentSink
}

// ReleaseRef names the release a scan is about, in the platform's own terms.
//
// Deliberately three plain strings. A provider that groups by release needs
// what a PERSON calls it - so that somebody can find their release in the
// scanner's own interface by typing what they call it here - and nothing
// internal would do.
type ReleaseRef struct {
	// Product is the product's configured name.
	Product string
	// Version is the release's version, as the vendor publishes it.
	Version string
	// Label is the release as the interface names it, for progress messages.
	Label string
}

// Named reports whether there is enough here to group by.
func (r ReleaseRef) Named() bool { return r.Product != "" && r.Version != "" }

// DocumentSink receives raw scanner bodies as a scan produces them.
type DocumentSink interface {
	// Document is handed one body. Must be safe to call from several
	// goroutines, and must not block for long - it is called on the request
	// path, and a sink that waits on a database write is a sink that slows the
	// scan down.
	Document(Document)
}

// DocumentSinkFunc adapts a function to DocumentSink.
type DocumentSinkFunc func(Document)

// Document implements DocumentSink.
func (f DocumentSinkFunc) Document(d Document) { f(d) }

// Progress reports what a retrieval is doing, so the interface can say
// "fetching 42 of 157 from JFrog Xray" instead of spinning.
//
// An interface rather than a channel because the reporters are various - an
// HTTP progress endpoint, a test, nothing at all - and because a nil Progress
// must be free. Use Report's helper rather than calling a possibly-nil value.
type Progress interface {
	// Stage names what phase the work is in, and how far through it is.
	// total <= 0 means "not known yet", which is the honest state while a tree
	// is still being walked.
	Stage(name string, done, total int)
	// Note records something worth saying that is not a position - "Xray is
	// disabled for internal-jfrog", "served from cache".
	Note(string)
}

// ReportStage calls p.Stage when p is not nil.
func ReportStage(p Progress, name string, done, total int) {
	if p != nil {
		p.Stage(name, done, total)
	}
}

// ReportNote calls p.Note when p is not nil.
func ReportNote(p Progress, note string) {
	if p != nil {
		p.Note(note)
	}
}

// Detailed is an optional extension of Progress: a reporter that can be told
// how important a line is, and whether it replaces the last one like it.
//
// # Why an extension rather than a wider Progress
//
// Because a Progress may be a test, an HTTP endpoint, or nothing, and two of
// those have no notion of a level. The one implementation that renders a
// transcript wants three things the others do not, and widening the interface
// to suit it would put three stub methods in every other implementation.
//
// # What "replace" is for
//
// A line that says where the work has got to - "retrieved scan results for 96
// of 157 images" - arrives thirty times with a different number in it. Counted
// as repeats it reads "(x30)" beside a number that was never wrong; it is ONE
// line whose value changes, and that is what replace says.
type Detailed interface {
	// Record adds a line. `replace` overwrites the last line of the same shape
	// instead of counting it as a repeat.
	Record(level string, replace bool, note string)
}

// Working is an optional extension of Progress: a reporter that can name what
// is in a worker's hands right now.
//
// # Why the names are worth carrying
//
// A bar and a count answer "how far". They do not answer the question somebody
// actually asks in front of a job that has not moved for a minute, which is
// "is this working at all" - and a list of names that changes every few seconds
// answers it however still the bar is. It is also the only thing that
// distinguishes a slow registry from a wedged one.
type Working interface {
	// Begin records that something is now being worked on, End that it is not.
	Begin(name string)
	End(name string)
	// SetConcurrency records how many may be in flight at once, so a watcher
	// can tell one-at-a-time from sixteen-at-a-time.
	SetConcurrency(n int)
}

// Statusing is an optional progress extension for work that reports a state
// for every completed item.
type Statusing interface {
	SetStatuses(map[string]int)
}

// ReportBegin names something now being worked on, when p can carry one.
func ReportBegin(p Progress, name string) {
	if w, ok := p.(Working); ok && p != nil {
		w.Begin(name)
	}
}

// ReportEnd retires a name ReportBegin put up.
func ReportEnd(p Progress, name string) {
	if w, ok := p.(Working); ok && p != nil {
		w.End(name)
	}
}

// ReportConcurrency records how many things may be in flight at once.
func ReportConcurrency(p Progress, n int) {
	if w, ok := p.(Working); ok && p != nil {
		w.SetConcurrency(n)
	}
}

// ReportStatuses records the current counts by scanner-reported state.
func ReportStatuses(p Progress, statuses map[string]int) {
	if s, ok := p.(Statusing); ok && p != nil {
		s.SetStatuses(statuses)
	}
}

// ReportInfo records something the reader wants to know that is not a problem.
//
// The distinction matters more than it looks. Every note used to be written at
// warning level, so "requesting scan results for 157 images, skipping 103 that
// are not container images" - a sentence describing a sync doing exactly what
// it should - arrived in the transcript wearing the same colour as a scanner
// that could not be reached.
func ReportInfo(p Progress, note string) { record(p, LevelInfo, false, note) }

// ReportProgress records where the work has got to, REPLACING the last such
// line rather than stacking beside it.
func ReportProgress(p Progress, note string) { record(p, LevelInfo, true, note) }

// ReportWarning records something that went wrong but did not stop the work.
func ReportWarning(p Progress, note string) { record(p, LevelWarning, false, note) }

// ReportWarningUpdate records a recurring problem as one line that updates -
// a scanner backing off says the same thing with a smaller number each time,
// and stacking those tellings turns one situation into a list of failures.
func ReportWarningUpdate(p Progress, note string) { record(p, LevelWarning, true, note) }

func record(p Progress, level string, replace bool, note string) {
	if p == nil {
		return
	}
	if d, ok := p.(Detailed); ok {
		d.Record(level, replace, note)
		return
	}
	p.Note(note)
}

// Log levels, named here rather than in sync.go so a provider can reach them
// without importing the syncer's vocabulary.
const (
	LevelInfo    = "info"
	LevelSuccess = "success"
	LevelWarning = "warning"
	LevelError   = "error"
)

// Stage names. Constants rather than literals because the interface renders a
// specific sentence per stage and a typo would render nothing.
const (
	StageResolving   = "resolving"
	StageFetching    = "fetching"
	StageCached      = "cached"
	StageCorrelating = "correlating"
	StageScanning    = "scanning"
	StageComparing   = "comparing"
	StageExporting   = "exporting"
	// StageFailing counts the artifacts the scanner would not answer for.
	//
	// A stage rather than a note, because it is a POSITION - it goes up as the
	// work proceeds - and because "142 of 258" tells a watcher the work is
	// moving and nothing else. On a scanner that is timing out, the number
	// beside it is the one that matters.
	StageFailing = "failing"
)

// Resolver finds the providers that can answer for a repository.
//
// Separated from Provider because "which scanner covers this artifact" is a
// configuration question and "what does that scanner say" is not. The
// implementation lives beside the registry factory, where the configured
// repositories already are.
type Resolver interface {
	// ProviderFor returns the provider named by the scope.
	//
	// # Why this takes a scope rather than a product and a repository
	//
	// Because a repository can have more than one scanner, and the scope is
	// already the thing that says which one: it is the storage key, the
	// authorization boundary, and now the address of an answerer. Before there
	// was a second scanner, "the provider for this repository" was
	// unambiguous; a signature that stayed that way would have made the
	// SECOND scanner's rows land in the first one's storage.
	//
	// Returns ErrNoProvider when the repository has no such scanner - which is
	// an ordinary answer and not a failure.
	ProviderFor(ctx context.Context, scope Scope) (Provider, error)

	// ProvidersFor names every scanner switched on for one repository, in the
	// order they should be asked.
	//
	// A separate call rather than a list of providers, because building one is
	// a credential resolution and a transport, and the caller that needs this
	// most - the sync, deciding what work there is - needs the NAMES before it
	// decides to do any of it. An empty list is an ordinary answer: a
	// repository with no scanner is a correctly configured system.
	ProvidersFor(ctx context.Context, product, repository string) ([]string, error)
}

// ErrNoProvider means no scanner covers the repository. Callers turn it into a
// disabled report rather than an error page: a Quay source with no scanner is a
// correctly configured system.
var ErrNoProvider = errors.New("security: no provider is configured for this repository")

// Disabled is the provider used where a repository has a scanner that is
// switched off. It answers, and every answer says so.
//
// A null object rather than a nil check at every call site, because the nil
// check is the one that gets forgotten and the failure mode of forgetting it is
// a release that looks clean.
type Disabled struct {
	// ProviderName is the scanner that would have answered.
	ProviderName string
	// Reason is what to tell the user - "Xray is not enabled for repository
	// jfrog-prod".
	Reason string
}

// Name implements Provider.
func (d Disabled) Name() string {
	if d.ProviderName == "" {
		return "none"
	}
	return d.ProviderName
}

// Enabled implements Provider. Always false, which is the point.
func (d Disabled) Enabled() bool { return false }

// Scan implements Provider: one disabled report per artifact, no I/O.
func (d Disabled) Scan(_ context.Context, refs []ArtifactRef, opts ScanOptions) ([]Report, error) {
	ReportNote(opts.Progress, d.Reason)
	now := time.Now().UTC()
	out := make([]Report, 0, len(refs))
	for _, ref := range refs {
		out = append(out, Report{
			Artifact:    ref,
			Status:      StatusDisabled,
			Provider:    d.Name(),
			Message:     d.Reason,
			RetrievedAt: now,
		})
	}
	return out, nil
}
