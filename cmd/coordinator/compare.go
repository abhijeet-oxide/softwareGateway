package main

import (
	"context"
	"fmt"

	"github.com/abhijeet-oxide/softwareGateway/internal/compare"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// compareImpl joins configuration, the catalog and the client factory for the
// comparison — the same three things resolverImpl joins, for the same reason:
// this is the composition root, and internal/compare staying free of them is
// what lets it be tested against a fake registry and literals.
type compareImpl struct{ *resolverImpl }

// Compare checks what a destination actually holds against what the source
// published.
//
// The DESTINATION is asked in this call. Nothing here reads a transfer's record
// of what it did, and that is the entire point — a transfer can report every
// job successful and leave the destination wrong, which is what happened.
func (c compareImpl) Compare(
	ctx context.Context, productName string, pkg store.PackageRow, to string,
) (compare.Report, error) {
	p, ok := c.products.Get(productName)
	if !ok {
		return compare.Report{}, fmt.Errorf("product %q is not configured", productName)
	}

	target, err := resolveTarget(p, to)
	if err != nil {
		return compare.Report{}, err
	}

	tree, complete, err := c.packages.ReadExpandedTree(ctx, pkg.ID)
	if err != nil {
		return compare.Report{}, err
	}
	if !complete || len(tree.Artifacts) == 0 {
		// A comparison against a half-known source would report the parts it
		// happens to know about and silently omit the rest, which is the one
		// answer worse than no answer for a command whose whole job is to say
		// whether anything is missing.
		return compare.Report{}, fmt.Errorf(
			"package %s has not been fully expanded, so there is nothing to compare "+
				"against: run `transferctl packages inspect`", pkg.Tag)
	}

	return compare.Run(ctx, c.targetFactory(productName, target), compare.Options{
		Tree:             tree,
		SourceRepository: pkg.SourceRepository,
		TargetBasePath:   target.Repository,
		RootTags:         []string{pkg.Tag},
		Concurrency:      target.Concurrency.PerRegistry,
	})
}

// targetFactory builds a client for any path beneath a configured target.
//
// Beneath, not at: a bundle's components land in their own paths under the
// target's configured prefix, so a comparison needs a handle per path — and
// each one must carry the TARGET's credential, because the paths are the
// vendor's structure rather than separate configuration.
func (c compareImpl) targetFactory(
	productName string, target product.Target,
) compare.ClientFactory {
	return func(repository string) (registry.Repository, error) {
		return c.clients.For(v1.JobEndpoint{
			Product:    productName,
			Name:       target.Name,
			Registry:   target.Registry,
			Repository: repository,
			Type:       string(target.Type),
			Role:       string(product.RoleTarget),
		})
	}
}

// resolveTarget picks the destination to compare against.
//
// Named, or the product's default. Refusing to guess between several is
// deliberate: comparing against the wrong destination produces a page of
// confident mismatches, which is worse than being asked which one.
func resolveTarget(p *product.Product, name string) (product.Target, error) {
	var enabled []product.Target
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		if name != "" && t.Name == name {
			return t, nil
		}
		enabled = append(enabled, t)
	}

	if name != "" {
		return product.Target{}, fmt.Errorf(
			"product %q has no enabled target %q", p.Metadata.Name, name)
	}
	for _, t := range enabled {
		if t.Default {
			return t, nil
		}
	}
	if len(enabled) == 1 {
		return enabled[0], nil
	}
	return product.Target{}, fmt.Errorf(
		"product %q has %d enabled targets and no default, so --to is required",
		p.Metadata.Name, len(enabled))
}

// compile-time check that the composition root satisfies what the API needs.
var _ interface {
	Compare(ctx context.Context, productName string, pkg store.PackageRow, to string) (compare.Report, error)
} = compareImpl{}
