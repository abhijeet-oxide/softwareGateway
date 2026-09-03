package config

import "strings"

// The stage vocabulary: what a task IS.
//
// This platform has no built-in notion of download, onboard or promote. It has
// TASKS, and every one of them is a line in system configuration. A task moves
// a release from one stage to another, says what to check on the way, and names
// the scanners that run once it lands.
//
// The consequence is the point: a site that adds a fourth task gets a fourth
// action in the interface, and a site that renames `Promote to Prod` renames
// the button. Neither is a code change, and nothing in the API, the interface
// or the database contains the string "promote".

// CheckMode is how strictly a task applies one of its checks.
//
// Three values, and the middle one is the reason there are three: a check
// somebody wants to SEE but not be stopped by is the normal way a check is
// introduced to an estate that has never run it.
type CheckMode string

const (
	// CheckDisabled does not run the check, and records nothing.
	CheckDisabled CheckMode = "disabled"
	// CheckEnabled runs the check and records the result. A failure is
	// reported and the task continues.
	CheckEnabled CheckMode = "enabled"
	// CheckEnforce runs the check and stops the task when it fails.
	CheckEnforce CheckMode = "enforce"
)

// ValidCheckModes is the closed set, for validation messages.
var ValidCheckModes = []CheckMode{CheckDisabled, CheckEnabled, CheckEnforce}

// Runs reports whether the check executes at all.
func (m CheckMode) Runs() bool { return m == CheckEnabled || m == CheckEnforce }

// Blocks reports whether a failure stops the task.
func (m CheckMode) Blocks() bool { return m == CheckEnforce }

// Normalize returns the mode with an empty value resolved to disabled.
//
// Absent means OFF for a check, inverting this schema's usual "absent means on"
// convention, and deliberately: these checks reach third systems and cost
// minutes, so a task that never mentioned one should not silently acquire it.
func (m CheckMode) Normalize() CheckMode {
	switch CheckMode(strings.ToLower(strings.TrimSpace(string(m)))) {
	case CheckEnabled:
		return CheckEnabled
	case CheckEnforce:
		return CheckEnforce
	default:
		return CheckDisabled
	}
}

// SourceStage is the reserved `from` naming a product's own sources rather than
// one of its stages. Legal on the first task only.
const SourceStage = "source"

// StageConfig holds the ordered tasks a release can go through.
type StageConfig struct {
	Tasks []Task `koanf:"tasks"`
}

// Task is one move a release can make.
type Task struct {
	// Name is the verb: `download`, `onboard`, `promote`. It appears in the
	// API path and in the audit trail, so it is a resource ID rather than
	// prose - lowercase, hyphens, no spaces.
	Name string `koanf:"name"`

	// DisplayName is what the button says. Prose, and the only field here a
	// reader ever sees.
	DisplayName string `koanf:"displayName"`

	// From and To are stage names, matched against a target's `stage`. From
	// may be SourceStage on the first task.
	From string `koanf:"from"`
	To   string `koanf:"to"`

	// Purge deletes the release from the `from` stage once `to` holds it.
	//
	// Off by default, and that direction is deliberate: a deletion nobody
	// asked for is not recoverable by re-reading the configuration, and the
	// estate that wants a landing zone emptied should have to say so.
	Purge bool `koanf:"purge"`

	// Verify and Compliance are the checks this task applies.
	//
	// A check this build cannot run is recorded as SKIPPED and the task
	// proceeds, whatever the mode says. That is what lets a site write
	// `verify: enforce` today, before signature verification exists, and have
	// it start enforcing the day it ships without an edit here.
	Verify     CheckMode `koanf:"verify"`
	Compliance CheckMode `koanf:"compliance"`

	// Scanners names the scanners that run once the release has landed, keyed
	// into the deployment's `scanners:` block.
	Scanners []string `koanf:"scanners"`
}

// FromSource reports whether this task reads the product's sources rather than
// an earlier stage.
func (t Task) FromSource() bool {
	return strings.EqualFold(strings.TrimSpace(t.From), SourceStage)
}

// Label is what to show for this task, falling back to its name.
//
// A task with no displayName is a misconfiguration rather than a crash: the
// name is a poor button but an honest one, and refusing to render the action at
// all would hide a working task over a cosmetic omission.
func (t Task) Label() string {
	if d := strings.TrimSpace(t.DisplayName); d != "" {
		return d
	}
	return t.Name
}

// Task looks one up by name.
func (s StageConfig) Task(name string) (Task, bool) {
	for _, t := range s.Tasks {
		if t.Name == name {
			return t, true
		}
	}
	return Task{}, false
}

// Normalized returns the tasks with check modes resolved, so no consumer has to
// ask what an empty string meant.
func (s StageConfig) Normalized() []Task {
	out := make([]Task, len(s.Tasks))
	copy(out, s.Tasks)
	for i := range out {
		out[i].Verify = out[i].Verify.Normalize()
		out[i].Compliance = out[i].Compliance.Normalize()
	}
	return out
}
