package pipeline

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
)

// Validating the task vocabulary.
//
// These run once, over system configuration, at startup and in
// `transferctl config check`. They are deliberately strict: a task list is read
// by every product in the estate, so a mistake here is a mistake everywhere,
// and it is far cheaper to refuse the document than to discover at 3am that two
// tasks both claim to move a release out of lab.

var taskNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Error is one problem with the task list, located by index.
type Error struct {
	Field   string
	Message string
	Hint    string
}

func (e Error) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s: %s - %s", e.Field, e.Message, e.Hint)
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Errors is every problem at once, so one round trip fixes the file.
type Errors []Error

func (e Errors) Error() string {
	parts := make([]string, len(e))
	for i, err := range e {
		parts[i] = err.Error()
	}
	return strings.Join(parts, "; ")
}

// ErrOrNil returns nil when empty.
func (e Errors) ErrOrNil() error {
	if len(e) == 0 {
		return nil
	}
	return e
}

// Validate checks the site's task list.
func Validate(tasks []config.Task) error {
	var errs Errors

	seenName := map[string]int{}
	seenFrom := map[string]int{}
	seenTo := map[string]int{}

	for i, t := range tasks {
		field := fmt.Sprintf("stage.tasks[%d]", i)

		name := strings.TrimSpace(t.Name)
		switch {
		case name == "":
			errs = append(errs, Error{field + ".name", "is required",
				"the name is the verb in the API path and the audit trail"})
		case !taskNameRE.MatchString(name):
			errs = append(errs, Error{field + ".name",
				fmt.Sprintf("%q is not a resource ID", name),
				"lowercase letters, digits and hyphens; the display name is where prose goes"})
		default:
			if prev, dup := seenName[name]; dup {
				errs = append(errs, Error{field + ".name",
					fmt.Sprintf("%q is already declared by stage.tasks[%d]", name, prev),
					"task names are the API's vocabulary and must be unique"})
			}
			seenName[name] = i
		}

		from := strings.TrimSpace(t.From)
		to := strings.TrimSpace(t.To)

		if from == "" {
			errs = append(errs, Error{field + ".from", "is required",
				fmt.Sprintf("a stage name, or %q for the product's own sources", config.SourceStage)})
		}
		if to == "" {
			errs = append(errs, Error{field + ".to", "is required", "the stage this task moves the release into"})
		}
		if from != "" && from == to {
			errs = append(errs, Error{field + ".to",
				fmt.Sprintf("is the same stage as `from` (%q)", to),
				"a task moves a release between two stages"})
		}

		// Only the FIRST task may read the sources. Without this, a task could
		// pull production straight from a vendor registry, which is precisely
		// the property the stage model exists to guarantee - and guaranteeing
		// it here means no target needs a `promotionOnly` flag.
		if t.FromSource() && i != 0 {
			errs = append(errs, Error{field + ".from",
				fmt.Sprintf("only the first task may read %q", config.SourceStage),
				"a later stage is reached by moving through the ones before it, never from a vendor"})
		}

		// Two tasks leaving the same stage would make "what can this release do
		// now" ambiguous, and Available assumes it is not.
		if from != "" {
			if prev, dup := seenFrom[from]; dup {
				errs = append(errs, Error{field + ".from",
					fmt.Sprintf("stage %q is already left by stage.tasks[%d]", from, prev),
					"a release in one stage must have exactly one move available"})
			}
			seenFrom[from] = i
		}
		if to != "" {
			if prev, dup := seenTo[to]; dup {
				errs = append(errs, Error{field + ".to",
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
		errs = append(errs, Error{"stage.tasks[0].from",
			fmt.Sprintf("the first task reads %q rather than %q", tasks[0].From, config.SourceStage),
			"nothing else brings software in, so no release would ever enter the route"})
	}

	return errs.ErrOrNil()
}

// validateMode rejects a check mode outside the closed set.
//
// An empty value is accepted and means disabled - see CheckMode.Normalize. A
// MISSPELLED one is not: `verify: enforced` silently meaning "off" is how a
// site believes it is enforcing something it is not.
func validateMode(field string, m config.CheckMode) Errors {
	raw := strings.TrimSpace(string(m))
	if raw == "" {
		return nil
	}
	for _, valid := range config.ValidCheckModes {
		if config.CheckMode(raw) == valid {
			return nil
		}
	}
	return Errors{{field,
		fmt.Sprintf("%q is not a check mode", raw),
		fmt.Sprintf("one of %s", strings.Join(modeNames(), ", "))}}
}

func modeNames() []string {
	out := make([]string, 0, len(config.ValidCheckModes))
	for _, m := range config.ValidCheckModes {
		out = append(out, string(m))
	}
	return out
}
