package render_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
)

// The messages below are helm's own, copied from a real vendor orb whose charts
// failed seventeen different ways. They are the reason this classification
// exists: an undifferentiated list of these is how four separate conversations -
// three with the vendor and one with us - become "the tool is broken".
func TestClassifyFailureReadsHelmsOwnMessages(t *testing.T) {
	cases := []struct {
		msg  string
		want render.FailureKind
	}{
		// A subchart rendered without the umbrella that supplies its globals.
		// The commonest failure in a vendor orb by a wide margin.
		{`helm template failed for crdb-redisio: Error: execution error at ` +
			`(crdb-redisio/templates/values-chart-check.yaml:24:6): global.registry must be specified`,
			render.FailureNeedsValues},
		{`helm template failed for cmdb: Error: execution error at ` +
			`(cmdb/templates/tests/test-jobs.yaml:2:4): Registry Must be provided for image 'cmdb-admin'`,
			render.FailureNeedsValues},
		{`helm template failed for cdc: Error: execution error at ` +
			`(cdc/templates/deployment.yaml:38:20): A valid global registry value is required!`,
			render.FailureNeedsValues},

		// The vendor's own defaults refused by the vendor's own schema.
		{`helm template failed for cbur: Error: values don't meet the specifications of the ` +
			`schema(s) in the following chart(s):` + "\n" + `cbur: - at '/global': missing property 'registry'`,
			render.FailureSchema},

		{`helm template failed for rgm: Error: execution error at ` +
			`(rgm/templates/statefulset.yaml:149:29) executing "rgm/templates/statefulset.yaml" ` +
			`at <.Values.timezone.timeZoneEnv>: nil pointer evaluating interface {}.timeZoneEnv`,
			render.FailureTemplate},

		{`helm template failed for ml-inference-server-crd: Error: parse error at ` +
			`(ml-inference-server-crd/templates/zts_values-compact.yaml:13): unexpected EOF`,
			render.FailureInvalidYAML},

		{`rendering web timed out after 1m30s: context deadline exceeded`, render.FailureTimeout},
		{`helm template failed for x: signal: killed`, render.FailureTimeout},
		{`exec: "helm": executable file not found in $PATH`, render.FailureRenderer},
		{`something nobody has seen before`, render.FailureUnknown},

		// THE TRAP. helm appends this sentence to a great many failures, and it
		// contains the words "invalid YAML" - so matching the whole message
		// classified a nil dereference as a YAML parse error, which is a vendor
		// defect reported as the wrong vendor defect on the line that tells
		// them where to look. Found by the development estate the moment it was
		// given charts that fail the way real ones do.
		{`helm template failed for x: Error: template: x/templates/tz.yaml:6:16: ` +
			`executing "x/templates/tz.yaml" at <.Values.timezone.timeZoneEnv>: ` +
			`nil pointer evaluating interface {}.timeZoneEnv` + "\n\n" +
			`Use --debug flag to render out invalid YAML`,
			render.FailureTemplate},
		{`helm template failed for y: Error: execution error at ` +
			`(y/templates/chart-check.yaml:2:4): global.registry must be specified` + "\n\n" +
			`Use --debug flag to render out invalid YAML`,
			render.FailureNeedsValues},
	}
	for _, c := range cases {
		if got := render.ClassifyFailure(errors.New(c.msg)); got != c.want {
			t.Errorf("ClassifyFailure(%.70q…) = %q, want %q", c.msg, got, c.want)
		}
	}
	if got := render.ClassifyFailure(nil); got != "" {
		t.Errorf("a nil error classified as %q", got)
	}
}

func TestClassifyFailureUnwrapsSentinels(t *testing.T) {
	for _, c := range []struct {
		err  error
		want render.FailureKind
	}{
		{fmt.Errorf("rendering: %w", context.DeadlineExceeded), render.FailureTimeout},
		{fmt.Errorf("loading: %w", render.ErrHelmUnavailable), render.FailureRenderer},
		{errors.New("charts/x: the archive holds no Chart.yaml, so it is not a Helm chart"),
			render.FailureChart},
	} {
		if got := render.ClassifyFailure(c.err); got != c.want {
			t.Errorf("ClassifyFailure(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// The rule the retry rests on: `helm template` is a pure function of the chart
// and the flags, so a template error returns the same template error every
// time. Retrying it costs the person waiting three times the delay for the
// identical message.
func TestOnlyFailuresARetryCouldFixAreRetryable(t *testing.T) {
	retryable := map[render.FailureKind]bool{
		render.FailureTimeout:  true,
		render.FailureRenderer: true,
		// Unrecognised, so retried once precisely to find out whether it is
		// deterministic rather than assuming it is.
		render.FailureUnknown: true,
	}
	for _, k := range []render.FailureKind{
		render.FailureNeedsValues, render.FailureSchema, render.FailureTemplate,
		render.FailureInvalidYAML, render.FailureChart,
		render.FailureTimeout, render.FailureRenderer, render.FailureUnknown,
	} {
		if got := k.Retryable(); got != retryable[k] {
			t.Errorf("%s.Retryable() = %v, want %v", k, got, retryable[k])
		}
		if k.Label() == "" || k.Explain() == "" {
			t.Errorf("%s has no label or no explanation, so the coverage table has "+
				"nothing to show but a stack trace", k)
		}
	}
}

// The key a values file would have to supply, pulled out of eight different
// paragraphs of helm. Six of the eight charts that failed in a real orb failed
// for one reason - a `global.registry` an umbrella supplies - and the sentences
// they failed with had nothing in common but that key.
func TestMissingValueNamesTheKeyAValuesFileWouldSupply(t *testing.T) {
	for _, c := range []struct{ msg, want string }{
		// The guard template a subchart ships to fail loudly without its
		// umbrella. Note `values-chart-check.yaml`: a filename with a dot in
		// it, which must not be read as a values key.
		{`helm template failed for crdb-redisio: Error: execution error at ` +
			`(crdb-redisio/templates/values-chart-check.yaml:24:6): global.registry must be specified`,
			"global.registry"},

		// The schema states the path exactly, as a pointer plus the property.
		{`helm template failed for cbur: Error: values don't meet the specifications of the ` +
			`schema(s) in the following chart(s):` + "\n" + `cbur: - at '/global': missing property 'registry'`,
			"global.registry"},
		{`Error: values don't meet the specifications of the schema(s):` + "\n" +
			`x: - at '': missing property 'registry'`, "registry"},

		// A nil dereference names the key it was reaching for.
		{`helm template failed for rgm: Error: execution error at ` +
			`(rgm/templates/statefulset.yaml:149:29) executing "rgm/templates/statefulset.yaml" ` +
			`at <.Values.timezone.timeZoneEnv>: nil pointer evaluating interface {}.timeZoneEnv`,
			"timezone.timeZoneEnv"},

		// A sentence a chart author wrote, naming no key. Inferring `registry`
		// from it would be this tool making up a values path.
		{`helm template failed for cmdb: Error: execution error at ` +
			`(cmdb/templates/tests/test-jobs.yaml:2:4): Registry Must be provided for image 'cmdb-admin'`,
			""},
		// A file that does not parse has no key either: no values reach it.
		{`Error: parse error at (ml/templates/zts_values-compact.yaml:13): unexpected EOF`, ""},
	} {
		if got := render.MissingValue(errors.New(c.msg)); got != c.want {
			t.Errorf("MissingValue(%.60q…) = %q, want %q", c.msg, got, c.want)
		}
	}
	if got := render.MissingValue(nil); got != "" {
		t.Errorf("MissingValue(nil) = %q", got)
	}
}

// A chart whose only failure is in `templates/tests/` installs perfectly: helm
// install never applies a test hook. Saying so is the difference between a
// vendor fixing a manifest and a vendor dismissing the report.
func TestInTestHookSeparatesAHookFromTheChartItself(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
		file string
	}{
		{`Error: execution error at (cmdb/templates/tests/test-jobs.yaml:2:4): ` +
			`Registry Must be provided`, true, "cmdb/templates/tests/test-jobs.yaml"},
		{`Error: execution error at (csdc/templates/tests/etcd-status.yaml:31:16): boom`,
			true, "csdc/templates/tests/etcd-status.yaml"},
		{`Error: execution error at (sbc-cdc/templates/deployment.yaml:38:20): ` +
			`A valid global registry value is required!`, false, "sbc-cdc/templates/deployment.yaml"},
		{`Error: execution error at (rgm/templates/statefulset.yaml:149:29) ` +
			`executing "rgm/templates/statefulset.yaml" at <.Values.timezone.timeZoneEnv>: nil pointer`,
			false, "rgm/templates/statefulset.yaml"},
		{`exec: "helm": executable file not found in $PATH`, false, ""},
	} {
		err := errors.New(c.msg)
		if got := render.InTestHook(err); got != c.want {
			t.Errorf("InTestHook(%.60q…) = %v, want %v", c.msg, got, c.want)
		}
		if got := render.FailingTemplate(err); got != c.file {
			t.Errorf("FailingTemplate(%.60q…) = %q, want %q", c.msg, got, c.file)
		}
	}
}

// The cause is the clause a reader acts on, with helm's frames stripped.
//
// # The defect this exists for
//
// A run log showed thirteen consecutive lines reading "<chart>: Template
// error" and nothing else. helm's message was captured and stored the whole
// time - the coverage table rendered it correctly - but the extraction that
// made it readable lived in TypeScript, so the log, which is the screen that is
// up WHILE a run is going, had no cause on it at all.
func TestCauseStripsHelmsFrames(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a required value, wrapped in three frames",
			in: "helm template failed for cfx-adrf-chart: Error: execution error at " +
				"(cfx-adrf-chart/templates/chart-check.yaml:2:4): global.registry must be specified",
			want: "global.registry must be specified",
		},
		{
			name: "a nil dereference, with the template frame helm nests",
			in: "helm template failed for svs: Error: template: svs/templates/deploy.yaml:14:22: " +
				`executing "svs/templates/deploy.yaml" at <.Values.image.tag>: ` +
				"nil pointer evaluating interface {}.tag",
			want: "nil pointer evaluating interface {}.tag",
		},
		{
			name: "only the first line, because the rest is the stack",
			in: "helm template failed for crr: Error: parse error at (crr/templates/_helpers.tpl:8): " +
				"unexpected EOF\n  at <include \"crr.labels\">\n  more frames",
			want: "unexpected EOF",
		},
		{
			name: "helm's --debug advice is not the cause",
			in: "helm template failed for x: Error: YAML parse error on x/templates/a.yaml: " +
				"error converting YAML to JSON\n\nUse --debug flag to render out invalid YAML",
			want: "YAML parse error on x/templates/a.yaml: error converting YAML to JSON",
		},
		{
			name: "a message with no recognised frame is returned whole",
			in:   "something nobody has seen before",
			want: "something nobody has seen before",
		},
		{
			name: "an empty message stays empty",
			in:   "",
			want: "",
		},
		{
			// Stripping everything would leave a row with a classification and
			// a blank, which is the state this function exists to prevent.
			name: "a message that is nothing but frames keeps the head",
			in:   "Error:",
			want: "Error:",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := render.Cause(c.in); got != c.want {
				t.Errorf("Cause() = %q, want %q", got, c.want)
			}
		})
	}
}
