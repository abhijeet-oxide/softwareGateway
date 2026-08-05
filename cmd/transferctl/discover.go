package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// newDiscoverCommand is the top-level verb.
//
// Discovery was under `packages` — `transferctl packages discover` — and that
// was the wrong shape. Discovery is not an operation on packages; it is what
// PRODUCES them. Burying the primary verb of the system two levels down, next
// to the commands that read its output, is how a CLI ends up needing a
// cheat sheet.
//
// It also could not express the most common operator action: scan everything.
// A shell loop over `products list` puts the definition of "everything" in the
// caller, where it can disagree with which products are actually enabled.
func newDiscoverCommand() *cobra.Command {
	var (
		source string
		wait   bool
	)

	cmd := &cobra.Command{
		Use:   "discover [product]",
		Short: "Scan sources for newly published packages",
		Long: "Scans now, rather than waiting for the interval.\n\n" +
			"With no argument, scans EVERY product being polled — the usual thing\n" +
			"to do after a maintenance window, or when you want to know what a\n" +
			"vendor has shipped since you last looked. A fleet-wide scan never\n" +
			"blocks: follow it with `transferctl discover status`.\n\n" +
			"With a product name, scans that product and waits for the result.\n\n" +
			"Safe to repeat. A re-scan of unchanged content discovers nothing, and\n" +
			"a request arriving while a scan runs joins it rather than starting a\n" +
			"second one.",
		Args:    cobra.MaximumNArgs(1),
		Aliases: []string{"scan"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClient()

			if len(args) == 0 {
				if source != "" {
					return fmt.Errorf("--source names a source of ONE product, so it needs " +
						"a product argument: transferctl discover <product> --source <name>")
				}
				resp, err := client.DiscoverAll(cmd.Context())
				if err != nil {
					return err
				}
				return render(stdout(), opts.output, resp, func(w io.Writer) error {
					return renderDiscoverAll(w, resp)
				})
			}

			product := args[0]
			if !wait {
				resp, err := client.StartDiscovery(cmd.Context(), product, source)
				if err != nil {
					return err
				}
				return render(stdout(), opts.output, resp, func(w io.Writer) error {
					return renderDiscoverStarted(w, product, resp)
				})
			}

			// The progress watcher runs alongside the blocking request on its own
			// connection, so a slow scan shows what it is doing rather than a
			// blank terminal.
			watchCtx, stopWatch := context.WithCancel(cmd.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				watchDiscovery(watchCtx, client, product, stderr())
			}()

			resp, err := client.DiscoverPackages(cmd.Context(), product, source)

			stopWatch()
			<-done

			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderDiscoverResult(w, product, resp)
			})
		},
	}

	cmd.Flags().StringVar(&source, "source", "",
		"scan only this source of the product (default: every source)")
	cmd.Flags().BoolVar(&wait, "wait", true,
		"wait for the scan to finish; --wait=false starts it and returns immediately")

	cmd.AddCommand(newDiscoverStatusCommand())

	// It reaches vendor registries through the Coordinator, so it belongs with
	// the slow commands.
	contactsRegistries(cmd)
	return cmd
}

func renderDiscoverAll(w io.Writer, r *v1.DiscoverAllResponse) error {
	if len(r.Products) == 0 {
		fmt.Fprintln(w, "No products are being polled.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Discovery runs on the leader, and only for products with at least one")
		fmt.Fprintln(w, "enabled source. `transferctl products list` shows which.")
		return nil
	}

	switch {
	case r.Started > 0 && r.AlreadyRunning > 0:
		fmt.Fprintf(w, "Started %d scan(s) across %d product(s); %d source(s) were already scanning.\n",
			r.Started, len(r.Products), r.AlreadyRunning)
	case r.Started > 0:
		fmt.Fprintf(w, "Started %d scan(s) across %d product(s).\n", r.Started, len(r.Products))
	default:
		fmt.Fprintf(w, "Every source of all %d product(s) was already scanning; nothing new was started.\n",
			len(r.Products))
	}
	fmt.Fprintln(w)

	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  PRODUCT\tSTARTED\tALREADY RUNNING\t")
	failures := 0
	for _, p := range r.Products {
		note := ""
		if p.Error != "" {
			note = p.Error
			failures++
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%s\n", p.Product, p.Sources, p.AlreadyRunning, note)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "  transferctl discover status        # follow them")

	if failures > 0 {
		// Non-zero so a script notices, while still reporting the products that
		// did start. One broken product must not look like a working fleet.
		return partialFailureError{
			msg: fmt.Sprintf("%d product(s) could not be scanned", failures)}
	}
	return nil
}
