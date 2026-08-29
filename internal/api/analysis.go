package api

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Starting, stopping and re-running a release's analysis.
//
// # Where analysis actually runs, since the question keeps being asked
//
// ON THE COORDINATOR, never on a worker. A worker moves bytes between two
// registries and holds no database credentials; analysis reads MANIFESTS -
// kilobytes of JSON - and writes the tree it finds straight into the database,
// so shipping it to a worker would mean shipping the database to a worker.
//
// Two things start one: discovery's background analyser, which walks recently
// published releases unasked (internal/discovery/analyse.go), and this API,
// when a reader presses the button. Both take the SAME claim, so they cannot
// walk the same release twice, and both leave the outcome on the release's own
// row - `analysisState` and `analysisError`.
//
// # Why stopping needs a registry of cancel functions
//
// The claim in the database is what every replica can see, and releasing it is
// what makes a release stop reading as claimed. It does not stop the walking:
// a goroutine already inside a three-hundred-manifest tree carries on talking
// to the vendor's registry until its own deadline.
//
// For a walk THIS replica started, that is fixable and worth fixing - the
// cancel function is right here. So the walks are registered as they start and
// cancelled by ID, and the response says which of the two happened, because
// "stopped" and "released the claim and asked it to stop" are different
// promises and an operator should not have to guess which they were given.
type analysisRunner struct {
	mu      sync.Mutex
	running map[int64]context.CancelFunc
}

func newAnalysisRunner() *analysisRunner {
	return &analysisRunner{running: map[int64]context.CancelFunc{}}
}

// begin derives a cancellable context for one walk and registers it.
//
// The returned function both releases the registration and cancels the
// context, so a caller `defer`s it exactly once and cannot leak either.
func (a *analysisRunner) begin(ctx context.Context, packageID int64) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)

	a.mu.Lock()
	// A second walk of the same release cannot happen - the database claim
	// prevents it - but if one ever did, the later one owns the registration
	// and the earlier one's deregistration must not remove it. Compared by
	// identity for exactly that reason.
	a.running[packageID] = cancel
	a.mu.Unlock()

	return ctx, func() {
		a.mu.Lock()
		delete(a.running, packageID)
		a.mu.Unlock()
		cancel()
	}
}

// stop cancels a walk this process is running, reporting whether there was one.
func (a *analysisRunner) stop(packageID int64) bool {
	a.mu.Lock()
	cancel, ok := a.running[packageID]
	delete(a.running, packageID)
	a.mu.Unlock()

	if ok {
		cancel()
	}
	return ok
}

// errAnalysisStopped marks a walk that ended because somebody stopped it.
//
// It exists so the finishing code can tell "this went wrong" from "this was
// called off", and record neither a failure nor a reason for one - a red
// `Analysis failed` tag on a release somebody deliberately stopped is a
// falsehood, and it also hides the release from the background analyser, which
// skips failed rows.
var errAnalysisStopped = errors.New("the analysis was stopped")

// handleCancelAnalysis serves POST
// /api/v1/products/{product}/packages/{package}:cancelAnalysis.
//
// # Why a walk needs a stop at all
//
// A release with an unusually large tree is minutes of round trips against the
// vendor's registry, and the reader who started it may have started the wrong
// one, or may need the source's request budget for a download that matters
// more. Until now the only way out of a walk was to wait for its deadline -
// twenty minutes - or to restart the Coordinator.
//
// It is also the only cure for the state a stopped walk USED to leave behind:
// a release reading `Analyzing` with nothing analysing it, until the staleness
// sweep noticed half an hour later.
func (s *Server) handleCancelAnalysis(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil {
		Error(w, r, v1.CodeUnavailable, "this Coordinator has no package store")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, chi.URLParam(r, "package"))
	if !ok {
		return
	}

	// The claim FIRST, and the in-memory cancel second.
	//
	// This order is what makes the operation safe to lose a race with. The
	// claim is the thing every replica can see, so releasing it is what
	// actually stops the release reading as claimed; cancelling the goroutine
	// afterwards is a courtesy to this process's own resources. Doing it the
	// other way round would let the cancelled walk's own FinishAnalysis land
	// after the release, re-marking a release nobody is walking.
	released, err := s.deps.Packages.CancelAnalysis(r.Context(), pkg.ID)
	if err != nil {
		s.internal(w, r, "stop the analysis", err)
		return
	}
	here := s.analyses.stop(pkg.ID)

	fresh, err := s.deps.Packages.GetPackageByID(r.Context(), pkg.ID)
	if err != nil {
		s.internal(w, r, "read the package", err)
		return
	}

	WriteJSON(w, r, http.StatusOK, v1.CancelAnalysisResponse{
		Product: productName,
		Package: packageReferenceOf(pkg),
		// False is not a failure: the walk finished between the reader
		// deciding to stop it and the request arriving.
		Stopped: released,
		// Whether the walking itself has ended, or only the claim on it. A
		// walk started on another replica keeps reading the vendor's registry
		// until its own deadline, and saying so is better than implying a
		// promise this Coordinator cannot keep.
		StoppedHere: here,
		Package_:    toAPIPackage(productName, fresh),
	})
}

// analysisEnded records how a walk finished, treating a stop as neither.
//
// A cancelled walk has already had its claim released by the handler above, so
// writing anything here would either re-mark the release or overwrite a fresh
// claim somebody has since taken. Doing nothing is the correct write.
func analysisEnded(cause error) (record bool, err error) {
	switch {
	case cause == nil:
		return true, nil
	case errors.Is(cause, context.Canceled), errors.Is(cause, errAnalysisStopped):
		return false, nil
	default:
		return true, cause
	}
}
