package discovery

import (
	"fmt"
	"regexp"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// tagFilter decides which tags a scan looks at.
//
// Applied BEFORE any ResolveTag, so filtering bounds scan cost as well as
// scope. That is the stated mitigation for a repository with tens of thousands
// of tags (docs/design/07 §3).
type tagFilter struct {
	include []*regexp.Regexp
	exclude []*regexp.Regexp
}

// compileTagFilter compiles a source's filters once, at loop start.
//
// Compiling per scan would recompile the same patterns every fifteen minutes
// forever, and — worse — would turn a configuration error into a recurring
// runtime failure rather than one loud complaint at startup.
//
// Patterns are RE2 (Go regexp): linear time, no backtracking. A user-supplied
// pattern evaluated inside a polling loop would, under a backtracking engine,
// be a denial-of-service vector (docs/design/02 §5.4).
func compileTagFilter(f product.TagFilters) (tagFilter, error) {
	var out tagFilter

	for _, p := range f.Include {
		re, err := regexp.Compile(p)
		if err != nil {
			return tagFilter{}, fmt.Errorf("tagFilters.include %q: %w", p, err)
		}
		out.include = append(out.include, re)
	}
	for _, p := range f.Exclude {
		re, err := regexp.Compile(p)
		if err != nil {
			return tagFilter{}, fmt.Errorf("tagFilters.exclude %q: %w", p, err)
		}
		out.exclude = append(out.exclude, re)
	}
	return out, nil
}

// admits reports whether a tag survives the filter.
//
// Include first, then exclude. No include patterns means everything is
// included; exclude always wins over include, so a narrow carve-out can be
// expressed without rewriting the include list.
func (f tagFilter) admits(tag string) bool {
	if len(f.include) > 0 {
		matched := false
		for _, re := range f.include {
			if re.MatchString(tag) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, re := range f.exclude {
		if re.MatchString(tag) {
			return false
		}
	}
	return true
}
