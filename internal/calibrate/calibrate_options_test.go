package calibrate

import (
	"slices"
	"testing"
	"time"
)

// A run that probes nothing must not be reachable by passing a bad option: the
// report it produces is an empty table with no explanation, which reads as a
// broken path rather than as a bad request.
func TestOptionsFallBackToTheDefaultSweep(t *testing.T) {
	got := Options{Levels: []int{0, -3}}.withDefaults()
	if !slices.Equal(got.Levels, DefaultLevels()) {
		t.Errorf("levels = %v, want the default sweep %v", got.Levels, DefaultLevels())
	}
	if got.Budget != DefaultBudget {
		t.Errorf("budget = %s, want %s", got.Budget, DefaultBudget)
	}

	// A real sweep is kept, and sorted: the plateau check compares each level
	// with the one below it, which is meaningless out of order.
	got = Options{Levels: []int{8, 1, 4}, Budget: time.Second}.withDefaults()
	if !slices.Equal(got.Levels, []int{1, 4, 8}) {
		t.Errorf("levels = %v, want them ascending", got.Levels)
	}
}
