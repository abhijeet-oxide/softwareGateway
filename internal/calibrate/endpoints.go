package calibrate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// endpoint is one end of the path, resolved to something probeable.
type endpoint struct {
	name string
	role product.Role
	cfg  registry.ClientConfig
	// limits are what the configuration currently asks for, kept so advice can
	// say "you have 32, the knee is 4" rather than only "the knee is 4".
	limits product.Concurrency
}

// resolveSource picks the source to read from and resolves its client config.
func (c *Calibrator) resolveSource(p *product.Product, opts Options) (endpoint, error) {
	src, err := pickSource(p, opts.Source)
	if err != nil {
		return endpoint{}, err
	}

	repo := strings.TrimSpace(opts.SourceRepository)
	if repo == "" {
		if declared := src.DeclaredRepositories(); len(declared) > 0 {
			repo = declared[0]
		}
	}

	e := v1.JobEndpoint{
		Product: p.Metadata.Name, Role: string(product.RoleSource),
		Name: src.Name, Registry: src.Registry, Repository: repo, Type: string(src.Type),
	}
	cfg, err := regclient.ConfigFor(p, c.secrets, e)
	if err != nil {
		return endpoint{}, err
	}

	// A source that enumerates its repositories names none, and a throughput
	// probe needs one to read from. Asking the catalog is what discovery would
	// do, so the repository probed is one this product genuinely reads.
	if cfg.Repository == "" {
		found, err := firstRepository(context.Background(), cfg)
		if err != nil {
			return endpoint{}, fmt.Errorf(
				"source %q names no repositories and the registry catalog could not "+
					"supply one (%w); name one with --source-repository", src.Name, err)
		}
		cfg.Repository = found
	}

	return endpoint{name: src.Name, role: product.RoleSource, cfg: cfg, limits: src.Concurrency}, nil
}

// resolveTarget picks the target to write to and resolves its client config.
func (c *Calibrator) resolveTarget(p *product.Product, opts Options) (endpoint, error) {
	tgt, err := pickTarget(p, opts.Target)
	if err != nil {
		return endpoint{}, err
	}

	e := v1.JobEndpoint{
		Product: p.Metadata.Name, Role: string(product.RoleTarget),
		Name: tgt.Name, Registry: tgt.Registry, Repository: tgt.Repository,
		Type: string(tgt.Type),
	}
	cfg, err := regclient.ConfigFor(p, c.secrets, e)
	if err != nil {
		return endpoint{}, err
	}
	if cfg.Repository == "" {
		return endpoint{}, fmt.Errorf("target %q declares no repository to write to", tgt.Name)
	}

	return endpoint{name: tgt.Name, role: product.RoleTarget, cfg: cfg, limits: tgt.Concurrency}, nil
}

// pickSource resolves a name, or the only candidate.
//
// Refusing to guess between several is deliberate: the sources of one product
// are different vendors on different links, and calibrating the wrong one
// produces advice that is precisely as confident and entirely inapplicable.
func pickSource(p *product.Product, name string) (product.Source, error) {
	var enabled []product.Source
	for _, s := range p.Spec.Sources {
		if s.IsEnabled() {
			enabled = append(enabled, s)
		}
	}

	if name != "" {
		for _, s := range enabled {
			if s.Name == name {
				return s, nil
			}
		}
		return product.Source{}, fmt.Errorf("product %q has no enabled source %q (it has %s)",
			p.Metadata.Name, name, nameList(sourceNames(enabled)))
	}

	switch len(enabled) {
	case 0:
		return product.Source{}, fmt.Errorf("product %q has no enabled source", p.Metadata.Name)
	case 1:
		return enabled[0], nil
	default:
		return product.Source{}, fmt.Errorf(
			"product %q has %d sources, so --from is required (it has %s)",
			p.Metadata.Name, len(enabled), nameList(sourceNames(enabled)))
	}
}

// pickTarget resolves a name, the default, or the only candidate — the same
// order `transfers create` uses, so calibration and transfer land on the same
// destination when neither is told one.
func pickTarget(p *product.Product, name string) (product.Target, error) {
	var enabled []product.Target
	for _, t := range p.Spec.Targets {
		if t.IsEnabled() {
			enabled = append(enabled, t)
		}
	}

	if name != "" {
		for _, t := range enabled {
			if t.Name == name {
				return t, nil
			}
		}
		return product.Target{}, fmt.Errorf("product %q has no enabled target %q (it has %s)",
			p.Metadata.Name, name, nameList(targetNames(enabled)))
	}

	// The same resolution `transfers create` uses, so calibrating without --to
	// measures the destination a transfer without --to would actually use.
	if t, ok := p.DefaultTarget(); ok {
		return t, nil
	}
	if len(enabled) == 0 {
		return product.Target{}, fmt.Errorf("product %q has no enabled target", p.Metadata.Name)
	}
	return product.Target{}, fmt.Errorf(
		"product %q has %d targets and none is the default, so --to is required (it has %s)",
		p.Metadata.Name, len(enabled), nameList(targetNames(enabled)))
}

// firstRepository asks the catalog for one repository to read from.
func firstRepository(ctx context.Context, cfg registry.ClientConfig) (string, error) {
	cat, err := generic.NewCatalog(generic.CatalogConfig{
		Registry:  cfg.Registry,
		PlainHTTP: cfg.PlainHTTP,
		Transport: transportConfig(cfg),
	})
	if err != nil {
		return "", err
	}
	repos, err := cat.ListAllRepositories(ctx, 50)
	if err != nil {
		return "", err
	}
	if len(repos) == 0 {
		return "", fmt.Errorf("the catalog is empty")
	}
	sort.Strings(repos)
	return repos[0], nil
}

func sourceNames(ss []product.Source) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func targetNames(ts []product.Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func nameList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
