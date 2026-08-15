package main

import (
	"fmt"
	"io"
	"strings"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
	"github.com/spf13/cobra"
)

// Why a transfer is failing, as opposed to which jobs are failing.
//
// # The output shape, and why it is not a table
//
// `transfers jobs --failed` is a table, and for this question a table is the
// wrong instrument. Five hundred manifests rejected by the destination are five
// hundred rows that differ only in the digest and the destination path — the
// two parts of the message carrying no information about what went wrong — and
// the part that does is at the END of a line the terminal has already cut off.
// No column width fixes that, because the problem is not the width. It is that
// the same fact is being restated five hundred times.
//
// So this prints one BLOCK per distinct cause: what happened, how many jobs it
// is holding, whether they will retry themselves, one example to go and look
// at, and what to do. Usually two or three blocks, on one screen.

func newTransfersFailuresCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Say why a transfer is failing",
		Long: "Groups every failing job by CAUSE rather than listing them.\n" +
			"A bundle whose manifests are all rejected has hundreds of failing\n" +
			"jobs and one reason; this prints the reason.",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := newClient().ListTransferFailures(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderFailures(w, resp)
			})
		},
	}

	takes(cmd, "failures", transferArg())
	return cmd
}

func renderFailures(w io.Writer, resp *v1.ListFailuresResponse) error {
	if len(resp.Failures) == 0 {
		fmt.Fprintln(w, "Nothing is failing in this transfer.")
		return nil
	}

	failed, retrying := 0, 0
	for _, g := range resp.Failures {
		failed += g.Failed
		retrying += g.Retrying
	}

	fmt.Fprintf(w, "%s in transfer %s: %s.\n\n",
		jobsAffected(failed, retrying), shortID(resp.TransferID),
		plural(len(resp.Failures), "distinct cause", "distinct causes"))

	for i, g := range resp.Failures {
		if i > 0 {
			fmt.Fprintln(w)
		}
		renderFailure(w, g)
	}

	// The next step, stated once at the bottom rather than after every block.
	if anyRetryable(resp.Failures) && failed > 0 {
		fmt.Fprintf(w, "\nOnce the cause is gone: transferctl transfers retry %s\n",
			shortID(resp.TransferID))
	}
	return nil
}

// renderFailure prints one cause: what it is, then why, then where, then what
// to do about it.
//
// # Why the classification leads and the message does not
//
// The message is a sentence of arbitrary length — a registry's own words, a
// path, a status — and it used to be the first line of the block. On a real
// failure it wrapped across the terminal before the reader reached the counts,
// so the two short facts that decide whether this block is worth reading at all
// (what class of failure it is, and how much it is holding) arrived after the
// one that does not fit. They lead now, and the message sits under them.
func renderFailure(w io.Writer, g v1.FailureGroup) {
	head := make([]string, 0, 4)
	if g.Class != "" {
		head = append(head, g.Class)
	}
	head = append(head, jobsAffected(g.Failed, g.Retrying))
	if len(g.Kinds) > 0 {
		head = append(head, strings.Join(g.Kinds, " and "))
	}
	if len(g.Waves) > 0 {
		head = append(head, waveList(g.Waves))
	}

	fmt.Fprintf(w, "  %s\n", strings.Join(head, " · "))
	fmt.Fprintf(w, "    %s\n", g.Message)

	if g.ExampleDigest != "" {
		where := shortDigest(g.ExampleDigest)
		if g.ExampleRepository != "" {
			where += " → " + g.ExampleRepository
		}
		fmt.Fprintf(w, "    Example  %s\n", where)
	}

	fmt.Fprintf(w, "    Action   %s\n", advice(g))
}

// advice turns a class into the sentence an operator needs.
//
// The class already decides whether the retry policy will try again, so this
// says the same thing in words rather than leaving the reader to look it up —
// and, crucially, distinguishes "this will fix itself" from "this will fail
// identically eight more times and then stop".
func advice(g v1.FailureGroup) string {
	switch g.Class {
	case "auth":
		// A credential that pushed the content and is refused only on the tag
		// is not a wrong credential, and saying "check the secret" sends the
		// reader to the one place the answer is not. Applying a tag is a write
		// to a path the earlier writes never touched, and on registries that
		// separate the two — Artifactory wants Delete on the permission target
		// to move an existing tag — it is refused separately.
		//
		// Recognised by the leading verb of the registry's own message, which
		// is the operation the write attempted. The message carries the rest of
		// what the reader needs — including whether the tag already existed —
		// so this says only what is not already on the line above it.
		if strings.HasPrefix(g.Message, "tag ") {
			return "Not retryable. The credential writes content to this path but " +
				"is refused the tag; check the target's permission to write tags there."
		}
		return "Not retryable. The credential was refused; check the target's secret, then retry."
	case "unsupported":
		return "Not retryable. The registry rejects this content."
	case "not_found":
		return "Not retryable. Content the job expected at the target is absent."
	case "blob_unknown":
		return "Self-healing. The affected blobs are re-queued automatically."
	case "configuration":
		return "Not retryable. The worker cannot execute this job; check `transferctl health`."
	case "rate_limited":
		return "Retrying, backing off as the registry directed."
	case "timeout", "unavailable":
		return "Retrying. If it does not recover, measure the path with `transferctl calibrate`."
	case "integrity":
		return "Retrying, capped low. Content did not hash as claimed."
	default:
		if g.Retryable {
			return "Retrying."
		}
		return "Not retryable."
	}
}

// jobsAffected says how much is stuck and how much is still trying, because
// the two need different responses and a single total hides which is which.
func jobsAffected(failed, retrying int) string {
	switch {
	case failed > 0 && retrying > 0:
		return fmt.Sprintf("%s failed, %d retrying", plural(failed, "job", "jobs"), retrying)
	case failed > 0:
		return plural(failed, "job failed", "jobs failed")
	case retrying > 0:
		return plural(retrying, "job retrying", "jobs retrying")
	default:
		return "no jobs"
	}
}

func waveList(waves []int) string {
	if len(waves) == 1 {
		return fmt.Sprintf("wave %d", waves[0])
	}
	out := make([]string, 0, len(waves))
	for _, v := range waves {
		out = append(out, fmt.Sprint(v))
	}
	return "waves " + strings.Join(out, ", ")
}

func anyRetryable(groups []v1.FailureGroup) bool {
	for _, g := range groups {
		if g.Retryable {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
