// Package statemachine provides the transition guard used by every stateful
// entity in the system.
//
// See docs/design/10-state-machines.md section 1. The rule the package exists
// to enforce: there is no `UPDATE ... SET state = ...` anywhere else in the
// codebase. Every transition goes through Machine.Apply, which rejects any
// (from, event) pair not present in its table rather than falling through to a
// default or silently no-opping.
package statemachine

import (
	"errors"
	"fmt"
)

// ErrIllegalTransition is returned when (from, event) is not in the table.
// Callers surface it as 409 FAILED_PRECONDITION with the current state.
// See docs/design/09-api.md section 8.
var ErrIllegalTransition = errors.New("illegal state transition")

// Transition is one edge: applying Event while in From yields To.
type Transition[S ~string, E ~string] struct {
	From  S
	Event E
	To    S
}

// Machine is a transition table for one entity's lifecycle.
// Safe for concurrent use once built: Apply performs only map reads.
type Machine[S ~string, E ~string] struct {
	name     string
	edges    map[key[S, E]]S
	states   map[S]bool
	terminal map[S]bool
}

type key[S ~string, E ~string] struct {
	from  S
	event E
}

// New builds a machine from its transitions. Terminal states are those a
// transition may never leave; they are recorded so IsTerminal can answer
// without scanning the table, and so tests can assert the terminal set matches
// the design document.
func New[S ~string, E ~string](name string, terminal []S, transitions ...Transition[S, E]) *Machine[S, E] {
	m := &Machine[S, E]{
		name:     name,
		edges:    make(map[key[S, E]]S, len(transitions)),
		states:   make(map[S]bool),
		terminal: make(map[S]bool, len(terminal)),
	}
	for _, s := range terminal {
		m.terminal[s] = true
		m.states[s] = true
	}
	for _, t := range transitions {
		m.edges[key[S, E]{t.From, t.Event}] = t.To
		m.states[t.From] = true
		m.states[t.To] = true
	}
	return m
}

// Apply returns the state reached by applying event in state from.
//
// It never falls through to a default and never returns `from` unchanged on an
// unknown event: an unmodelled transition is an error, because the alternative
// is an UPDATE that quietly rewrites a completed record.
func (m *Machine[S, E]) Apply(from S, event E) (S, error) {
	to, ok := m.edges[key[S, E]{from, event}]
	if !ok {
		var zero S
		return zero, fmt.Errorf("%w: %s cannot %q from %q", ErrIllegalTransition, m.name, event, from)
	}
	return to, nil
}

// Can reports whether the transition is legal, without computing it.
func (m *Machine[S, E]) Can(from S, event E) bool {
	_, ok := m.edges[key[S, E]{from, event}]
	return ok
}

// IsTerminal reports whether the state is terminal. Note that `failed` is
// deliberately NOT terminal in most machines — it is retryable, which is the
// point of modelling retry as an explicit event.
func (m *Machine[S, E]) IsTerminal(s S) bool { return m.terminal[s] }

// Knows reports whether the state appears anywhere in the table. Used by tests
// to assert that the CHECK constraints in the migrations enumerate exactly the
// states the machine defines, and no others.
func (m *Machine[S, E]) Knows(s S) bool { return m.states[s] }

// States returns every state in the table. Order is not defined.
func (m *Machine[S, E]) States() []S {
	out := make([]S, 0, len(m.states))
	for s := range m.states {
		out = append(out, s)
	}
	return out
}

// Name identifies the machine in error messages.
func (m *Machine[S, E]) Name() string { return m.name }
