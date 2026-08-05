package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

func newProductsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "products",
		Aliases: []string{"product"},
		Short:   "Inspect configured products",
		Long: "Products are configured in Git and applied by Flux; they are\n" +
			"read-only over the API. An API that could mutate them would create\n" +
			"a second source of truth that Flux would immediately revert.",
	}
	cmd.AddCommand(newProductsListCommand(), newProductsDescribeCommand())
	return cmd
}

func newProductsListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
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
				fmt.Fprintln(tw, "NAME\tSOURCES\tTARGETS\tAUTO-DOWNLOAD\tVERIFICATION\tOWNER")
				for _, p := range resp.Products {
					fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n",
						p.ProductID,
						len(p.Sources),
						len(p.Targets),
						autoDownloadSummary(p.AutoDownload),
						verificationSummary(p.Verification),
						dash(p.Owner),
					)
				}
				return tw.Flush()
			})
		},
	}
}

func newProductsDescribeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "describe <product>",
		Short: "Show a product's sources, targets and policies",
		Args:  cobra.ExactArgs(1),
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
}

func renderProductDetail(w io.Writer, p *v1.Product) error {
	fmt.Fprintf(w, "Product      %s\n", p.ProductID)
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
	fmt.Fprintln(tw, "  NAME\tREGISTRY\tREPOSITORY\tTYPE\tDISCOVERY\tDOWNLOADS")
	for _, s := range p.Sources {
		discovery := "disabled"
		if s.Discovery != nil && s.Discovery.Enabled {
			discovery = fmt.Sprintf("every %ds", s.Discovery.IntervalSeconds)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%d\n",
			s.Name, s.Registry, s.Repository, s.Type, discovery, s.RateLimits.MaxConcurrentDownloads)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Targets")
	tw = newTabWriter(w)
	fmt.Fprintln(tw, "  NAME\tREGISTRY\tREPOSITORY\tTYPE\tDEFAULT\tPROMOTION-ONLY\tUPLOADS")
	for _, t := range p.Targets {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			t.Name, t.Registry, t.Repository, t.Type,
			yesNo(t.Default), yesNo(t.PromotionOnly), t.RateLimits.MaxConcurrentUploads)
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
		fmt.Fprintln(w, "  disabled — packages from this product are not signature-verified")
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
