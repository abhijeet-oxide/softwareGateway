package render

import (
	"context"
	"errors"
	"strings"
)

// Why a chart did not render, and whether rendering it again could help.
//
// # Why this is classified at all
//
// "helm template failed" is one sentence over a coverage table of ninety-five
// charts, seventeen of which failed for four completely different reasons. In a
// real orb those reasons were: a subchart that requires `global.registry` and
// was rendered without an umbrella to supply it, a values.schema.json the
// vendor's own defaults violate, a template dereferencing a nil, and a file
// that is not valid YAML. Those are four different conversations - three with
// the vendor and one with us - and an undifferentiated list of stack traces is
// how they all become "the tool is broken".
//
// # Why the classification decides the retry rather than a counter
//
// Retrying a deterministic template error is not resilience, it is three times
// the work for the same error and three times the wait for the person watching.
// `helm template` is a pure function of the chart and the flags: if it returned
// "nil pointer evaluating interface {}.timeZoneEnv" once, it returns it every
// time. What CAN succeed on a second attempt is a render that never got to run
// its templates - killed by a deadline, refused a file descriptor, a binary
// that was being replaced under us.
//
// So the retry is decided by what the failure IS, and a chart that will not be
// retried says so, with the reason. That is the honest version of "we tried
// three times".

// FailureKind is why a chart did not render, in the terms a reader acts on.
type FailureKind string

const (
	// FailureNeedsValues is a chart that cannot render at its own defaults
	// because it requires values it does not ship - a subchart expecting an
	// umbrella's `global.*`, or a required value with no default.
	//
	// The commonest by far in a vendor orb, and the least like a defect in
	// either the chart or this tool: it is a chart that is not installable on
	// its own, which is a true and useful thing to report.
	FailureNeedsValues FailureKind = "needs_values"
	// FailureSchema is values.schema.json refusing the chart's own defaults.
	// A vendor defect, and one they can reproduce with `helm template` alone.
	FailureSchema FailureKind = "schema"
	// FailureTemplate is a template that ran and failed - a nil dereference, a
	// function misused, a required call. A vendor defect.
	FailureTemplate FailureKind = "template"
	// FailureInvalidYAML is output that is not YAML, or a source file that is
	// not. A vendor defect, and the one most likely to be an indentation bug in
	// a conditional block nobody exercises.
	FailureInvalidYAML FailureKind = "invalid_yaml"
	// FailureTimeout is a render that exceeded its deadline. RETRYABLE: a
	// loaded Coordinator and a template loop look the same on the first
	// attempt, and only one of them is still there on the second.
	FailureTimeout FailureKind = "timeout"
	// FailureRenderer is helm itself failing to run - not found, not
	// executable, killed. RETRYABLE, because a binary being replaced under a
	// running Coordinator is a real and transient thing.
	FailureRenderer FailureKind = "renderer"
	// FailureChart is an artifact that is not a usable chart at all: no
	// Chart.yaml, unreadable values.yaml, nothing to render.
	FailureChart FailureKind = "chart"
	// FailureUnknown is everything else. Retried once, because an unclassified
	// failure is one this code has not seen and a single retry is a cheap way
	// to find out whether it is deterministic.
	FailureUnknown FailureKind = "unknown"
)

// Label is the kind in the words the coverage table shows.
func (k FailureKind) Label() string {
	switch k {
	case FailureNeedsValues:
		return "Requires values"
	case FailureSchema:
		return "Values rejected by schema"
	case FailureTemplate:
		return "Template error"
	case FailureInvalidYAML:
		return "Invalid YAML"
	case FailureTimeout:
		return "Render timed out"
	case FailureRenderer:
		return "Renderer unavailable"
	case FailureChart:
		return "Not a usable chart"
	default:
		return "Render failed"
	}
}

// Explain is what the reader does about it, in one sentence.
func (k FailureKind) Explain() string {
	switch k {
	case FailureNeedsValues:
		return "This chart cannot be rendered at its own defaults: it requires values it does " +
			"not ship, which an umbrella chart or a site values file would supply. Every check " +
			"needing its objects reports as undecided rather than as a pass."
	case FailureSchema:
		return "The chart's own default values are rejected by its values.schema.json. " +
			"Reproducible by the vendor with `helm template` and no arguments."
	case FailureTemplate:
		return "A template ran and failed. Reproducible by the vendor with `helm template` " +
			"and no arguments."
	case FailureInvalidYAML:
		return "The chart produced output that is not valid YAML, so no object could be read " +
			"from it."
	case FailureTimeout:
		return "The render exceeded its deadline. Retried; a template that does not terminate " +
			"fails the same way twice."
	case FailureRenderer:
		return "helm could not be run. This is a fault on this Coordinator, not in the chart."
	case FailureChart:
		return "The artifact does not contain a chart this renderer can load."
	default:
		return "The renderer failed for a reason this Coordinator does not recognise. The " +
			"message below is helm's own."
	}
}

// Retryable reports whether a second attempt could plausibly succeed.
//
// Deterministic failures are not retried, and that is not laziness: `helm
// template` is a pure function of the chart and the flags, so a template error
// returns the same template error every time. Retrying it costs the person
// waiting three times the delay for the identical message.
func (k FailureKind) Retryable() bool {
	switch k {
	case FailureTimeout, FailureRenderer, FailureUnknown:
		return true
	default:
		return false
	}
}

// ClassifyFailure reads helm's own message and says what kind of failure it is.
//
// Matched on helm's phrasing rather than on an exit code, because helm exits 1
// for all of them. The phrases below are the ones helm actually emits; a
// message matching none of them is FailureUnknown, which is retried once
// precisely so an unrecognised failure is not silently treated as permanent.
func ClassifyFailure(err error) FailureKind {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	if errors.Is(err, ErrHelmUnavailable) {
		return FailureRenderer
	}

	msg := strings.ToLower(stripHelmAdvice(err.Error()))
	switch {
	case strings.Contains(msg, "timed out"), strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "signal: killed"):
		return FailureTimeout
	case strings.Contains(msg, "executable file not found"),
		strings.Contains(msg, "no such file or directory: helm"),
		strings.Contains(msg, "permission denied"),
		strings.Contains(msg, "text file busy"):
		return FailureRenderer

	// The order below matters where phrases co-occur. A schema failure names
	// the schema explicitly, so it is tested before the generic "required".
	case strings.Contains(msg, "specifications of the schema"),
		strings.Contains(msg, "values.schema.json"),
		strings.Contains(msg, "missing property"):
		return FailureSchema
	case strings.Contains(msg, "must be specified"),
		strings.Contains(msg, "must be provided"),
		strings.Contains(msg, "is required"),
		strings.Contains(msg, "required value"),
		strings.Contains(msg, "at <required>"):
		return FailureNeedsValues
	case strings.Contains(msg, "did not find expected"),
		strings.Contains(msg, "unexpected eof"),
		strings.Contains(msg, "error converting yaml"),
		strings.Contains(msg, "error unmarshaling json"),
		strings.Contains(msg, "mapping values are not allowed"),
		strings.Contains(msg, "yaml: line"):
		return FailureInvalidYAML
	case strings.Contains(msg, "nil pointer evaluating"),
		strings.Contains(msg, "execution error"),
		strings.Contains(msg, "wrong type for value"),
		strings.Contains(msg, "function \""),
		strings.Contains(msg, "at <"):
		return FailureTemplate
	case strings.Contains(msg, "chart.yaml"), strings.Contains(msg, "not a helm chart"),
		strings.Contains(msg, "holds no chart.yaml"):
		return FailureChart
	}
	return FailureUnknown
}

// MaxRenderAttempts bounds a retryable failure.
//
// Two, not five. The failures that are retried at all are ones where the render
// never reached the chart's templates, and a Coordinator that cannot run helm
// twice in a row is not going to manage it on the fifth - it is going to spend
// five deadlines per chart across ninety-five charts, which turns a slow run
// into an unusable one.
const MaxRenderAttempts = 2

// stripHelmAdvice removes the boilerplate helm appends to most failures.
//
// # Why this is not a nicety
//
// helm ends a great many errors - template errors, nil dereferences, execution
// errors - with "Use --debug flag to render out invalid YAML". That sentence
// contains the words "invalid YAML", and matching it classified a nil pointer
// dereference as a YAML parse error: a vendor defect reported as the wrong
// vendor defect, on the line that tells them where to look.
//
// Found by the development estate the moment it was given charts that fail the
// way real ones do, which is the whole argument for having built those.
func stripHelmAdvice(msg string) string {
	for _, advice := range []string{
		"Use --debug flag to render out invalid YAML",
		"use --debug flag to render out invalid yaml",
	} {
		if i := strings.Index(msg, advice); i >= 0 {
			msg = msg[:i] + msg[i+len(advice):]
		}
	}
	return strings.TrimSpace(msg)
}
