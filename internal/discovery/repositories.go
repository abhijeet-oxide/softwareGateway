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
	// A source that enumerates has nothing else to fall back on, so this is
	// where the scan's failure comes from. The message matters: a vendor
	// forbidding `_catalog` is common — the credential is usually good for
	// pulling a named repository and not for enumerating the registry — and
	// the fix is to name the repositories, which is what describeCatalogError
	// says.
	CatalogErr error
}

// resolveRepositories determines which repositories a source should scan.
//
// One question decides it: did configuration name any?
//
//	repositories named   ─▶ scan exactly those
//	none named           ─▶ every repository on the registry,
//	                        narrowed by discovery.repositoryFilters
//
// There is no separate "enable discovery" switch. Naming nothing IS the
// statement "I do not know them yet, find them" — which is the case that
// matters, because a product whose components each ship as a new repository
// cannot list them in advance.
//
// Filters are applied either way, so there is one rule rather than one per
// origin. Their real use is the enumerated case, where they are what keeps a
// shared registry's other tenants out of scope.
func resolveRepositories(
	ctx context.Context, src product.Source, f filter, catalog CatalogLister,
) RepositorySet {
	var res RepositorySet

	candidates := src.DeclaredRepositories()
	declared := len(candidates)

	if src.EnumeratesRepositories() {
		// A source that names no repositories has nothing to scan without
		// enumeration, so a missing catalog client is a wiring failure, not a
		// condition to skip past.
		//
		// The guard here used to be `&& catalog != nil`, which silently produced
		// zero repositories: the scan then returned success, in under a
		// millisecond, having made no network call at all, and `packages
		// discover` reported "Nothing new" for a source nothing had looked at.
		if catalog == nil {
			res.CatalogErr = errors.New(
				"this source names no repositories, so they must come from the registry's " +
					"catalog, but no catalog client was built for it")
		} else {
			maxRepos := src.Discovery.EffectiveMaxRepositories()
			found, err := catalog.ListAllRepositories(ctx, maxRepos)
			if err != nil {
				res.CatalogErr = err
			} else {
				candidates = append(candidates, found...)
				if len(found) >= maxRepos {
					res.Truncated = true
				}
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
		return "the registry refused to list its repositories: this credential is " +
			"probably scoped to pulling named repositories rather than enumerating " +
			"the registry. Name them under `repositories:` instead"
	case errors.Is(err, registry.ErrNotFound):
		return "the registry does not implement /v2/_catalog, so its repositories " +
			"cannot be discovered. Name them under `repositories:` instead"
	default:
		return err.Error()
	}
}
