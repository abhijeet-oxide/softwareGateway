package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// An argument error has one job: leave the reader able to type the command
// correctly on the next attempt, without opening --help and without guessing.
//
// These assert the CONTENT rather than the wording, so the text can be improved
// without breaking them — but the four things below have to be present, because
// each one was missing from the message this replaced and each one cost a round
// trip.
func TestArgErrorSaysWhatIsMissingAndHowToFindIt(t *testing.T) {
	cmd := &cobra.Command{Short: "inspect a package"}
	root := &cobra.Command{Use: "transferctl"}
	pkgs := &cobra.Command{Use: "packages"}
	root.AddCommand(pkgs)
	pkgs.AddCommand(cmd)
	takes(cmd, "inspect", productArg(), packageArg())

	err := cmd.Args(cmd, []string{"my-product"})
	if err == nil {
		t.Fatal("one argument to a two-argument command was accepted")
	}
	msg := err.Error()

	for _, want := range []struct{ what, substr string }{
		{"the full command path", "transferctl packages inspect"},
		{"the shape of the call", "<product> <package>"},
		{"which argument is missing", "missing"},
		{"what a package reference IS", "tag"},
		{"an example of one", "orb_23.8.1076"},
		{"where to find a value", "transferctl packages list"},
	} {
		if !strings.Contains(msg, want.substr) {
			t.Errorf("the message does not carry %s (%q):\n%s", want.what, want.substr, msg)
		}
	}
}

// The caret has to land under the argument that is missing, not under the first
// one — the whole point is not having to count words.
func TestArgErrorMarksTheMissingArgument(t *testing.T) {
	specs := []argSpec{productArg(), packageArg()}
	line := usageLine("transferctl packages inspect", specs)
	mark := marker("transferctl packages inspect", specs, 1)

	caret := strings.Index(mark, "^")
	if caret < 0 {
		t.Fatalf("no caret in %q", mark)
	}
	if got := strings.Index(line, "<package>"); got != caret {
		t.Errorf("caret at column %d, <package> at column %d:\n%s\n%s",
			caret, got, line, mark)
	}
}

// A bad invocation is not the same failure as a request that did not find
// anything, and a script has to be able to tell them apart.
func TestArgErrorsExitTwo(t *testing.T) {
	cmd := &cobra.Command{Use: "x"}
	takes(cmd, "x", productArg())

	err := cmd.Args(cmd, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := exitCodeFor(err); got != exitUsage {
		t.Errorf("exit code = %d, want %d (usage)", got, exitUsage)
	}
}

// An optional argument must not be reported as missing, and the message has to
// say what happens when it is left out.
func TestOptionalArgumentsAreOptional(t *testing.T) {
	cmd := &cobra.Command{Use: "discover"}
	takes(cmd, "discover", optionalProductArg("scans every product being polled"))

	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("an optional argument was required: %v", err)
	}
	if err := cmd.Args(cmd, []string{"p"}); err != nil {
		t.Errorf("supplying the optional argument failed: %v", err)
	}

	err := cmd.Args(cmd, []string{"p", "extra"})
	if err == nil {
		t.Fatal("a second argument was accepted by a one-argument command")
	}
	if !strings.Contains(err.Error(), "too many") {
		t.Errorf("message does not say there are too many arguments:\n%s", err)
	}
	if !strings.Contains(cmd.Long, "scans every product being polled") {
		t.Errorf("the help does not say what omitting the argument does:\n%s", cmd.Long)
	}
}

// A mistyped subcommand used to print help and EXIT ZERO, which to a script is
// indistinguishable from success.
func TestUnknownSubcommandFailsAndSuggests(t *testing.T) {
	// The children need a RunE: cobra's SuggestionsFor skips a command it
	// considers unavailable, and "has nothing to run" is one of the ways to be
	// unavailable. Real commands all have one.
	noop := func(*cobra.Command, []string) error { return nil }
	parent := group(&cobra.Command{Use: "packages"})
	parent.AddCommand(&cobra.Command{Use: "inspect", Short: "inspect it", RunE: noop})
	parent.AddCommand(&cobra.Command{Use: "list", Short: "list them", RunE: noop})

	err := parent.Args(parent, []string{"inspct"})
	if err == nil {
		t.Fatal("an unknown subcommand was accepted")
	}
	if got := exitCodeFor(err); got != exitUsage {
		t.Errorf("exit code = %d, want %d", got, exitUsage)
	}

	msg := err.Error()
	if !strings.Contains(msg, "inspct") {
		t.Errorf("the message does not repeat what was typed:\n%s", msg)
	}
	if !strings.Contains(msg, "Did you mean") || !strings.Contains(msg, "inspect") {
		t.Errorf("no suggestion for a one-letter typo:\n%s", msg)
	}
	if !strings.Contains(msg, "list") {
		t.Errorf("the message does not list what is available:\n%s", msg)
	}
}

// A group invoked bare must say what it needs rather than printing help and
// succeeding.
func TestBareGroupFails(t *testing.T) {
	parent := group(&cobra.Command{Use: "packages"})
	parent.AddCommand(&cobra.Command{Use: "list", Short: "list them",
		RunE: func(*cobra.Command, []string) error { return nil }})

	err := parent.RunE(parent, nil)
	if err == nil {
		t.Fatal("a group with no subcommand succeeded")
	}
	if !strings.Contains(err.Error(), "needs a subcommand") {
		t.Errorf("unexpected message:\n%s", err)
	}
}

// The usage line, the argument list and the validator all come from one
// declaration, so they cannot drift — which they had: `Use` said
// `<product> <package>` while the error said "accepts 2 arg(s)".
func TestTakesDrivesUseAndHelpTogether(t *testing.T) {
	cmd := &cobra.Command{Short: "s"}
	takes(cmd, "describe", productArg(), packageArg())

	if cmd.Use != "describe <product> <package>" {
		t.Errorf("Use = %q", cmd.Use)
	}
	for _, want := range []string{"<product>", "<package>", "transferctl products list"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("the long help does not mention %q:\n%s", want, cmd.Long)
		}
	}
}

// Every command a user can reach must document its arguments. A new command
// that forgets is the failure mode this whole file exists to prevent, and a
// review is not a reliable place to catch it.
func TestEveryCommandDocumentsItsArguments(t *testing.T) {
	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, c := range cmd.Commands() {
			if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
				continue
			}
			if len(c.Commands()) > 0 {
				// A group: it must reject an unknown subcommand rather than
				// printing help and succeeding.
				if c.Args == nil || c.RunE == nil {
					t.Errorf("%s holds subcommands but is not wrapped in group()", c.CommandPath())
				}
				walk(c)
				continue
			}
			if !strings.Contains(c.Use, "<") && !strings.Contains(c.Use, "[") {
				// Takes no arguments; nothing to document.
				if c.Args == nil {
					t.Errorf("%s does not declare Args, so it accepts anything silently",
						c.CommandPath())
				}
				continue
			}
			if !strings.Contains(c.Long, "Arguments:") {
				t.Errorf("%s takes arguments but does not explain them; use takes()",
					c.CommandPath())
			}
		}
	}
	walk(newRootCommand())
}
