package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/baseline"
	celc "github.com/abhijeet-oxide/softwareGateway/internal/compliance/cel"
	chartrender "github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
)

// The compliance commands: does a release follow this organization's own
// Kubernetes and CNF standards.
//
// # Why the CLI is the first client
//
// The same reason it is for everything else here. A feature that only exists
// behind a screen cannot be run in CI, cannot be reproduced by a vendor, and
// cannot be debugged without the whole platform standing up. `transferctl
// compliance ./charts` runs the identical engine against a directory, so a
// vendor can reproduce any finding in the report they were sent.

func newComplianceCommand() *cobra.Command {
	cmd := group(&cobra.Command{
		Use:   "compliance",
		Short: "Check a release against this organization's Kubernetes and CNF standards",
	})
	cmd.AddCommand(newComplianceCheckCommand())
	cmd.AddCommand(newCompliancePoliciesCommand())
	return cmd
}

func newComplianceCheckCommand() *cobra.Command {
	var (
		policyPaths []string
		product     string
		release     string
		registries  []string
		kubeVersion string
		noProbe     bool
		showPasses  bool
		failOn      string
	)

	cmd := &cobra.Command{
		Short: "Check a directory of charts and manifests",
		Long: "Renders every Helm chart under a directory and evaluates the loaded\n" +
			"policy packs against the Kubernetes objects they produce.\n\n" +
			"Every finding names one resource: the chart, the template it came\n" +
			"from, the object, the container and the field. That is what makes a\n" +
			"finding something a vendor can act on rather than a score.\n\n" +
			"Rendering needs the helm binary. Without it, charts cannot be\n" +
			"rendered and the run is reported INCONCLUSIVE - never as a pass. A\n" +
			"release whose charts were never rendered has not been shown to\n" +
			"comply with anything.",
		RunE: func(cc *cobra.Command, args []string) error {
			dir := args[0]
			ctx, cancel := context.WithTimeout(cc.Context(), 15*time.Minute)
			defer cancel()

			cat, err := loadCatalog(policyPaths)
			if err != nil {
				return err
			}

			helm := chartrender.Helm{KubeVersion: kubeVersion}.WithDefaults()
			version, helmErr := helm.Version(ctx)
			if helmErr != nil {
				// Reported, not fatal. Chart-structure work still happens and
				// the run says what it could not decide.
				fmt.Fprintf(os.Stderr, "warning: %v\n", helmErr)
			}

			loader := chartrender.Loader{
				Helm: helm, Probe: !noProbe,
				HelmAvailable: helmErr == nil, HelmVersion: version,
			}
			base := compliance.Address{Product: product, Release: release}
			rel, probe, err := loader.Load(ctx, dir, base)
			if err != nil {
				return err
			}
			rel.Config = map[string]any{"approvedRegistries": toAnyList(registries)}

			started := time.Now().UTC()
			eng := &compliance.Engine{Catalog: cat, Determiner: probe, MaxResults: 200_000}
			run, err := compliance.Execute(ctx, eng, rel, started)
			if err != nil {
				return err
			}

			view := run
			if !showPasses {
				view = withoutPasses(run)
			}
			if err := emit(view, func(w io.Writer) error {
				return renderComplianceRun(w, run, showPasses)
			}); err != nil {
				return err
			}
			return exitFor(run, failOn)
		},
	}

	cmd.Flags().StringSliceVar(&policyPaths, "policies", nil,
		"directories of additional policy packs; the shipped baseline is always loaded")
	cmd.Flags().StringVar(&product, "product", "", "product name to record on every finding")
	cmd.Flags().StringVar(&release, "release", "", "release tag to record on every finding")
	cmd.Flags().StringSliceVar(&registries, "approved-registry", nil,
		"a registry SUP-02 accepts; repeatable. With none, SUP-02 does not apply")
	cmd.Flags().StringVar(&kubeVersion, "kube-version", chartrender.DefaultKubeVersion,
		"Kubernetes version to render against; pinned so two runs agree")
	cmd.Flags().BoolVar(&noProbe, "no-determinacy", false,
		"skip the second render; every finding then reports determinacy 'unknown'")
	cmd.Flags().BoolVar(&showPasses, "show-passes", false,
		"include passing and skipped checks, which is the coverage half of the report")
	cmd.Flags().StringVar(&failOn, "fail-on", "block",
		"exit non-zero on: block|warn|any|never. Inconclusive always exits non-zero")

	takes(cmd, "check", argSpec{
		Name: "directory",
		Help: "a directory of Helm charts and Kubernetes manifests",
	})
	return cmd
}

func newCompliancePoliciesCommand() *cobra.Command {
	var policyPaths []string

	cmd := &cobra.Command{
		Short: "List the loaded checks and what each one asserts",
		Long: "The rulebook, on its own. A vendor who asks what will be checked\n" +
			"before they ship gets this, and a reviewer settling an argument\n" +
			"about a finding reads the rationale here rather than the code.",
		RunE: func(_ *cobra.Command, _ []string) error {
			cat, err := loadCatalog(policyPaths)
			if err != nil {
				return err
			}
			view := policyCatalogue{
				BundleDigest: cat.BundleDigest,
				Packs:        cat.Packs(),
				Checks:       cat.Checks(),
			}
			return emit(view, func(w io.Writer) error {
				return renderPolicies(w, view)
			})
		},
	}
	cmd.Flags().StringSliceVar(&policyPaths, "policies", nil,
		"directories of additional policy packs")
	takes(cmd, "policies")
	return cmd
}

// policyCatalogue is the rulebook as a machine-readable document.
type policyCatalogue struct {
	BundleDigest string                  `json:"bundleDigest"`
	Packs        []compliance.PackStatus `json:"packs"`
	Checks       []compliance.Check      `json:"checks"`
}

// loadCatalog compiles the shipped baseline plus any operator packs.
//
// The baseline is written to a temporary directory rather than parsed
// specially, so it goes through the identical loader an operator's own pack
// does. A shipped check that would fail an operator's loader is a shipped
// check that lies about the contract.
func loadCatalog(extra []string) (*compliance.Catalog, error) {
	comp, err := celc.NewCompiler()
	if err != nil {
		return nil, err
	}
	files, err := baseline.Files()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "sgw-baseline-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	for name, body := range files {
		if err := os.WriteFile(dir+"/"+name, body, 0o600); err != nil {
			return nil, err
		}
	}

	cat, err := (&compliance.Loader{Compiler: comp}).Load(append([]string{dir}, extra...)...)
	if err != nil {
		return nil, err
	}
	for _, p := range cat.Packs() {
		if !p.OK() {
			fmt.Fprintf(os.Stderr, "warning: policy pack %q did not load: %s\n",
				p.Name, strings.Join(p.Errors, "; "))
		}
	}
	return cat, nil
}

// withoutPasses is the default view: what needs attention, with the coverage
// numbers still stated so nobody mistakes a short list for a small denominator.
func withoutPasses(run *compliance.Run) *compliance.Run {
	trimmed := *run
	trimmed.Results = nil
	for _, r := range run.Results {
		if r.Outcome != compliance.OutcomePass && r.Outcome != compliance.OutcomeSkip {
			trimmed.Results = append(trimmed.Results, r)
		}
	}
	return &trimmed
}

// exitFor turns a verdict into an exit code.
//
// Inconclusive always fails, whatever --fail-on says. "Some of it could not be
// checked" is not a result a pipeline should proceed on, and making it
// suppressible would make the suppression the default within a month.
func exitFor(run *compliance.Run, failOn string) error {
	if run.Verdict == compliance.VerdictInconclusive {
		return partialFailureError{fmt.Sprintf(
			"inconclusive: %d check(s) could not be decided", run.Counts.Error)}
	}
	switch strings.ToLower(failOn) {
	case "never":
		return nil
	case "any":
		if run.Counts.Fail > 0 {
			return partialFailureError{fmt.Sprintf("%d finding(s)", run.Counts.Fail)}
		}
	case "warn":
		if run.Counts.Blocking > 0 || run.Counts.Warning > 0 {
			return partialFailureError{fmt.Sprintf("%d blocking, %d warning",
				run.Counts.Blocking, run.Counts.Warning)}
		}
	default: // block
		if run.Counts.Blocking > 0 {
			return partialFailureError{fmt.Sprintf("%d blocking finding(s)", run.Counts.Blocking)}
		}
	}
	return nil
}

func toAnyList(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// emit is render() bound to the global output flag and stdout, which every
// command in this file wants.
func emit(v any, tableFn func(io.Writer) error) error {
	return render(stdout(), opts.output, v, tableFn)
}

// renderComplianceRun prints the run the way somebody reads it: the verdict,
// then the findings grouped by chart, then what was covered.
func renderComplianceRun(w io.Writer, run *compliance.Run, showPasses bool) error {
	fmt.Fprintf(w, "%s\n", strings.ToUpper(run.Verdict.Label()))
	fmt.Fprintf(w, "%d blocking, %d warning, %d informational, %d could not be decided\n",
		run.Counts.Blocking, run.Counts.Warning, run.Counts.Info, run.Counts.Error)
	fmt.Fprintf(w, "%d checks over %d results: %d pass, %d fail, %d not applicable\n",
		run.Checks, run.Counts.Total(), run.Counts.Pass, run.Counts.Fail, run.Counts.Skip)
	if run.BundleDigest != "" {
		fmt.Fprintf(w, "rulebook %s", short(run.BundleDigest))
		if run.HelmVersion != "" {
			fmt.Fprintf(w, " · helm %s · kube %s", run.HelmVersion, run.KubeVersion)
		}
		fmt.Fprintln(w)
	}
	if run.Truncated {
		fmt.Fprintln(w, "\nWARNING: the result list was truncated; this report is incomplete.")
	}

	// Charts that did not render come first: everything below them is a
	// smaller denominator than it looks.
	var broken []compliance.ChartStatus
	for _, c := range run.Charts {
		if c.Status != compliance.RenderOK {
			broken = append(broken, c)
		}
	}
	if len(broken) > 0 {
		fmt.Fprintf(w, "\n%d chart(s) did not render:\n", len(broken))
		for _, c := range broken {
			fmt.Fprintf(w, "  %s %s: %s\n", c.Name, c.Version, firstLine(c.Error))
		}
	}

	byChart := map[string][]compliance.Result{}
	var order []string
	for _, r := range run.Results {
		if !showPasses && (r.Outcome == compliance.OutcomePass || r.Outcome == compliance.OutcomeSkip) {
			continue
		}
		key := r.Address.Chart
		if key == "" {
			key = "(release)"
		}
		if _, seen := byChart[key]; !seen {
			order = append(order, key)
		}
		byChart[key] = append(byChart[key], r)
	}
	sort.Strings(order)

	for _, chart := range order {
		fmt.Fprintf(w, "\n%s\n", chart)
		tw := newTabWriter(w)
		fmt.Fprintln(tw, "  OUTCOME\tSEV\tCHECK\tWHERE\tDETAIL")
		for _, r := range byChart[chart] {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
				strings.ToUpper(string(r.Outcome)), severityAbbrev(r.Severity), r.CheckID,
				whereWithinChart(r.Address), detailOf(r))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(order) == 0 {
		fmt.Fprintln(w, "\nNo findings.")
	}
	return nil
}

// whereWithinChart drops the chart, which is already the heading.
func whereWithinChart(a compliance.Address) string {
	parts := make([]string, 0, 4)
	if a.SourceFile != "" {
		parts = append(parts, a.SourceFile)
	}
	if r := a.Resource(); r != "" {
		parts = append(parts, r)
	}
	if a.Container != "" {
		parts = append(parts, "container "+a.Container)
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " → ")
}

// detailOf is the one thing a reader most needs: what was seen, what was
// wanted, and - for a failure - whether the vendor or the site owns it.
func detailOf(r compliance.Result) string {
	switch r.Outcome {
	case compliance.OutcomeError:
		return firstLine(r.Error)
	case compliance.OutcomeFail:
		s := r.Message
		if r.Determinacy == compliance.DeterminacyConfigurable {
			s += "  [overridable in values]"
		}
		return s
	default:
		return r.Message
	}
}

func renderPolicies(w io.Writer, c policyCatalogue) error {
	fmt.Fprintf(w, "%d check(s) from %d pack(s), rulebook %s\n\n",
		len(c.Checks), len(c.Packs), short(c.BundleDigest))

	tw := newTabWriter(w)
	fmt.Fprintln(tw, "PACK\tPREFIXES\tCHECKS\tSTATUS")
	for _, p := range c.Packs {
		status := "ok"
		if !p.OK() {
			status = "BROKEN: " + firstLine(strings.Join(p.Errors, "; "))
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", p.Name, strings.Join(p.Prefixes, ","), p.Checks, status)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	fmt.Fprintln(w)
	tw = newTabWriter(w)
	fmt.Fprintln(tw, "ID\tSEV\tCATEGORY\tTITLE")
	for _, ch := range c.Checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			ch.ID, severityAbbrev(ch.Severity), ch.Category, ch.Title)
	}
	return tw.Flush()
}

func severityAbbrev(s compliance.Severity) string {
	switch s {
	case compliance.SeverityBlock:
		return "BLOCK"
	case compliance.SeverityWarn:
		return "warn"
	case compliance.SeverityInfo:
		return "info"
	default:
		return "-"
	}
}

func short(digest string) string {
	d := strings.TrimPrefix(digest, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
