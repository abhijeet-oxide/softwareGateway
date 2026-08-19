package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"golang.org/x/term"
	"sigs.k8s.io/yaml"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// render emits a value in the requested format.
//
// table is the default because these outputs are read by humans at a terminal;
// json and yaml exist so the same command is scriptable without a second code
// path. tableFn is only called for the table format, so building a table is
// not paid for when piping to jq.
func render(w io.Writer, format string, v any, tableFn func(io.Writer) error) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(v)

	case "yaml", "yml":
		// sigs.k8s.io/yaml routes through the JSON tags, so YAML output uses
		// the same field names as the API rather than Go field names.
		b, err := yaml.Marshal(v)
		if err != nil {
			return fmt.Errorf("marshal yaml: %w", err)
		}
		_, err = w.Write(b)
		return err

	case "table", "":
		if tableFn == nil {
			return fmt.Errorf("table output is not available for this command; use -o json")
		}
		return tableFn(w)

	default:
		return fmt.Errorf("unknown output format %q: expected table, json or yaml", format)
	}
}

// newTabWriter returns a writer that aligns columns for terminal output.
func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, tabGap, ' ', 0)
}

func stdout() io.Writer { return os.Stdout }

// ---------------------------------------------------------------------------
// fitting a table to the terminal
// ---------------------------------------------------------------------------

// A row wider than the terminal WRAPS, and a wrapped table is not a table: the
// second half of every row lands under the first half of the next one, columns
// stop lining up, and the reader is left reassembling rows by eye. It is worst
// on exactly the listings worth reading, because the rows that make a table
// wide are the ones with the most in them.
//
// So the columns that CAN give are shortened until the row fits, and the rest
// are left alone - a digest or a percentage means nothing shortened.

const (
	// tabGap is the padding newTabWriter puts between columns, and therefore
	// part of what a row costs. Defined here so the fitting and the writer
	// cannot disagree about it.
	tabGap = 3
	// minCell is the narrowest a shortened column may become. Below this a
	// path is two fragments and an ellipsis, which is not a shorter name for
	// something - it is a different string that happens to be short.
	minCell = 14
	// minUsableWidth is the narrowest terminal worth fitting to. Anything less
	// cannot hold these tables under any policy, and truncating to it would
	// destroy every column rather than the widest one.
	minUsableWidth = 40
)

// terminalWidth is how many columns the output has, or 0 when that is not a
// question with an answer.
//
// Zero for a pipe or a file, and that is deliberate rather than a fallback: a
// redirected listing is being read by something whose width nothing here knows,
// and shortening a path that is about to be grepped would corrupt the answer to
// keep a screen tidy that nobody is looking at.
//
// COLUMNS wins where it is set, because a person who exported it has said what
// they want; otherwise the terminal is asked directly, which is the only thing
// that works on a Windows console, where COLUMNS is not set by default.
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	if v := strings.TrimSpace(os.Getenv("COLUMNS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= minUsableWidth {
			return n
		}
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width < minUsableWidth {
		return 0
	}
	return width
}

// fitCells shortens the ELASTIC columns of a table, in place, until a row fits
// the given width.
//
// The widest elastic column gives first, and keeps giving until it is no longer
// the widest - so a table with one enormous column loses from that one, and a
// table with two loses from both evenly. Nothing is taken from a column that is
// already at minCell, and nothing at all is taken when the width is unknown.
//
// # Nothing is taken when nothing would be gained
//
// A table can be too wide to fit at any shortening: the columns that cannot be
// shortened, plus the padding between fifteen of them, can exceed the terminal
// on their own. Shortening then buys nothing - the row wraps either way - and
// costs the reader every path in the listing. So the floor is checked FIRST,
// and a table that cannot fit is left exactly as it was.
//
// That is not a corner case. It is what a 120-column terminal does to the
// transfer listing, and the first version of this shortened four columns to
// their floors on the way to a row that still wrapped.
func fitCells(rows [][]string, elastic []int, width int) {
	if width <= 0 || len(rows) == 0 {
		return
	}

	columns := len(rows[0])
	caps := make([]int, columns)
	for _, row := range rows {
		for i, cell := range row {
			if i < columns && utf8.RuneCountInString(cell) > caps[i] {
				caps[i] = utf8.RuneCountInString(cell)
			}
		}
	}

	if !fits(caps, elastic, width) {
		return
	}

	for rowWidth(caps) > width {
		widest := -1
		for _, i := range elastic {
			if i < columns && caps[i] > minCell && (widest < 0 || caps[i] > caps[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		caps[widest]--
	}

	for _, row := range rows {
		for _, i := range elastic {
			if i < len(row) {
				row[i] = middleTruncate(row[i], caps[i])
			}
		}
	}
}

// fits reports whether this table could be made to fit at all - every elastic
// column shortened as far as it may go, and the rest left as they are.
func fits(caps []int, elastic []int, width int) bool {
	floored := append([]int(nil), caps...)
	for _, i := range elastic {
		if i < len(floored) && floored[i] > minCell {
			floored[i] = minCell
		}
	}
	return rowWidth(floored) <= width
}

// rowWidth is what one row costs on screen: its cells plus the padding between
// them.
func rowWidth(caps []int) int {
	out := 0
	for _, n := range caps {
		out += n
	}
	if len(caps) > 1 {
		out += tabGap * (len(caps) - 1)
	}
	return out
}

// middleTruncate shortens s to at most n runes by removing the MIDDLE.
//
// Not the end, which is the usual choice and the wrong one for what these
// columns hold. A repository reference carries its two halves of the identity
// at its two ends - `cfx-near/orbs/cfx-5000-k8s-215952-edgenac-…-ncm` says
// WHERE at the front and WHICH at the back - and a column of them shares the
// front. Cutting the tail leaves every row reading identically, which is the
// one outcome a shortened column must not produce: a listing whose rows can no
// longer be told apart has lost more than the characters it dropped.
//
// The tail keeps the odd rune, because that is the end that differs.
func middleTruncate(s string, n int) string {
	runes := []rune(s)
	if n <= 0 || len(runes) <= n {
		return s
	}
	if n <= 2 {
		// No room for two fragments and a mark. One end, whole, beats two
		// fragments of one character each.
		return string(runes[:n])
	}

	keep := n - 1 // the ellipsis is one rune, and is spent from the budget
	tail := (keep + 1) / 2
	return string(runes[:keep-tail]) + "…" + string(runes[len(runes)-tail:])
}

// dash renders an empty value as an em dash so a table never has holes that
// look like a rendering bug.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "–"
	}
	return s
}

// yesNo renders a boolean for table output.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// humanConcurrency renders the resolved limit for one registry.
//
// The rate appears only when there is one, because zero means "no artificial
// limit" and printing "0/s" reads as a registry that has been throttled to a
// standstill.
func humanConcurrency(c v1.Concurrency) string {
	if c.RequestsPerSecond > 0 {
		return fmt.Sprintf("%d, %d/s", c.PerRegistry, c.RequestsPerSecond)
	}
	return strconv.Itoa(c.PerRegistry)
}

// ---------------------------------------------------------------------------
// display names
// ---------------------------------------------------------------------------

// A listing hides two kinds of noise, and NEITHER involves this package knowing
// anything about a vendor.
//
// Both the TAG and the REPOSITORY come pre-shortened from the server, as
// `displayTag` and `displayRepository`, computed by the source's vendor plugin
// at discovery - so `orbs/cfx-5000-k8s:orb_23.8.1076` renders as
// `cfx-5000-k8s:23.8.1076` only because a plugin said those are this vendor's
// conventions. A source with no `vendor` set gets neither, which is what any
// conformant registry gets. Both spellings resolve as input, so copying what
// you see always works.
//
// The repository used to be shortened HERE, by dropping the prefix every row in
// view happened to share. That needed no vendor knowledge, which was the appeal
// and also the bug: it trimmed paths on registries with no such convention, and
// it made a row say different things depending on which other rows were on the
// page. The rule now comes from a statement about the vendor rather than from
// the shape of a result set.

// signedMark renders a package's signature status for a table.
//
// Three values, not two. `n/a` is "nobody looked" - a source whose layout does
// not attempt signature discovery - and it is deliberately not `no`, which
// would be a confident claim nobody checked.
func signedMark(s v1.SignatureStatus) string {
	switch s {
	case v1.SignatureSigned:
		return "yes"
	case v1.SignatureUnsigned:
		return "no"
	default:
		return notAvailable
	}
}

// displayTag renders a package's tag for a table.
//
// The shortened form comes from the SERVER, computed by the source's layout
// plugin, because only that plugin knows which part of a tag is the vendor's
// structural noise. This package deliberately cannot tell `orb_23.8.1076` from
// a tag that genuinely begins with those characters.
func displayTag(p v1.Package) string {
	if p.DisplayTag != "" {
		return p.DisplayTag
	}
	return p.Tag
}

// displayRepository renders a package's repository for a table.
//
// Same contract as displayTag, and the same reason: only the source's vendor
// plugin knows that `orbs/` is structural, so the shortened form is computed
// there and this package cannot tell it from a path that genuinely begins with
// those characters.
func displayRepository(p v1.Package) string {
	if p.DisplayRepository != "" {
		return p.DisplayRepository
	}
	return p.SourceRepository
}
