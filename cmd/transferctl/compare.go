package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Comparing what a destination holds against what the source published.
//
// A transfer reports what it DID. It says 2489 jobs succeeded and 63.7 GiB
// moved, and every one of those numbers can be true while the destination is
// wrong: a tag that failed to apply, content deleted afterwards, a bundle
// assembled by two transfers of which one was stopped. There was no way to
// check — the only way to find out was to go and pull things by hand.
//
// So this asks the destination, artifact by artifact, and compares its answers
// against the source's own structure. It reads no transfer record.

func newCompareCommand() *cobra.Command {
	var to string

	cmd := &cobra.Command{
		Short: "Check that a destination holds what the source published",
		Long: "Asks the DESTINATION registry what it has, artifact by artifact, and\n" +
			"compares it against the source's own structure — every image, chart\n" +
			"and file, in every place the layout puts it, with its digest, its size\n" +
			"and the tags it should answer to.\n\n" +
			"It reads no transfer record. A transfer can report every job successful\n" +
			"and leave the destination wrong, and this is what says so.\n\n" +
			"Rows that DISAGREE come first, then the rows that match. Exits non-zero\n" +
			"when anything disagrees, so it can be the last line of a pipeline.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ComparePackage(cmd.Context(), args[0], args[1], to)
			if err != nil {
				return err
			}
			if err := render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderCompare(w, resp)
			}); err != nil {
				return err
			}
			if resp.Mismatched > 0 {
				return partialFailureError{msg: fmt.Sprintf(
					"%d of %d did not match", resp.Mismatched, len(resp.Rows))}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "",
		"the target to compare against (default: the product's default target)")

	// It reaches the destination registry through the Coordinator, so it
	// belongs with the slow commands.
	contactsRegistries(cmd)
	takes(cmd, "compare", productArg(), packageArg())
	return cmd
}

func renderCompare(w io.Writer, r *v1.CompareResponse) error {
	if len(r.Rows) == 0 {
		fmt.Fprintln(w, "Nothing to compare: the package has no artifacts recorded.")
		return nil
	}

	fmt.Fprintf(w, "%s %s vs %s\n", r.Product, r.Package, compareTarget(r))
	fmt.Fprintln(w)

	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  \tTYPE\tNAME\tSOURCE\tTARGET\tMATCH")
	for _, row := range r.Rows {
		marker := "  "
		verdict := "yes"
		if len(row.Differences) > 0 {
			// The rows somebody is looking for, marked so they are findable
			// with the eye rather than only by position.
			marker = "! "
			verdict = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			marker, row.Type, row.Name,
			describeSideOf(row.Source), describeSideOf(row.Target), verdict)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	renderDifferences(w, r)

	fmt.Fprintln(w)
	fmt.Fprintf(w, "%d of %d matched.\n", r.Matched, len(r.Rows))
	return nil
}

// renderDifferences lists what actually disagrees, under the table.
//
// Under it rather than in it, because a difference is a sentence and a table
// cell is not: "1.25.212 points at sha256:8533f4a71a43, not sha256:438001f263ab"
// does not fit a column, and truncating it removes the half that says what is
// wrong.
func renderDifferences(w io.Writer, r *v1.CompareResponse) {
	if r.Mismatched == 0 {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Differences")
	for _, row := range r.Rows {
		if len(row.Differences) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s\n", row.Name)
		for _, d := range row.Differences {
			fmt.Fprintf(w, "    %s\n", d)
		}
	}
}

// describeSideOf is one end of a row in one cell: the digest and the size.
//
// A dash where the content is absent, because that is the whole finding for
// that row and it should be visible without reading the sentence underneath.
func describeSideOf(s v1.CompareSide) string {
	if !s.Present {
		return "absent"
	}
	out := shortDigest(s.Digest)
	if size := int64Of(s.Size); size > 0 {
		out += "  " + humanBytes(s.Size)
	}
	return out
}

// compareTarget names the destination that was asked.
func compareTarget(r *v1.CompareResponse) string {
	if r.Target != "" {
		return r.Target
	}
	// The server resolved the product's default. Naming the repository it
	// actually reached beats printing nothing, and the rows all carry it.
	for _, row := range r.Rows {
		if row.Target.Repository != "" {
			return baseOf(row.Target.Repository)
		}
	}
	return "the default target"
}

// baseOf is the leading path segment shared by a bundle's destinations — the
// target's configured prefix.
func baseOf(repository string) string {
	if i := strings.Index(repository, "/"); i > 0 {
		return repository[:i]
	}
	return repository
}
