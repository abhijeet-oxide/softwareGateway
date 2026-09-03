package pipeline_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/pipeline"
	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// Resolution is on the interface's hot path, so it is held to hot-path rules.
//
// Every release page, every package row that shows what can be done to it, and
// every action request re-derives the route. On a busy estate that is thousands
// of resolutions a minute while transfers are saturating the workers - so the
// two properties here are that it is safe to call from many goroutines at once,
// and that it stays cheap as a product grows.
//
// The reason it CAN be held to those rules is that it takes two documents and
// returns a value. It reads no store, opens no connection and takes no lock, so
// a page render cannot be made slow by a busy queue - which is the whole point
// of keeping this a package of pure functions.
//
// # The allocation these benchmarks caught
//
// A Target is 280 bytes, and TargetsInStage sized its result to the number of
// targets in the PRODUCT rather than the number in the stage - so a product with
// three stages of a dozen targets allocated capacity for thirty-six of them
// three times over, on every render. HasStage then built the whole matching set
// and threw it away to read its length.
//
//	                        before   after
//	BenchmarkResolve        7350ns   3537ns
//	                       18576 B    4752 B
//	                      8 allocs  6 allocs
//
// Neither was visible as a slow page. Both were visible here, which is what
// these benchmarks are for.

// bigProduct is a product at the top of the range anyone has: five stages, and
// a stage that fans out to a dozen mirrors.
func bigProduct(targetsPerStage int) *product.Product {
	stages := []string{"external", "lab", "prod"}
	targets := make([]product.Target, 0, len(stages)*targetsPerStage)
	for _, stage := range stages {
		for i := range targetsPerStage {
			targets = append(targets, product.Target{
				Name:       fmt.Sprintf("%s-%d", stage, i),
				Stage:      stage,
				Registry:   "registry.example.com",
				Repository: fmt.Sprintf("p/%s/%d", stage, i),
			})
		}
	}
	return productWith(targets...)
}

// Concurrent resolution must be race-free. Run with -race, where this is the
// assertion; without it, it still catches a panic under contention.
func TestResolveIsSafeUnderConcurrency(t *testing.T) {
	const goroutines = 64
	const each = 200

	tasks := siteTasks()
	p := bigProduct(4)

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				pl := pipeline.Resolve(tasks, p)
				if len(pl.Steps) != 3 {
					t.Errorf("steps = %d, want 3", len(pl.Steps))
					return
				}
				for _, stage := range []string{"", "external", "lab", "prod"} {
					_ = pl.Actions(pipeline.Location{Stage: stage})
				}
			}
		}()
	}
	wg.Wait()
}

// Resolution must not be affected by anything shared: two callers resolving
// different products at the same time have to get their own answers.
//
// The failure this catches is a step holding a slice that aliases the caller's
// task list - the kind of sharing that only shows up when two products with
// different routes are rendered at the same moment.
func TestConcurrentResolutionOfDifferentProductsDoesNotCross(t *testing.T) {
	const goroutines = 32

	full := bigProduct(2)
	collapsed := productWith(target("lab", "lab"))

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*2)

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := pipeline.Resolve(siteTasks(), full).Describe(); got != "source → external → lab → prod" {
				errs <- fmt.Errorf("full product resolved to %q", got)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := pipeline.Resolve(siteTasks(), collapsed).Describe(); got != "source → lab" {
				errs <- fmt.Errorf("collapsed product resolved to %q", got)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

// Resolving must not mutate the task list it was given.
//
// It normalizes check modes, and a normalization written back into the caller's
// slice would silently rewrite the site's configuration in memory - so the
// second product to be resolved would see modes the file never contained.
func TestResolveDoesNotMutateTheSiteTasks(t *testing.T) {
	tasks := siteTasks()
	tasks[1].Verify = "" // unset; Resolve normalizes it to disabled

	pipeline.Resolve(tasks, bigProduct(1))

	if tasks[1].Verify != "" {
		t.Errorf("Resolve rewrote the caller's task list: verify = %q, want it untouched",
			tasks[1].Verify)
	}
}

// A page render's worth of resolution has to stay in the microseconds, because
// it happens per row and the row count is the size of somebody's catalogue.
//
// The bound is deliberately loose - a hundred microseconds is thirty times what
// this measures on an idle machine - because the assertion worth keeping is
// "this is not doing I/O", and anything doing I/O misses it by orders of
// magnitude rather than by a factor.
func TestResolutionStaysCheapEnoughForAPageRender(t *testing.T) {
	tasks := siteTasks()
	p := bigProduct(12) // 36 targets: a fan-out nobody has yet

	const iterations = 1000
	start := time.Now()
	for range iterations {
		pl := pipeline.Resolve(tasks, p)
		_ = pl.Actions(pipeline.Location{Stage: "external"})
	}
	per := time.Since(start) / iterations

	if per > 100*time.Microsecond {
		t.Errorf("resolve+actions took %s each, want under 100µs - "+
			"something on this path is doing more than reading two documents", per)
	}
	t.Logf("resolve+actions over %d targets: %s each", len(p.Spec.Targets), per)
}

func BenchmarkResolve(b *testing.B) {
	tasks := siteTasks()
	p := bigProduct(4)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = pipeline.Resolve(tasks, p)
	}
}

func BenchmarkResolveAndActions(b *testing.B) {
	tasks := siteTasks()
	p := bigProduct(4)
	loc := pipeline.Location{Stage: "external"}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = pipeline.Resolve(tasks, p).Actions(loc)
	}
}

// The validation every startup runs, over a vocabulary the size a site would
// ever write.
func BenchmarkValidate(b *testing.B) {
	tasks := make([]config.Task, 0, 8)
	tasks = append(tasks, config.Task{Name: "t0", From: config.SourceStage, To: "s0"})
	for i := 1; i < 8; i++ {
		tasks = append(tasks, config.Task{
			Name: fmt.Sprintf("t%d", i),
			From: fmt.Sprintf("s%d", i-1),
			To:   fmt.Sprintf("s%d", i),
		})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = config.ValidateTasks(tasks)
	}
}
