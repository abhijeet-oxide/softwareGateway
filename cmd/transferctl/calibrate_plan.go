package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Deciding WHAT to calibrate, before anything expensive happens.
//
// # Why this is not left to the server
//
// A calibration measures one repository, and a product spans forty. The server
// can pick one and does, but the choice is a judgement - which repository is
// representative of what this product actually moves - and a judgement made
// silently inside a five-minute network test is one nobody can check. The first
// version picked the first repository declared, measured `cfx-5000-product/aaa`
// (one tag, nothing over 256 KiB), and reported the whole source as
// unmeasurable.
//
// So the choice is made here, from evidence the Coordinator already has -
// discovered packages and their sizes - SHOWN, and confirmed. The server keeps
// its own fallback for API callers, and an explicit --source-repository skips
// all of it.

// calibrationPlan is everything the run needs, resolved and explicable.
type calibrationPlan struct {
	product string

	source     v1.Repository
	target     v1.Repository
	repository string
	// why explains how the repository was chosen, in a few words. Shown, so a
	// choice made on the reader's behalf is one they can disagree with.
	why string
	// otherRepositories is how many were not chosen.
	otherRepositories int
}

// resolvePlan turns flags plus configuration into a plan, prompting where a
// mandatory choice is missing and a person is there to make it.
func resolvePlan(
	ctx context.Context, client *v1.Client, in io.Reader, out io.Writer,
	product string, o calibrateOptions,
) (calibrationPlan, error) {
	p, err := client.GetProduct(ctx, product)
	if err != nil {
		return calibrationPlan{}, err
	}

	plan := calibrationPlan{product: product}

	plan.source, err = chooseRepository(in, out, "source", enabledOf(p.Sources), o.from,
		func(r v1.Repository) bool { return false })
	if err != nil {
		return calibrationPlan{}, err
	}
	plan.target, err = chooseRepository(in, out, "target", enabledOf(p.Targets), o.to,
		func(r v1.Repository) bool { return r.Default })
	if err != nil {
		return calibrationPlan{}, err
	}

	plan.repository, plan.why, plan.otherRepositories =
		chooseSourceRepository(ctx, client, product, plan.source, o.repository)
	return plan, nil
}

// chooseRepository resolves one end: the named one, the default, the only one,
// or - where a person is watching - the one they pick from a list.
//
// The list matters more than it looks. `--from is required (it has cfx-near,
// cfx-mirror)` is a correct error and a wasted round trip: the command already
// knows the options, the person is sitting there, and asking is one keystroke
// against re-running with a flag.
func chooseRepository(
	in io.Reader, out io.Writer, role string, candidates []v1.Repository,
	named string, isDefault func(v1.Repository) bool,
) (v1.Repository, error) {
	if named != "" {
		for _, c := range candidates {
			if c.Name == named {
				return c, nil
			}
		}
		return v1.Repository{}, usageError{msg: fmt.Sprintf(
			"no enabled %s named %q (this product has %s)",
			role, named, strings.Join(repositoryNames(candidates), ", "))}
	}

	switch len(candidates) {
	case 0:
		return v1.Repository{}, usageError{msg: "this product has no enabled " + role}
	case 1:
		return candidates[0], nil
	}
	for _, c := range candidates {
		if isDefault(c) {
			return c, nil
		}
	}

	if !interactive(in, out) {
		return v1.Repository{}, usageError{msg: fmt.Sprintf(
			"this product has %d %ss and none is the default, so --%s is required (%s)",
			len(candidates), role, flagFor(role), strings.Join(repositoryNames(candidates), ", "))}
	}

	fmt.Fprintf(out, "\nThis product has %d %ss. Which one?\n\n", len(candidates), role)
	for i, c := range candidates {
		fmt.Fprintf(out, "  %d) %-20s %s\n", i+1, c.Name, c.Registry)
	}
	choice, err := askNumber(in, out, "\nChoose 1-"+strconv.Itoa(len(candidates))+": ", len(candidates))
	if err != nil {
		return v1.Repository{}, err
	}
	return candidates[choice-1], nil
}

// chooseSourceRepository picks the repository to measure, and says why.
//
// The best evidence available is what discovery already found: the repository
// holding the largest package is the one whose blobs a transfer will actually
// spend its time on. Falling back through "the only one" to "let the
// Coordinator decide" keeps this working on a product nobody has scanned yet.
func chooseSourceRepository(
	ctx context.Context, client *v1.Client, product string, source v1.Repository, named string,
) (repository, why string, others int) {
	if named != "" {
		return named, "named with --source-repository", 0
	}

	declared := source.Repositories
	if source.Repository != "" {
		declared = append([]string{source.Repository}, declared...)
	}

	if biggest, size, ok := largestDiscovered(ctx, client, product); ok {
		return biggest, "holds the largest discovered package (" + size + ")",
			otherCount(declared, biggest)
	}
	if len(declared) == 1 {
		return declared[0], "the source's only repository", 0
	}
	if len(declared) > 1 {
		// Nothing discovered yet, so there is no size to compare. Say that
		// rather than presenting an arbitrary pick as a considered one.
		return declared[0], "first declared; nothing discovered yet to compare sizes",
			len(declared) - 1
	}
	return "", "chosen by the Coordinator from the registry catalog", 0
}

// largestDiscovered finds the repository with the biggest measured package.
func largestDiscovered(
	ctx context.Context, client *v1.Client, product string,
) (repository, size string, ok bool) {
	resp, err := client.ListPackages(ctx, product, v1.ListPackagesOptions{PageSize: 200})
	if err != nil || resp == nil {
		// Not an error worth failing over: this is a heuristic, and a product
		// nobody has scanned simply has no evidence to offer.
		return "", "", false
	}

	var best int64
	for _, pkg := range resp.Packages {
		if pkg.SourceRepository == "" || pkg.TotalBytes == nil {
			continue
		}
		n := int64Of(*pkg.TotalBytes)
		if n > best {
			best, repository = n, pkg.SourceRepository
		}
	}
	if repository == "" {
		return "", "", false
	}
	return repository, humanBytes(v1.Int64String(strconv.FormatInt(best, 10))), true
}

func otherCount(declared []string, chosen string) int {
	n := 0
	for _, d := range declared {
		if d != chosen {
			n++
		}
	}
	return n
}

// confirm shows the plan and asks. Returns false when the answer is no.
//
// Shown even when it will not ask - a run that measured something other than
// what the reader assumed is worse than a run they had to approve.
func confirm(in io.Reader, out io.Writer, plan calibrationPlan, o calibrateOptions,
	levels []int, estimated string, assumeYes bool,
) (bool, error) {
	fmt.Fprintf(out, "Calibrate %s\n\n", plan.product)
	fmt.Fprintf(out, "  source        %s  %s\n", plan.source.Name, plan.source.Registry)
	fmt.Fprintf(out, "    repository  %s\n", dash(plan.repository))
	if plan.why != "" {
		detail := plan.why
		if plan.otherRepositories > 0 {
			detail += fmt.Sprintf("; %d other(s) not measured", plan.otherRepositories)
		}
		fmt.Fprintf(out, "                %s\n", detail)
	}
	fmt.Fprintf(out, "  target        %s  %s\n", plan.target.Name, plan.target.Registry)
	if plan.target.Repository != "" {
		fmt.Fprintf(out, "    writes to   %s/…\n", plan.target.Repository)
	}
	fmt.Fprintf(out, "  sweep         %s streams, %s each\n",
		joinInts(levels), humanDuration(budgetOr(o.budget)))
	fmt.Fprintf(out, "  write probe   %s\n", writeProbeState(o.noWrite))
	fmt.Fprintf(out, "  estimated     %s\n", estimated)

	if assumeYes || !interactive(in, out) {
		fmt.Fprintln(out)
		return true, nil
	}

	fmt.Fprint(out, "\nThis moves real data against both registries. Continue? [y/N]: ")
	answer, err := readLine(in)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		fmt.Fprintln(out)
		return true, nil
	default:
		return false, nil
	}
}

func writeProbeState(noWrite bool) string {
	if noWrite {
		return "off (--no-write): only the read half is measured"
	}
	return "on: real bytes, in an upload session that is cancelled, never committed"
}

// interactive reports whether there is a person to ask.
//
// Both ends have to be a terminal: a piped stdin has no answer to give, and a
// redirected stdout means nobody is reading the question.
func interactive(in io.Reader, out io.Writer) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return isTerminal(out)
}

func askNumber(in io.Reader, out io.Writer, prompt string, max int) (int, error) {
	for range 3 {
		fmt.Fprint(out, prompt)
		line, err := readLine(in)
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && n >= 1 && n <= max {
			return n, nil
		}
		fmt.Fprintf(out, "  %q is not between 1 and %d.\n", strings.TrimSpace(line), max)
	}
	return 0, usageError{msg: "no valid choice given"}
}

func readLine(in io.Reader) (string, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return line, nil
}

func enabledOf(rs []v1.Repository) []v1.Repository {
	out := make([]v1.Repository, 0, len(rs))
	for _, r := range rs {
		if r.Enabled {
			out = append(out, r)
		}
	}
	return out
}

func repositoryNames(rs []v1.Repository) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func flagFor(role string) string {
	if role == "source" {
		return "from"
	}
	return "to"
}

// budgetOr renders the per-level budget, filling in the default the server
// would apply so the plan states what will actually happen rather than a blank.
func budgetOr(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Second
	}
	return d
}

func joinInts(levels []int) string {
	if len(levels) == 0 {
		levels = []int{1, 2, 4, 8, 16}
	}
	parts := make([]string, 0, len(levels))
	for _, l := range levels {
		parts = append(parts, strconv.Itoa(l))
	}
	return strings.Join(parts, ", ")
}
