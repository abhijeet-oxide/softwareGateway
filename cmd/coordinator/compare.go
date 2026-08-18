package main

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/api"
	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// compareImpl joins configuration, the catalog and the client factory for the
// comparison — the same three things resolverImpl joins, for the same reason:
// this is the composition root, and internal/compare staying free of them is
// what lets it be tested against a fake registry and literals.
type compareImpl struct {
	*resolverImpl
	// layouts is what lets a comparison name a vendor's components the way the
	// release page names them. THE COMPOSITION ROOT IS THE ONLY PLACE THIS MAY
	// BE RESOLVED — internal/compare takes the resulting function and never
	// learns whose it is.
	layouts *vendors.Registry
}

// Compare walks two places and reports what is different.
//
// Both ends are resolved the same way, and that symmetry is the point: a source
// and a target, two targets, or one place at two versions are the same call with
// different arguments. Nothing here knows which of those the caller meant.
func (c compareImpl) Compare(
	ctx context.Context, productName string, a, b api.ComparePoint, fileBudget int64,
) (compare.Report, error) {
	p, ok := c.products.Get(productName)
	if !ok {
		return compare.Report{}, fmt.Errorf("product %q is not configured", productName)
	}

	// THE DEFAULT IS THE QUESTION PEOPLE ACTUALLY ASK. With no endpoint named
	// on either side and one version, "compare this release" means "did it
	// land at my destination" — so the second end becomes the default target.
	//
	// With TWO versions it means something else entirely, and the two ends stay
	// where they are: comparing two releases is a question about the vendor's
	// catalogue, and answering it against a destination by default would report
	// everything not yet copied as a difference.
	if a.Endpoint == "" && b.Endpoint == "" && a.Package.Tag == b.Package.Tag {
		if target, ok := defaultTarget(p); ok {
			b.Endpoint = target.Name
		}
	}

	specA, clientA, err := c.side(p, a)
	if err != nil {
		return compare.Report{}, err
	}
	specB, clientB, err := c.side(p, b)
	if err != nil {
		return compare.Report{}, err
	}

	// The two sides must be distinguishable in the output, and where they are
	// the same place at two versions the endpoint name alone is not.
	if specA.Label == specB.Label && a.Package.Tag != b.Package.Tag {
		specA.Label += " " + displayName(a.Package)
		specB.Label += " " + displayName(b.Package)
	}

	return compare.Run(ctx, clientA, clientB, compare.Options{
		A: specA, B: specB,
		Concurrency: concurrencyOf(p),
		FileBudget:  resolveFileBudget(fileBudget),
		Classify:    vendors.ClassifierFor(c.layouts, layoutNames(p)),
	})
}

// side resolves one end into where to read and how to reach it.
//
// An endpoint name is looked up among the product's TARGETS first and its
// SOURCES second, and an empty one means the repository the package was
// discovered in. Targets first because that is the common case for this command
// — the interesting comparisons are about destinations — and because a source
// and a target sharing a name is a configuration somebody would have to work at.
func (c compareImpl) side(
	p *product.Product, point api.ComparePoint,
) (compare.SideSpec, compare.ClientFactory, error) {
	pkg := point.Package
	if pkg.SourceRepository == "" {
		return compare.SideSpec{}, nil, fmt.Errorf(
			"package %s has no source repository recorded, so there is nothing to "+
				"locate it by", pkg.Tag)
	}

	spec := compare.SideSpec{References: referencesFor(pkg)}

	if target, ok := findTarget(p, point.Endpoint); ok {
		spec.Label = target.Name
		spec.BasePath = target.Repository
		spec.Repository = transfer.DestinationPath(target.Repository, pkg.SourceRepository)
		// A TARGET is a place this system wrote, so it is the one end that owes
		// a component under the component's own name. A source owes nothing of
		// the kind — see compare.SideSpec.PublishesComponentsByName.
		spec.PublishesComponentsByName = true
		return spec, c.factory(p.Metadata.Name, target.Name, target.Registry,
			string(target.Type), string(product.RoleTarget)), nil
	}

	source, err := findSource(p, point.Endpoint)
	if err != nil {
		return compare.SideSpec{}, nil, err
	}
	// A source holds the vendor's own paths, unprefixed — so a component's
	// `ref.name` is already the repository it lives in, and BasePath stays
	// empty.
	spec.Label = source.Name
	spec.Repository = pkg.SourceRepository
	return spec, c.factory(p.Metadata.Name, source.Name, source.Registry,
		string(source.Type), string(product.RoleSource)), nil
}

// referencesFor is what to walk from, most complete first.
//
// The wrapper where a vendor has one: only it reaches both the payload and the
// signature, so walking it compares the whole release rather than the part a
// consumer happens to pull. The payload tag is the fallback, and it is a real
// state rather than a failure — a destination holding only the payload is
// exactly what a transfer that stopped early leaves behind.
//
// BOTH SPELLINGS OF EACH ROOT, tag and digest. A missing TAG and a missing
// MANIFEST are different failures with different fixes, and a list carrying
// only the wrapper's tag could not tell them apart: a destination holding the
// wrapper untagged fell all the way through to the payload, so the comparison
// walked the wrapper on one side and the payload on the other and called the
// resulting digest mismatch a content difference. With the digest in the list
// both sides walk the wrapper, and the missing tag is reported as the missing
// tag it is.
func referencesFor(pkg store.PackageRow) []string {
	var refs []string
	if pkg.TransferRootTag != "" {
		refs = append(refs, pkg.TransferRootTag)
	}
	if pkg.TransferRootDigest != "" {
		refs = append(refs, pkg.TransferRootDigest)
	}
	if pkg.Tag != "" {
		refs = append(refs, pkg.Tag)
	}
	if pkg.ManifestDigest != "" {
		// Last resort, and it only helps on the side the package was
		// discovered from — but on that side it always works, which makes
		// "the tag is gone" a finding rather than an error.
		refs = append(refs, pkg.ManifestDigest)
	}
	return refs
}

// factory builds a client for any path on one endpoint's registry.
//
// Any path, not one: a bundle's components live in their own repositories under
// the same registry and credential, and the comparison has to reach each of
// them to answer whether a component is pullable under its own name.
func (c compareImpl) factory(
	productName, endpointName, host, registryType, role string,
) compare.ClientFactory {
	return func(repository string) (registry.Repository, error) {
		return c.clients.For(v1.JobEndpoint{
			Product:    productName,
			Name:       endpointName,
			Registry:   host,
			Repository: repository,
			Type:       registryType,
			Role:       role,
		})
	}
}

// findTarget resolves a configured target by name.
//
// An UNNAMED end is never a target, not even the default one. It is the source,
// because that is the end a package is defined by — and guessing a destination
// nobody named is how somebody ends up reading a confident page of mismatches
// about the wrong registry.
func findTarget(p *product.Product, name string) (product.Target, bool) {
	if name == "" {
		return product.Target{}, false
	}
	for _, t := range p.Spec.Targets {
		if t.IsEnabled() && t.Name == name {
			return t, true
		}
	}
	return product.Target{}, false
}

// defaultTarget is the destination to assume when nobody named one.
//
// The declared default, or the only enabled target. With several and no default
// there is nothing to assume, and the comparison falls back to source-against-
// source — which produces "identical" rather than a page of mismatches about a
// registry the caller did not mean.
func defaultTarget(p *product.Product) (product.Target, bool) {
	var enabled []product.Target
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		if t.Default {
			return t, true
		}
		enabled = append(enabled, t)
	}
	if len(enabled) == 1 {
		return enabled[0], true
	}
	return product.Target{}, false
}

// layoutNames is the vendor layouts a product's sources declare.
//
// A comparison may run entirely between TARGETS, which declare no layout of
// their own — but what sits in a target is what a source published, so the
// source's layouts are what name it. Anything else would classify the same
// component differently at the two ends of one comparison and report a
// difference that is not there.
func layoutNames(p *product.Product) []string {
	out := make([]string, 0, len(p.Spec.Sources))
	for _, s := range p.Spec.Sources {
		out = append(out, s.VendorLayout())
	}
	return out
}

// defaultFileBudget is how much layer content a comparison downloads by default
// in order to answer "which FILE changed" rather than "which layer changed".
//
// Sized against what it is FOR. A vendor's configuration bundle is kilobytes to
// a few megabytes, and this opens every one of them; an image layer is hundreds
// of megabytes, and this opens none of them. That asymmetry is the whole design
// — the expensive layers are exactly the ones whose file listing nobody wants.
const defaultFileBudget int64 = 64 << 20

// resolveFileBudget reads the caller's intent: unset means the default, and a
// negative value means "do not open anything".
func resolveFileBudget(requested int64) int64 {
	switch {
	case requested > 0:
		return requested
	case requested < 0:
		return 0
	default:
		return defaultFileBudget
	}
}

// concurrencyOf is the ceiling an operator set for this product's registries.
//
// A comparison opens the same connections a transfer does, so it obeys the same
// limit rather than choosing one here.
func concurrencyOf(p *product.Product) int {
	for _, s := range p.Spec.Sources {
		if s.Concurrency.PerRegistry > 0 {
			return s.Concurrency.PerRegistry
		}
	}
	for _, t := range p.Spec.Targets {
		if t.Concurrency.PerRegistry > 0 {
			return t.Concurrency.PerRegistry
		}
	}
	return 0
}

// findSource resolves a configured source by name, or the product's only one.
func findSource(p *product.Product, name string) (product.Source, error) {
	var enabled []product.Source
	for _, s := range p.Spec.Sources {
		if !s.IsEnabled() {
			continue
		}
		if name != "" && s.Name == name {
			return s, nil
		}
		enabled = append(enabled, s)
	}

	if name != "" {
		return product.Source{}, fmt.Errorf(
			"product %q has no enabled source or target named %q",
			p.Metadata.Name, name)
	}
	if len(enabled) == 1 {
		return enabled[0], nil
	}
	if len(enabled) == 0 {
		return product.Source{}, fmt.Errorf(
			"product %q has no enabled source", p.Metadata.Name)
	}
	// Refusing to guess: comparing against the wrong registry produces a page
	// of confident mismatches, which is worse than being asked which one.
	return product.Source{}, fmt.Errorf(
		"product %q has %d enabled sources, so the end must be named",
		p.Metadata.Name, len(enabled))
}

// displayName is the vendor's shortened spelling of a version where there is
// one, so a header reads `25.7_mp2604_2131` rather than `orb_25.7_mp2604_2131`.
func displayName(pkg store.PackageRow) string {
	if pkg.DisplayTag != "" {
		return pkg.DisplayTag
	}
	return pkg.Tag
}

// compile-time check that the composition root satisfies what the API needs.
var _ api.Comparer = compareImpl{}
