package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newProductsCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:     "products",
		Aliases: []string{"product"},
		Short:   "Inspect configured products",
		Long: "Products are configured in Git and applied by Flux; they are\n" +
			"read-only over the API. An API that could mutate them would create\n" +
			"a second source of truth that Flux would immediately revert.",
	})
	cmd.AddCommand(
		newProductsListCommand(),
		newProductsDescribeCommand(),
		newProductsCheckCommand(),
	)
	return cmd
}

func newProductsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Args:  cobra.NoArgs,
		Short: "List configured products",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := newClient().ListProducts(cmd.Context())
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				if len(resp.Products) == 0 {
					fmt.Fprintln(w, "No products configured.")
					return nil
				}
				tw := newTabWriter(w)
				fmt.Fprintln(tw, "NAME\tSTATE\tSOURCES\tREPOSITORIES\tTARGETS\tDISCOVERY\tAUTO-DOWNLOAD\tVERIFICATION\tOWNER")
				disabled := 0
				for _, p := range resp.Products {
					if !p.Enabled {
						disabled++
					}
					fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\t%s\t%s\t%s\t%s\n",
						p.ProductID,
						enabledState(p.Enabled),
						len(p.Sources),
						sourceRepositorySummary(p.Sources),
						len(p.Targets),
						discoverySummary(p.Sources),
						autoDownloadSummary(p.AutoDownload),
						verificationSummary(p.Verification),
						dash(p.Owner),
					)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
				if disabled > 0 {
					fmt.Fprintln(w)
					fmt.Fprintf(w, "%d product(s) disabled: still configured and validated, but not running.\n", disabled)
					fmt.Fprintln(w, "Re-enable with `metadata.enabled: true`; their discovered packages are kept.")
				}
				fmt.Fprintln(w)
				fmt.Fprintln(w, "transferctl products describe <name>   full configuration")
				fmt.Fprintln(w, "transferctl products check <name>      test registry connectivity")
				return nil
			})
		},
	}
}

func newProductsDescribeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Show a product's sources, targets and policies",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := newClient().GetProduct(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, p, func(w io.Writer) error {
				return renderProductDetail(w, p)
			})
		},
	}

	takes(cmd, "describe", productArg())
	return cmd
}

// enabledState renders the on/off state as an alignable token.
func enabledState(enabled bool) string {
	if enabled {
		return "active"
	}
	return "DISABLED"
}

// repositoryList renders a source's repositories for a detail view.
//
// The full list rather than a count: `describe` is where an operator confirms
// exactly what is being scanned, and a number does not answer that.
func repositoryList(s v1.Repository) string {
	switch {
	case s.RepositoryDiscovery && len(s.Repositories) == 0:
		return "(from catalog)"
	case s.RepositoryDiscovery:
		return strings.Join(s.Repositories, ", ") + ", + catalog"
	case len(s.Repositories) == 0:
		return dash("")
	default:
		return strings.Join(s.Repositories, ", ")
	}
}

// sourceRepositorySummary counts the repositories a product's sources cover.
//
// Worth a column now that one source can span several: "3 sources" no longer
// tells you how much is being scanned.
func sourceRepositorySummary(sources []v1.Repository) string {
	total, discovering := 0, 0
	for _, s := range sources {
		total += len(s.Repositories)
		if s.RepositoryDiscovery {
			discovering++
		}
	}
	switch {
	case discovering > 0 && total > 0:
		return fmt.Sprintf("%d + catalog", total)
	case discovering > 0:
		return "from catalog"
	default:
		return fmt.Sprintf("%d", total)
	}
}

func discoverySummary(sources []v1.Repository) string {
	enabled := 0
	for _, s := range sources {
		if s.Discovery != nil && s.Discovery.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		return "off"
	}
	return fmt.Sprintf("%d of %d", enabled, len(sources))
}

func newProductsCheckCommand() *cobra.Command {
	cmd := &cobra.Command{
		Short: "Test that configured registries are reachable and credentials work",
		Long: "Probes every repository a product declares: DNS and TLS, whether the\n" +
			"credential is accepted, and whether it grants the access the product\n" +
			"needs.\n\n" +
			"This is NOT part of `transferctl health`, deliberately. Health answers\n" +
			"\"is the service working?\" and the same machinery backs Kubernetes\n" +
			"readiness - if a vendor's outage made it fail, a vendor's bad afternoon\n" +
			"would pull our own pods out of service, and you could not tell whose\n" +
			"fault an unhealthy reading was.\n\n" +
			"Run this after editing configuration, when onboarding a vendor, or when\n" +
			"discovery is finding nothing and you want to know why.\n\n" +
			"With no argument, every configured product is checked.",
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			resp, err := newClient().CheckConnectivity(cmd.Context(), name)
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderConnectivity(w, resp)
			})
		},
	}

	// Every repository of every product, each a TLS handshake and at least two
	// round trips to a third party. The default 30 seconds is not enough and
	// never was.
	contactsRegistries(cmd)
	takes(cmd, "check", optionalProductArg("checks every configured product"))
	return cmd
}

func renderConnectivity(w io.Writer, resp *v1.CheckConnectivityResponse) error {
	if len(resp.Products) == 0 {
		fmt.Fprintln(w, "No products configured.")
		return nil
	}

	failures := 0
	for i, p := range resp.Products {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "%s  %s\n", checkMark(p.Status), p.Product)
		if p.Status == v1.CheckSkipped && len(p.Repositories) == 0 {
			fmt.Fprintln(w, "      disabled - nothing probed")
			continue
		}

		for _, r := range p.Repositories {
			ref := r.Registry
			if r.Repository != "" {
				ref += "/" + r.Repository
			}
			fmt.Fprintf(w, "\n  %s  %s (%s)\n", checkMark(r.Status), ref, r.Role)
			if r.Name != "" {
				fmt.Fprintf(w, "      declared as %q\n", r.Name)
			}

			for _, s := range r.Steps {
				line := fmt.Sprintf("      %-8s %s", string(s.Status), s.Name)
				if s.LatencyMs > 0 {
					line += fmt.Sprintf("  (%.0fms)", s.LatencyMs)
				}
				fmt.Fprintln(w, line)
				if s.Detail != "" {
					fmt.Fprintf(w, "               %s\n", s.Detail)
				}
				// The hint is the part that turns a diagnosis into a fix, so it
				// is printed even when the run is otherwise terse.
				if s.Hint != "" {
					fmt.Fprintf(w, "               → %s\n", s.Hint)
				}
			}
			if r.Status == v1.CheckFailed {
				failures++
			}
		}
	}

	fmt.Fprintln(w)
	switch resp.Status {
	case v1.CheckOK:
		fmt.Fprintln(w, "All configured repositories are reachable and authorised.")
	case v1.CheckWarning:
		fmt.Fprintln(w, "Reachable, with warnings above.")
	default:
		fmt.Fprintf(w, "%d repository/repositories cannot be used as configured.\n", failures)
		return partialFailureError{msg: fmt.Sprintf("%d repository check(s) failed", failures)}
	}
	return nil
}

// checkMark renders a status as a short, alignable token.
//
// Text rather than colour or emoji: this output is read in terminals that
// mangle both, and piped into logs where neither survives.
func checkMark(s v1.CheckStatus) string {
	switch s {
	case v1.CheckOK:
		return "[ ok ]"
	case v1.CheckWarning:
		return "[warn]"
	case v1.CheckSkipped:
		return "[skip]"
	default:
		return "[FAIL]"
	}
}

func renderProductDetail(w io.Writer, p *v1.Product) error {
	fmt.Fprintf(w, "Product      %s\n", p.ProductID)
	if !p.Enabled {
		fmt.Fprintln(w, "State        DISABLED - configured and validated, but not running")
	}
	if p.DisplayName != "" {
		fmt.Fprintf(w, "Display      %s\n", p.DisplayName)
	}
	if p.Description != "" {
		fmt.Fprintf(w, "Description  %s\n", p.Description)
	}
	fmt.Fprintf(w, "Owner        %s\n", dash(p.Owner))
	// The config hash lets an operator confirm which revision the Coordinator
	// actually loaded, rather than which revision Git believes is current.
	fmt.Fprintf(w, "Config       %s\n", shortHash(p.ConfigHash))
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Sources")
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  NAME\tSTATE\tREGISTRY\tREPOSITORIES\tTYPE\tDISCOVERY\tCONCURRENCY")
	for _, s := range p.Sources {
		discovery := "off"
		if s.Discovery != nil && s.Discovery.Enabled {
			discovery = fmt.Sprintf("every %ds", s.Discovery.IntervalSeconds)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Name, enabledState(s.Enabled), s.Registry, repositoryList(s), s.Type,
			discovery, humanConcurrency(s.Concurrency))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Targets")
	tw = newTabWriter(w)
	fmt.Fprintln(tw, "  NAME\tSTATE\tREGISTRY\tREPOSITORY\tTYPE\tDEFAULT\tPROMOTION-ONLY\tCONCURRENCY")
	for _, t := range p.Targets {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, enabledState(t.Enabled), t.Registry, t.Repository, t.Type,
			yesNo(t.Default), yesNo(t.PromotionOnly), humanConcurrency(t.Concurrency))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if p.AutoDownload.Enabled && len(p.AutoDownload.Rules) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Auto-download rules (first match wins)")
		tw = newTabWriter(w)
		fmt.Fprintln(tw, "  NAME\tTAG PATTERN\tTARGETS\tPRIORITY")
		for _, r := range p.AutoDownload.Rules {
			targets := "default"
			if len(r.Targets) > 0 {
				targets = fmt.Sprint(r.Targets)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%d\n", r.Name, r.TagPattern, targets, r.Priority)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Verification")
	if !p.Verification.Enabled {
		// Worth stating plainly: an unverified product is a gap in the supply
		// chain, not a neutral default.
		fmt.Fprintln(w, "  disabled - packages from this product are not signature-verified")
		return nil
	}
	fmt.Fprintf(w, "  policy       %s\n", p.Verification.Policy)
	fmt.Fprintf(w, "  mode         %s\n", dash(p.Verification.Mode))
	fmt.Fprintf(w, "  at source    %s\n", yesNo(p.Verification.AtSource))
	fmt.Fprintf(w, "  at dest      %s\n", yesNo(p.Verification.AtDestination))
	return nil
}

func autoDownloadSummary(a v1.AutoDownloadSummary) string {
	if !a.Enabled {
		return "off"
	}
	return fmt.Sprintf("%d rule(s)", len(a.Rules))
}

func verificationSummary(v v1.VerificationSummary) string {
	if !v.Enabled {
		return "off"
	}
	if v.Policy != "" {
		return v.Policy
	}
	return "on"
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return dash(h)
}
