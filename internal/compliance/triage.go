package compliance

// Triage vocabulary: the four things a reader needs in order to act on a
// finding without already knowing Kubernetes.
//
// # Why these are enums and not free text
//
// Every one of them is a field somebody sorts or filters a report by. A free
// string produces "Chart", "chart template", "Chart Template" and "the chart"
// in one export, and a column nobody can group by is a column nobody uses.
// A closed set also means the catalogue page can offer them as filters, and
// that a pack author who invents a fifth value is told so at load rather than
// discovering it in a spreadsheet.

// Confidence says how firmly a check can assert its finding from the delivered
// artifacts alone.
//
// # What this is for
//
// The largest category of unproductive argument about a compliance report is
// the finding that is technically correct and contextually wrong: the tool
// reports an absent NetworkPolicy, the platform applies one centrally, and the
// vendor concludes the tool is unreliable. Confidence puts that on the record
// before the argument starts - the finding says, in the row, that it depends on
// a fact the tool cannot see.
//
// It also carries a rule with teeth: a `needs-review` check may not be
// blocking. See Check.Validate.
type Confidence string

const (
	// ConfidenceConfirmed means the manifest says so. No assumption, nothing to
	// supply: `runAsUser: 0` is root, and there is no context that changes it.
	ConfidenceConfirmed Confidence = "confirmed"
	// ConfidenceProbable means the manifest says so and a platform fact could
	// still make it harmless - a missing NetworkPolicy where the cluster
	// applies one for every namespace.
	ConfidenceProbable Confidence = "probable"
	// ConfidenceNeedsReview means the condition may be exactly what this
	// workload requires, and somebody who knows the workload has to say. A
	// dataplane asking for host network access is the shape.
	ConfidenceNeedsReview Confidence = "needs-review"
)

// Label is the confidence in the words the report uses.
func (c Confidence) Label() string {
	switch c {
	case ConfidenceConfirmed:
		return "Confirmed from the chart"
	case ConfidenceProbable:
		return "Likely, unless the platform provides it"
	case ConfidenceNeedsReview:
		return "Needs someone who knows this workload"
	default:
		return ""
	}
}

// Valid reports whether c is one of the three, or empty.
func (c Confidence) Valid() bool {
	switch c {
	case "", ConfidenceConfirmed, ConfidenceProbable, ConfidenceNeedsReview:
		return true
	}
	return false
}

// Timing is when the consequence of a finding actually arrives.
//
// It is what makes a list of findings orderable by urgency rather than only by
// severity. "Happens on every upgrade" and "happens if a node fails" are both
// serious and they are not the same week's work.
type Timing string

const (
	// TimingInstall - the first install fails, or comes up wrong.
	TimingInstall Timing = "install"
	// TimingUpgrade - it bites on the next helm upgrade.
	TimingUpgrade Timing = "upgrade"
	// TimingMaintenance - it bites when a server is drained for patching.
	TimingMaintenance Timing = "node-maintenance"
	// TimingLoad - it bites under load, or as data grows.
	TimingLoad Timing = "under-load"
	// TimingFailure - it bites when something else has already failed, which is
	// the worst time to discover it.
	TimingFailure Timing = "on-failure"
	// TimingAlways - it is true the whole time the release is running, which is
	// what most security exposure is.
	TimingAlways Timing = "continuously"
)

// Label is the timing as a sentence fragment a report can print after "Bites".
func (t Timing) Label() string {
	switch t {
	case TimingInstall:
		return "when the release is installed"
	case TimingUpgrade:
		return "on the next upgrade"
	case TimingMaintenance:
		return "when a server is taken out for maintenance"
	case TimingLoad:
		return "under load"
	case TimingFailure:
		return "when something else has already failed"
	case TimingAlways:
		return "the whole time the release is running"
	default:
		return ""
	}
}

// Valid reports whether t is one of the six, or empty.
func (t Timing) Valid() bool {
	switch t {
	case "", TimingInstall, TimingUpgrade, TimingMaintenance, TimingLoad, TimingFailure, TimingAlways:
		return true
	}
	return false
}

// FixOwner is who changes something to make the finding go away.
//
// # Why "could not be established" is not one of the values
//
// It was, in an earlier report, in a column headed "Owner" that actually held
// something else entirely, and roughly a third of the rows read "Could not be
// established". A reader cannot route that anywhere. Where ownership genuinely
// depends on the site, the value is FixOwnerDecision and the rationale names
// who decides - which is a sentence somebody can act on.
type FixOwner string

const (
	// FixOwnerTemplate - the chart's own templates. The vendor changes it.
	FixOwnerTemplate FixOwner = "chart-template"
	// FixOwnerValues - the values file. Whoever installs it changes it, with no
	// new chart.
	FixOwnerValues FixOwner = "chart-values"
	// FixOwnerApplication - the software itself has to change: a probe endpoint
	// that does not exist yet, an image that cannot run as a non-root user.
	FixOwnerApplication FixOwner = "application"
	// FixOwnerPipeline - the build or release pipeline: digests, registries,
	// provenance.
	FixOwnerPipeline FixOwner = "build-pipeline"
	// FixOwnerPlatform - the cluster's owners, not the vendor. Reported anyway,
	// attributed correctly: silence is worse than a finding somebody else owns.
	FixOwnerPlatform FixOwner = "platform-team"
	// FixOwnerDecision - somebody has to decide before anybody can act.
	FixOwnerDecision FixOwner = "needs-decision"
)

// Label is the owner in the words the report uses.
func (f FixOwner) Label() string {
	switch f {
	case FixOwnerTemplate:
		return "The chart's templates"
	case FixOwnerValues:
		return "The values file"
	case FixOwnerApplication:
		return "The application itself"
	case FixOwnerPipeline:
		return "The build pipeline"
	case FixOwnerPlatform:
		return "The platform team"
	case FixOwnerDecision:
		return "Needs a decision first"
	default:
		return ""
	}
}

// Valid reports whether f is one of the six, or empty.
func (f FixOwner) Valid() bool {
	switch f {
	case "", FixOwnerTemplate, FixOwnerValues, FixOwnerApplication,
		FixOwnerPipeline, FixOwnerPlatform, FixOwnerDecision:
		return true
	}
	return false
}

// FixEffort is how much work the fix is, so a reader can plan a release rather
// than discover its cost one ticket at a time.
type FixEffort string

const (
	// FixEffortLow - a few lines of YAML, no application change.
	FixEffortLow FixEffort = "low"
	// FixEffortMedium - a chart change with something to verify: a new object,
	// a rollout to watch, a value to agree.
	FixEffortMedium FixEffort = "medium"
	// FixEffortHigh - the software or the pipeline has to change.
	FixEffortHigh FixEffort = "high"
)

// Label is the effort as the report prints it.
func (f FixEffort) Label() string {
	switch f {
	case FixEffortLow:
		return "Low - a few lines of YAML"
	case FixEffortMedium:
		return "Medium - a chart change to verify"
	case FixEffortHigh:
		return "High - the software or pipeline changes"
	default:
		return ""
	}
}

// Valid reports whether f is one of the three, or empty.
func (f FixEffort) Valid() bool {
	switch f {
	case "", FixEffortLow, FixEffortMedium, FixEffortHigh:
		return true
	}
	return false
}
