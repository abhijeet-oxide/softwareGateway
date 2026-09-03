package config_test

import (
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
)

// siteTasks is the vocabulary from docs/design/examples/config.yaml, which the
// tests below hold this validator to.
func siteTasks() []config.Task {
	return []config.Task{
		{
			Name: "download", DisplayName: "Download",
			From: config.SourceStage, To: "external",
			Verify: config.CheckEnforce, Compliance: config.CheckEnforce,
			Scanners: []string{"xray", "anchore"},
		},
		{
			Name: "onboard", DisplayName: "Onboard to Lab",
			From: "external", To: "lab", Purge: true,
			Compliance: config.CheckEnabled,
			Scanners:   []string{"xray", "anchore"},
		},
		{
			Name: "promote", DisplayName: "Promote to Prod",
			From: "lab", To: "prod",
			Scanners: []string{"xray"},
		},
	}
}

func TestValidateAcceptsTheSiteVocabulary(t *testing.T) {
	if err := config.ValidateTasks(siteTasks()); err != nil {
		t.Fatalf("the shipped example does not validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name  string
		tasks []config.Task
		want  string
	}{
		{
			name: "a later task reading the sources",
			tasks: []config.Task{
				{Name: "download", From: config.SourceStage, To: "external"},
				{Name: "promote", From: config.SourceStage, To: "prod"},
			},
			want: "only the first task may read",
		},
		{
			name: "two tasks leaving one stage",
			tasks: []config.Task{
				{Name: "download", From: config.SourceStage, To: "external"},
				{Name: "onboard", From: "external", To: "lab"},
				{Name: "sideload", From: "external", To: "prod"},
			},
			want: "is already left by",
		},
		{
			name: "two tasks entering one stage",
			tasks: []config.Task{
				{Name: "download", From: config.SourceStage, To: "external"},
				{Name: "onboard", From: "external", To: "lab"},
				{Name: "reonboard", From: "lab", To: "lab"},
			},
			want: "same stage as `from`",
		},
		{
			name: "a duplicate task name",
			tasks: []config.Task{
				{Name: "download", From: config.SourceStage, To: "external"},
				{Name: "download", From: "external", To: "lab"},
			},
			want: "is already declared by",
		},
		{
			name: "a task name that is not a resource ID",
			tasks: []config.Task{
				{Name: "Onboard to Lab", From: config.SourceStage, To: "external"},
			},
			want: "is not a resource ID",
		},
		{
			name:  "a route that never reads the sources",
			tasks: []config.Task{{Name: "promote", From: "lab", To: "prod"}},
			want:  "nothing else brings software in",
		},
		{
			name: "a missing destination",
			tasks: []config.Task{
				{Name: "download", From: config.SourceStage},
			},
			want: "to: is required",
		},
		{
			// The one that matters most: `enforced` silently meaning "off" is
			// how a site believes it is enforcing something it is not.
			name: "a misspelled check mode",
			tasks: []config.Task{
				{Name: "download", From: config.SourceStage, To: "external", Verify: "enforced"},
			},
			want: "is not a check mode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.ValidateTasks(tc.tasks)
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// An empty check mode is legal and means disabled; only a misspelling is not.
func TestValidateAcceptsAnUnsetCheckMode(t *testing.T) {
	tasks := []config.Task{{Name: "download", From: config.SourceStage, To: "external"}}
	if err := config.ValidateTasks(tasks); err != nil {
		t.Fatalf("unset check modes should be legal: %v", err)
	}
}

func TestCheckModeSemantics(t *testing.T) {
	cases := []struct {
		mode                 config.CheckMode
		runs, blocks         bool
		normalizesToDisabled bool
	}{
		{config.CheckDisabled, false, false, true},
		{config.CheckEnabled, true, false, false},
		{config.CheckEnforce, true, true, false},
		{"", false, false, true},
		{"ENFORCE", true, true, false},
	}

	for _, tc := range cases {
		m := tc.mode.Normalize()
		if m.Runs() != tc.runs {
			t.Errorf("%q.Runs() = %v, want %v", tc.mode, m.Runs(), tc.runs)
		}
		if m.Blocks() != tc.blocks {
			t.Errorf("%q.Blocks() = %v, want %v", tc.mode, m.Blocks(), tc.blocks)
		}
		if (m == config.CheckDisabled) != tc.normalizesToDisabled {
			t.Errorf("%q normalized to %q", tc.mode, m)
		}
	}
}

// Every task reports a label, so a button is never blank.
func TestTaskLabelFallsBackToItsName(t *testing.T) {
	if got := (config.Task{Name: "promote"}).Label(); got != "promote" {
		t.Errorf("Label() = %q, want the name", got)
	}
	if got := (config.Task{Name: "promote", DisplayName: "Promote to Prod"}).Label(); got != "Promote to Prod" {
		t.Errorf("Label() = %q, want the display name", got)
	}
}
