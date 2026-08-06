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
// The TAG comes pre-shortened from the server as `displayTag`, computed by the
// source's layout plugin at discovery — so `orb_23.8.1076` renders as
// `23.8.1076` only because a plugin said that is this vendor's convention. Both
// spellings resolve as input, so copying what you see always works.
//
// The REPOSITORY is shortened here, by dropping the prefix EVERY row shares.
// That needs no vendor knowledge at all: if every repository in view sits under
// `orbs/`, the segment carries no information and goes; if they differ, it
// stays. The same argument as hiding the repository column entirely when a
// product has only one.

// commonRepositoryPrefix returns the leading path segments every repository
// shares, including the trailing slash, or "" when they share none.
//
// Only whole segments — trimming `cfx-5000-` off `cfx-5000-k8s` and
// `cfx-5000-db` would be shortening at a boundary that means nothing.
func commonRepositoryPrefix(paths []string) string {
	var segments []string
	first := true

	for _, p := range paths {
		if p == "" {
			continue
		}
		parts := strings.Split(p, "/")
		// The last segment is the repository's own name and is never dropped,
		// or a set of one would render as nothing at all.
		parts = parts[:len(parts)-1]

		if first {
			segments = parts
			first = false
			continue
		}
		n := 0
		for n < len(segments) && n < len(parts) && segments[n] == parts[n] {
			n++
		}
		segments = segments[:n]
		if len(segments) == 0 {
			return ""
		}
	}
	if len(segments) == 0 {
		return ""
	}
	return strings.Join(segments, "/") + "/"
}

// shortRepository drops a prefix from a repository path for display.
func shortRepository(path, prefix string) string {
	if prefix != "" && len(path) > len(prefix) && strings.HasPrefix(path, prefix) {
		return path[len(prefix):]
	}
	return path
}

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
