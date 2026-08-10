package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

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
	return tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
}

func stdout() io.Writer { return os.Stdout }

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
// at discovery — so `orbs/cfx-5000-k8s:orb_23.8.1076` renders as
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
// Three values, not two. `n/a` is "nobody looked" — a source whose layout does
// not attempt signature discovery — and it is deliberately not `no`, which
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
