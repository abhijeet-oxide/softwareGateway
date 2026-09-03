// Package anchore reaches Anchore Enterprise and answers in the platform's own
// security vocabulary.
//
// See docs/security/Anchore.md for the integration methodology this implements
// and docs/design/21-security-posture.md §12 for the boundary it sits behind.
//
// # Why this is a package of its own and Xray is not
//
// Xray lives inside the JFrog repository plugin because it is not a separate
// system: it is a second endpoint on a platform this codebase already speaks
// to, reached with the credential the repository already holds. Reaching it
// from anywhere else would mean a second credential for one host and a second
// route to it, and the day those two disagree is the day Xray reports nothing
// while replication works perfectly.
//
// Anchore genuinely is a separate system. It has its own address, its own
// credential, its own account model, and - the part that shapes this whole
// package - it does not scan a repository. It PULLS an image, one at a time,
// on its own schedule, and only after somebody has told it the image exists.
// So this package does three things Xray's does not:
//
//  1. It SUBMITS. An image Anchore has never been told about produces no
//     findings, not because it is clean but because nothing has looked. The
//     sync submits, and it says so.
//  2. It WAITS. Analysis is asynchronous and minutes long. A sync that read
//     the vulnerability endpoint the instant after submitting would report a
//     release as unscanned every time it was first synced.
//  3. It GROUPS. A release here is one thing to a person and several images to
//     a registry, and Anchore's Application/Version model is exactly that
//     shape - so a release becomes an Application Version with its images
//     associated to it, and the release is one row in Anchore's own interface
//     rather than a hundred and fifty-seven unrelated ones.
//
// # What it deliberately does not do
//
// It does not replicate anything. Anchore pulls the image from the registry the
// release was already replicated to, using registry credentials configured
// inside Anchore - which is the one place they can be, because it is Anchore's
// own network making the request. This package's job is to name the image;
// somebody else's job is to have put it there.
package anchore

import "time"

// ProviderName is what every finding from this package is stamped with.
//
// It reaches stored rows, export columns and the interface, and it is part of
// the storage key for every scan, detail and document row this provider
// writes - so it does not change. Ever. Changing it would orphan every finding
// synced before the change and report the release as unscanned.
const ProviderName = "anchore"

// Label is the scanner as a person writes it.
const Label = "Anchore"

// The API this package speaks.
//
// Anchore serves its v2 API under a `/v2` prefix (the OpenAPI document's only
// declared server). Configuration names the platform base URL - the thing an
// operator has in a browser - and the prefix is appended here, because a
// configuration field that has to end in `/v2` is a configuration field that
// half the time does not.
const apiPrefix = "/v2"

// Analysis states Anchore reports for an image.
//
// Strings rather than an enum because they are wire values, and a state this
// build has not heard of must be carried through and shown rather than mapped
// onto one of these and acted on wrongly.
const (
	// AnalysisNotAnalyzed means Anchore has the record and has not started.
	AnalysisNotAnalyzed = "not_analyzed"
	// AnalysisAnalyzing means it is working. Neither a success nor a failure,
	// and the state a first sync of a release spends most of its time in.
	AnalysisAnalyzing = "analyzing"
	// AnalysisAnalyzed is the only state whose vulnerabilities are complete.
	AnalysisAnalyzed = "analyzed"
	// AnalysisFailed is terminal. The image is not going to be analysed
	// without somebody looking at why.
	AnalysisFailed = "analysis_failed"
)

// terminal reports whether an analysis state will not change on its own.
func terminal(status string) bool {
	switch status {
	case AnalysisAnalyzed, AnalysisFailed:
		return true
	}
	return false
}

// Defaults for the Anchore path.
//
// Every one of them is a ceiling rather than a target, and the numbers differ
// from Xray's because the requests differ: Anchore answers per image, not per
// batch of fifty, so a release is hundreds of small requests rather than a
// handful of large ones.
const (
	// DefaultRequestTimeout bounds one call end to end. Lower than Xray's,
	// because no single Anchore call summarises fifty images - the biggest one
	// is a single image's vulnerability list.
	DefaultRequestTimeout = 60 * time.Second

	// DefaultConcurrency is how many image requests may be in flight.
	//
	// Higher than Xray's ten, because these are per-image reads rather than
	// server-side aggregations over fifty checksums, and because a release of
	// a hundred and fifty images IS a hundred and fifty requests here - a
	// concurrency of six would make a first sync twenty-five serial minutes.
	// Bounded all the same: Anchore's policy engine is the expensive part of
	// its estate and this is somebody else's capacity.
	DefaultConcurrency = 12

	// NOTHING HERE WAITS FOR ANALYSIS, and there is deliberately no knob for
	// how long it would.
	//
	// There was one - a sync submitted every image and then waited up to ten
	// minutes for Anchore to finish. The number was defensible (a typical
	// container image analyses in one to three minutes, in parallel) and the
	// design was not: Anchore analyses on its own schedule and nobody can
	// promise a bound on it, so a sync that waited had its duration set by
	// somebody else's queue, held a claim on the release throughout, and
	// reported the release as unscanned every time the wait ran out.
	//
	// Registering a release and reading its results are now separate acts.
	// Register returns as soon as Anchore has been told; a sync reads whatever
	// has finished. Neither blocks on the other, and a knob for how long to
	// wait would be a knob for how long to be wrong.

	// The retry budget, kept well inside one request timeout so a failing
	// Anchore surfaces as Anchore's own error rather than as our deadline.
	retryAttempts  = 3
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 4 * time.Second
)
