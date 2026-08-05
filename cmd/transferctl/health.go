package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newHealthCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "health",
		Short: "Check the Coordinator and every configured dependency",
		Long: "Runs the DEEP health check: the database, the worker fleet, every\n" +
			"configured registry, and the notification channels.\n\n" +
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
		return nil
	}

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

	fmt.Fprintln(w)
	fmt.Fprintf(w, "Overall: %s\n", resp.Status)
	return nil
}
