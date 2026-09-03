package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// A product whose target has BOTH scanners on: Xray, which indexes the
// repository, and Anchore, which has to be told the images exist. The
// difference between them is the whole subject of these tests.
const replicateProductDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: vendor
      registry: registry.example.com
      repository: vendor-a/platform
      anonymous: true
  targets:
    - name: internal
      registry: internal.example.com
      repository: mirror/vendor-a
      type: jfrog
      credentialsRef:
        secretName: jfrog
      default: true
      xrayEnabled: true
      anchoreEnabled: true
`

// fakeReplicator stands in for a scanner that has to be told about images.
//
// It records what it was asked to register and writes the row the real service
// would, through the real store - so the read paths under test are production
// ones rather than a second implementation that can drift from them.
type fakeReplicator struct {
	registrations *store.SecurityRegistrations
	// registrable names the providers that need telling. Xray is deliberately
	// absent, which is what makes the "no button for Xray" tests meaningful.
	registrable map[string]bool

	calls     int
	lastScope security.Scope
	// result is what a run "finds". The zero value registers everything.
	result *security.Registration
	// err is returned instead of running.
	err error
}

func (f *fakeReplicator) Registrable(_ context.Context, scope security.Scope) bool {
	return f.registrable[scope.Provider]
}

func (f *fakeReplicator) Replicate(
	ctx context.Context, req security.ReplicateRequest,
) (security.ReplicateResult, error) {
	f.calls++
	f.lastScope = req.Scope
	if f.err != nil {
		return security.ReplicateResult{}, f.err
	}
	if err := f.registrations.Claim(ctx, req.PackageID, req.Scope.Provider); err != nil {
		return security.ReplicateResult{}, err
	}

	reg := security.Registration{
		Provider: req.Scope.Provider, Expected: len(req.Artifacts),
		Submitted: len(req.Artifacts), Associated: len(req.Artifacts),
		Application: "vendor-a", Version: req.Release.Version,
		ApplicationID: "app-1", VersionID: "ver-1",
		URL: "https://anchore.example.com/applications/app-1/versions/ver-1",
		At:  time.Now().UTC(),
	}
	if f.result != nil {
		reg = *f.result
		reg.Provider = req.Scope.Provider
	}
	reg.Settle()
	log := []security.SyncLogEntry{
		{At: time.Now().UTC(), Level: security.LogInfo, Message: "Registered."},
	}
	if err := f.registrations.Record(ctx, req.PackageID, reg, log); err != nil {
		return security.ReplicateResult{}, err
	}
	return security.ReplicateResult{Registration: reg, Log: log}, nil
}

func newReplicateHarness(t *testing.T, adjust func(*fakeReplicator)) (*apiHarness, *fakeReplicator) {
	t.Helper()
	rep := &fakeReplicator{registrable: map[string]bool{"anchore": true}}
	if adjust != nil {
		adjust(rep)
	}
	h := newAPIHarnessWith(t, func(d *Deps) {
		pkgSec := store.NewPackageSecurity(d.Store)
		regs := store.NewSecurityRegistrations(d.Store)
		rep.registrations = regs

		d.SecurityStore = harnessSecurityStore{pkgSec, store.NewSecurity(d.Store)}
		d.SecurityIndex = store.NewSecurity(d.Store)
		d.SecurityReplicate = rep
		d.SecurityRegistrations = regs
	}, replicateProductDoc)
	return h, rep
}

// The button works, and it says what it did.
func TestReplicateRegistersTheRelease(t *testing.T) {
	h, rep := newReplicateHarness(t, nil)
	_ = h.seedPackage("25.7.2131", "sha256:aaa")
	ref := "sha256:aaa"

	var out v1.ReplicateSecurityResponse
	if code := h.post("/api/v1/products/vendor-a/packages/"+ref+":replicateSecurity",
		`{}`, &out); code != http.StatusOK {
		t.Fatalf("replicate returned %d", code)
	}
	if rep.calls != 1 {
		t.Fatalf("the scanner was asked %d times", rep.calls)
	}
	// ONLY the scanner that needs telling. Xray indexes the repository, so
	// replicating to it is a request for something that does not exist.
	if rep.lastScope.Provider != "anchore" {
		t.Errorf("replicated to %q, want anchore", rep.lastScope.Provider)
	}
	if len(out.Registrations) != 1 {
		t.Fatalf("expected one registration, got %d", len(out.Registrations))
	}
	reg := out.Registrations[0]
	if reg.State != string(security.RegistrationComplete) {
		t.Errorf("state = %q", reg.State)
	}
	if reg.URL == "" || reg.Application == "" {
		t.Errorf("the response did not carry where to open it in Anchore: %+v", reg)
	}
	if len(out.Log) == 0 {
		t.Error("a button that ran should return its transcript")
	}
}

// The state reaches the security page, which is where the notice is drawn.
func TestSecurityResponseCarriesTheRegistration(t *testing.T) {
	h, _ := newReplicateHarness(t, nil)
	_ = h.seedPackage("25.7.2131", "sha256:aaa")
	ref := "sha256:aaa"

	// BEFORE: never replicated, and the page has to be able to say so.
	var before v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/"+ref+"/security", &before); code != http.StatusOK {
		t.Fatalf("read returned %d", code)
	}
	if len(before.Registrations) != 1 {
		t.Fatalf("expected one registration entry, got %d", len(before.Registrations))
	}
	if before.Registrations[0].State != "" {
		t.Errorf("an unreplicated release reported state %q", before.Registrations[0].State)
	}
	if !before.Registrations[0].CanReplicate {
		t.Error("the button was not offered on a release that needs replicating")
	}
	// Xray must NOT appear: it indexes the repository and there is nothing to
	// replicate to it, so a button for it would be a control that does nothing.
	for _, r := range before.Registrations {
		if r.Provider == "jfrog-xray" {
			t.Error("a scanner that indexes the repository was offered a Replicate button")
		}
	}

	if code := h.post("/api/v1/products/vendor-a/packages/"+ref+":replicateSecurity",
		`{}`, nil); code != http.StatusOK {
		t.Fatalf("replicate returned %d", code)
	}

	var after v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/"+ref+"/security", &after); code != http.StatusOK {
		t.Fatalf("read returned %d", code)
	}
	if after.Registrations[0].State != string(security.RegistrationComplete) {
		t.Errorf("state after replicating = %q", after.Registrations[0].State)
	}
	if after.Registrations[0].RegisteredAt == "" {
		t.Error("a completed replication has no time on it")
	}
}

// A partial run is not a failure: the images that have landed are registered,
// and the rest need the button again once they are transferred.
func TestPartialReplicationIsReportedAsPartial(t *testing.T) {
	h, _ := newReplicateHarness(t, func(r *fakeReplicator) {
		r.result = &security.Registration{
			Expected: 10, Submitted: 6, Associated: 6,
			Failed: map[string]string{"sha256:zzz": "This image is not in the registry Anchore pulls from."},
		}
	})
	_ = h.seedPackage("25.7.2131", "sha256:aaa")
	ref := "sha256:aaa"

	var out v1.ReplicateSecurityResponse
	if code := h.post("/api/v1/products/vendor-a/packages/"+ref+":replicateSecurity",
		`{}`, &out); code != http.StatusOK {
		t.Fatalf("replicate returned %d", code)
	}
	reg := out.Registrations[0]
	if reg.State != string(security.RegistrationPartial) {
		t.Fatalf("state = %q, want partial", reg.State)
	}
	if reg.Outstanding != 4 {
		t.Errorf("outstanding = %d, want 4", reg.Outstanding)
	}
	if reg.Error == "" {
		t.Error("a partial run must say what was left out")
	}
	// And the button stays available, because pressing it again is the remedy.
	if !reg.CanReplicate {
		t.Error("a partial replication withdrew the button that fixes it")
	}
}

// Two people pressing at once see one operation.
func TestReplicateRefusesWhileOneIsRunning(t *testing.T) {
	h, rep := newReplicateHarness(t, nil)
	pkgID := h.seedPackage("25.7.2131", "sha256:aaa")
	ref := "sha256:aaa"

	// Hold the claim, as a run in flight would.
	if err := rep.registrations.Claim(context.Background(), pkgID, "anchore"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if code := h.post("/api/v1/products/vendor-a/packages/"+ref+":replicateSecurity",
		`{}`, nil); code != http.StatusConflict {
		// The same status the sync endpoint answers a second press with, so
		// the two controls behave identically for the identical situation.
		t.Fatalf("a second press returned %d, want 409", code)
	}
}

// Naming a scanner that does not need telling falls back rather than refusing:
// a stale browser tab must not turn a button into an error.
func TestReplicateIgnoresAnUnregistrableProvider(t *testing.T) {
	h, rep := newReplicateHarness(t, nil)
	_ = h.seedPackage("25.7.2131", "sha256:aaa")
	ref := "sha256:aaa"

	if code := h.post("/api/v1/products/vendor-a/packages/"+ref+":replicateSecurity",
		`{"provider":"jfrog-xray"}`, nil); code != http.StatusOK {
		t.Fatalf("replicate returned %d", code)
	}
	if rep.lastScope.Provider != "anchore" {
		t.Errorf("fell back to %q, want anchore", rep.lastScope.Provider)
	}
}

// A deployment with no scanner that needs telling never draws the notice.
func TestNoRegistrationEntriesWithoutARegistrableScanner(t *testing.T) {
	h, _ := newReplicateHarness(t, func(r *fakeReplicator) {
		r.registrable = map[string]bool{}
	})
	_ = h.seedPackage("25.7.2131", "sha256:aaa")
	ref := "sha256:aaa"

	var out v1.PackageSecurityResponse
	if code := h.get("/api/v1/products/vendor-a/packages/"+ref+"/security", &out); code != http.StatusOK {
		t.Fatalf("read returned %d", code)
	}
	if len(out.Registrations) != 0 {
		t.Errorf("a deployment with nothing to replicate offered %d buttons", len(out.Registrations))
	}
}
