package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// progressInterval is how often the live line is refreshed.
//
// One second: fast enough that the display never looks frozen, slow enough that
// polling costs nothing — the endpoint reads in-memory counters.
const progressInterval = time.Second

// watchDiscovery renders a live progress line while a scan runs.
//
// It exists because a synchronous scan against a slow registry is
// indistinguishable from a hung one. `packages discover` blocked for two and a
// half minutes against a registry whose manifest fetches were timing out, printed
// nothing at all in the meantime, and then reported a timeout. Everything the
// operator needed — that it had reached the registry, which repository it was
// on, that it was making progress — existed on the server and was simply never
// asked for.
//
// Everything here goes to STDERR. Stdout carries the result, and `-o json | jq`
// must keep working.
func watchDiscovery(ctx context.Context, c *v1.Client, product string, out io.Writer) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()

	term := isTerminal(out)
	lastWidth := 0

	// A separate, short deadline per poll. A status request that hangs must not
	// hold up the next one, and it must never be the reason the command fails —
	// this is decoration, and decoration that breaks the operation is worse than
	// no decoration.
	poll := func() {
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		st, err := c.DiscoveryStatus(pollCtx, product)
		if err != nil {
			return
		}
		line := progressLine(st)
		if line == "" {
			return
		}

		if !term {
			// Not a terminal: no carriage-return trickery, and far less often,
			// so a CI log gets a heartbeat rather than a thousand lines.
			fmt.Fprintln(out, "  "+line)
			return
		}
		// Pad to erase whatever the previous, possibly longer, line left behind.
		pad := ""
		if n := lastWidth - len(line); n > 0 {
			pad = strings.Repeat(" ", n)
		}
		lastWidth = len(line)
		fmt.Fprintf(out, "\r  %s%s", line, pad)
	}

	for {
		select {
		case <-ctx.Done():
			if term && lastWidth > 0 {
				// Clear the line so the summary is not printed over the top of
				// a half-finished progress display.
				fmt.Fprintf(out, "\r%s\r", strings.Repeat(" ", lastWidth+2))
			}
			return
		case <-ticker.C:
			poll()
		}
	}
}

// progressLine renders one status snapshot as a single line.
//
// Returns "" when there is nothing worth showing, so an idle poll leaves the
// display alone rather than flickering between states.
func progressLine(st *v1.DiscoveryStatusResponse) string {
	if st == nil || !st.Running {
		return ""
	}

	var parts []string
	for _, s := range st.Sources {
		if !s.Scanning {
			continue
		}
		parts = append(parts, describeScanning(s))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "   ")
}

func describeScanning(s v1.DiscoverySourceState) string {
	elapsed := humanMillis(s.ElapsedMs)

	switch s.Phase {
	case "ENUMERATING_REPOSITORIES":
		// The phase where a slow registry hurts most, and the one where a
		// counter is impossible — we do not know the total until it answers. So
		// say what we are waiting for instead of showing a fake bar.
		return fmt.Sprintf("%s: listing the registry's repositories… %s", s.Source, elapsed)

	case "LISTING_TAGS", "RESOLVING_TAGS":
		b := &strings.Builder{}
		fmt.Fprintf(b, "%s: ", s.Source)
		if s.RepositoriesTotal > 0 {
			// "done of total" plus in-flight, never "current of total":
			// repositories are scanned in parallel, so there is no single current
			// one — and without the in-flight count the first minute of a
			// concurrent scan reads as "0 of 48", which looks like nothing
			// happening when sixteen requests are outstanding.
			fmt.Fprintf(b, "%d/%d repos", s.RepositoriesDone, s.RepositoriesTotal)
			if s.RepositoriesInFlight > 0 {
				fmt.Fprintf(b, " (%d in flight)", s.RepositoriesInFlight)
			}
		}
		if s.Phase == "RESOLVING_TAGS" && s.TagsTotal > 0 {
			fmt.Fprintf(b, " · %d/%d tags", s.TagsResolved, s.TagsTotal)
		}
		if s.Artifacts > 0 {
			// The counter that keeps moving while a single large tag is being
			// walked. Without it the display sits on "1/43 tags" for minutes
			// while hundreds of manifest fetches succeed.
			fmt.Fprintf(b, " · %d artifacts", s.Artifacts)
		}
		if s.NewPackages > 0 {
			fmt.Fprintf(b, " · %d new", s.NewPackages)
		}
		if s.Errors > 0 {
			fmt.Fprintf(b, " · %d error(s)", s.Errors)
		}
		fmt.Fprintf(b, " · %s", elapsed)
		return b.String()

	default:
		return fmt.Sprintf("%s: scanning… %s", s.Source, elapsed)
	}
}

// isTerminal reports whether w is an interactive terminal.
//
// Deliberately conservative: anything it cannot positively identify as a
// terminal is treated as a file or a pipe, because a redirected log full of
// carriage returns is worse than one with no live display.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func stderr() io.Writer { return os.Stderr }
