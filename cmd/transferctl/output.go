package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"sigs.k8s.io/yaml"
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
