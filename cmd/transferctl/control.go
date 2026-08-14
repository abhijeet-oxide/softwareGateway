package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Stopping and starting a transfer that is already under way.
//
// The three verbs differ in what they do to the work ALREADY DONE, not in what
// they do to the work remaining — and that is the thing an operator needs to be
// sure of before typing any of them:
//
//	pause    nothing new starts; everything done stays done
//	resume   the inverse
//	stop     give up on the rest; what already landed stays at the destination
//
// None of them deletes anything. Half a bundle at the destination is
// unreferenced blobs and untagged manifests: invisible to consumers, because a
// tag is applied only once everything beneath it is present, and free to the
// next transfer, which will find them and skip them.

func newTransfersPauseCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Stop handing out jobs, keeping everything already done",
		Long: "Nothing new starts. Jobs already in flight FINISH — abandoning a\n" +
			"nine-gigabyte blob nine tenths of the way through to honour a pause a\n" +
			"fraction of a second sooner is a bad trade.\n\n" +
			"Resume picks up exactly where this left off.",
		RunE: controlRunner("pause"),
	}
	takes(cmd, "pause", transferArg())
	return cmd
}

func newTransfersResumeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Make a paused transfer's jobs leasable again",
		Long: "The transfer returns to `ready` rather than to `running`: nothing is\n" +
			"in flight at that moment, and a worker leasing the first job is what\n" +
			"makes it running — through the same path every other transfer takes.",
		RunE: controlRunner("resume"),
	}
	takes(cmd, "resume", transferArg())
	return cmd
}

func newTransfersStopCommand() *cobra.Command {
	cmd := &cobra.Command{
		Aliases: []string{"cancel"},
		Short:   "Give up on the rest of a transfer",
		Long: "Everything not yet started is cancelled. Everything already in flight\n" +
			"stops at its worker's next checkpoint, which is why this reports\n" +
			"`cancelling` rather than `cancelled` while anything is running.\n\n" +
			"What already reached the destination STAYS there. It is untagged and\n" +
			"unreferenced, so nobody can pull it by accident, and a later transfer\n" +
			"of the same content will find it and skip it.\n\n" +
			"Not reversible: `resume` acts on a pause. Create the transfer again.",
		RunE: controlRunner("stop"),
	}
	takes(cmd, "stop", transferArg())
	return cmd
}

// controlRunner is the body all three share.
func controlRunner(verb string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		resp, err := newClient().ControlTransfer(cmd.Context(), args[0], verb)
		if err != nil {
			return err
		}
		return render(stdout(), opts.output, resp, func(w io.Writer) error {
			return renderControl(w, verb, resp)
		})
	}
}

// renderControl says what happened, in one line where one line is the truth.
//
// The second line appears only when it is load-bearing: a `stop` that leaves
// jobs in flight has NOT finished stopping, and a reader who is not told that
// will watch a `cancelling` transfer and wonder whether it is stuck.
func renderControl(w io.Writer, verb string, r *v1.TransferControlResponse) error {
	switch verb {
	case "pause":
		fmt.Fprintf(w, "Paused %s — %s will not be handed out.\n",
			shortID(r.TransferID), plural(r.Jobs, "job", "jobs"))
	case "resume":
		fmt.Fprintf(w, "Resumed %s — %s are leasable again.\n",
			shortID(r.TransferID), plural(r.Jobs, "job", "jobs"))
	default:
		fmt.Fprintf(w, "Stopped %s — %s cancelled.\n",
			shortID(r.TransferID), plural(r.Jobs, "job", "jobs"))
	}

	if r.InFlight > 0 {
		fmt.Fprintf(w, "  %s still in flight; %s\n",
			plural(r.InFlight, "job", "jobs"), inFlightNote(verb))
	}
	return nil
}

func inFlightNote(verb string) string {
	if verb == "stop" {
		return "the transfer is cancelling until they report."
	}
	return "they will finish."
}
