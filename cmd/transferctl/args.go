package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// Argument errors, and why they get this much machinery.
//
// Cobra's default is one line:
//
//	Error: accepts 2 arg(s), received 1
//
// It names a COUNT and nothing else. It does not say what the two arguments
// are, which one is missing, what shape the missing one takes, or how to find a
// value for it - and it is produced by the command that already knows all four.
// Somebody who mistypes `transferctl packages inspect my-product` gets a number
// and has to go and read `--help` to learn that the second argument is a tag.
//
// Worse, that first attempt often "works": omit the tag and you get
// `accepts 2 arg(s), received 1`, but pass a PRODUCT and a wrong tag and you
// get `package not found`, which reads like the package is gone rather than
// like the reference was wrong.
//
// So a command declares its arguments once, and that declaration produces the
// usage line, the per-argument explanation, the example, and the error when
// something is missing. One source, and it cannot drift from the command it
// documents.

// argSpec is one positional argument.
type argSpec struct {
	// Name is the placeholder, without angle brackets: "product", "package".
	Name string
	// What it is, in a few words, as a person would say it.
	Help string
	// Find is the command that lists valid values, when one exists. This is the
	// part that turns an error into something actionable: nobody guesses a
	// product name, and everybody can run one command that prints them.
	Find string
	// Optional marks a trailing argument that may be omitted, and Default says
	// what happens then.
	Optional bool
	Default  string
}

// usageError is a bad invocation, as opposed to a request that failed.
//
// Separated so it can exit 2 rather than 1: a script that cannot tell "I called
// this wrong" from "the thing I asked about does not exist" retries the wrong
// one.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// takes declares a command's positional arguments.
//
// It sets Use, Args and the argument section of the long help from one
// declaration, so the three cannot disagree - and they did: `Use` said
// `<product> <package>` while the error said "accepts 2 arg(s)", and only one
// of those was in front of the user when it mattered.
func takes(cmd *cobra.Command, verb string, specs ...argSpec) {
	cmd.Use = usageLine(verb, specs)
	cmd.Args = func(c *cobra.Command, args []string) error {
		required := 0
		for _, s := range specs {
			if !s.Optional {
				required++
			}
		}
		if len(args) >= required && len(args) <= len(specs) {
			return nil
		}
		return usageError{msg: argMessage(c, specs, args)}
	}

	if section := argSection(specs); section != "" {
		cmd.Long = strings.TrimRight(cmd.Long, "\n") + "\n\n" + section
		cmd.Long = strings.TrimLeft(cmd.Long, "\n")
	}
}

// usageLine renders "inspect <product> <package>".
func usageLine(verb string, specs []argSpec) string {
	out := verb
	for _, s := range specs {
		if s.Optional {
			out += " [" + s.Name + "]"
			continue
		}
		out += " <" + s.Name + ">"
	}
	return out
}

// argSection is the per-argument block appended to the long help.
func argSection(specs []argSpec) string {
	if len(specs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Arguments:\n")
	b.WriteString(argLines(specs))
	return strings.TrimRight(b.String(), "\n")
}

// argLines renders the per-argument block, aligned to the widest placeholder so
// a long one such as <directory> does not push its own description out of line.
func argLines(specs []argSpec) string {
	width := 0
	for _, s := range specs {
		if n := len(angled(s)); n > width {
			width = n
		}
	}

	var b strings.Builder
	for _, s := range specs {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, angled(s), s.Help)
		if s.Default != "" {
			fmt.Fprintf(&b, "  %-*s  omitted: %s\n", width, "", s.Default)
		}
		if s.Find != "" {
			fmt.Fprintf(&b, "  %-*s  list them: %s\n", width, "", s.Find)
		}
	}
	return b.String()
}

func angled(s argSpec) string {
	if s.Optional {
		return "[" + s.Name + "]"
	}
	return "<" + s.Name + ">"
}

// argMessage is the whole point of this file: an error that says what was
// expected, WHICH ONE is missing, and where to find a value for it.
func argMessage(cmd *cobra.Command, specs []argSpec, args []string) string {
	path := cmd.CommandPath()

	var b strings.Builder
	switch {
	case len(args) > len(specs):
		fmt.Fprintf(&b, "too many arguments: %s takes %s, and got %d.",
			path, countPhrase(specs), len(args))
	case len(args) == 0:
		fmt.Fprintf(&b, "%s needs %s, and got none.", path, countPhrase(specs))
	default:
		fmt.Fprintf(&b, "%s needs %s, and got %d.", path, countPhrase(specs), len(args))
	}

	fmt.Fprintf(&b, "\n\n  %s\n", usageLine(path, specs))

	// Mark what is missing under the usage line, so the eye lands on it without
	// counting words.
	if len(args) < len(specs) {
		b.WriteString("  " + marker(path, specs, len(args)) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(argLines(specs))
	return strings.TrimRight(b.String(), "\n")
}

// marker draws a caret under the first argument that was not supplied.
func marker(path string, specs []argSpec, given int) string {
	col := len(path)
	for i, s := range specs {
		if i == given {
			return strings.Repeat(" ", col+1) + strings.Repeat("^", len(angled(s))) + " missing"
		}
		col += 1 + len(angled(s))
	}
	return ""
}

// countPhrase renders "2 arguments" or "1 or 2 arguments" for an optional tail.
func countPhrase(specs []argSpec) string {
	required := 0
	for _, s := range specs {
		if !s.Optional {
			required++
		}
	}
	switch {
	case required == len(specs) && required == 1:
		return "1 argument"
	case required == len(specs):
		return fmt.Sprintf("%d arguments", required)
	case required == 0 && len(specs) == 1:
		return "at most 1 argument"
	default:
		return fmt.Sprintf("%d to %d arguments", required, len(specs))
	}
}

// group configures a command that only holds subcommands.
//
// Two fixes in three lines. An unrecognised subcommand used to print the help
// text and EXIT ZERO - so `transferctl pkg inspct a b` looked, to a script,
// exactly like success. And cobra's suggestion machinery is off by default,
// which throws away the one thing it can offer for a typo.
func group(cmd *cobra.Command) *cobra.Command {
	cmd.Args = func(c *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		return usageError{msg: unknownSubcommand(c, args[0])}
	}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if len(args) > 0 {
			return usageError{msg: unknownSubcommand(c, args[0])}
		}
		return usageError{msg: missingSubcommand(c)}
	}
	cmd.SilenceUsage = true
	// Off by default in cobra, which throws away the one thing it can offer for
	// a typo. Two is the usual edit distance for a transposition or a dropped
	// letter - `inspct` for `inspect`, `descibe` for `describe`.
	cmd.SuggestionsMinimumDistance = 2
	return cmd
}

func unknownSubcommand(cmd *cobra.Command, name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s has no subcommand %q.\n", cmd.CommandPath(), name)

	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		b.WriteString("\nDid you mean:\n")
		for _, s := range suggestions {
			fmt.Fprintf(&b, "  %s %s\n", cmd.CommandPath(), s)
		}
	}
	b.WriteString("\n" + subcommandList(cmd))
	return strings.TrimRight(b.String(), "\n")
}

func missingSubcommand(cmd *cobra.Command) string {
	return fmt.Sprintf("%s needs a subcommand.\n\n%s",
		cmd.CommandPath(), subcommandList(cmd))
}

func subcommandList(cmd *cobra.Command) string {
	var b strings.Builder
	b.WriteString("Available:\n")

	width := 0
	for _, c := range visibleSubcommands(cmd) {
		if n := len(c.Name()); n > width {
			width = n
		}
	}
	for _, c := range visibleSubcommands(cmd) {
		fmt.Fprintf(&b, "  %s %-*s   %s\n", cmd.CommandPath(), width, c.Name(), c.Short)
	}
	return strings.TrimRight(b.String(), "\n")
}

func visibleSubcommands(cmd *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, c := range cmd.Commands() {
		if c.Hidden || c.Name() == "help" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// Shared argument descriptions, so every command that takes a product or a
// package says the same thing about it.
func productArg() argSpec {
	return argSpec{
		Name: "product",
		Help: "the configured product to look in",
		Find: "transferctl products list",
	}
}

func optionalProductArg(whenOmitted string) argSpec {
	a := productArg()
	a.Optional = true
	a.Default = whenOmitted
	return a
}

// packageArg is the one that caused the confusion this file exists for.
//
// A package is identified by a TAG, not by a product version, and a bare
// product name is never enough. Saying that here - with the forms and an
// example - is the difference between one attempt and three.
func packageArg() argSpec {
	return argSpec{
		Name: "package",
		Help: "a tag, a digest, or repository:tag - e.g. orb_23.8.1076, " +
			"sha256:ccbd…, orbs/cfx-5000-k8s:orb_23.8.1076",
		Find: "transferctl packages list <product>",
	}
}
