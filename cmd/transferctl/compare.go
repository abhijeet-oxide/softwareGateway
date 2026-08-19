package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Comparing two places.
//
// The questions operators ask look like several tools and are one: did the
// transfer land, did the promotion land, what changed in this release, was
// anything mutated, is there anything there nobody put. All of them are "walk
// two bundles and align their components", so all of them are this command with
// different arguments.
//
// Nothing here reads a transfer record. A transfer reports what it DID - 2489
// jobs succeeded, 63.7 GiB moved - and every one of those numbers can be true
// while the destination is wrong.

func newCompareCommand() *cobra.Command {
	var (
		from       string
		to         string
		at         string
		all        bool
		showFiles  bool
		fileBudget int64
	)

	cmd := &cobra.Command{
		Short: "Compare a release between two places, or two releases in one",
		Long: "Walks BOTH ends and aligns them component by component - every image,\n" +
			"chart and file, with its digest, its size and the name it answers to.\n\n" +
			"The ends are symmetric, so one command covers every question:\n\n" +
			"  compare P 25.7                          did it land at the default target?\n" +
			"  compare P 25.7 --to stage               ...at that one?\n" +
			"  compare P 25.7 --from lab --to prod     did the promotion land?\n" +
			"  compare P 25.7 25.6                     what changed in the release?\n" +
			"  compare P 25.7 25.6 --at stage          ...and did all of it arrive?\n\n" +
			"For a component that changed, the answer is given in FILES: `2 files\n" +
			"changed` rather than `2 layers changed`, and --files names them. This\n" +
			"costs nothing and downloads nothing - an OCI artifact names one file\n" +
			"per layer and states its content digest, so two of those lists aligned\n" +
			"by path IS the answer.\n\n" +
			"By default only the DIFFERENCES are printed. --all shows every\n" +
			"component, including the ones that agree.\n\n" +
			"Exits non-zero when the two ends differ, so it can end a pipeline.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if at != "" && (from != "" || to != "") {
				return usageError{msg: "--at names both ends at once; use it or " +
					"--from/--to, not both"}
			}
			if at != "" {
				from, to = at, at
			}

			resp, err := newClient().ComparePackage(cmd.Context(), args[0], args[1],
				v1.CompareRequest{
					From: from, To: to, Against: against(args),
					FileBudgetBytes: budgetBytes(fileBudget),
				})
			if err != nil {
				return err
			}
			if err := render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderCompare(w, resp, all, showFiles)
			}); err != nil {
				return err
			}
			if differences(resp) > 0 {
				return partialFailureError{msg: fmt.Sprintf(
					"%s differ", plural(differences(resp), "component", "components"))}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&from, "from", "",
		"the first end: a source or target name (default: where the package was discovered)")
	cmd.Flags().StringVar(&to, "to", "",
		"the second end: a source or target name (default: the product's default target, "+
			"or the source when two versions are named)")
	cmd.Flags().StringVar(&at, "at", "",
		"both ends are this one place - for comparing two versions")
	cmd.Flags().BoolVar(&all, "all", false,
		"show every component, not only the ones that differ")
	cmd.Flags().BoolVar(&showFiles, "files", false,
		"for a changed component, name the files that changed rather than counting them")
	cmd.Flags().Int64Var(&fileBudget, "file-budget", 0,
		"accepted and ignored: comparing files costs nothing and downloads nothing")
	_ = cmd.Flags().MarkDeprecated("file-budget",
		"file comparison reads the manifests and downloads nothing, so there is no budget to set")

	// It reaches both registries through the Coordinator, so it belongs with
	// the slow commands.
	contactsRegistries(cmd)
	takes(cmd, "compare", productArg(), packageArg(), againstArg())
	return cmd
}

// againstArg is the optional second version.
//
// Positional rather than a flag, because `compare P 25.7 25.6` is how somebody
// says it out loud, and the two arguments are the same kind of thing.
func againstArg() argSpec {
	return argSpec{
		Name:     "against",
		Help:     "a second version, to compare against the first",
		Optional: true,
		Default:  "compares the same version in two places",
	}
}

// budgetBytes turns the flag's MiB into the bytes the API takes.
//
// MiB in the flag because that is the unit somebody thinks in when deciding
// whether to let a comparison pull a layer, and bytes on the wire because a
// unit in an API field is a second thing to get wrong.
func budgetBytes(mib int64) int64 {
	if mib < 0 {
		// Negative is "open nothing", and it has to survive the conversion:
		// -1 MiB in bytes is still negative, which is what the server reads.
		return -1
	}
	return mib << 20
}

func against(args []string) string {
	if len(args) > 2 {
		return args[2]
	}
	return ""
}

func differences(r *v1.CompareResponse) int {
	return r.Changed + r.OnlyA + r.OnlyB
}

// renderCompare lays the comparison out as a diff, because that is what it is.
//
// The markers are the ones everybody already reads without being told:
//
//   - only on the first end
//   - only on the second
//     ~  present on both, and different
//     (blank) identical
func renderCompare(w io.Writer, r *v1.CompareResponse, all, layers bool) error {
	fmt.Fprintf(w, "%s\n\n", r.Product)
	fmt.Fprintf(w, "  A  %s\n", endLine(r.A))
	fmt.Fprintf(w, "  B  %s\n", endLine(r.B))

	if len(r.Rows) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Neither end has any components.")
		return nil
	}

	shown := renderRows(w, r, all)
	renderDetails(w, r, layers)
	renderExtras(w, r)

	fmt.Fprintln(w)
	fmt.Fprintln(w, summaryLine(r))
	if hidden := len(r.Rows) - shown; !all && hidden > 0 && differences(r) > 0 {
		fmt.Fprintf(w, "%d identical not shown; --all shows every component.\n", hidden)
	}
	return nil
}

func endLine(e v1.CompareEnd) string {
	if e.Label == "" {
		return e.Reference
	}
	return fmt.Sprintf("%-12s %s", e.Label, e.Reference)
}

// renderRows prints the table and reports how many rows it printed.
//
// Nothing is printed at all when there is nothing to show: an identical
// comparison ending in a bare column header reads as a tool that failed to
// produce output, where one line saying "Identical" reads as an answer.
func renderRows(w io.Writer, r *v1.CompareResponse, all bool) int {
	rows := make([]v1.CompareRow, 0, len(r.Rows))
	for _, row := range r.Rows {
		if row.Verdict == "same" && !all {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return 0
	}

	fmt.Fprintln(w)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, " \tTYPE\tCOMPONENT\tA\tB")
	for _, row := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			verdictMark(row.Verdict), row.Type, row.Name,
			cell(row.A), cell(row.B))
	}
	_ = tw.Flush()
	return len(rows)
}

func verdictMark(verdict string) string {
	switch verdict {
	case "only-a":
		return "-"
	case "only-b":
		return "+"
	case "changed":
		return "~"
	default:
		return " "
	}
}

// cell is one end's account of a component: what it is called and what it is.
func cell(s *v1.CompareSide) string {
	if s == nil {
		return "absent"
	}
	out := shortDigest(s.Digest)
	if s.Tag != "" {
		out = s.Tag + "  " + out
	}
	return out
}

// renderDetails states each disagreement as a sentence, under the table.
//
// Under it rather than in it, because a difference is a sentence and a table
// cell is not: "cfx-5000-product/lms:1.25.212 points at sha256:8533f4a71a43 on
// the second side" does not fit a column, and truncating it removes the half
// that says what is wrong.
func renderDetails(w io.Writer, r *v1.CompareResponse, showFiles bool) {
	if differences(r) == 0 {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Differences")
	for _, row := range r.Rows {
		if len(row.Differences) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s %s\n", verdictMark(row.Verdict), row.Name)
		for _, d := range row.Differences {
			fmt.Fprintf(w, "      %s\n", d)
		}
		renderFileChange(w, row, showFiles)
	}
}

// renderFileChange says what changed INSIDE a component.
//
// Counted by default and named on request, because the two answer different
// questions. "3 files changed" is what somebody scanning a release wants; the
// paths are what somebody who has found the interesting component wants, and
// printing four hundred of them at the first reader would bury everything else.
func renderFileChange(w io.Writer, row v1.CompareRow, showFiles bool) {
	summary := fileSummary(row)
	if summary == "" {
		return
	}

	if !showFiles {
		fmt.Fprintf(w, "      %s; --files names them\n", summary)
		return
	}

	fmt.Fprintf(w, "      %s\n", summary)
	// Changed, added, removed - in that order, because that is the order they
	// matter in. Unchanged files are carried by the API for context and are
	// not printed: a component with four hundred files and one edit would bury
	// the edit under the other three hundred and ninety-nine.
	for _, mark := range []struct {
		verdict string
		symbol  string
	}{{"changed", "~"}, {"only-b", "+"}, {"only-a", "-"}} {
		for _, f := range row.Files {
			if f.Verdict == mark.verdict {
				fmt.Fprintf(w, "        %s %s\n", mark.symbol, f.Path)
			}
		}
	}
}

// fileSummary counts the file-level change in one clause.
func fileSummary(row v1.CompareRow) string {
	counts := map[string]int{}
	for _, f := range row.Files {
		counts[f.Verdict]++
	}

	var parts []string
	if n := counts["changed"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", n))
	}
	if n := counts["only-b"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d added", n))
	}
	if n := counts["only-a"]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return plural(total(row), "file", "files") + ": " + strings.Join(parts, ", ")
}

// total is how many of a component's files DIFFER - not how many it has.
func total(row v1.CompareRow) int {
	n := 0
	for _, f := range row.Files {
		if f.Verdict != "same" {
			n++
		}
	}
	return n
}

// renderExtras names content in a bundle's own repository that the release does
// not account for.
//
// Only for the bundle's repository: a component's repository legitimately holds
// every other version of that component and is deliberately not asked. What
// counts as unexplained is decided by what each tag RESOLVES TO, so this is
// content the release genuinely does not reach rather than a tag spelled
// unfamiliarly.
func renderExtras(w io.Writer, r *v1.CompareResponse) {
	for _, side := range []struct {
		end       v1.CompareEnd
		tags      []string
		truncated bool
	}{
		{r.A, r.ExtraTagsA, r.ExtraTruncatedA},
		{r.B, r.ExtraTagsB, r.ExtraTruncatedB},
	} {
		if len(side.tags) == 0 && !side.truncated {
			continue
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Also in %s, not part of this release\n", side.end.Label)
		if len(side.tags) > 0 {
			fmt.Fprintf(w, "  %s\n", strings.Join(side.tags, ", "))
		}
		if side.truncated {
			// A partial account presented as a whole one is worse than none:
			// somebody would conclude the rest of the repository was checked.
			fmt.Fprintln(w, "  (the repository holds more tags than this check "+
				"resolves; the list above is partial)")
		}
	}
}

// summaryLine is the one line somebody reads before anything else.
func summaryLine(r *v1.CompareResponse) string {
	if differences(r) == 0 && len(r.ExtraTagsA) == 0 && len(r.ExtraTagsB) == 0 {
		return fmt.Sprintf("Identical: %s match.",
			plural(r.Same, "component", "components"))
	}

	parts := []string{fmt.Sprintf("%d identical", r.Same)}
	if r.Changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed", r.Changed))
	}
	if r.OnlyA > 0 {
		parts = append(parts, fmt.Sprintf("%d only in A", r.OnlyA))
	}
	if r.OnlyB > 0 {
		parts = append(parts, fmt.Sprintf("%d only in B", r.OnlyB))
	}
	return strings.Join(parts, ", ") + "."
}
