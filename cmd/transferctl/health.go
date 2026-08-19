package main

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newHealthCommand() *cobra.Command {
	return &cobra.Command{
		Use: "health",
		// Declared so a stray argument is refused rather than silently
		// ignored, which reads as the command having accepted it.
		Args:  cobra.NoArgs,
		Short: "Check the Coordinator and every configured dependency",
		Long: "Runs the DEEP health check: the database, the worker fleet, every\n" +
			"configured registry, and the notification channels.\n\n" +
			"The worker table is the one to read when a transfer is slower than it\n" +
			"should be. LOAD is jobs in flight over each worker's configured\n" +
			"ceiling: a fleet sitting well under its ceiling is a queue with\n" +
			"nothing leasable in it, not a worker that is stuck.\n\n" +
			"This is deliberately not what Kubernetes polls. A liveness probe\n" +
			"that checked these would restart the Coordinator whenever a vendor\n" +
			"registry had a bad afternoon.\n\n" +
			"Exit codes: 0 healthy, 1 degraded or down, 3 unreachable.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := newClient().HealthCheck(cmd.Context())
			if err != nil {
				return err
			}

			if err := render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderHealthTable(w, resp)
			}); err != nil {
				return err
			}

			// A degraded system must not exit 0, or a monitoring script that
			// runs this will report success while a registry is unreachable.
			if resp.Status != v1.HealthHealthy {
				return partialFailureError{fmt.Sprintf("overall status: %s", resp.Status)}
			}
			return nil
		},
	}
}

func renderHealthTable(w io.Writer, resp *v1.HealthCheckResponse) error {
	role := "follower"
	if resp.Leader {
		role = "leader"
	}

	fmt.Fprintf(w, "Coordinator            %s\n", opts.endpoint)
	fmt.Fprintf(w, "  status               %s\n", resp.Status)
	fmt.Fprintf(w, "  version              %s\n", dash(resp.Version))
	fmt.Fprintf(w, "  role                 %s\n", role)
	fmt.Fprintln(w)

	if len(resp.Checks) == 0 {
		fmt.Fprintln(w, "No dependency checks registered.")
	} else {
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "DEPENDENCY\tSTATUS\tLATENCY\tDETAIL")
		for _, c := range resp.Checks {
			detail := c.Detail
			if c.Error != "" {
				detail = c.Error
			}
			fmt.Fprintf(tw, "%s\t%s\t%.1fms\t%s\n", c.Name, c.Status, c.LatencyMs, dash(detail))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if err := renderWorkers(w, resp.Workers); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Overall: %s\n", resp.Status)
	return nil
}

// renderWorkers is the half of a health check that was missing.
//
// "Are the workers up, what are they running, and how much of their capacity is
// in use" is the first question of any incident and had no route at all: the
// workers table existed and nothing wrote to it. LOAD is the column that
// answers it - a fleet configured for 32 jobs each and sitting at 1/32 is a
// queue with nothing leasable in it, not a worker that is stuck.
func renderWorkers(w io.Writer, workers []v1.Worker) error {
	fmt.Fprintln(w)
	if len(workers) == 0 {
		fmt.Fprintln(w, "Workers                no worker has ever leased from this Coordinator")
		return nil
	}

	tw := newTabWriter(w)
	fmt.Fprintln(tw, "WORKER\tSTATE\tLOAD\tVERSION\tLAST SEEN\tDETAIL")
	for _, k := range workers {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			k.WorkerID, k.State, workerLoad(k), dash(k.Version),
			lastSeen(k.LastHeartbeat), dash(workerDetail(k)))
	}
	return tw.Flush()
}

// workerLoad is jobs in flight over the worker's configured ceiling.
//
// The ceiling is what the operator wrote in worker.maxConcurrentJobs, so a
// mismatch between the file and this column is a worker running configuration
// nobody thinks it has - which is the first thing to check when the fleet is
// slower than it should be.
func workerLoad(k v1.Worker) string {
	if k.MaxConcurrency <= 0 {
		return fmt.Sprintf("%d/?", k.LeasedJobs)
	}
	return fmt.Sprintf("%d/%d", k.LeasedJobs, k.MaxConcurrency)
}

// workerDetail says the one thing a reader could not work out from the row.
func workerDetail(k v1.Worker) string {
	switch {
	case k.State == "STALE":
		return "not heartbeating; its jobs are being returned to the queue"
	case k.LeasedJobs != k.ActiveJobs:
		// The worker's own count against the Coordinator's. They diverge when
		// leases were reaped underneath a worker that is still working on them.
		return fmt.Sprintf("the worker reports %d running against %d leased here",
			k.ActiveJobs, k.LeasedJobs)
	case k.LeasedJobs == 0:
		return "idle"
	default:
		return ""
	}
}

func lastSeen(at string) string {
	t, ok := parseTime(at)
	if !ok {
		return "-"
	}
	return humanDuration(time.Since(t)) + " ago"
}
