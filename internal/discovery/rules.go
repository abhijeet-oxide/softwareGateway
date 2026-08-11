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
func compileRules(a product.AutoDownload) (ruleSet, error) {
	set := ruleSet{enabled: a.Enabled}
	for _, r := range a.Rules {
		re, err := regexp.Compile(r.TagPattern)
		if err != nil {
			return ruleSet{}, fmt.Errorf("autoDownload rule %q pattern %q: %w", r.Name, r.TagPattern, err)
		}
		set.rules = append(set.rules, compiledRule{rule: r, pattern: re})
	}
	return set, nil
}

// match returns the first rule matching a tag.
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
		if c.pattern.MatchString(tag) {
			return c.rule, true
		}
	}
	return product.Rule{}, false
}

// requestID derives a stable request ID from the idempotency key.
//
// Stable rather than random: a retry that races the unique constraint would
// otherwise insert with a different primary key and rely on the constraint to
// reject it. Deriving the ID means the two attempts are byte-identical.
func requestID(key string) string {
	if len(key) < 32 {
		return key
	}
	return "req_" + key[:32]
}

// resolveTargets maps a rule's target names to repository row IDs.
//
// A rule naming no targets uses the product's default target. A product with
// several targets and no default is a configuration error caught at load, so
// reaching here without a resolvable target means the rule names something that
// does not exist.
func resolveTargets(p *product.Product, rule product.Rule, repoIDs map[string]int64) ([]int64, []string, error) {
	names := rule.Targets
	if len(names) == 0 {
		def, ok := p.DefaultTarget()
		if !ok {
			return nil, nil, fmt.Errorf(
				"rule %q names no targets and product %q has no default target",
				rule.Name, p.Metadata.Name)
		}
		names = []string{def.Name}
	}

	ids := make([]int64, 0, len(names))
	for _, name := range names {
		target, ok := p.Target(name)
		if !ok {
			return nil, nil, fmt.Errorf("rule %q names unknown target %q", rule.Name, name)
		}
		if target.PromotionOnly {
			return nil, nil, fmt.Errorf(
				"rule %q names target %q, which is promotionOnly: it can only be reached by promotion from another target",
				rule.Name, name)
		}
		id, ok := repoIDs[name]
		if !ok {
			return nil, nil, fmt.Errorf("target %q has no catalog row: reconciliation has not run", name)
		}
		ids = append(ids, id)
	}
	return ids, names, nil
}
