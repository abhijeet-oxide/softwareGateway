package source_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/source"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Concurrency is an optimisation, and an optimisation that can change an answer
// is a defect. These are the tests that say it cannot.
//
// The failure they exist to catch is subtle and would be believed for months:
// charts are fetched by several workers, the results are appended as they land,
// and the report's chart order - and with it every result's seq, and with that
// which chart the evidence budget runs out on - becomes a function of which
// download was quickest. Two runs of the same bytes would then differ, and rule
// 5 is the rule the whole feature rests on.

// barrierBlobs proves both halves of the claim, deterministically.
//
// Every request waits at a barrier until ALL of them have arrived, so the fetch
// is concurrent or the test deadlocks - there is no timing window in which a
// serial fetch quietly passes. They are then released in REVERSE, so the
// registry answers last-requested-first and a fetch that appended results as
// they landed would produce its charts backwards.
type barrierBlobs struct {
	bodies  map[string][]byte
	index   map[string]int
	arrive  sync.WaitGroup
	release []chan struct{}

	mu    sync.Mutex
	order []string
}

func newBarrierBlobs(n int) *barrierBlobs {
	b := &barrierBlobs{
		bodies: map[string][]byte{}, index: map[string]int{},
		release: make([]chan struct{}, n),
	}
	b.arrive.Add(n)
	for i := range b.release {
		b.release[i] = make(chan struct{})
	}
	return b
}

func (b *barrierBlobs) ReadBlob(_ context.Context, _ string, _ store.PackageRow, digest string) (io.ReadCloser, error) {
	i := b.index[digest]

	// Everybody in flight before anybody returns.
	b.arrive.Done()
	b.arrive.Wait()

	// Released last-first: the tail goes immediately, and each one hands the
	// baton to the request before it.
	<-b.release[i]

	b.mu.Lock()
	b.order = append(b.order, digest)
	b.mu.Unlock()
	if i > 0 {
		close(b.release[i-1])
	}

	body, ok := b.bodies[digest]
	if !ok {
		return nil, fmt.Errorf("no such blob %s", digest)
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

// A release's charts must come back in the order the release lists them, not in
// the order the registry answered.
func TestFetchIsOrderedByTheRelease(t *testing.T) {
	const n = 8

	registry := newBarrierBlobs(n)
	var candidates []store.ChartCandidate
	for i := 0; i < n; i++ {
		digest := fmt.Sprintf("sha256:%02d", i)
		registry.bodies[digest] = chartOf(t, fmt.Sprintf("chart-%02d", i))
		registry.index[digest] = i
		candidates = append(candidates, store.ChartCandidate{
			Digest: digest, LayerDigest: digest, LayerCount: 1,
			Ref: fmt.Sprintf("charts/chart-%02d", i),
		})
	}
	// Start the reverse cascade.
	close(registry.release[n-1])

	f := source.Fetcher{Blobs: registry, Concurrency: n}
	res, err := f.Fetch(context.Background(), "p", store.PackageRow{}, candidates,
		compliance.NopReporter{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(res.Root) }()

	if len(res.Charts) != n {
		t.Fatalf("got %d charts, want %d", len(res.Charts), n)
	}
	// The premise: the registry really did answer in the opposite order. The
	// barrier makes this certain rather than likely, so a failure below is the
	// fetch and never the test.
	if got := registry.order[0]; got != fmt.Sprintf("sha256:%02d", n-1) {
		t.Fatalf("the registry answered %s first, so this test is not testing what it says", got)
	}

	for i, c := range res.Charts {
		want := fmt.Sprintf("charts/chart-%02d", i)
		if c.Ref != want {
			t.Fatalf("chart %d is %s, want %s - the report's order follows the registry, "+
				"not the release", i, c.Ref, want)
		}
		if c.Err != nil {
			t.Errorf("%s: %v", c.Ref, c.Err)
		}
	}
}

// Concurrency must not be allowed to change WHICH charts the budget refuses.
//
// The budget is a running total. Accumulated as downloads land, it would refuse
// a different set on every run - so a report's coverage would depend on network
// timing. It is decided before anything is fetched, in the release's order.
func TestTheByteBudgetIsDecidedInReleaseOrder(t *testing.T) {
	body := chartOf(t, "c")
	size := int64(len(body))

	bodies := blobs{}
	var candidates []store.ChartCandidate
	for i := 0; i < 6; i++ {
		d := fmt.Sprintf("sha256:%d", i)
		bodies[d] = body
		candidates = append(candidates, store.ChartCandidate{
			Digest: d, LayerDigest: d, LayerSize: size, LayerCount: 1,
			Ref: fmt.Sprintf("charts/c%d", i),
		})
	}

	// Room for three and a bit.
	f := source.Fetcher{
		Blobs:       bodies,
		Concurrency: 6,
		Budgets:     source.Budgets{PerRelease: size*3 + 1},
	}

	var first []string
	for run := 0; run < 5; run++ {
		res, err := f.Fetch(context.Background(), "p", store.PackageRow{}, candidates,
			compliance.NopReporter{})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range res.Charts {
			got = append(got, c.Ref)
		}
		_ = os.RemoveAll(res.Root)

		if run == 0 {
			first = got
			if len(got) != 3 {
				t.Fatalf("the budget admitted %d charts, want 3: %v", len(got), got)
			}
			// In the release's order, so the first three are c0, c1, c2.
			if got[0] != "charts/c0" || got[2] != "charts/c2" {
				t.Fatalf("the budget admitted %v, not the first three the release lists", got)
			}
			if len(res.Skipped) == 0 {
				t.Error("the refused charts were not recorded; a chart nobody checked is " +
					"not a chart that passed")
			}
			continue
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("run %d admitted %v, run 0 admitted %v - the report's coverage "+
				"depends on which download finished first", run, got, first)
		}
	}
}

// The reporter is written from every worker at once. It is the runner's live
// progress, read by a poller on another goroutine while they do it.
func TestTheReporterIsSafeFromEveryWorker(t *testing.T) {
	bodies := blobs{}
	var candidates []store.ChartCandidate
	for i := 0; i < 12; i++ {
		d := fmt.Sprintf("sha256:%d", i)
		bodies[d] = chartOf(t, fmt.Sprintf("c%d", i))
		candidates = append(candidates, store.ChartCandidate{
			Digest: d, LayerDigest: d, LayerCount: 1, Ref: fmt.Sprintf("charts/c%d", i),
		})
	}

	rep := &countingReporter{}
	stop := make(chan struct{})
	var polling sync.WaitGroup
	polling.Add(1)
	go func() {
		defer polling.Done()
		for {
			select {
			case <-stop:
				return
			default:
				rep.read()
			}
		}
	}()

	f := source.Fetcher{Blobs: bodies, Concurrency: 8}
	res, err := f.Fetch(context.Background(), "p", store.PackageRow{}, candidates, rep)
	close(stop)
	polling.Wait()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(res.Root)

	if got := rep.counts().ChartsFetched; got != 12 {
		t.Errorf("the reporter counted %d fetched charts, want 12", got)
	}
	if rep.advances() != 12 {
		t.Errorf("the reporter was advanced %d times, want 12", rep.advances())
	}
}

// countingReporter is a Reporter with the locking a real one has, so the race
// detector sees the same shape production does.
type countingReporter struct {
	mu       sync.Mutex
	c        compliance.ProgressCounts
	advanced int
	begun    map[string]struct{}
	events   int
}

func (r *countingReporter) Stage(compliance.Stage, int, int, string) {}
func (r *countingReporter) Concurrency(int)                          {}

func (r *countingReporter) Advance(n int) {
	r.mu.Lock()
	r.advanced += n
	r.mu.Unlock()
}

func (r *countingReporter) Begin(what string) {
	r.mu.Lock()
	if r.begun == nil {
		r.begun = map[string]struct{}{}
	}
	r.begun[what] = struct{}{}
	r.mu.Unlock()
}

func (r *countingReporter) End(what string) {
	r.mu.Lock()
	delete(r.begun, what)
	r.mu.Unlock()
}

func (r *countingReporter) Count(mutate func(*compliance.ProgressCounts)) {
	r.mu.Lock()
	mutate(&r.c)
	r.mu.Unlock()
}

func (r *countingReporter) Event(compliance.EventKind, string, ...any) {
	r.mu.Lock()
	r.events++
	r.mu.Unlock()
}

func (r *countingReporter) counts() compliance.ProgressCounts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c
}

func (r *countingReporter) advances() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.advanced
}

// read is what a poller does: touch everything, hold nothing.
func (r *countingReporter) read() {
	r.mu.Lock()
	_ = r.c
	_ = len(r.begun)
	r.mu.Unlock()
}
