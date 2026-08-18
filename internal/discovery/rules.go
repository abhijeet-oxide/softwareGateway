package discovery

import (
	"fmt"
	"regexp"

	"github.com/abhijeet-oxide/softwareGateway/internal/download"
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

// resolvedStep is one hop of a rule's chain, with its catalog row resolved.
type resolvedStep struct {
	Name      string
	RepoID    int64
	Index     int
	DependsOn string // the NAME of the step this one waits for
}

// resolveChain closes a rule's destinations over `mirror.from`, orders them,
// and resolves each to a catalog row.
//
// A rule names the destinations somebody cares about; the ordering comes from
// the targets' own configuration. The derivation itself lives in
// internal/download so that the CLI's dry run and this path cannot disagree
// about what a rule would do — a preview computed by different code from the
// act is a preview nobody should trust.
func resolveChain(p *product.Product, rule product.Rule, repoIDs map[string]int64) ([]resolvedStep, error) {
	chain, err := download.Derive(p, rule.Targets)
	if err != nil {
		return nil, fmt.Errorf("rule %q: %w", rule.Name, err)
	}

	out := make([]resolvedStep, 0, len(chain.Steps))
	for _, step := range chain.Steps {
		target, ok := p.Target(step.Target)
		if !ok {
			return nil, fmt.Errorf("rule %q names unknown target %q", rule.Name, step.Target)
		}
		if target.PromotionOnly {
			return nil, fmt.Errorf(
				"rule %q reaches target %q, which is promotionOnly: it can only be reached by promotion from another target",
				rule.Name, step.Target)
		}
		id, ok := repoIDs[step.Target]
		if !ok {
			return nil, fmt.Errorf("target %q has no catalog row: reconciliation has not run", step.Target)
		}
		out = append(out, resolvedStep{
			Name: step.Target, RepoID: id, Index: step.Index, DependsOn: step.From,
		})
	}
	return out, nil
}

// stepNames renders a chain for a log line and an audit record.
func stepNames(steps []resolvedStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

// stepIDs is the repository rows a chain touches, for the idempotency key.
func stepIDs(steps []resolvedStep) []int64 {
	out := make([]int64, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.RepoID)
	}
	return out
}
