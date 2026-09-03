package security

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Registering a release with a scanner that has to be TOLD about it.
//
// # Why this is a separate act, and not a phase of a sync
//
// It was a phase of a sync, and the argument against that is operational rather
// than architectural: analysis takes as long as it takes. Anchore pulls an image
// and analyses it on its own schedule, and on a busy deployment or a large image
// that is not ten minutes - there is no bound anybody can promise. A sync that
// submitted and then waited was a sync whose duration was set by somebody else's
// queue, holding a claim on the release the whole time, and reporting a release
// as unscanned every time the wait ran out.
//
// So the two are separated at the seam where they genuinely differ:
//
//	REGISTER  tell the scanner these images exist, group them, and return.
//	          Seconds. Bounded by our own request count, not by analysis.
//	SYNC      read whatever the scanner has finished. Never submits, never
//	          waits, and is exactly as fast as reading.
//
// Register is idempotent to the bone, because it is a button somebody presses
// again when they are not sure it worked: an image already submitted is not
// resubmitted, an application that exists is reused, an association already
// there is left alone.
//
// # Why association does NOT wait for analysis
//
// The integration guide says to associate only analysed images, and the reason
// it gives is sound: a version holding unanalysed images reports less than it
// appears to. The reason to do it anyway is stronger. Association is what makes
// the release exist in the scanner's own interface, and deferring it until
// analysis finishes means the thing a person goes to Anchore to look at does
// not appear until the analysis they are waiting for is already done - which is
// precisely when they no longer need it. Associating up front, the version fills
// in as its images finish, and the platform's own coverage numbers say how much
// of it is answerable yet.

// Registrar is a scanner that must be told about an artifact before it can
// answer about one.
//
// An optional extension of Provider rather than more methods on it, for the
// same reason DocumentProvider is: Xray indexes a repository and has nothing to
// register, and an interface carrying a method its main implementation must stub
// is an interface that lies about what its implementations do.
type Registrar interface {
	Provider

	// Register tells the scanner about these artifacts and groups them, and
	// returns as soon as that is done - never waiting for analysis.
	//
	// A per-artifact failure is recorded in the result, never returned as an
	// error: one image the scanner would not accept must not lose the other
	// hundred and fifty-six. An error return is reserved for a failure that
	// makes the whole request meaningless - a cancelled context, a credential
	// the scanner rejects outright.
	Register(ctx context.Context, refs []ArtifactRef, opts RegisterOptions) (Registration, error)

	// RegistrationFor reports what the scanner currently holds for a release,
	// without changing anything.
	//
	// Used to answer "is this still registered" against the scanner rather than
	// against our own record - for the case where somebody deleted the
	// application in Anchore, which our record cannot know about.
	RegistrationFor(ctx context.Context, refs []ArtifactRef, opts RegisterOptions) (Registration, error)
}

// RegisterOptions modulates one registration.
type RegisterOptions struct {
	// Release names the release, which is what the scanner's own grouping is
	// built from - the product's name and the release's version.
	Release ReleaseRef
	// Progress reports what is happening, and may be nil.
	Progress Progress
}

// RegistrationState is how far a release's registration got.
type RegistrationState string

const (
	// RegistrationNone means nobody has registered this release.
	RegistrationNone RegistrationState = ""
	// RegistrationRunning means a registration is under way.
	RegistrationRunning RegistrationState = "registering"
	// RegistrationComplete means every image was accepted and grouped.
	RegistrationComplete RegistrationState = "registered"
	// RegistrationPartial means some images were accepted and some were not.
	//
	// Its own state rather than a failure, because it is the ordinary outcome
	// of registering a release that is still being transferred: the images that
	// have landed are registered and answerable, and the rest need the button
	// pressing again once they arrive. Reporting that as failed would hide the
	// half that worked.
	RegistrationPartial RegistrationState = "partial"
	// RegistrationFailed means the scanner could not be reached, or refused.
	RegistrationFailed RegistrationState = "failed"
)

// Label is the state in the words the interface shows.
func (s RegistrationState) Label() string {
	switch s {
	case RegistrationRunning:
		return "Replicating"
	case RegistrationComplete:
		return "Replicated"
	case RegistrationPartial:
		return "Partly replicated"
	case RegistrationFailed:
		return "Replication failed"
	default:
		return "Not replicated"
	}
}

// Done reports whether this state means the scanner has everything.
func (s RegistrationState) Done() bool { return s == RegistrationComplete }

// Registration is what a scanner holds for one release.
type Registration struct {
	// Provider is the scanner - "anchore".
	Provider string            `json:"provider"`
	State    RegistrationState `json:"state"`

	// Expected is the artifacts this release wanted registered, Submitted the
	// ones this run told the scanner about, and AlreadyKnown the ones it had.
	//
	// The three are reported separately because a second press of the button
	// should visibly do nothing, and "submitted 0, already known 157" is how a
	// reader sees that rather than wondering whether it ran.
	Expected     int `json:"expected"`
	Submitted    int `json:"submitted"`
	AlreadyKnown int `json:"alreadyKnown"`
	// Skipped is how many were never offered, because an earlier failure
	// established that none of them could succeed. Counted inside Failed, and
	// reported separately so the message can say the run stopped early rather
	// than implying every image was tried.
	Skipped int `json:"skipped,omitempty"`

	// Associated is how many of them the scanner's own grouping holds, read
	// BACK rather than assumed. A successful write is not evidence of the final
	// state, and a group holding three quarters of a release reports three
	// quarters of the truth while reading like all of it.
	Associated int `json:"associated"`

	// Application and Version are the scanner's own names and identifiers for
	// this release, and URL is where a person opens it there.
	Application   string `json:"application,omitempty"`
	ApplicationID string `json:"applicationId,omitempty"`
	Version       string `json:"version,omitempty"`
	VersionID     string `json:"versionId,omitempty"`
	URL           string `json:"url,omitempty"`

	// Failed maps an artifact's reference to why the scanner would not take it.
	Failed map[string]string `json:"failed,omitempty"`
	// Message explains a state that is not complete, in words with an action.
	Message string `json:"message,omitempty"`

	// Analysed is how many registered images the scanner has finished with, at
	// the moment this was read.
	//
	// Reported so the page can say "157 registered, 40 analysed so far" - which
	// is the honest answer to "why has the sync not found anything yet" and the
	// reason nothing here waits.
	Analysed int `json:"analysed"`
	// Outcomes keeps full target pull strings for the finished replication
	// record, so a partial outcome can be inspected and retried without
	// reconstructing image paths from an aggregate error.
	Outcomes RegistrationOutcomes `json:"outcomes,omitempty"`

	At time.Time `json:"at"`
}

// RegistrationOutcomes is the per-image evidence from one replication.
type RegistrationOutcomes struct {
	Replicated []string `json:"replicated,omitempty"`
	Analysed   []string `json:"analysed,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

// Complete reports whether every expected artifact is registered and grouped.
func (r Registration) Complete() bool {
	return r.Expected > 0 && len(r.Failed) == 0 && r.Associated >= r.Expected
}

// Outstanding is how many artifacts the scanner still does not hold.
func (r Registration) Outstanding() int {
	n := r.Expected - r.Associated
	if n < 0 {
		return 0
	}
	return n
}

// Settle decides the state from the counts, so one rule produces it everywhere.
func (r *Registration) Settle() {
	switch {
	case r.Expected == 0:
		r.State = RegistrationComplete
	case r.Associated == 0 && len(r.Failed) > 0:
		r.State = RegistrationFailed
	case r.Complete():
		r.State = RegistrationComplete
	default:
		r.State = RegistrationPartial
	}
}

// FailedRefs is the artifacts the scanner would not take, sorted, so a message
// and a stored row read the same on two runs.
func (r Registration) FailedRefs() []string {
	out := make([]string, 0, len(r.Failed))
	for ref := range r.Failed {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// FirstFailure is one reason the scanner gave, for a one-line message.
//
// One rather than all of them: a release of three hundred images against a
// scanner that cannot reach the registry produces three hundred identical
// sentences, and the useful part is the sentence.
//
// Counted, because the count is what says how much of the release this is
// about. "1 of 154 images failed" and "154 of 154 images failed" carry the same
// reason and mean entirely different things, and a message that gave only the
// reason made the second look like the first.
func (r Registration) FirstFailure() string {
	reason := ""
	for _, ref := range r.FailedRefs() {
		// The reason an image was SKIPPED is a consequence of the real one, and
		// leading with it would report the symptom as the cause.
		if reason == "" || strings.HasPrefix(reason, skippedPrefix) {
			reason = r.Failed[ref]
		}
		if !strings.HasPrefix(reason, skippedPrefix) {
			break
		}
	}
	if reason == "" {
		return ""
	}
	count := ""
	if total := r.Expected; total > 0 {
		count = fmt.Sprintf("%d of %d images failed replication. ", len(r.Failed), total)
	}
	if r.Skipped > 0 {
		count += fmt.Sprintf("%d were not submitted after the first failure. ", r.Skipped)
	}
	return count + reason
}

// skippedPrefix marks a failure that is a consequence rather than a cause.
const skippedPrefix = "Not submitted:"
