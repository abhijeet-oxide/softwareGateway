package transfer

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// Resolution is where every combination the operator can express is decided:
// one source to one target, one to many, one target to another, one target to
// many. The rules are small; what they refuse matters as much as what they
// allow, because the thing being refused is an unintended push to production.

type fakeCatalog struct {
	view ProductView
	// rows is the catalog as it actually is, keyed by ID - which is not the
	// same as configuration. An enumerating source owns a row per repository
	// it found, named "<source>/<path>", and the package points at one of
	// those rather than at the configured name.
	rows map[int64]RepoView
	// nextID is what EnsureTarget hands out for a target nothing has been
	// written to yet.
	nextID int64
	// holding is which targets have actually received the release, which is
	// what settles an origin configuration cannot.
	holding []string
}

func (f fakeCatalog) ProductView(context.Context, string) (ProductView, error) {
	return f.view, nil
}

func (f fakeCatalog) RepositoryByID(_ context.Context, id int64) (RepoView, error) {
	r, ok := f.rows[id]
	if !ok {
		return RepoView{}, errNoRow
	}
	return r, nil
}

func (f *fakeCatalog) EnsureTarget(_ context.Context, _ string, t RepoView) (int64, error) {
	f.nextID++
	id := 900 + f.nextID
	if f.rows == nil {
		f.rows = map[int64]RepoView{}
	}
	t.RepositoryID = id
	f.rows[id] = t
	return id, nil
}

// holding is which targets the release is actually in, keyed by target name.
// Empty means the history says nothing, which is the state every test that
// does not care about it wants.
func (f fakeCatalog) TargetsHolding(context.Context, int64) ([]string, error) {
	return f.holding, nil
}

var errNoRow = errors.New("no such repository")

// twoRegions is the shape the question was really about: several targets per
// environment, so nothing can be resolved by "the lab one".
func twoRegions() ProductView {
	return ProductView{
		Name:          "nokia",
		PromotionFrom: "lab",
		PromotionTo:   "production",
		Sources: []RepoView{
			{RepositoryID: 1, Name: "near", Role: "source", Registry: "near.nokia.com"},
		},
		Targets: []RepoView{
			{RepositoryID: 10, Name: "lab-eu", Role: "target", Environment: "lab",
				Registry: "eu.jfrog.io", Repository: "nokia-lab", Default: true},
			{RepositoryID: 11, Name: "lab-us", Role: "target", Environment: "lab",
				Registry: "us.jfrog.io", Repository: "nokia-lab"},
			{RepositoryID: 20, Name: "prod-eu", Role: "target", Environment: "production",
				Registry: "eu.jfrog.io", Repository: "nokia-prod", PromotionOnly: true},
			{RepositoryID: 21, Name: "prod-us", Role: "target", Environment: "production",
				Registry: "us.jfrog.io", Repository: "nokia-prod", PromotionOnly: true},
		},
	}
}

// oneOfEach is the simple deployment: nothing to disambiguate, so promote
// needs no flags at all.
func oneOfEach() ProductView {
	v := twoRegions()
	v.Targets = []RepoView{v.Targets[0], v.Targets[2]}
	return v
}

func resolve(t *testing.T, view ProductView, req CreateRequest) (Resolved, error) {
	t.Helper()

	// The package was discovered in a row the ENUMERATING source owns, named
	// "near/orbs/CFX-5000-k8s" - not "near". Resolving the configured name
	// would find nothing, which is exactly the bug this shape reproduces.
	rows := map[int64]RepoView{
		100: {RepositoryID: 100, Name: "near/orbs/CFX-5000-k8s", Role: "source",
			Registry: "near.nokia.com", Repository: "orbs/CFX-5000-k8s"},
	}
	for _, t := range view.Targets {
		if t.RepositoryID != 0 {
			rows[t.RepositoryID] = t
		}
	}

	req.Row = store.PackageRow{ID: 7, ProductID: 1, SourceRepoID: 100}
	return NewRequester(nil, &fakeCatalog{view: view, rows: rows}).Resolve(t.Context(), req)
}

func targetNames(r Resolved) []string {
	out := make([]string, 0, len(r.Targets))
	for _, t := range r.Targets {
		out = append(out, t.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// Replication
// ---------------------------------------------------------------------------

// The ordinary case: no --from, no --to. The origin is where the package came
// from and the destination is the default target.
func TestReplicationDefaultsToDiscoveredSourceAndDefaultTarget(t *testing.T) {
	got, err := resolve(t, twoRegions(), CreateRequest{Product: "nokia"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != "replicate" {
		t.Errorf("operation is %q, want replicate", got.Operation)
	}
	// The row the package actually came from, not the configured name.
	if got.Origin.RepositoryID != 100 {
		t.Errorf("origin row is %d, want 100", got.Origin.RepositoryID)
	}
	if names := targetNames(got); len(names) != 1 || names[0] != "lab-eu" {
		t.Errorf("targets are %v, want [lab-eu]", names)
	}
}

// One source, several targets.
func TestReplicationFansOutToNamedTargets(t *testing.T) {
	got, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", To: []string{"lab-eu", "lab-us"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := targetNames(got); len(names) != 2 {
		t.Fatalf("targets are %v, want both labs", names)
	}
}

// A production registry configured as promotionOnly must not be reachable
// directly from a vendor. This is the guard that makes that configuration mean
// something.
func TestReplicationRefusesAPromotionOnlyTarget(t *testing.T) {
	_, err := resolve(t, twoRegions(), CreateRequest{Product: "nokia", To: []string{"prod-eu"}})
	if err == nil {
		t.Fatal("replicating straight into a promotionOnly target was allowed")
	}
	if !strings.Contains(err.Error(), "promotionOnly") {
		t.Errorf("error does not explain why: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Promotion
// ---------------------------------------------------------------------------

// One lab, one production: the whole point of the environment key is that this
// needs no flags.
func TestPromoteResolvesTheEnvironmentPathWithNoFlags(t *testing.T) {
	got, err := resolve(t, oneOfEach(), CreateRequest{Product: "nokia", Promote: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != "promote" {
		t.Errorf("operation is %q, want promote", got.Operation)
	}
	if got.Origin.Name != "lab-eu" {
		t.Errorf("origin is %q, want lab-eu", got.Origin.Name)
	}
	if names := targetNames(got); len(names) != 1 || names[0] != "prod-eu" {
		t.Errorf("targets are %v, want [prod-eu]", names)
	}
}

// Several labs, several productions. Guessing here would push to a production
// registry nobody named, so it is refused - and the error has to name the
// candidates and the flag, or the operator is left guessing at spellings.
func TestPromoteRefusesToGuessBetweenSeveralTargets(t *testing.T) {
	_, err := resolve(t, twoRegions(), CreateRequest{Product: "nokia", Promote: true})
	if err == nil {
		t.Fatal("an ambiguous promotion was resolved rather than refused")
	}
	for _, want := range []string{"lab-eu", "lab-us", "--from"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestPromoteRefusesAmbiguousDestination(t *testing.T) {
	_, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", Promote: true, From: "lab-eu",
	})
	if err == nil {
		t.Fatal("an ambiguous destination was resolved rather than refused")
	}
	for _, want := range []string{"prod-eu", "prod-us", "--to-environment"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

// One target to many, said deliberately.
func TestPromoteToAnEnvironmentFansOut(t *testing.T) {
	got, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", Promote: true, From: "lab-eu", ToEnvironment: "production",
	})
	if err != nil {
		t.Fatal(err)
	}
	if names := targetNames(got); len(names) != 2 {
		t.Fatalf("targets are %v, want both production targets", names)
	}
}

// Promotion moves between targets. A vendor source as origin is a replication
// wearing the wrong word, and the error should say which command to use.
func TestPromoteRefusesAVendorSource(t *testing.T) {
	_, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", Promote: true, From: "near", To: []string{"prod-eu"},
	})
	if err == nil {
		t.Fatal("promoting from a vendor source was allowed")
	}
	if !strings.Contains(err.Error(), "transfers create") {
		t.Errorf("error does not point at the right command: %v", err)
	}
}

// The operation follows the origin even when nobody said "promote", so
// `transfers create --from lab-eu` is a promotion and gets promotion's rules -
// including access to a promotionOnly destination.
func TestOperationIsDerivedFromTheOrigin(t *testing.T) {
	got, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", From: "lab-eu", To: []string{"prod-eu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != "promote" {
		t.Errorf("operation is %q, want promote - the origin is a target", got.Operation)
	}
}

// A copy from something to itself would open a transfer, lease jobs and move
// nothing.
func TestOriginMayNotAlsoBeADestination(t *testing.T) {
	_, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", From: "lab-eu", To: []string{"prod-eu", "lab-eu"},
	})
	if err == nil {
		t.Fatal("a copy onto the origin was allowed")
	}
}

// A name nobody configured should list what is configured rather than just
// saying no.
func TestUnknownNameListsWhatExists(t *testing.T) {
	_, err := resolve(t, twoRegions(), CreateRequest{Product: "nokia", To: []string{"lab-ap"}})
	if err == nil {
		t.Fatal("an unconfigured target was accepted")
	}
	if !strings.Contains(err.Error(), "lab-eu") {
		t.Errorf("error does not list the available names: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Deduction: ask only what cannot be worked out
// ---------------------------------------------------------------------------

// An enumerating source owns a row per repository it found, so the configured
// name resolves to nothing. Naming it must still work - checked against the
// package rather than looked up, which is what stopped a zero row ID reaching
// the insert as a foreign-key violation.
func TestFromMayNameAnEnumeratingSource(t *testing.T) {
	got, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", From: "near", To: []string{"lab-eu"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin.RepositoryID != 100 {
		t.Errorf("origin row is %d, want the package's own row 100", got.Origin.RepositoryID)
	}
}

// Naming a source the package did not come from is a mistake worth catching,
// not a silent fallback.
func TestFromRejectsASourceThePackageDidNotComeFrom(t *testing.T) {
	_, err := resolve(t, twoRegions(), CreateRequest{
		Product: "nokia", From: "somewhere-else", To: []string{"lab-eu"},
	})
	if err == nil {
		t.Fatal("an origin the package never came from was accepted")
	}
}

// One target is not a choice. `default: true` should not be needed to say the
// only thing that could be meant.
func TestSingleTargetNeedsNoFlagAndNoDefault(t *testing.T) {
	view := twoRegions()
	view.Targets = []RepoView{{
		RepositoryID: 10, Name: "lab", Role: "target",
		Registry: "eu.jfrog.io", Repository: "nokia-lab",
	}}

	got, err := resolve(t, view, CreateRequest{Product: "nokia"})
	if err != nil {
		t.Fatal(err)
	}
	if names := targetNames(got); len(names) != 1 || names[0] != "lab" {
		t.Errorf("targets are %v, want [lab]", names)
	}
}

// Two targets where one refuses direct replication still leaves one candidate,
// so a replication needs no flag there either.
func TestReplicationDeducesTheOnlyReplicableTarget(t *testing.T) {
	view := twoRegions()
	view.Targets = []RepoView{
		{RepositoryID: 10, Name: "lab", Role: "target", Registry: "eu.jfrog.io"},
		{RepositoryID: 20, Name: "prod", Role: "target", Registry: "eu.jfrog.io",
			PromotionOnly: true},
	}

	got, err := resolve(t, view, CreateRequest{Product: "nokia"})
	if err != nil {
		t.Fatal(err)
	}
	if names := targetNames(got); len(names) != 1 || names[0] != "lab" {
		t.Errorf("targets are %v, want [lab]", names)
	}
}

// The same shape promotes with no flags and no environment key: promotion goes
// out of a replicable target and into a promotionOnly one.
func TestPromoteDeducesWithoutEnvironments(t *testing.T) {
	view := twoRegions()
	view.Targets = []RepoView{
		{RepositoryID: 10, Name: "lab", Role: "target", Registry: "eu.jfrog.io"},
		{RepositoryID: 20, Name: "prod", Role: "target", Registry: "eu.jfrog.io",
			PromotionOnly: true},
	}

	got, err := resolve(t, view, CreateRequest{Product: "nokia", Promote: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin.Name != "lab" {
		t.Errorf("origin is %q, want lab", got.Origin.Name)
	}
	if names := targetNames(got); len(names) != 1 || names[0] != "prod" {
		t.Errorf("targets are %v, want [prod]", names)
	}
}

// A target nothing has been written to has no catalog row. Naming it must
// create one rather than fail on a constraint that names neither the target
// nor the flag.
func TestTargetWithoutARowGetsOne(t *testing.T) {
	view := twoRegions()
	view.Targets = []RepoView{{
		Name: "lab", Role: "target", Registry: "eu.jfrog.io", Repository: "nokia-lab",
	}}

	got, err := resolve(t, view, CreateRequest{Product: "nokia"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Targets[0].RepositoryID == 0 {
		t.Error("the target still has no catalog row, so the insert would violate its foreign key")
	}
}
