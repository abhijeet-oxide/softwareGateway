package cel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	celgo "cel.dev/cel-go/cel"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// Compiler turns declared checks into evaluable programs.
//
// One per catalogue. The environment is built once and every check is compiled
// against it at load, which is the property that matters: a mistyped field, an
// unknown function or a condition that is not a boolean fails in front of the
// person editing the pack, named with its check and its column, rather than on
// the release where the branch is first taken.
type Compiler struct {
	env *celgo.Env
}

// NewCompiler builds the compiler and its environment.
func NewCompiler() (*Compiler, error) {
	env, err := NewEnv()
	if err != nil {
		return nil, fmt.Errorf("building the expression environment: %w", err)
	}
	return &Compiler{env: env}, nil
}

var _ compliance.Compiler = (*Compiler)(nil)

// Compile turns one check into a Program, reporting every problem it can see.
func (c *Compiler) Compile(check compliance.Check) (compliance.Program, error) {
	p := &program{check: check, env: c.env}

	if w := check.AppliesTo.Where; w != "" {
		ast, err := c.compileBool(w)
		if err != nil {
			return nil, fmt.Errorf("appliesTo.where: %w", err)
		}
		p.applies = ast
	}

	built, err := buildAssert(check.Assert)
	if err != nil {
		return nil, err
	}
	p.expected = built.Expected
	p.locus = built.Locus
	p.source = built.Source

	if p.assert, err = c.compileBool(built.Source); err != nil {
		// The generated source is included because for a shorthand check the
		// author never wrote it, and an error naming a column in a string they
		// have not seen is not an error they can act on.
		return nil, fmt.Errorf("assert: %w\n  compiled to: %s", err, built.Source)
	}

	// Per-term programs let a failure say which of four required paths is the
	// missing one. Compiled here so a broken term is a load error like any
	// other.
	for _, t := range built.Terms {
		ct := compiledTerm{locus: t.Locus, expected: t.Expected}
		if ct.cond, err = c.compileBool(t.Source); err != nil {
			return nil, fmt.Errorf("assert term %q: %w", t.Source, err)
		}
		if t.Observed != "" {
			if ct.observed, err = c.compileString(t.Observed); err != nil {
				return nil, fmt.Errorf("observed for %q: %w", t.Locus, err)
			}
		}
		p.terms = append(p.terms, ct)
	}

	if check.Assert.Observed != "" {
		if p.observed, err = c.compileString(check.Assert.Observed); err != nil {
			return nil, fmt.Errorf("observed: %w", err)
		}
	}
	if check.Assert.Message != "" {
		if p.message, err = c.compileString(check.Assert.Message); err != nil {
			return nil, fmt.Errorf("message: %w", err)
		}
	}
	if sup := check.Assert.SupersededBy; sup != nil {
		if p.superseded, err = c.compileBool(sup.When); err != nil {
			return nil, fmt.Errorf("supersededBy.when: %w", err)
		}
	}
	return p, nil
}

// compileBool compiles a condition and refuses one that is not a boolean.
//
// A check whose assertion returns a string is an author who wrote a value where
// a condition belongs; under CEL's truthiness rules that would not be an error
// at all, and the check would pass everything it applied to while reading on
// the catalogue page as a rule being enforced. Refusing it at load is the
// difference between a typo and a silent hole in the baseline.
func (c *Compiler) compileBool(src string) (*celgo.Ast, error) {
	ast, err := c.compile(src)
	if err != nil {
		return nil, err
	}
	if ast.OutputType() != celgo.BoolType && ast.OutputType() != celgo.DynType {
		return nil, fmt.Errorf("expression is a %s, not a condition: an assertion must evaluate to true or false", ast.OutputType())
	}
	return ast, nil
}

// compileString compiles an expression used for a message or an observed value.
func (c *Compiler) compileString(src string) (*celgo.Ast, error) {
	ast, err := c.compile(src)
	if err != nil {
		return nil, err
	}
	switch ast.OutputType() {
	case celgo.StringType, celgo.DynType:
		return ast, nil
	default:
		return nil, fmt.Errorf("expression is a %s; wrap it in string()", ast.OutputType())
	}
}

func (c *Compiler) compile(src string) (*celgo.Ast, error) {
	ast, issues := c.env.Compile(src)
	if issues != nil && issues.Err() != nil {
		return nil, cleanIssues(issues.Err())
	}
	return ast, nil
}

// cleanIssues makes cel-go's multi-line diagnostics readable in a log line and
// in an API field, without discarding the column markers that make them useful.
func cleanIssues(err error) error {
	msg := strings.TrimSpace(err.Error())
	msg = strings.TrimPrefix(msg, "ERROR: <input>:")
	return errors.New(strings.ReplaceAll(msg, "\n", "\n  "))
}

// compiledTerm is one assertion of a check, with the expressions that describe
// it when it fails.
type compiledTerm struct {
	cond     *celgo.Ast
	observed *celgo.Ast
	locus    string
	expected string
}

// program is one compiled check.
//
// # Why the planned programs are per run and the ASTs are not
//
// Compilation - parse and type check - is where every error a person can fix
// lives, and it happens once, at load. Planning binds the engine functions to
// one run's resource index, because "which PodDisruptionBudget selects this
// workload" has no answer outside a release. Planning is cheap and produces no
// new errors of the kind an author cares about, so doing it per run costs a few
// hundred microseconds and buys load-time diagnostics.
type program struct {
	check compliance.Check
	env   *celgo.Env

	applies    *celgo.Ast
	assert     *celgo.Ast
	observed   *celgo.Ast
	message    *celgo.Ast
	superseded *celgo.Ast
	terms      []compiledTerm

	source   string
	expected string
	locus    string

	mu      sync.Mutex
	boundTo *compliance.Index
	planned *plannedPrograms
}

type plannedPrograms struct {
	applies    celgo.Program
	assert     celgo.Program
	observed   celgo.Program
	message    celgo.Program
	superseded celgo.Program
	terms      []plannedTerm
}

type plannedTerm struct {
	cond     celgo.Program
	observed celgo.Program
	locus    string
	expected string
}

// Source is the CEL this check compiled to, shown in the API so an author can
// see what their YAML became.
func (p *program) Source() string { return p.source }

func (p *program) plan(idx *compliance.Index) (*plannedPrograms, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.planned != nil && p.boundTo == idx {
		return p.planned, nil
	}
	runtime, err := newRuntimeEnv(idx)
	if err != nil {
		return nil, err
	}
	opts := programOptions()
	out := &plannedPrograms{}
	mk := func(ast *celgo.Ast) (celgo.Program, error) {
		if ast == nil {
			return nil, nil
		}
		return runtime.Program(ast, opts...)
	}
	if out.applies, err = mk(p.applies); err != nil {
		return nil, err
	}
	if out.assert, err = mk(p.assert); err != nil {
		return nil, err
	}
	if out.observed, err = mk(p.observed); err != nil {
		return nil, err
	}
	if out.message, err = mk(p.message); err != nil {
		return nil, err
	}
	if out.superseded, err = mk(p.superseded); err != nil {
		return nil, err
	}
	for _, t := range p.terms {
		pt := plannedTerm{locus: t.locus, expected: t.expected}
		if pt.cond, err = mk(t.cond); err != nil {
			return nil, err
		}
		if pt.observed, err = mk(t.observed); err != nil {
			return nil, err
		}
		out.terms = append(out.terms, pt)
	}
	p.boundTo, p.planned = idx, out
	return out, nil
}

// Applies evaluates the check's `where` predicate.
func (p *program) Applies(ctx context.Context, subj compliance.Subject, idx *compliance.Index) (bool, error) {
	planned, err := p.plan(idx)
	if err != nil {
		return false, err
	}
	if planned.applies == nil {
		return true, nil
	}
	act := activation(subj, idx.Release())
	v, _, err := planned.applies.ContextEval(ctx, act)
	if err != nil {
		return false, err
	}
	return asBool(v.Value())
}

// Evaluate judges one subject.
func (p *program) Evaluate(ctx context.Context, subj compliance.Subject, idx *compliance.Index) (compliance.Judgement, error) {
	planned, err := p.plan(idx)
	if err != nil {
		return compliance.Judgement{}, err
	}
	act := activation(subj, idx.Release())

	v, _, err := planned.assert.ContextEval(ctx, act)
	if err != nil {
		// An expression that faulted has not shown non-compliance. Reporting it
		// as a failure would send a vendor after a defect in their chart that
		// is really a defect in the check.
		return compliance.Judgement{Err: fmt.Errorf("evaluating %s: %w", p.check.ID, err)}, nil
	}
	ok, err := asBool(v.Value())
	if err != nil {
		return compliance.Judgement{Err: err}, nil
	}

	j := compliance.Judgement{Compliant: ok, Expected: p.expected, Locus: p.locus}
	if ok {
		// A check that exists to report what is there, rather than to reject
		// it, says so on a pass as well. See Assert.ObserveOnPass.
		if p.check.Assert.ObserveOnPass && planned.observed != nil {
			if ov, _, oerr := planned.observed.ContextEval(ctx, act); oerr == nil {
				j.Observed = str(ov.Value())
			}
		}
		return j, nil
	}

	// A failure another check already owns is stood down here rather than
	// reported: acting on it would change nothing until that one is fixed. The
	// engine records it as a skip naming the other check, so it stays in the
	// full record and out of the list of things to do.
	if planned.superseded != nil {
		sv, _, serr := planned.superseded.ContextEval(ctx, act)
		if serr == nil {
			if b, berr := asBool(sv.Value()); berr == nil && b {
				j.SupersededBy = p.check.Assert.SupersededBy.Check
				j.SupersededBecause = p.check.Assert.SupersededBy.Because
				return j, nil
			}
		}
	}

	// Find the term that actually failed, so a check with four required paths
	// names the missing one rather than restating its own title.
	for _, t := range planned.terms {
		tv, _, terr := t.cond.ContextEval(ctx, act)
		if terr != nil {
			continue
		}
		if b, berr := asBool(tv.Value()); berr == nil && b {
			continue
		}
		if t.locus != "" {
			j.Locus = t.locus
		}
		if t.expected != "" {
			j.Expected = t.expected
		}
		if t.observed != nil {
			if ov, _, oerr := t.observed.ContextEval(ctx, act); oerr == nil {
				j.Observed = str(ov.Value())
			}
		}
		break
	}

	// An author-supplied observed or message wins: they know what matters about
	// their own check.
	if planned.observed != nil {
		if ov, _, oerr := planned.observed.ContextEval(ctx, act); oerr == nil {
			j.Observed = str(ov.Value())
		}
	}
	if planned.message != nil {
		if mv, _, merr := planned.message.ContextEval(ctx, act); merr == nil {
			j.Message = str(mv.Value())
		}
	}
	if j.Message == "" {
		j.Message = p.defaultMessage(subj, j)
	}
	return j, nil
}

// defaultMessage writes the sentence a vendor reads when the author did not.
//
// It names the subject, the field and both values, because "PDB-02 failed" sends
// somebody back to this tool and "StatefulSet etcd: spec.minAvailable is 1,
// expected >= 2" does not.
func (p *program) defaultMessage(subj compliance.Subject, j compliance.Judgement) string {
	var b strings.Builder
	b.WriteString(subj.Describe())
	b.WriteString(": ")
	if j.Locus != "" {
		b.WriteString(j.Locus)
	}
	// "is X, expected Y" reads correctly for a value and badly for a phrase,
	// and an author's observed expression may legitimately return either. A
	// dash joins both without asserting a grammar.
	if j.Observed != "" {
		if j.Locus != "" {
			b.WriteString(" — ")
		}
		b.WriteString(j.Observed)
	}
	if j.Expected != "" {
		if j.Locus != "" || j.Observed != "" {
			b.WriteString("; expected ")
		} else {
			b.WriteString("expected ")
		}
		b.WriteString(j.Expected)
	}
	if j.Observed == "" && j.Expected == "" {
		b.WriteString(" does not satisfy ")
		b.WriteString(p.check.Title)
	}
	return b.String()
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return scalarString(v)
}
