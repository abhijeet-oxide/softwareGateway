package render

import (
	"context"
	"errors"
	"regexp"
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

// ---------------------------------------------------------------------------
// What helm's message names, pulled out of it
//
// A classification says WHICH conversation a failure belongs to. These two say
// what to put in it, and both come out of the same sentence the coverage table
// already shows.

// valuePath matches a dotted values key: `global.registry`, `timezone.timeZoneEnv`.
//
// At least one dot, because a single bare word in an English sentence is a word
// - "registry must be specified" names no key, and reporting `registry` from it
// would be an invention. Ends on a word character so trailing punctuation stays
// out of the key.
var valuePath = regexp.MustCompile(`\.?Values\.([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+)` +
	`|\b([a-z][A-Za-z0-9_]*(?:\.[A-Za-z0-9_]+)+)\b`)

// schemaProperty matches the JSON-schema rejection helm reports verbatim:
// `at '/global': missing property 'registry'`.
var schemaProperty = regexp.MustCompile(`at '([^']*)':\s*missing property '([^']+)'`)

// templateLocation matches the two ways helm names the file that failed:
// `(chart/templates/x.yaml:24:6)` and `executing "chart/templates/x.yaml"`.
var templateLocation = regexp.MustCompile(`\(([^()\s]+\.(?:yaml|yml|tpl|txt))[:0-9]*\)` +
	`|executing "([^"]+)"`)

// fileLikeSuffix is the extension set templateLocation matches, used to keep a
// filename from being read as a values key: `values-chart-check.yaml` has a dot
// in it and is not a path into anybody's values file.
var fileLikeSuffix = regexp.MustCompile(`\.(?:yaml|yml|tpl|json|txt|tgz|go)$`)

// MissingValue is the values key the chart demanded, or "".
//
// # Why this is worth extracting rather than leaving in the message
//
// Six of the eight charts that failed in a real orb failed for one reason:
// they are subcharts, and an umbrella supplies their `global.registry`. The
// coverage table showed each of them a different paragraph of helm - one an
// `execution error` from a guard template, one a schema rejection, one a nil
// dereference - and the single fact they had in common, which is the fact that
// decides what to do about them, was not on screen anywhere.
//
// With the key extracted the table can group them, and the run can say what a
// site values file would have to supply to check this release properly. That is
// the honest answer to "why did these not render", and it is one column rather
// than eight paragraphs.
//
// Empty where helm named no key. `Registry Must be provided for image
// 'cmdb-admin'` is a sentence a chart author wrote, and inferring `registry`
// from it would be this tool making up a values path.
func MissingValue(err error) string {
	if err == nil {
		return ""
	}
	msg := stripHelmAdvice(err.Error())

	// The schema's own words first: it is the one form that states the path
	// exactly, as a JSON pointer plus the property that was absent from it.
	if m := schemaProperty.FindStringSubmatch(msg); m != nil {
		parent := strings.Trim(strings.ReplaceAll(m[1], "/", "."), ".")
		if parent == "" {
			return m[2]
		}
		return parent + "." + m[2]
	}

	// Everything else is prose, so the file locations come out first: a
	// template called `values-chart-check.yaml` has a dot in it and would
	// otherwise read as a values key.
	prose := templateLocation.ReplaceAllString(msg, " ")

	for _, m := range valuePath.FindAllStringSubmatch(prose, -1) {
		key := m[1]
		if key == "" {
			key = m[2]
		}
		if key == "" || fileLikeSuffix.MatchString(key) {
			continue
		}
		return key
	}
	return ""
}

// FailingTemplate is the chart-relative template helm named, or "".
func FailingTemplate(err error) string {
	if err == nil {
		return ""
	}
	m := templateLocation.FindStringSubmatch(stripHelmAdvice(err.Error()))
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// InTestHook reports whether the failure came from a helm test hook.
//
// # Why this changes what a reader does about it
//
// `helm install` never applies `templates/tests/`. Those manifests run only
// under `helm test`, so a chart whose ONLY failure is in one installs perfectly
// in a cluster and still cannot be checked here - and telling a vendor "your
// chart does not render" about a test job they have never run is how a true
// finding gets dismissed along with the rest of the report.
//
// It is stated rather than worked around. `helm template --skip-tests` was the
// obvious fix and does not work: it filters test manifests out of the OUTPUT,
// after every template has executed, so a `fail` inside one still aborts the
// render. Measured against helm v3.16.3 with a chart built to fail exactly that
// way, with and without the flag, and the error was byte for byte identical.
func InTestHook(err error) bool {
	path := FailingTemplate(err)
	if path == "" {
		return false
	}
	path = strings.ToLower(path)
	return strings.Contains(path, "/templates/tests/") ||
		strings.HasPrefix(path, "templates/tests/")
}
