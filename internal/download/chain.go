// Package download derives what a download rule actually does.
//
// A rule names the DESTINATIONS somebody cares about. The order between them
// is not in the rule and never will be: it comes from the targets' own
// `replication.mirror.from`, which is where the chain jfrog-store → ocp-prod is
// already declared. Writing it a second time in the rule would mean that on the
// day the two disagreed, one of them would be silently authoritative.
//
// See docs/design/20 §3.5 and §4.
package download

import (
	"fmt"
	"sort"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// Step is one hop of a chain.
type Step struct {
	// Target is the configured target this step writes to, or asks a registry
	// to fill.
	Target string

	// Index is the step's position. Steps sharing an index have no dependency
	// between them and run concurrently — which is how fan-out and chaining
	// end up being the same feature rather than two.
	Index int

	// From is the target this one pulls from, empty when it pulls from the
	// product's sources. It is the edge the ordering came from.
	From string

	// Delegated reports whether the registry moves the bytes for this step.
	Delegated bool

	// NamedByRule distinguishes a destination the rule asked for from a hop
	// pulled in by the closure. Both are planned; only the first was typed,
	// and an error about the second has to say which target dragged it in.
	NamedByRule bool
}

// Chain is a rule's destinations, closed over `mirror.from` and ordered.
type Chain struct {
	Steps []Step
}

// Targets returns every target in the chain, in execution order.
func (c Chain) Targets() []string {
	out := make([]string, 0, len(c.Steps))
	for _, s := range c.Steps {
		out = append(out, s.Target)
	}
	return out
}

// MaxIndex is the highest step index, which is the depth of the deepest chain.
func (c Chain) MaxIndex() int {
	max := 0
	for _, s := range c.Steps {
		if s.Index > max {
			max = s.Index
		}
	}
	return max
}

// Predecessor returns the step a given target waits for.
func (c Chain) Predecessor(target string) (Step, bool) {
	for _, s := range c.Steps {
		if s.Target != target || s.From == "" {
			continue
		}
		for _, p := range c.Steps {
			if p.Target == s.From {
				return p, true
			}
		}
	}
	return Step{}, false
}

// Describe renders the chain the way it is shown wherever a rule is displayed.
//
// The chain is implicit to TYPE and never implicit to read: every place a rule
// appears — `rules describe`, a dry run, the UI row — shows this.
func (c Chain) Describe() string {
	if len(c.Steps) == 0 {
		return "nothing"
	}
	byIndex := map[int][]string{}
	for _, s := range c.Steps {
		byIndex[s.Index] = append(byIndex[s.Index], s.Target)
	}
	indexes := make([]int, 0, len(byIndex))
	for i := range byIndex {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)

	out := ""
	for n, i := range indexes {
		if n > 0 {
			out += " → "
		}
		names := byIndex[i]
		sort.Strings(names)
		for j, name := range names {
			if j > 0 {
				out += " + "
			}
			out += name
		}
	}
	return out
}

// Derive closes a rule's destinations over `mirror.from` and orders them.
//
// Because `mirror.from` names exactly ONE upstream, the graph is a forest and a
// cycle is the only structural error possible — which is what makes the sort
// six lines instead of a general topological sort with a worklist.
func Derive(p *product.Product, named []string) (Chain, error) {
	declared := make(map[string]product.Target, len(p.Spec.Targets))
	for _, t := range p.Spec.Targets {
		declared[t.Name] = t
	}

	if len(named) == 0 {
		def, ok := p.DefaultTarget()
		if !ok {
			return Chain{}, fmt.Errorf(
				"the rule names no targets and product %q has no default target", p.Metadata.Name)
		}
		named = []string{def.Name}
	}

	// Closure. `from` is followed transitively, so naming the tail of a chain
	// names the chain.
	inChain := map[string]bool{}
	fromOf := map[string]string{}
	namedSet := map[string]bool{}
	order := []string{}

	for _, name := range named {
		namedSet[name] = true
		for cur := name; cur != ""; {
			if inChain[cur] {
				break
			}
			t, ok := declared[cur]
			if !ok {
				return Chain{}, fmt.Errorf("target %q is not declared in product %q", cur, p.Metadata.Name)
			}
			inChain[cur] = true
			order = append(order, cur)

			m, isMirror := t.MirrorSettings()
			if !isMirror || m.From == "" {
				break
			}
			fromOf[cur] = m.From
			cur = m.From
		}
	}

	// Depth. A step's index is how many hops separate it from a target that
	// pulls from the product's sources, so a target with no `from` is index 0
	// and everything that waits on it is one higher.
	depth := map[string]int{}
	var resolve func(name string, seen map[string]bool) (int, error)
	resolve = func(name string, seen map[string]bool) (int, error) {
		if d, ok := depth[name]; ok {
			return d, nil
		}
		if seen[name] {
			return 0, fmt.Errorf(
				"targets form a mirror.from cycle through %q; a chain has to end at a target something actually writes to", name)
		}
		seen[name] = true

		from, ok := fromOf[name]
		if !ok || !inChain[from] {
			// Either it pulls from the sources, or its upstream is outside the
			// chain — an `externalReference`, or a hop another rule fills. Both
			// mean nothing in THIS chain precedes it.
			depth[name] = 0
			return 0, nil
		}
		d, err := resolve(from, seen)
		if err != nil {
			return 0, err
		}
		depth[name] = d + 1
		return depth[name], nil
	}

	chain := Chain{}
	for _, name := range order {
		d, err := resolve(name, map[string]bool{})
		if err != nil {
			return Chain{}, err
		}
		from := fromOf[name]
		if !inChain[from] {
			// Recorded as no predecessor, because a chain cannot wait on
			// something it does not contain.
			from = ""
		}
		chain.Steps = append(chain.Steps, Step{
			Target:      name,
			Index:       d,
			From:        from,
			Delegated:   declared[name].Delegated(),
			NamedByRule: namedSet[name],
		})
	}

	// Sorted by index, then by name so the plan is stable — a dry run that
	// reordered between two identical invocations would be unreviewable.
	sort.SliceStable(chain.Steps, func(i, j int) bool {
		if chain.Steps[i].Index != chain.Steps[j].Index {
			return chain.Steps[i].Index < chain.Steps[j].Index
		}
		return chain.Steps[i].Target < chain.Steps[j].Target
	})
	return chain, nil
}
