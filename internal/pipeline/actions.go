package pipeline

import (
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// What a release can do next, and why it cannot do the rest.
//
// This is the answer the release page renders as buttons and the API enforces
// on a request. Both read it from here, so a request the interface would not
// have offered is refused with the same sentence the interface would have
// shown - which is the difference between a disabled button and a mystery.

// Action is one task as it applies to one release right now.
type Action struct {
	// Name and Label are the task's, so a caller renders a button without
	// knowing what a task is.
	Name  string
	Label string
	From  string
	To    string

	// Available reports whether this action can be taken now.
	Available bool
	// Reason says why not, in a sentence naming the fix. Empty when Available.
	Reason string

	// Targets are what this action would write, in declaration order.
	Targets []string
}

// Location is where a release currently sits.
//
// Stage is empty for a release that has been discovered and has not landed
// anywhere yet, which is a different state from "in the first stage" and the
// distinction decides whether the entry action is offered.
type Location struct {
	Stage string
}

// Landed reports whether the release is in a stage at all.
func (l Location) Landed() bool { return l.Stage != "" }

// Actions returns every step of the route as an action, in route order, with
// exactly one of them available.
//
// # Why the unavailable ones are returned rather than filtered out
//
// Because "Promote to Prod is not offered" and "this release is in external, so
// onboard it to lab first" are answers of very different quality, and the
// second one is only possible if the caller is told about the step it cannot
// take yet. The interface shows the available action as its primary button and
// has the rest to explain with; a filtered list would leave it guessing.
func (pl Pipeline) Actions(loc Location) []Action {
	out := make([]Action, 0, len(pl.Steps))

	for _, s := range pl.Steps {
		a := Action{
			Name: s.Name, Label: s.Label(), From: s.From, To: s.To,
			Targets: targetNames(s.Targets),
		}

		switch {
		case s.To == loc.Stage:
			a.Reason = fmt.Sprintf("This release is already in %s.", s.To)
		case s.FromSource() && !loc.Landed():
			a.Available = true
		case s.FromSource() && loc.Landed():
			// The entry step, on a release that has already entered. Re-running
			// it would re-fetch from the vendor over a release that is further
			// along, which is not what anybody pressing a button here means.
			a.Reason = fmt.Sprintf("This release has already been brought in; it is in %s.", loc.Stage)
		case s.From == loc.Stage:
			a.Available = true
		case !loc.Landed():
			a.Reason = fmt.Sprintf(
				"This release has not been brought in yet, so there is nothing in %s to move.", s.From)
		default:
			a.Reason = fmt.Sprintf("This release is in %s. It has to reach %s first.", loc.Stage, s.From)
		}

		out = append(out, a)
	}

	return out
}

// Available returns the one action a release can take now, if any.
//
// At most one, by construction: a step is available when its `from` is where
// the release is, and a release is in one place. A route whose tasks share a
// `from` would break that, which is why Validate rejects one.
func (pl Pipeline) Available(loc Location) (Action, bool) {
	for _, a := range pl.Actions(loc) {
		if a.Available {
			return a, true
		}
	}
	return Action{}, false
}

func targetNames(targets []product.Target) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, t.Name)
	}
	return out
}
