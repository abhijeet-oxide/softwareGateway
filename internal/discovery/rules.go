package discovery

import (
	"fmt"
	"regexp"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// ruleSet is a product's auto-download rules, compiled once.
type ruleSet struct {
	enabled bool
	rules   []compiledRule
}

type compiledRule struct {
	rule    product.Rule
	pattern *regexp.Regexp
}

// compileRules compiles a product's auto-download rules.
func compileRules(p *product.Product) (ruleSet, error) {
	set := ruleSet{enabled: p.DownloadEnabled()}
	for _, r := range p.DownloadRules() {
		re, err := regexp.Compile(r.TagPattern)
		if err != nil {
			return ruleSet{}, fmt.Errorf("download rule %q pattern %q: %w", r.Name, r.TagPattern, err)
		}
		set.rules = append(set.rules, compiledRule{rule: r, pattern: re})
	}
	return set, nil
}

// match returns the first rule matching a tag.
//
// A rule that is disabled, or that does not accept the discovery trigger, is
// skipped WITHOUT consuming the match — so a disabled `ga-releases` lets a
// later rule see the tag rather than silently swallowing it. First-match-wins
// is about which rule applies, not about which rule looked.
//
// FIRST MATCH WINS, in configured order. Not all-match: two rules matching one
// tag with different priorities and different targets has no sensible
// interpretation, and picking "the most specific" would require a specificity
// order over regexes that does not exist (docs/design/02 §5.4).
func (s ruleSet) match(tag string) (product.Rule, bool) {
	if !s.enabled {
		return product.Rule{}, false
	}
	for _, c := range s.rules {
		if !c.pattern.MatchString(tag) {
			continue
		}
		if !c.rule.IsEnabled() || !c.rule.TriggeredBy(product.TriggerDiscovery) {
			continue
		}
		return c.rule, true
	}
	return product.Rule{}, false
}
