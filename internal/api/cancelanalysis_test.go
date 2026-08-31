package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A walk somebody can start and cannot stop is a walk they learn not to start.
//
// Before this endpoint the only ways out of a walk started by mistake - the
// wrong release, a source whose request budget a download needed more - were to
// wait twenty minutes for the deadline or to restart the Coordinator.
func TestStoppingAWalkReleasesTheClaimAndCancelsTheWalking(t *testing.T) {
	walking, release := make(chan struct{}), make(chan struct{})
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Discovery = &blockingDiscoverer{
			fakeDiscoverer: &fakeDiscoverer{running: true},
			arrived:        walking,
			release:        release,
		}
	})
	defer close(release)
	id := h.seedPackage("orb_25.7.2131", "sha256:"+strings.Repeat("a", 64))

	var started v1.InspectPackageResponse
	if code := h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:inspect",
		`{"wait":false}`, &started); code != http.StatusOK {
		t.Fatalf("starting the walk = %d, want 200", code)
	}
	<-walking

	var stop v1.CancelAnalysisResponse
	if code := h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:cancelAnalysis",
		`{}`, &stop); code != http.StatusOK {
		t.Fatalf("stopping the walk = %d, want 200", code)
	}

	if !stop.Stopped {
		t.Error("the response says there was nothing to stop, over a walk that was running")
	}
	// The claim and the walking are two promises, and this replica is the one
	// doing the walking, so it owes both.
	if !stop.StoppedHere {
		t.Error("stoppedHere is false on the replica that started the walk, so the " +
			"reader is told the walk carries on when it does not")
	}

	// RELEASED, not failed. A red `Analysis failed` tag on a release somebody
	// deliberately stopped is a falsehood - and the background analyser skips
	// failed rows, so it would also stop the release ever being walked again.
	if got := analysisStateOf(t, h, id); got != "" {
		t.Errorf("analysisState = %q after a stop, want it unheld so the release "+
			"can be analysed again", got)
	}
	if stop.Package_.AnalysisState != "" {
		t.Errorf("the response reports analysisState %q, want it clear", stop.Package_.AnalysisState)
	}
}

// A stopped walk finishing must not write anything: the claim has already been
// released, and re-marking the release would either put a failure on it or
// overwrite a claim somebody has since taken.
func TestAStoppedWalkDoesNotMarkTheReleaseWhenItUnwinds(t *testing.T) {
	walking, release := make(chan struct{}), make(chan struct{})
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Discovery = &blockingDiscoverer{
			fakeDiscoverer: &fakeDiscoverer{running: true},
			arrived:        walking,
			release:        release,
		}
	})
	id := h.seedPackage("orb_25.7.2131", "sha256:"+strings.Repeat("c", 64))

	var started v1.InspectPackageResponse
	h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:inspect", `{"wait":false}`, &started)
	<-walking

	var stop v1.CancelAnalysisResponse
	h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:cancelAnalysis", `{}`, &stop)

	// Let the blocked walk unwind however it likes. Whatever it does, the row
	// must stay unheld.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := analysisStateOf(t, h, id); got != "" {
			t.Fatalf("the unwinding walk wrote analysisState = %q over a release "+
				"that had already been released", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Nothing to stop is an answer, not a failure: the walk finished between the
// reader deciding to stop it and the request arriving.
func TestStoppingAWalkThatIsNotRunningIsNotAnError(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Discovery = &fakeDiscoverer{running: true}
	})
	h.seedPackage("orb_25.7.2131", "sha256:"+strings.Repeat("d", 64))

	var stop v1.CancelAnalysisResponse
	if code := h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:cancelAnalysis",
		`{}`, &stop); code != http.StatusOK {
		t.Fatalf("code = %d, want 200 - an idle release is a state to report", code)
	}
	if stop.Stopped || stop.StoppedHere {
		t.Errorf("claims to have stopped something: %+v", stop)
	}
}

// A claim taken by ANOTHER process - discovery's background analyser, or
// another replica - is still releasable from here. That is the whole reason the
// claim lives in the database rather than in memory.
func TestAWalkThisReplicaIsNotRunningStillReleasesItsClaim(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Discovery = &fakeDiscoverer{running: true}
	})
	id := h.seedPackage("orb_25.7.2131", "sha256:"+strings.Repeat("e", 64))

	if _, err := h.packages.ClaimAnalysis(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var stop v1.CancelAnalysisResponse
	if code := h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:cancelAnalysis",
		`{}`, &stop); code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if !stop.Stopped {
		t.Error("the claim was not released")
	}
	// And the response says so honestly: this Coordinator cannot promise the
	// other one has stopped reading the vendor's registry.
	if stop.StoppedHere {
		t.Error("claims to have cancelled a walk it was not running")
	}
	if got := analysisStateOf(t, h, id); got != "" {
		t.Errorf("analysisState = %q, want it unheld", got)
	}
}

// A release that has been stopped can be analysed again immediately - which is
// the retrigger half of the same feature.
func TestAStoppedReleaseCanBeAnalysedAgain(t *testing.T) {
	h := newAPIHarnessWith(t, func(d *Deps) {
		d.Discovery = &fakeDiscoverer{running: true}
	})
	id := h.seedPackage("orb_25.7.2131", "sha256:"+strings.Repeat("f", 64))

	if _, err := h.packages.ClaimAnalysis(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	var stop v1.CancelAnalysisResponse
	h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:cancelAnalysis", `{}`, &stop)

	var again v1.InspectPackageResponse
	if code := h.post("/api/v1/products/vendor-a/packages/orb_25.7.2131:inspect",
		`{"wait":false}`, &again); code != http.StatusOK {
		t.Fatalf("re-analysing a stopped release = %d, want 200", code)
	}
	if !again.Started {
		t.Error("the walk was not restarted")
	}
}

func analysisStateOf(t *testing.T, h *apiHarness, id int64) string {
	t.Helper()
	pkg, err := h.packages.GetPackageByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.AnalysisState == store.AnalysisFailed {
		// Named, because "failed" is the specific wrong answer these tests
		// exist to rule out.
		return "failed (" + pkg.AnalysisError + ")"
	}
	return pkg.AnalysisState
}
