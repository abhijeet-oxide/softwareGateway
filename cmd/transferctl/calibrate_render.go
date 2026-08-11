package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Rendering a calibration.
//
// Two halves, and the second is the point. The tables are the evidence and the
// suggestions are the answer, so the suggestions come last — the position a
// reader's eye ends on — and each carries the measurement behind it, so the
// answer can be checked rather than believed.

func renderCalibration(w io.Writer, resp *v1.CalibrateResponse) error {
	fmt.Fprintf(w, "Calibration  %s\n", resp.Product)
	fmt.Fprintf(w, "  measured from      %s\n", resp.MeasuredFrom)
	fmt.Fprintf(w, "  took               %s\n",
		humanDuration(time.Duration(resp.DurationSec*float64(time.Second))))

	renderCalibrationSide(w, "SOURCE", resp.Source)
	renderCalibrationSide(w, "TARGET", resp.Target)

	if err := renderSuggestions(w, resp.Suggestions); err != nil {
		return err
	}

	for _, n := range resp.Notes {
		fmt.Fprintf(w, "\nNote: %s\n", n)
	}
	return nil
}

func renderCalibrationSide(w io.Writer, heading string, s v1.CalibrationSide) {
	ref := s.Registry
	if s.Repository != "" {
		ref += "/" + s.Repository
	}

	fmt.Fprintf(w, "\n%s  %s  %s\n", heading, dash(s.Name), ref)
	fmt.Fprintf(w, "  route              %s\n", s.Route.Configured)
	if s.Route.DirectTested {
		fmt.Fprintf(w, "  direct route       %s\n", describeDirect(s.Route))
	}
	if s.RTTMs > 0 {
		fmt.Fprintf(w, "  round trip         %.0fms\n", s.RTTMs)
	}

	if s.Skipped != "" {
		fmt.Fprintf(w, "  not measured       %s\n", s.Skipped)
		return
	}
	if len(s.Levels) == 0 {
		return
	}

	fmt.Fprintln(w)
	tw := newTabWriter(w)
	fmt.Fprintln(tw, "  STREAMS\tRATE\tPER STREAM\tMOVED\tREQUESTS\tERRORS\tTTFB")
	for _, l := range s.Levels {
		marker := "  "
		if l.Concurrency == s.Knee {
			// The level worth configuring, marked in the table rather than only
			// named in the prose underneath it.
			marker = "> "
		}
		fmt.Fprintf(tw, "%s%d\t%s\t%s\t%s\t%d\t%s\t%.0fms\n",
			marker, l.Concurrency,
			rateString(l.Rate), rateString(l.PerStream), humanBytes(l.Bytes),
			l.Requests, errorCell(l), l.TTFBMs)
	}
	if err := tw.Flush(); err != nil {
		return
	}

	if s.StillClimbing {
		fmt.Fprintf(w, "  still rising at %d streams — the ceiling is above this sweep\n",
			s.Levels[len(s.Levels)-1].Concurrency)
	}
	for _, l := range s.Levels {
		if l.FirstError != "" {
			fmt.Fprintf(w, "  first error at %d streams: %s\n", l.Concurrency, l.FirstError)
			break
		}
	}
}

// describeDirect says what happens without the proxy, in one line.
func describeDirect(r v1.CalibrationRoute) string {
	if !r.DirectReachable {
		return "UNREACHABLE — the proxy is the only route out (" + r.DirectDetail + ")"
	}
	if r.ProxiedRate <= 0 || r.DirectRate <= 0 {
		return "reachable"
	}
	return fmt.Sprintf("reachable: %s direct against %s through the proxy",
		rateString(r.DirectRate), rateString(r.ProxiedRate))
}

func renderSuggestions(w io.Writer, suggestions []v1.CalibrationSuggestion) error {
	fmt.Fprintln(w)
	if len(suggestions) == 0 {
		fmt.Fprintln(w, "No suggestions: nothing measured differed from what is configured.")
		return nil
	}

	fmt.Fprintln(w, "Suggestions")
	for _, s := range suggestions {
		fmt.Fprintf(w, "\n  %s %s\n", severityMark(s.Severity), suggestionHeadline(s))
		for _, line := range wrap(s.Evidence, 76) {
			fmt.Fprintf(w, "      %s\n", line)
		}
	}
	return nil
}

// suggestionHeadline is the change itself, on one line: what to set, where,
// and from what to what.
func suggestionHeadline(s v1.CalibrationSuggestion) string {
	var b strings.Builder
	if s.Setting != "" {
		b.WriteString(s.Setting)
	}
	if s.Scope != "" {
		if b.Len() > 0 {
			b.WriteString("  ")
		}
		b.WriteString("(" + s.Scope + ")")
	}
	switch {
	case s.Current != "" && s.Suggested != "":
		b.WriteString("  " + s.Current + " -> " + s.Suggested)
	case s.Suggested != "":
		if b.Len() > 0 {
			b.WriteString("  ")
		}
		b.WriteString(s.Suggested)
	}
	return b.String()
}

// severityMark is a glyph rather than a word, so the headline starts in the
// same column on every line and the list scans vertically.
func severityMark(severity string) string {
	switch severity {
	case "critical":
		return "!!"
	case "recommended":
		return " !"
	default:
		return "  "
	}
}

func rateString(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "-"
	}
	return humanBytes(v1.Int64String(fmt.Sprintf("%d", int64(bytesPerSecond)))) + "/s"
}

// errorCell keeps throttling visible rather than folded into an error count. A
// 429 is the registry stating a limit, and it must not read as a generic
// failure.
func errorCell(l v1.CalibrationLevel) string {
	switch {
	case l.Throttled > 0:
		return fmt.Sprintf("%d (%d throttled)", l.Errors, l.Throttled)
	case l.Errors > 0:
		return fmt.Sprint(l.Errors)
	default:
		return "-"
	}
}

// wrap breaks evidence prose to a width, on word boundaries.
func wrap(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}

	var (
		lines []string
		line  strings.Builder
	)
	for _, word := range words {
		if line.Len() > 0 && line.Len()+1+len(word) > width {
			lines = append(lines, line.String())
			line.Reset()
		}
		if line.Len() > 0 {
			line.WriteString(" ")
		}
		line.WriteString(word)
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return lines
}
