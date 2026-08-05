package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// resolveTimeout runs the root command's flag parsing and PreRun for one
// argument vector, and reports the timeout the client would have been built
// with. It stops short of executing the command.
func resolveTimeout(t *testing.T, args ...string) time.Duration {
	t.Helper()

	opts = globalOptions{}
	root := newRootCommand()

	cmd, flags, err := root.Find(args)
	if err != nil {
		t.Fatalf("find %v: %v", args, err)
	}
	if err := cmd.ParseFlags(flags); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	if cmd.PreRun != nil {
		cmd.PreRun(cmd, nil)
	}
	return opts.timeout
}

// TestSlowCommandsGetALongerDefault covers the reported failure: `products
// check` and `packages discover` contact third-party registries through the
// Coordinator, and a shared 30-second deadline cut them off mid-work.
func TestSlowCommandsGetALongerDefault(t *testing.T) {
	fast := [][]string{
		{"health"},
		{"products", "list"},
		{"packages", "list", "vendor-a"},
	}
	for _, args := range fast {
		if got := resolveTimeout(t, args...); got != defaultTimeout {
			t.Errorf("%v: timeout = %s, want the default %s", args, got, defaultTimeout)
		}
	}

	slow := [][]string{
		{"products", "check"},
		{"products", "check", "vendor-a"},
		{"packages", "discover", "vendor-a"},
	}
	for _, args := range slow {
		if got := resolveTimeout(t, args...); got != slowTimeout {
			t.Errorf("%v: timeout = %s, want the slow default %s", args, got, slowTimeout)
		}
	}
}

// An explicit --timeout must survive on a slow command. Quietly replacing it
// would make the flag advisory, and there are legitimate reasons to want a
// short one — a scripted liveness probe, for instance.
func TestExplicitTimeoutWinsOnSlowCommands(t *testing.T) {
	if got := resolveTimeout(t, "products", "check", "--timeout", "5s"); got != 5*time.Second {
		t.Errorf("timeout = %s, want the 5s the operator asked for", got)
	}
}

func TestSwgwTimeoutEnvIsHonoured(t *testing.T) {
	t.Setenv("SWGW_TIMEOUT", "90s")

	if got := resolveTimeout(t, "health"); got != 90*time.Second {
		t.Errorf("fast command: timeout = %s, want 90s from SWGW_TIMEOUT", got)
	}
	// Set deliberately, so it applies to slow commands too rather than being
	// overridden by a default the operator has already replaced.
	if got := resolveTimeout(t, "products", "check"); got != 90*time.Second {
		t.Errorf("slow command: timeout = %s, want 90s from SWGW_TIMEOUT", got)
	}
}

func TestUnparseableEnvTimeoutFallsBack(t *testing.T) {
	t.Setenv("SWGW_TIMEOUT", "not-a-duration")

	// Non-empty but unusable: the fallback duration applies, and because the
	// operator did not successfully express a preference, the slow default
	// still would not fire. Better a working command than a startup failure on
	// a variable nobody meant to set.
	if got := resolveTimeout(t, "health"); got != defaultTimeout {
		t.Errorf("timeout = %s, want the default %s", got, defaultTimeout)
	}
}

func TestTimeoutErrorNamesTheFlag(t *testing.T) {
	hint := hintFor(errors.New("wrapped: " + v1.ErrTimeout.Error()))
	if hint != "" {
		t.Error("a lookalike message must not trigger the hint; only the sentinel does")
	}

	hint = hintFor(v1.ErrTimeout)
	if !strings.Contains(hint, "--timeout") {
		t.Errorf("the hint should name the flag, got: %q", hint)
	}
	if !strings.Contains(hint, "SWGW_TIMEOUT") {
		t.Errorf("the hint should name the environment variable, got: %q", hint)
	}
}

func TestUnreachableAndTimeoutShareExitCode(t *testing.T) {
	if got := exitCodeFor(v1.ErrTimeout); got != exitUnreachable {
		t.Errorf("exit code for a timeout = %d, want %d", got, exitUnreachable)
	}
	if got := exitCodeFor(v1.ErrUnreachable); got != exitUnreachable {
		t.Errorf("exit code for unreachable = %d, want %d", got, exitUnreachable)
	}
}

func TestProgressLineIsTruncatedToTerminalWidth(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := truncate(long, 78)

	if n := len([]rune(got)); n != 78 {
		t.Errorf("truncated to %d runes, want 78 — a longer line wraps, and a "+
			"carriage return then only rewrites the last wrapped row, which "+
			"looks exactly like a display that has stopped updating", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("a truncated line should say so")
	}
	if short := truncate("abc", 78); short != "abc" {
		t.Errorf("a short line must pass through unchanged, got %q", short)
	}
}

// Runes, not bytes: cutting a multi-byte character in half produces a
// replacement glyph that shifts the line and defeats the padding.
func TestTruncateCountsRunesNotBytes(t *testing.T) {
	s := strings.Repeat("é", 40) // 80 bytes, 40 runes
	if got := truncate(s, 40); got != s {
		t.Errorf("40 runes should fit in 40 columns, got %q", got)
	}
}

func TestProgressWidthHonoursColumns(t *testing.T) {
	t.Setenv("COLUMNS", "120")
	if got := progressWidth(); got != 118 {
		t.Errorf("progressWidth = %d, want 118 (COLUMNS minus the indent)", got)
	}

	t.Setenv("COLUMNS", "nonsense")
	if got := progressWidth(); got != 78 {
		t.Errorf("progressWidth = %d, want the 78 fallback", got)
	}
}
