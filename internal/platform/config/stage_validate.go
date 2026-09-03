package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Validating the task vocabulary.
//
// These run once, over system configuration, at startup and in
// `transferctl config check`. They are deliberately strict: a task list is read
// by every product in the estate, so a mistake here is a mistake everywhere,
// and it is far cheaper to refuse the document than to discover at 3am that two
// tasks both claim to move a release out of lab.

var taskNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// TaskError is one problem with the task list, located by index.
type TaskError struct {
	Field   string
	Message string
	Hint    string
}

func (e TaskError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s - %s", e.Field, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// TaskErrors is every problem at once, so one round trip fixes the file.
type TaskErrors []TaskError

func (e TaskErrors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

// ErrOrNil returns nil when empty.
func (e TaskErrors) ErrOrNil() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// ValidateTasks checks the site's task list.
func ValidateTasks(tasks []Task) error {
	var errs TaskErrors

	seenName := map[string]int{}
	seenFrom := map[string]int{}
	seenTo := map[string]int{}

	for i, t := range tasks {
		field := fmt.Sprintf("stage.tasks[%d]", i)

		name := strings.TrimSpace(t.Name)
		switch {
		case name == "":
			errs = append(errs, TaskError{field + ".name", "is required",
				"the name is the verb in the API path and the audit trail"})
		case !taskNameRE.MatchString(name):
			errs = append(errs, TaskError{field + ".name",
				fmt.Sprintf("%q is not a resource ID", name),
				"lowercase letters, digits and hyphens; the display name is where prose goes"})
		default:
			if prev, dup := seenName[name]; dup {
				errs = append(errs, TaskError{field + ".name",
					fmt.Sprintf("%q is already declared by stage.tasks[%d]", name, prev),
					"task names are the API's vocabulary and must be unique"})
			}
			seenName[name] = i
		}

		from := strings.TrimSpace(t.From)
		to := strings.TrimSpace(t.To)

		if from == "" {
			errs = append(errs, TaskError{field + ".from", "is required",
				fmt.Sprintf("a stage name, or %q for the product's own sources", SourceStage)})
		}
		if to == "" {
			errs = append(errs, TaskError{field + ".to", "is required", "the stage this task moves the release into"})
		}
		if from != "" && from == to {
			errs = append(errs, TaskError{field + ".to",
				fmt.Sprintf("is the same stage as `from` (%q)", to),
				"a task moves a release between two stages"})
		}

		// Only the FIRST task may read the sources. Without this, a task could
		// pull production straight from a vendor registry, which is precisely
		// the property the stage model exists to guarantee - and guaranteeing
		// it here means no target needs a `promotionOnly` flag.
		if t.FromSource() && i != 0 {
			errs = append(errs, TaskError{field + ".from",
				fmt.Sprintf("only the first task may read %q", SourceStage),
				"a later stage is reached by moving through the ones before it, never from a vendor"})
		}

		// Two tasks leaving the same stage would make "what can this release do
		// now" ambiguous, and Available assumes it is not.
		if from != "" {
			if prev, dup := seenFrom[from]; dup {
				errs = append(errs, TaskError{field + ".from",
					fmt.Sprintf("stage %q is already left by stage.tasks[%d]", from, prev),
					"a release in one stage must have exactly one move available"})
			}
			seenFrom[from] = i
		}
		if to != "" {
			if prev, dup := seenTo[to]; dup {
				errs = append(errs, TaskError{field + ".to",
					fmt.Sprintf("stage %q is already entered by stage.tasks[%d]", to, prev),
					"two tasks landing in one stage make its contents ambiguous"})
			}
			seenTo[to] = i
		}

		errs = append(errs, validateMode(field+".verify", t.Verify)...)
		errs = append(errs, validateMode(field+".compliance", t.Compliance)...)
	}

	// A route has to start somewhere. Tasks that only move between stages,
	// with nothing reading the sources, describe an estate where software
	// arrives by magic.
	if len(tasks) > 0 && !tasks[0].FromSource() {
		errs = append(errs, TaskError{"stage.tasks[0].from",
			fmt.Sprintf("the first task reads %q rather than %q", tasks[0].From, SourceStage),
			"nothing else brings software in, so no release would ever enter the route"})
	}

	return errs.ErrOrNil()
}

// validateMode rejects a check mode outside the closed set.
//
// An empty value is accepted and means disabled - see CheckMode.Normalize. A
// MISSPELLED one is not: `verify: enforced` silently meaning "off" is how a
// site believes it is enforcing something it is not.
func validateMode(field string, m CheckMode) TaskErrors {
	raw := strings.TrimSpace(string(m))
	if raw == "" {
		return nil
	}
	for _, valid := range ValidCheckModes {
		if CheckMode(raw) == valid {
			return nil
		}
	}
	return TaskErrors{{field,
		fmt.Sprintf("%q is not a check mode", raw),
		fmt.Sprintf("one of %s", strings.Join(modeNames(), ", "))}}
}

func modeNames() []string {
	out := make([]string, 0, len(ValidCheckModes))
	for _, m := range ValidCheckModes {
		out = append(out, string(m))
	}
	return out
}
