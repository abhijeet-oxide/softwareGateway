package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

func newConfigCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "config",
		Short: "Work with product configuration",
	})
	cmd.AddCommand(newConfigValidateCommand())
	return cmd
}

func newConfigValidateCommand() *cobra.Command {
	var secretsDir string

	cmd := &cobra.Command{
		Short: "Validate product configuration offline",
		Long: "Validates every product document in a directory using the SAME\n" +
			"validator the Coordinator runs at load.\n\n" +
			"This is the deliberate compensation for configuring products as\n" +
			"ConfigMaps rather than CRDs: there is no admission webhook, so this\n" +
			"runs in CI and catches the error before the pull request merges\n" +
			"rather than at reconcile time.\n\n" +
			"Secret existence is only checked when --secrets-dir is given, so\n" +
			"this works in CI where cluster Secrets are legitimately absent.",
		RunE: func(_ *cobra.Command, args []string) error {
			dir := args[0]

			var resolver *product.SecretResolver
			if secretsDir != "" {
				resolver = product.NewSecretResolver(secretsDir)
			}

			res, err := product.NewLoader(dir, resolver).Load()
			if err != nil {
				return err
			}

			report := buildValidationReport(dir, res)

			if err := render(stdout(), opts.output, report, func(w io.Writer) error {
				return renderValidationReport(w, report)
			}); err != nil {
				return err
			}

			if len(res.Invalid) > 0 {
				return partialFailureError{
					fmt.Sprintf("%d of %d file(s) invalid", len(res.Invalid), report.FileCount),
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&secretsDir, "secrets-dir", "",
		"also verify that referenced secrets exist under this directory")
	takes(cmd, "validate", argSpec{
		Name: "directory",
		Help: "a directory of product documents, e.g. ./dev/products",
	})
	return cmd
}

// validationReport is the machine-readable form, so CI can consume -o json.
type validationReport struct {
	Directory        string             `json:"directory"`
	FileCount        int                `json:"fileCount"`
	ValidCount       int                `json:"validCount"`
	DeprecationCount int                `json:"deprecationCount"`
	WarningCount     int                `json:"warningCount"`
	Results          []validationResult `json:"results"`
}

type validationResult struct {
	File    string            `json:"file"`
	Product string            `json:"product,omitempty"`
	Valid   bool              `json:"valid"`
	Summary string            `json:"summary,omitempty"`
	Errors  []validationError `json:"errors,omitempty"`

	// Deprecations are superseded keys the document still uses. They do not
	// make it invalid — the values are honoured — so they are reported
	// separately and never affect the exit code. A CI job that started failing
	// because a key was renamed is a CI job people learn to ignore.
	Deprecations []string `json:"deprecations,omitempty"`

	// Warnings are configurations that are valid and probably not intended.
	// Like deprecations they never affect the exit code — a warning that
	// fails CI is a warning somebody turns off.
	Warnings []validationError `json:"warnings,omitempty"`
}

type validationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func buildValidationReport(dir string, res product.LoadResult) validationReport {
	report := validationReport{
		Directory:  dir,
		FileCount:  len(res.Valid) + len(res.Invalid),
		ValidCount: len(res.Valid),
	}

	for _, p := range res.Valid {
		var warnings []validationError
		for _, w := range p.Warnings {
			warnings = append(warnings, validationError{Field: w.Field, Message: w.Message, Hint: w.Hint})
		}
		report.Results = append(report.Results, validationResult{
			File:    filepath.Base(p.SourceFile),
			Product: p.Metadata.Name,
			Valid:   true,
			Summary: fmt.Sprintf("%d source(s), %d target(s), %d rule(s)",
				len(p.Spec.Sources), len(p.Spec.Targets), len(p.Spec.AutoDownload.Rules)),
			Deprecations: p.Deprecations,
			Warnings:     warnings,
		})
		report.DeprecationCount += len(p.Deprecations)
		report.WarningCount += len(warnings)
	}

	for _, bad := range res.Invalid {
		r := validationResult{
			File:    filepath.Base(bad.File),
			Product: bad.Name,
			Valid:   false,
		}
		if errs, ok := product.AsErrors(bad.Err); ok {
			for _, e := range errs {
				r.Errors = append(r.Errors, validationError{
					Field: e.Field, Message: e.Message, Hint: e.Hint,
				})
			}
		} else {
			// A parse failure has no field path.
			r.Errors = append(r.Errors, validationError{Field: "", Message: bad.Err.Error()})
		}
		report.Results = append(report.Results, r)
	}

	// Stable order regardless of directory-walk order, so CI diffs are clean.
	sort.Slice(report.Results, func(i, j int) bool {
		return report.Results[i].File < report.Results[j].File
	})
	return report
}

func renderValidationReport(w io.Writer, report validationReport) error {
	if report.FileCount == 0 {
		fmt.Fprintf(w, "No product documents found in %s\n", report.Directory)
		return nil
	}

	fmt.Fprintln(w)
	for _, r := range report.Results {
		if r.Valid {
			fmt.Fprintf(w, "  %-28s OK     %s\n", r.File, r.Summary)
			for _, d := range r.Deprecations {
				fmt.Fprintf(w, "  %-28s        deprecated: %s\n", "", d)
			}
			for _, warn := range r.Warnings {
				fmt.Fprintf(w, "\n    WARNING  %s: %s\n", warn.Field, warn.Message)
				if warn.Hint != "" {
					fmt.Fprintf(w, "      %s\n", warn.Hint)
				}
			}
			if len(r.Warnings) > 0 {
				fmt.Fprintln(w)
			}
			continue
		}

		fmt.Fprintf(w, "  %-28s ERROR\n", r.File)
		for _, e := range r.Errors {
			if e.Field == "" {
				fmt.Fprintf(w, "\n    %s\n", e.Message)
				continue
			}
			fmt.Fprintf(w, "\n    %s: %s\n", e.Field, e.Message)
			if e.Hint != "" {
				fmt.Fprintf(w, "      %s\n", e.Hint)
			}
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "\n%d file(s), %d valid, %d error(s)",
		report.FileCount, report.ValidCount, report.FileCount-report.ValidCount)
	if report.WarningCount > 0 {
		fmt.Fprintf(w, ", %d warning(s)", report.WarningCount)
	}
	fmt.Fprintln(w)

	if report.DeprecationCount > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d deprecated key(s) in use. They still work — the values are honoured —\n",
			report.DeprecationCount)
		fmt.Fprintln(w, "but they are folded into `concurrency`, which replaced them:")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  concurrency:")
		fmt.Fprintln(w, "    perRegistry: 32        # requests in flight, and the connection pool")
		fmt.Fprintln(w, "    requestsPerSecond: 0   # optional; 0 means no artificial limit")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Usually the right move is to DELETE the old block and inherit the")
		fmt.Fprintln(w, "application-level default rather than restate it per source.")
	}
	return nil
}
