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
}

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

// Stage names. Constants rather than literals because the interface renders a
// specific sentence per stage and a typo would render nothing.
const (
	StageResolving   = "resolving"
	StageFetching    = "fetching"
	StageCached      = "cached"
	StageCorrelating = "correlating"
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

// Resolver finds the provider that can answer for a repository.
//
// Separated from Provider because "which scanner covers this artifact" is a
// configuration question and "what does that scanner say" is not. The
// implementation lives beside the registry factory, where the configured
// repositories already are.
type Resolver interface {
	// ProviderFor returns the provider covering one configured repository of
	// one product. Returns ErrNoProvider when the repository has no scanner -
	// which is an ordinary answer and not a failure.
	ProviderFor(ctx context.Context, product, repository string) (Provider, error)
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
