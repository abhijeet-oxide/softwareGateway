package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// `transferctl transfers retry` — carry on after whatever stopped it.

func newTransfersRetryCommand() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Aliases: []string{"resume"},
		Short:   "Requeue a transfer's failed jobs and carry on",
		Long: "Returns every job that gave up to the queue, with a fresh attempt\n" +
			"budget and no backoff, and reopens the transfer.\n\n" +
			"THIS RESUMES, IT DOES NOT RESTART. Blobs already at the destination\n" +
			"are found by the placement check or a HEAD and cost nothing the\n" +
			"second time, and a blob that was partway through keeps the bytes it\n" +
			"had moved. A transfer that was 80% done before the network went\n" +
			"picks up at 80%.\n\n" +
			"After an outage, `--all` retries every transfer that has failed jobs.\n" +
			"That is the usual shape: an outage does not fail one transfer, it\n" +
			"fails every transfer that was running.\n\n" +
			"Nothing here can restart a transfer that was CANCELLED — that was\n" +
			"somebody's decision — or one that succeeded.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(args) > 0 {
				return usageError{msg: "--all retries every failed transfer; do not also name one"}
			}
			if !all && len(args) == 0 {
				return usageError{msg: "name a transfer to retry, or pass --all"}
			}

			client := newClient()
			var (
				resp *v1.RetryTransferResponse
				err  error
			)
			if all {
				resp, err = client.RetryTransfers(cmd.Context())
			} else {
				resp, err = client.RetryTransfer(cmd.Context(), args[0])
			}
			if err != nil {
				return err
			}

			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderRetry(w, resp, all)
			})
		},
	}

	cmd.Flags().BoolVar(&all, "all", false,
		"retry every transfer that has failed jobs")

	// The argument is optional because --all replaces it. Declared anyway so
	// the usage line and the error name the thing rather than a count.
	arg := transferArg()
	arg.Optional = true
	arg.Default = "use --all to retry every failed transfer"
	takes(cmd, "retry", arg)
	return cmd
}

func renderRetry(w io.Writer, resp *v1.RetryTransferResponse, all bool) error {
	if len(resp.Transfers) == 0 {
		if all {
			fmt.Fprintln(w, "Nothing to retry: no transfer has a failed job.")
		} else {
			fmt.Fprintln(w, "Nothing to retry.")
		}
		return nil
	}

	// The single case reads as a sentence; the fleet case reads as a table.
	// One transfer's outcome does not need a header row, and forty do.
	if len(resp.Transfers) == 1 {
		return renderOneRetry(w, resp.Transfers[0])
	}

	tw := newTabWriter(w)
	fmt.Fprintln(tw, "ID\tREQUEUED\tSTATE\tDETAIL")
	for _, t := range resp.Transfers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			shortID(t.TransferID), requeuedCell(t), dash(t.State), dash(t.Error))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(w, "\nRequeued %d job(s) across %d transfer(s).\n",
		resp.Requeued, countRetried(resp))
	return nil
}

func renderOneRetry(w io.Writer, t v1.TransferRetry) error {
	if t.Error != "" {
		fmt.Fprintf(w, "Transfer %s was not retried: %s\n", shortID(t.TransferID), t.Error)
		return nil
	}
	if t.Requeued == 0 {
		// Not a failure, and worth distinguishing from "it worked": a transfer
		// with nothing failed is either still running or already finished, and
		// the retry changed nothing either way.
		fmt.Fprintf(w, "Transfer %s has no failed jobs; nothing was requeued.\n",
			shortID(t.TransferID))
		return nil
	}

	fmt.Fprintf(w, "Requeued %d failed job(s). Transfer %s is %s.\n",
		t.Requeued, shortID(t.TransferID), t.State)
	return nil
}

// requeuedCell keeps a transfer that could not be retried from reading as one
// that was retried and moved nothing.
func requeuedCell(t v1.TransferRetry) string {
	if t.Error != "" {
		return "-"
	}
	return fmt.Sprint(t.Requeued)
}

func countRetried(resp *v1.RetryTransferResponse) int {
	n := 0
	for _, t := range resp.Transfers {
		if t.Error == "" && t.Requeued > 0 {
			n++
		}
	}
	return n
}
