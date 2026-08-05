package discovery

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// CatalogLister enumerates a registry's repositories.
//
// A consumer-defined narrow interface: discovery needs one method, not the
// whole catalog client (docs/design/15 §6). It also keeps this package
// testable without a registry.
type CatalogLister interface {
	ListAllRepositories(ctx context.Context, maxEntries int) ([]string, error)
}

// filter is a compiled include/exclude pair.
//
// The same type serves tags and repositories: the semantics are identical, and
// two implementations of "include, then exclude" would eventually disagree.
type filter struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// compileFilters compiles an include/exclude pair once, at loop start.
//
// Compiling per scan would recompile the same patterns every fifteen minutes
// forever and — worse — turn a configuration error into a recurring runtime
// failure rather than one loud complaint at startup.
//
// Patterns are RE2 (Go regexp): linear time, no backtracking. A user-supplied
// pattern evaluated inside a polling loop would, under a backtracking engine,
// be a denial-of-service vector (docs/design/02 §5.4).
func compileFilters(field string, f product.Filters) (filter, error) {
	var out filter

	for _, p := range f.Include {
		re, err := regexp.Compile(p)
		if err != nil {
			return filter{}, fmt.Errorf("%s.include %q: %w", field, p, err)
		}
		out.include = append(out.include, re)
	}
	for _, p := range f.Exclude {
		re, err := regexp.Compile(p)
		if err != nil {
			return filter{}, fmt.Errorf("%s.exclude %q: %w", field, p, err)
		}
		out.exclude = append(out.exclude, re)
	}
	return out, nil
}

// admits reports whether a value survives the filter.
//
// No include patterns means everything is included; exclude always wins over
// include, so a narrow carve-out can be expressed without rewriting the include
// list.
func (f filter) admits(v string) bool {
	if len(f.include) > 0 {
		matched := false
		for _, re := range f.include {
			if re.MatchString(v) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, re := range f.exclude {
		if re.MatchString(v) {
			return false
		}
	}
	return true
}

// RepositorySet is the outcome of resolving a source's repositories.
type RepositorySet struct {
	// Repositories are the paths to scan, sorted, de-duplicated and filtered.
	Repositories []string
	// FromCatalog is how many came from `/v2/_catalog` rather than config.
	FromCatalog int
	// Filtered is how many candidates the filters rejected.
	Filtered int
	// Truncated reports that the catalog cap was hit and the set is partial.
	Truncated bool
	// CatalogErr records a catalog enumeration that failed.
	//
	// NOT fatal: a source that also names repositories explicitly keeps
	// scanning those. A vendor forbidding `_catalog` — which is common, since
	// the credential is usually good for pulling a named repository and not for
	// enumerating the registry — must not stop the repositories we were told
	// about from being scanned.
	CatalogErr error
}

// resolveRepositories determines which repositories a source should scan.
//
// Two ways in, one rule out:
//
//	explicit `repository`/`repositories`  ─┐
//	                                       ├─▶ repositoryFilters ─▶ scan set
//	catalog enumeration (opt-in)          ─┘
//
// Filters apply to BOTH, so there is one rule to learn rather than one per
// source of names. Filtering an explicitly named repository is unusual but
// consistent — and useful when a document lists a broad set and carves out a
// few.
func resolveRepositories(
	ctx context.Context, src product.Source, f filter, catalog CatalogLister,
) RepositorySet {
	var res RepositorySet

	candidates := src.DeclaredRepositories()
	declared := len(candidates)

	if src.RepositoryDiscovery.Enabled && catalog != nil {
		maxRepos := src.RepositoryDiscovery.EffectiveMaxRepositories()
		found, err := catalog.ListAllRepositories(ctx, maxRepos)
		switch {
		case err != nil:
			res.CatalogErr = err
		default:
			candidates = append(candidates, found...)
			if len(found) >= maxRepos {
				res.Truncated = true
			}
		}
	}

	seen := map[string]bool{}
	var kept []string
	for i, r := range candidates {
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		if !f.admits(r) {
			res.Filtered++
			continue
		}
		kept = append(kept, r)
		if i >= declared {
			res.FromCatalog++
		}
	}

	// Sorted so the scan order — and therefore the order rows are created and
	// logs are emitted — is stable across restarts. A registry's catalog order
	// is not guaranteed stable, and unstable ordering makes diffs between two
	// runs unreadable.
	sort.Strings(kept)
	res.Repositories = kept
	return res
}

// describeCatalogError turns a catalog failure into something an operator can
// act on.
//
// A 401 or 403 here almost always means the credential is scoped to pulling
// named repositories rather than to enumerating the registry — which is normal
// for a vendor-issued credential and not a misconfiguration on our side.
func describeCatalogError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, registry.ErrUnauthorized), errors.Is(err, registry.ErrForbidden):
		return "the registry refused catalog enumeration: the credential is probably " +
			"scoped to pulling named repositories rather than listing the registry. " +
			"Name the repositories under `repositories:` instead"
	case errors.Is(err, registry.ErrNotFound):
		return "the registry does not implement /v2/_catalog. " +
			"Name the repositories under `repositories:` instead"
	default:
		return err.Error()
	}
}
