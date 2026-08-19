package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// `transferctl calibrate` - measure the path, then say what to configure.

type calibrateOptions struct {
	from        string
	to          string
	repository  string
	concurrency string
	budget      time.Duration
	noWrite     bool
	bundleSize  string
	assumeYes   bool
}

func newCalibrateCommand() *cobra.Command {
	var o calibrateOptions

	cmd := &cobra.Command{
		Short: "Measure a source-to-target path and suggest settings for it",
		Long: "Answers the question the configuration file cannot: which setting is\n" +
			"currently limiting throughput on this path, and what should it be.\n\n" +
			"It reads real blobs from the source and streams real bytes to the\n" +
			"target at one, two, four, eight and sixteen concurrent streams, and\n" +
			"reports where the curve stops rising. Where a proxy is in force it\n" +
			"also measures the direct route, so \"bypass the proxy\" is a\n" +
			"measurement rather than folklore.\n\n" +
			"NOTHING IS LEFT BEHIND. The write probe opens an upload session,\n" +
			"pushes bytes into it and then cancels it, so no blob is committed and\n" +
			"no tag, manifest or blob appears in the target. Use --no-write to skip\n" +
			"it anyway, at the cost of only measuring the read half.\n\n" +
			"A product spans many repositories and only one is measured, so the\n" +
			"command shows which - chosen from the largest package discovery has\n" +
			"found - and asks before it starts. --source-repository names one\n" +
			"instead; -y skips the question.\n\n" +
			"This is real load on both registries for a few minutes. It is not\n" +
			"something to run on a schedule, and it is not `products check`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			levels, err := parseLevels(o.concurrency)
			if err != nil {
				return err
			}
			bundle, err := parseByteSize(o.bundleSize)
			if err != nil {
				return err
			}

			client := newClient()

			// Resolved and CONFIRMED before anything expensive runs. A
			// calibration measures one repository out of many and loads two
			// registries for minutes; both of those are worth a person's
			// glance first.
			plan, err := resolvePlan(cmd.Context(), client, os.Stdin, os.Stderr, args[0], o)
			if err != nil {
				return err
			}

			estimated := humanDuration(estimatedRuntime(levels, o.budget, !o.noWrite))
			ok, err := confirm(os.Stdin, os.Stderr, plan, o, levels, estimated, o.assumeYes)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "Cancelled; nothing was measured.")
				return nil
			}

			req := v1.CalibrateRequest{
				// Sent explicitly, even where the server would have resolved
				// the same thing: what was confirmed is what must be measured,
				// and a default re-evaluated at the far end could differ.
				Source:           plan.source.Name,
				Target:           plan.target.Name,
				SourceRepository: plan.repository,
				Levels:           levels,
				Write:            boolPtr(!o.noWrite),
			}
			if o.budget > 0 {
				req.BudgetSeconds = o.budget.Seconds()
			}
			if bundle > 0 {
				req.BundleBytes = v1.Int64String(strconv.FormatInt(bundle, 10))
			}

			// Said before the wait rather than after it. This command is
			// silent for minutes by design, and a terminal that has printed
			// nothing for four minutes is indistinguishable from one that has
			// hung.
			fmt.Fprintf(os.Stderr, "Measuring - about %s, longer if the link is slow.\n\n",
				estimated)

			resp, err := client.Calibrate(cmd.Context(), args[0], req)
			if err != nil {
				return err
			}
			return render(stdout(), opts.output, resp, func(w io.Writer) error {
				return renderCalibration(w, resp)
			})
		},
	}

	cmd.Flags().StringVar(&o.from, "from", "",
		"configured source to read from (default: the product's only source)")
	cmd.Flags().StringVar(&o.to, "to", "",
		"configured target to write to (default: the product's default target)")
	cmd.Flags().StringVar(&o.repository, "source-repository", "",
		"repository to read from, when the source enumerates several")
	cmd.Flags().StringVar(&o.concurrency, "concurrency", "",
		"concurrency levels to sweep, comma separated (default 1,2,4,8,16)")
	cmd.Flags().DurationVar(&o.budget, "budget", 0,
		"how long each level runs (default 5s)")
	cmd.Flags().BoolVar(&o.noWrite, "no-write", false,
		"skip the target probe, measuring only the read half")
	cmd.Flags().StringVar(&o.bundleSize, "bundle-size", "",
		"project the measured ceiling onto a transfer of this size, e.g. 30GiB")
	cmd.Flags().BoolVarP(&o.assumeYes, "yes", "y", false,
		"do not ask for confirmation before measuring")

	// The default 30 seconds is not close: the sweep alone is the budget times
	// the number of levels, per side.
	calibrationTimeout(cmd, &o)
	takes(cmd, "calibrate", productArg())
	return cmd
}

// calibrationTimeout raises the client timeout to cover the run it asked for.
//
// Derived from the flags rather than fixed, because --budget 30s --concurrency
// 1,2,4,8,16,32 is a legitimate request that would blow through any constant.
// An explicit --timeout still wins: a flag the tool silently overrides is not a
// flag.
func calibrationTimeout(cmd *cobra.Command, o *calibrateOptions) {
	cmd.PreRun = func(c *cobra.Command, _ []string) {
		if c.Flags().Changed("timeout") || strings.TrimSpace(os.Getenv("SWGW_TIMEOUT")) != "" {
			return
		}
		levels, err := parseLevels(o.concurrency)
		if err != nil {
			levels = nil
		}
		// Double the estimate and add a minute: the estimate counts probing
		// time, and a slow registry adds connection setup, sample discovery and
		// the cleanup of every cancelled upload session on top of it.
		opts.timeout = 2*estimatedRuntime(levels, o.budget, !o.noWrite) + time.Minute
	}
}

// estimatedRuntime is what to warn the operator about, and what to wait for.
//
// budget × (levels + 2) per side: the sweep, plus the two head-to-head samples
// the proxy comparison takes when a proxy is in force.
func estimatedRuntime(levels []int, budget time.Duration, write bool) time.Duration {
	if budget <= 0 {
		budget = 5 * time.Second
	}
	n := len(levels)
	if n == 0 {
		n = 5
	}
	sides := 1
	if write {
		sides = 2
	}
	return time.Duration(sides*(n+2)) * budget
}

// parseLevels reads "1,2,4,8" into a sweep.
func parseLevels(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}

	var out []int
	for _, field := range strings.Split(s, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		n, err := strconv.Atoi(field)
		if err != nil || n <= 0 {
			return nil, usageError{msg: fmt.Sprintf(
				"--concurrency %q: %q is not a positive number of streams", s, field)}
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, usageError{msg: "--concurrency: no levels given"}
	}
	return out, nil
}

// parseByteSize reads "30GiB", "500MB", "1024" into bytes.
//
// Both conventions accepted, and they mean different things: GiB is 1024³ and
// GB is 1000³. Registries and operators use both, and silently treating one as
// the other would put a 7% error into a projection presented to three
// significant figures.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}

	units := []struct {
		suffix string
		scale  int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30}, {"TIB", 1 << 40},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"TB", 1000 * 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30}, {"T", 1 << 40},
		{"B", 1},
	}

	upper := strings.ToUpper(s)
	for _, u := range units {
		if !strings.HasSuffix(upper, u.suffix) {
			continue
		}
		value := strings.TrimSpace(upper[:len(upper)-len(u.suffix)])
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n < 0 {
			return 0, usageError{msg: fmt.Sprintf("--bundle-size %q: %q is not a number", s, value)}
		}
		return int64(n * float64(u.scale)), nil
	}

	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || n < 0 {
		return 0, usageError{msg: fmt.Sprintf(
			"--bundle-size %q: expected a size such as 30GiB, 500MB or a number of bytes", s)}
	}
	return n, nil
}

func boolPtr(b bool) *bool { return &b }
