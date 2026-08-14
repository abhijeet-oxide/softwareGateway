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
// Nothing here reads a transfer record. A transfer reports what it DID — 2489
// jobs succeeded, 63.7 GiB moved — and every one of those numbers can be true
// while the destination is wrong.

func newCompareCommand() *cobra.Command {
	var (
		from    string
		to      string
		at      string
		all     bool
		verbose bool
	)

	cmd := &cobra.Command{
		Short: "Compare a release between two places, or two releases in one",
		Long: "Walks BOTH ends and aligns them component by component — every image,\n" +
			"chart and file, with its digest, its size and the name it answers to.\n\n" +
			"The ends are symmetric, so one command covers every question:\n\n" +
			"  compare P 25.7                          did it land at the default target?\n" +
			"  compare P 25.7 --to stage               ...at that one?\n" +
			"  compare P 25.7 --from lab --to prod     did the promotion land?\n" +
			"  compare P 25.7 25.6                     what changed in the release?\n" +
			"  compare P 25.7 25.6 --at stage          ...and did all of it arrive?\n\n" +
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
				v1.CompareRequest{From: from, To: to, Against: against(args)})
			if err != nil {
				return err
			}
			if err := render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderCompare(w, resp, all, verbose)
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
		"both ends are this one place — for comparing two versions")
	cmd.Flags().BoolVar(&all, "all", false,
		"show every component, not only the ones that differ")
	cmd.Flags().BoolVar(&verbose, "layers", false,
		"for a changed component, name the layers that changed")

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
func renderDetails(w io.Writer, r *v1.CompareResponse, layers bool) {
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
		if layers {
			renderFiles(w, row)
		} else if n := len(row.FilesAdded) + len(row.FilesRemoved); n > 0 {
			fmt.Fprintf(w, "      %s changed; --layers names them\n",
				plural(n, "layer", "layers"))
		}
	}
}

// renderFiles names what a change added and removed.
//
// Titled where the vendor titled the layer, which for a generic artifact is the
// path of the file inside it — so "which files changed" is answered literally
// rather than as a count of digests.
func renderFiles(w io.Writer, row v1.CompareRow) {
	for _, f := range row.FilesAdded {
		fmt.Fprintf(w, "      + %s\n", f)
	}
	for _, f := range row.FilesRemoved {
		fmt.Fprintf(w, "      - %s\n", f)
	}
}

// renderExtras names content in a bundle's own repository that the bundle does
// not account for.
//
// Only for the bundle's repository, where the question is well defined: an orb
// gets a repository to itself, so anything else in it is unexplained. A
// component's repository legitimately holds every other version of that
// component and is deliberately not asked.
func renderExtras(w io.Writer, r *v1.CompareResponse) {
	for _, side := range []struct {
		end  v1.CompareEnd
		tags []string
	}{{r.A, r.ExtraTagsA}, {r.B, r.ExtraTagsB}} {
		if len(side.tags) == 0 {
			continue
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Also in %s, not part of this release\n", side.end.Label)
		fmt.Fprintf(w, "  %s\n", strings.Join(side.tags, ", "))
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
