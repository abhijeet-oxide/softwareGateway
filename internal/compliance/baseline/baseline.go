// Package baseline is the policy pack this platform ships.
//
// # Why it is compiled in rather than mounted
//
// It is always present and cannot be removed by deleting a directory. A
// misconfigured policy mount would otherwise turn every release green, and a
// tool that reports "compliant" because it loaded no rules is worse than no
// tool - the number on the screen is the same either way, and only one of them
// is true.
//
// # Why it is YAML rather than Go
//
// The same reason an organization's own pack is: a check is data. The
// catalogue page lists it, the exporter prints it into the vendor's
// spreadsheet, and a reviewer sees a severity change as a one-line diff. It
// also means the shipped checks are written in exactly the form the
// documentation tells a team to use, so the examples in
// docs/compliance/02-authoring-checks.md are not a parallel dialect that
// drifts.
package baseline

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed packs/*.yaml
var packs embed.FS

// Files returns the shipped manifests, by name, in load order.
//
// Names are prefixed with a number so the order is the file order and the
// bundle digest is stable - the same property the on-disk loader gets from
// sorting paths.
func Files() (map[string][]byte, error) {
	entries, err := fs.ReadDir(packs, "packs")
	if err != nil {
		return nil, fmt.Errorf("reading the shipped policy pack: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".yaml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make(map[string][]byte, len(names))
	for _, n := range names {
		b, err := fs.ReadFile(packs, path.Join("packs", n))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", n, err)
		}
		out[n] = b
	}
	return out, nil
}

// Names is the shipped manifests in load order.
func Names() ([]string, error) {
	files, err := Files()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}
