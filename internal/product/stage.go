package product

import "strings"

// Stages: where a release IS, as opposed to how it got there.
//
// A stage is a name a site chose - `external`, `lab`, `prod` - and a target
// declares which one it sits in. That single field is the whole binding between
// a product document and the tasks in system configuration: a task moves a
// release `from` one stage `to` another, and the targets carrying the `to`
// stage are what it writes.
//
// # Why several targets may share one stage
//
// Because that IS the fan-out. A lab that means "the JFrog lab repository and
// both Quays the lab clusters pull from" is three targets in one stage, and
// onboarding writes all three as one act. There is deliberately no separate
// mirror list, no derived target and no second concept: a Quay mirror is an
// ordinary target with `type: quay` and a `replication:` block, and it fans out
// because it shares a stage, not because anything special was declared.
//
// Promotion is the same mechanism pointed at a different stage. A production
// stage holding a gold repository and a production Quay behaves exactly as the
// lab one does, which is the test of whether this is one feature or two.
//
// # Why it supersedes `environment` rather than joining it
//
// `environment` already meant this, and was already matched by name to resolve
// a promotion (see Promotion). Two fields for one fact is the failure this
// schema is being simplified to remove, so `stage` is the name and
// `environment` is the older spelling of it - accepted, folded, and reported as
// superseded.

// StageName returns the stage this target sits in.
//
// `stage` wins; `environment` is the older spelling and means the same thing.
// Empty means the target belongs to no stage, which is legal and means no task
// writes it - a target reachable only by an explicit request.
func (t Target) StageName() string {
	if s := strings.TrimSpace(t.Stage); s != "" {
		return s
	}
	return strings.TrimSpace(t.Environment)
}

// TargetsInStage returns every enabled target sitting in one stage, in
// declaration order.
//
// Declaration order rather than sorted: a document that lists its primary
// repository first and its mirrors after should be executed and displayed that
// way, and the order a person wrote is the only ordering information available.
//
// # Why it counts before it allocates
//
// A Target is 280 bytes, and this is called per task per page render. Sizing
// the result to the number of targets in the PRODUCT rather than the number in
// the STAGE looks free and is not: a product with three stages of a dozen
// targets allocated capacity for all thirty-six, three times over, on every
// render - 18 KB a call for 10 KB of answer, most of it never written to.
//
// Counting first costs a second pass over a slice already in cache and makes
// the allocation exact.
func (p Product) TargetsInStage(stage string) []Target {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return nil
	}

	n := 0
	for i := range p.Spec.Targets {
		if p.Spec.Targets[i].inStage(stage) {
			n++
		}
	}
	if n == 0 {
		return nil
	}

	out := make([]Target, 0, n)
	for i := range p.Spec.Targets {
		if p.Spec.Targets[i].inStage(stage) {
			out = append(out, p.Spec.Targets[i])
		}
	}
	return out
}

// HasStage reports whether the product has any enabled target in a stage.
//
// This is the predicate the pipeline skip rule is written against: a product
// with no `external` target does not have that stage, so the task landing there
// does not apply to it and the one after it re-points.
//
// It answers by looking rather than by building: asking "is there one" through
// TargetsInStage allocated and copied the whole matching set to read its
// length, on a path that runs for every task of every product.
func (p Product) HasStage(stage string) bool {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return false
	}
	for i := range p.Spec.Targets {
		if p.Spec.Targets[i].inStage(stage) {
			return true
		}
	}
	return false
}

// inStage is the predicate both of the above are written against, so they can
// never disagree about what counts.
func (t *Target) inStage(stage string) bool {
	return t.IsEnabled() && t.StageName() == stage
}

// Stages lists the stages this product has targets in, in declaration order,
// de-duplicated.
func (p Product) Stages() []string {
	seen := make(map[string]bool, len(p.Spec.Targets))
	out := make([]string, 0, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		name := t.StageName()
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
