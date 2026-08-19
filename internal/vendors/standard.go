package vendors

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// Layout names accepted in configuration.
const (
	// LayoutAuto is the default: standard behaviour, and the value to keep
	// unless a vendor genuinely deviates.
	LayoutAuto = "auto"
	// LayoutNone records every tag as its own package and looks for nothing.
	// Fastest, and the honest choice for a registry with no signing at all -
	// it produces `unknown` rather than a misleading `unsigned`.
	LayoutNone = "none"
	// LayoutStandard is OCI 1.1 referrers with the cosign tag schema as
	// fallback. Reserved: signature discovery lands with verification in M5,
	// so today it behaves as LayoutNone and says so.
	LayoutStandard = "standard"
)

// Registry resolves a configured layout name to an implementation.
//
// A map rather than a switch so a vendor package registers itself from its own
// init, which is what keeps the core free of any import of a vendors.
type Registry struct {
	layouts map[string]Layout
}

// NewRegistry returns a registry with the built-in layouts.
func NewRegistry() *Registry {
	r := &Registry{layouts: map[string]Layout{}}
	r.Register(Standard{})
	return r
}

// Register adds a layout. Called by vendor packages from init.
func (r *Registry) Register(l Layout) { r.layouts[l.Name()] = l }

// Get resolves a configured name.
//
// An unknown name is an ERROR, not a silent fallback to standard behaviour. A
// typo in `layout: nearr` that quietly disabled signature discovery would be
// invisible in every output - packages would simply read `unknown` forever, and
// nothing would say why.
func (r *Registry) Get(name string) (Layout, error) {
	switch strings.TrimSpace(name) {
	case "", LayoutAuto, LayoutStandard, LayoutNone:
		return Standard{}, nil
	}
	l, ok := r.layouts[name]
	if !ok {
		return nil, fmt.Errorf("unknown vendor %q (known: %s)",
			name, strings.Join(r.Names(), ", "))
	}
	return l, nil
}

// Names lists the registered layouts, sorted, for error messages and help.
func (r *Registry) Names() []string {
	out := []string{LayoutAuto, LayoutNone, LayoutStandard}
	for name := range r.layouts {
		if name != LayoutStandard {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Standard is the layout for a registry that does nothing unusual: one tag is
// one package, and nothing is bundled or renamed.
//
// It is what any OCI-conformant source gets, and it is what makes the plugin
// mechanism optional rather than mandatory - a vendor who publishes normally
// needs no plugin at all.
type Standard struct{}

func (Standard) Name() string { return LayoutStandard }

// Vocabulary is the OCI wording: a conformant source has repositories and tags,
// and calling them anything else would be the invention this package exists to
// prevent.
func (Standard) Vocabulary() Vocabulary { return StandardVocabulary() }

// LooksForSignatures is false today.
//
// Deliberately honest rather than optimistic. Referrers-based discovery lands
// with verification in M5; until it does, claiming to have looked would turn
// every package into a confident `unsigned`, which is exactly the false
// negative the three-state status exists to prevent.
func (Standard) LooksForSignatures() bool { return false }

// DisplayRepository shortens nothing.
//
// A conformant registry has no structural noise to remove, and inventing some -
// by dropping whatever prefix a page of results happens to share, say - would
// make a listing say different things about the same repository depending on
// what else was on screen.
func (Standard) DisplayRepository(string) string { return "" }

// DisplayTag shortens nothing, for the same reason.
func (Standard) DisplayTag(string) string { return "" }

// AccessoryTags is none: a conformant registry publishes a release as one tag,
// and anything attached to it is found through the referrers API rather than by
// guessing at names.
func (Standard) AccessoryTags(string) []string { return nil }

// ClassifyArtifact defers to the OCI rules, which is the whole point of a
// conformant registry: it declares what its content is and needs no help.
func (Standard) ClassifyArtifact(map[string]string) string { return "" }

// Group maps each scanned tag to one package, unchanged.
func (Standard) Group(
	_ context.Context, _ registry.Source, scanned []ScannedTag,
) ([]Package, error) {
	return Standard{}.Fallback(scanned), nil
}

// Fallback is Group without a context or an error, for callers recovering from
// a vendor Layout that failed.
//
// Degrading to one-package-per-tag means a vendor changing its convention makes
// the output noisier rather than making discovery find nothing - the latter
// being an outage, and the former an annoyance somebody notices and reports.
func (Standard) Fallback(scanned []ScannedTag) []Package {
	out := make([]Package, 0, len(scanned))
	for _, s := range scanned {
		out = append(out, Package{Tag: s.Tag, Descriptor: s.Descriptor})
	}
	return out
}
