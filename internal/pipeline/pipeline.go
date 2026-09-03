// Package pipeline resolves a product's route from the site's task vocabulary.
//
// The site declares TASKS - what a move between two stages is called, what it
// checks, what it scans. A product declares TARGETS, each sitting in a stage.
// Neither knows about the other, and this package is the join.
//
// # Why this is a package of pure functions
//
// Because it is the one piece of the flow every surface has to agree about. The
// interface asks "which button does this release get", the API asks "is this
// request legal", the auto-download path asks "where does a new release land",
// and the audit trail asks "what was this move called". Four callers deriving
// that separately is four chances to disagree with each other about somebody's
// production registry.
//
// So it takes configuration and a product, returns a route, and touches
// nothing else - no store, no registry, no clock. Everything it decides is a
// function of two documents, which is what makes it exhaustively testable and
// what keeps the answer the same in every caller.
package pipeline

import (
	"fmt"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// Step is one task as it applies to one product.
//
// It is the site's task with its `from` already resolved against the stages
// this product actually has, so a caller never has to re-run the skip rule.
type Step struct {
	config.Task

	// Targets are the product's targets in this step's destination stage, in
	// declaration order. More than one is the fan-out: the step writes them
	// all, and a Quay mirror is in here on exactly the same footing as the
	// primary repository.
	Targets []product.Target

	// Repointed records that this step's `from` was rewritten because the
	// stage the site named does not exist in this product. Carried so
	// `config check` can show a route that reads differently from the file
	// and say why, rather than leaving somebody to work it out.
	Repointed bool
	// ConfiguredFrom is the `from` as written, when Repointed.
	ConfiguredFrom string
}

// Pipeline is the ordered route a product's releases take.
type Pipeline struct {
	Steps []Step

	// Skipped names the tasks that do not apply, with the reason. Reported
	// rather than dropped silently: a product that has quietly lost its
	// promotion because somebody deleted a target should say so on the
	// configuration page, not just stop offering the button.
	Skipped []Skip
}

// Skip is a task that does not apply to this product.
type Skip struct {
	Task   string
	Stage  string
	Reason string
}

// Resolve joins the site's tasks to one product's targets.
//
// # The skip rule, which is the whole of the flexibility
//
// A task whose destination stage has no target in this product does not apply,
// and the next task that does apply reads from wherever the release actually
// is. A product with no `external` target therefore has auto-download landing
// straight in `lab`, with no configuration saying so and no special case in the
// engine - the route simply has one step, and its `from` is the sources.
//
// That is what lets one task vocabulary serve an estate where some teams have a
// landing zone, some go straight to lab, and some only mirror one upstream into
// one repository.
func Resolve(tasks []config.Task, p *product.Product) Pipeline {
	var out Pipeline
	if p == nil {
		return out
	}

	// The stage the next applicable step reads from. Starts at the sources,
	// and advances to each surviving step's destination.
	previous := config.SourceStage

	for _, t := range tasks {
		to := strings.TrimSpace(t.To)
		if to == "" {
			out.Skipped = append(out.Skipped, Skip{
				Task: t.Name, Stage: to,
				Reason: "the task names no destination stage",
			})
			continue
		}
		targets := p.TargetsInStage(to)
		if len(targets) == 0 {
			out.Skipped = append(out.Skipped, Skip{
				Task: t.Name, Stage: to,
				Reason: fmt.Sprintf("this product has no enabled target in stage %q", to),
			})
			continue
		}

		step := Step{Task: t, Targets: targets}
		step.Verify = t.Verify.Normalize()
		step.Compliance = t.Compliance.Normalize()

		// Keep the configured `from` when this product can honour it: the
		// sources are always available, and a stage this product has is
		// exactly what the site meant. Otherwise re-point at where the
		// release will actually be by the time this step can run.
		switch {
		case t.FromSource(), p.HasStage(t.From):
			step.From = strings.TrimSpace(t.From)
		default:
			step.Repointed = true
			step.ConfiguredFrom = strings.TrimSpace(t.From)
			step.From = previous
		}

		out.Steps = append(out.Steps, step)
		previous = to
	}

	return out
}

// Step returns the step for a task name.
func (pl Pipeline) Step(task string) (Step, bool) {
	for _, s := range pl.Steps {
		if s.Name == task {
			return s, true
		}
	}
	return Step{}, false
}

// Entry returns the step a newly discovered release enters at - the first one
// that reads from the product's sources.
//
// Reports false for a product whose route never touches its sources, which is a
// product that can only be moved by hand. That is a legitimate configuration
// and not an error, so the caller decides what to say about it.
func (pl Pipeline) Entry() (Step, bool) {
	for _, s := range pl.Steps {
		if s.FromSource() {
			return s, true
		}
	}
	return Step{}, false
}

// Stages lists the destination stages in route order.
func (pl Pipeline) Stages() []string {
	out := make([]string, 0, len(pl.Steps))
	for _, s := range pl.Steps {
		out = append(out, s.To)
	}
	return out
}

// Describe renders the route the way `config check` and the product page show
// it: `source → external → lab → prod`.
func (pl Pipeline) Describe() string {
	if len(pl.Steps) == 0 {
		return "no route: this product has no target in any stage the tasks name"
	}
	parts := make([]string, 0, len(pl.Steps)+1)
	parts = append(parts, pl.Steps[0].From)
	for _, s := range pl.Steps {
		parts = append(parts, s.To)
	}
	return strings.Join(parts, " → ")
}
