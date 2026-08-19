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
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
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

	// candidates are the repositories worth trying, most promising first.
	//
	// A source spanning forty repositories has no single "the" repository, and
	// picking the first one declared is how a calibration ends up measuring
	// `cfx-5000-product/aaa` - a repository that exists, contains one tag and
	// no blob worth timing. The probe walks this list until something is
	// actually measurable and reports which one it used.
	candidates []string

	// basePath is the target's configured repository prefix, kept separately
	// from cfg.Repository because a write probe must go to a path a transfer
	// would use - base + source path - and not to the bare prefix.
	basePath string
}

// resolveSource picks the source to read from and resolves its client config.
func (c *Calibrator) resolveSource(p *product.Product, opts Options) (endpoint, error) {
	src, err := pickSource(p, opts.Source)
	if err != nil {
		return endpoint{}, err
	}

	// An explicit choice is the whole candidate list: somebody who named a
	// repository meant that one, and silently falling through to another would
	// measure a path they did not ask about.
	candidates := src.DeclaredRepositories()
	if named := strings.TrimSpace(opts.SourceRepository); named != "" {
		candidates = []string{named}
	}

	first := ""
	if len(candidates) > 0 {
		first = candidates[0]
	}

	e := v1.JobEndpoint{
		Product: p.Metadata.Name, Role: string(product.RoleSource),
		Name: src.Name, Registry: src.Registry, Repository: first, Type: string(src.Type),
	}
	cfg, err := regclient.ConfigFor(p, c.secrets, e)
	if err != nil {
		return endpoint{}, err
	}

	// A source that enumerates its repositories names none, and a throughput
	// probe needs one to read from. Asking the catalog is what discovery would
	// do, so the repositories probed are ones this product genuinely reads.
	if len(candidates) == 0 {
		found, err := catalogRepositories(context.Background(), cfg)
		if err != nil {
			return endpoint{}, fmt.Errorf(
				"source %q names no repositories and the registry catalog could not "+
					"supply one (%w); name one with --source-repository", src.Name, err)
		}
		candidates = found
		cfg.Repository = candidates[0]
	}

	return endpoint{
		name: src.Name, role: product.RoleSource, cfg: cfg,
		limits: src.Concurrency, candidates: candidates,
	}, nil
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
	return endpoint{
		name: tgt.Name, role: product.RoleTarget, cfg: cfg,
		limits: tgt.Concurrency, basePath: tgt.Repository,
	}, nil
}

// writeProbePath is where the target probe opens its upload session.
//
// It is `base + source path`, exactly what the planner computes for a real
// job - not the target's configured repository, which is a PREFIX. That
// distinction cost a run: probing `apm0014228-oci-stage` directly returned
//
//	404 Not Found: not found
//
// from a registry that was working perfectly and had just accepted sixty
// gigabytes, because a prefix is not an image repository and cannot hold an
// upload. Deriving it through transfer.DestinationPath is what stops the probe
// and the transfer disagreeing about where bytes go.
func writeProbePath(target endpoint, sourceRepository string) string {
	if p := transfer.DestinationPath(target.basePath, sourceRepository); p != "" {
		return p
	}
	return target.cfg.Repository
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

// pickTarget resolves a name, the default, or the only candidate - the same
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

// catalogRepositories asks the catalog what there is to read from.
func catalogRepositories(ctx context.Context, cfg registry.ClientConfig) ([]string, error) {
	cat, err := generic.NewCatalog(generic.CatalogConfig{
		Registry:  cfg.Registry,
		PlainHTTP: cfg.PlainHTTP,
		Transport: transportConfig(cfg),
	})
	if err != nil {
		return nil, err
	}
	repos, err := cat.ListAllRepositories(ctx, 200)
	if err != nil {
		return nil, err
	}
	if len(repos) == 0 {
		return nil, fmt.Errorf("the catalog is empty")
	}
	sort.Strings(repos)
	return repos, nil
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
