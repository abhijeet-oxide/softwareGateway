package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// harness gives a checker, a registry, and a way to load a product document.
type harness struct {
	t       *testing.T
	reg     *fakeregistry.Registry
	checker *Checker
	secrets string
}

func newHarness(t *testing.T, opts ...fakeregistry.Option) *harness {
	t.Helper()

	reg := fakeregistry.New(opts...)
	t.Cleanup(reg.Close)

	secrets := t.TempDir()
	writeSecret(t, secrets, "good", "svc", "correct-horse")
	writeSecret(t, secrets, "bad", "svc", "wrong-password")

	return &harness{t: t, reg: reg, checker: NewChecker(product.NewSecretResolver(secrets)), secrets: secrets}
}

func writeSecret(t *testing.T, dir, name, user, pass string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "username"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "password"), []byte(pass), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) load(doc string) *product.Product {
	h.t.Helper()

	dir := h.t.TempDir()
	doc = strings.ReplaceAll(doc, "REGISTRY_HOST", h.reg.Host())
	if err := os.WriteFile(filepath.Join(dir, "p.yaml"), []byte(doc), 0o600); err != nil {
		h.t.Fatal(err)
	}

	res, err := product.NewLoader(dir, product.NewSecretResolver(h.secrets)).Load()
	if err != nil {
		h.t.Fatalf("load: %v", err)
	}
	if len(res.Invalid) > 0 {
		h.t.Fatalf("document invalid: %v", res.Invalid[0].Err)
	}
	return res.Valid[0]
}

// step finds a named step in a repository result.
func step(t *testing.T, r RepositoryResult, name string) Step {
	t.Helper()
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no step %q in %v", name, stepNames(r))
	return Step{}
}

func stepNames(r RepositoryResult) []string {
	out := make([]string, len(r.Steps))
	for i, s := range r.Steps {
		out[i] = s.Name
	}
	return out
}

func repoNamed(t *testing.T, res ProductResult, name string) RepositoryResult {
	t.Helper()
	for _, r := range res.Repositories {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no repository %q in result", name)
	return RepositoryResult{}
}

const anonDoc = `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      repositories: [suite/core, suite/database]
      anonymous: true
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repositories: prefix
      repositoryPrefix: mirror
      anonymous: true
      default: true
`

func TestHealthyConfigurationPasses(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage("suite/database", "v2.0.0", fakeregistry.NewLayer("b"))

	res := h.checker.CheckProduct(t.Context(), h.load(anonDoc))

	if res.Status != StatusOK {
		t.Fatalf("expected OK, got %s: %+v", res.Status, res.Repositories)
	}
	// Both source repositories plus the target.
	if len(res.Repositories) != 3 {
		t.Fatalf("expected 3 repositories checked, got %d", len(res.Repositories))
	}
}

// Every repository a source declares is probed, not just the first.
func TestEveryDeclaredRepositoryIsProbed(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	// suite/database is deliberately absent.

	res := h.checker.CheckProduct(t.Context(), h.load(anonDoc))

	if res.Status != StatusFailed {
		t.Fatalf("a missing source repository must fail the product, got %s", res.Status)
	}

	var failed int
	for _, r := range res.Repositories {
		if r.Repository == "suite/database" && r.Status == StatusFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Error("the missing repository must be the one reported")
	}
}

// The most common configuration mistake there is, and the reason the registry
// client reports 404 rather than flattening it into an empty list.
func TestTypoedSourceRepositoryIsAFailure(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))

	doc := strings.Replace(anonDoc, "[suite/core, suite/database]", "[suite/coer]", 1)
	res := h.checker.CheckProduct(t.Context(), h.load(doc))

	r := repoNamed(t, res, "vendor")
	if r.Status != StatusFailed {
		t.Fatalf("a typo'd source path must fail, got %s", r.Status)
	}
	s := step(t, r, "can list tags")
	if !strings.Contains(s.Detail, "does not exist") {
		t.Errorf("expected the detail to name the cause, got %q", s.Detail)
	}
	// The hint has to rule out the things that are FINE, or the operator
	// re-checks the host and the credential first.
	if !strings.Contains(s.Hint, "registry host and credential are fine") {
		t.Errorf("the hint should narrow the search, got %q", s.Hint)
	}
}

// A target does not exist until something is pushed to it. Reporting that as a
// failure would make every correctly-configured new destination look broken.
func TestMissingTargetRepositoryIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage("suite/database", "v2.0.0", fakeregistry.NewLayer("b"))
	// mirror/p is deliberately absent.

	res := h.checker.CheckProduct(t.Context(), h.load(anonDoc))

	target := repoNamed(t, res, "internal")
	if target.Status == StatusFailed {
		t.Fatalf("a target that does not exist yet must not fail: %+v", target.Steps)
	}
	s := step(t, target, "readable")
	if !strings.Contains(s.Hint, "first push") {
		t.Errorf("expected the hint to explain why this is normal, got %q", s.Hint)
	}
}

// Write access is not verified, and says so. "We did not check" and "it passed"
// must not look the same.
func TestTargetWriteAccessIsReportedAsUnverified(t *testing.T) {
	h := newHarness(t)
	res := h.checker.CheckProduct(t.Context(), h.load(anonDoc))

	s := step(t, repoNamed(t, res, "internal"), "writable")
	if s.Status != StatusSkipped {
		t.Errorf("write access cannot be verified without writing; expected SKIPPED, got %s", s.Status)
	}
	if s.Hint == "" {
		t.Error("a skipped check must say why it was skipped")
	}
}

func TestBadCredentialsAreReportedAsAuthFailure(t *testing.T) {
	h := newHarness(t, fakeregistry.WithBasicAuth("svc", "correct-horse"))
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))

	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      repository: suite/core
      credentialsRef: {secretName: bad}
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repositories: prefix
      repositoryPrefix: mirror
      credentialsRef: {secretName: good}
      default: true
`
	res := h.checker.CheckProduct(t.Context(), h.load(doc))

	r := repoNamed(t, res, "vendor")
	if r.Status != StatusFailed {
		t.Fatalf("wrong credentials must fail, got %s", r.Status)
	}
	// Reachability succeeded; only authentication failed. Reporting both as
	// failed would send the operator looking at the network.
	if s := step(t, r, "reachable"); s.Status != StatusOK {
		t.Errorf("the host WAS reachable; expected OK, got %s", s.Status)
	}
	if s := step(t, r, "authenticated"); s.Status != StatusFailed {
		t.Errorf("expected the auth step to fail, got %s", s.Status)
	}
	// Checks that cannot mean anything after auth failed are skipped, not
	// reported as separate failures.
	if s := step(t, r, "can list tags"); s.Status != StatusSkipped {
		t.Errorf("expected the read check to be skipped after an auth failure, got %s", s.Status)
	}
}

func TestGoodCredentialsPassWhereBadOnesFail(t *testing.T) {
	h := newHarness(t, fakeregistry.WithBasicAuth("svc", "correct-horse"))
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))

	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      repository: suite/core
      credentialsRef: {secretName: good}
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repositories: prefix
      repositoryPrefix: mirror
      credentialsRef: {secretName: good}
      default: true
`
	res := h.checker.CheckProduct(t.Context(), h.load(doc))

	if res.Status == StatusFailed {
		t.Fatalf("correct credentials must pass: %+v", res.Repositories)
	}
	if s := step(t, repoNamed(t, res, "vendor"), "credentials"); !strings.Contains(s.Detail, "svc") {
		t.Errorf("the resolved username should be shown so a wrong secret is obvious, got %q", s.Detail)
	}
}

func TestUnreachableHostSkipsDownstreamChecks(t *testing.T) {
	h := newHarness(t)

	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: nope.invalid
      repository: a/b
      anonymous: true
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repositories: prefix
      repositoryPrefix: mirror
      anonymous: true
      default: true
`
	res := h.checker.CheckProduct(t.Context(), h.load(doc))

	r := repoNamed(t, res, "vendor")
	if r.Status != StatusFailed {
		t.Fatalf("an unresolvable host must fail, got %s", r.Status)
	}
	// One cause, one failure. Reporting three would make the operator work
	// through consequences to find the reason.
	failures := 0
	for _, s := range r.Steps {
		if s.Status == StatusFailed {
			failures++
		}
	}
	if failures != 1 {
		t.Errorf("expected exactly 1 failure for 1 cause, got %d: %v", failures, r.Steps)
	}
	if s := step(t, r, "authenticated"); s.Status != StatusSkipped {
		t.Errorf("expected downstream checks skipped, got %s", s.Status)
	}
}

// The loader rejects a missing secret at validation, so this constructs the
// Product directly. Preflight must still handle it: a secret can be deleted or
// rotated away AFTER the configuration loaded, and then this is the check that
// says so.
func TestMissingSecretIsReportedBeforeAnyNetworkCall(t *testing.T) {
	h := newHarness(t)

	p := &product.Product{
		APIVersion: product.APIVersion,
		Kind:       product.Kind,
		Metadata:   product.Metadata{Name: "p"},
		Spec: product.Spec{
			Sources: []product.Source{{
				Name:           "vendor",
				Registry:       h.reg.Host(),
				Repository:     "suite/core",
				CredentialsRef: &product.CredentialsRef{SecretName: "does-not-exist"},
			}},
			Targets: []product.Target{{
				Name: "internal", Registry: h.reg.Host(), Repository: "mirror/p",
				Anonymous: true, Default: true,
			}},
		},
	}
	res := h.checker.CheckProduct(t.Context(), p)

	r := repoNamed(t, res, "vendor")
	if s := step(t, r, "credentials"); s.Status != StatusFailed {
		t.Fatalf("a missing secret must fail the credentials step, got %s", s.Status)
	}
	// Nothing downstream can mean anything, and dialling the registry to
	// discover that would be a wasted round trip.
	if s := step(t, r, "reachable"); s.Status != StatusSkipped {
		t.Errorf("expected reachability skipped when credentials cannot resolve, got %s", s.Status)
	}
}

// A source that names no repositories is checked for the permission it
// actually needs: listing the registry.
func TestCatalogPermissionIsCheckedWhenSourceEnumerates(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))

	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      anonymous: true
      discovery:
        repositoryFilters: {include: ['^suite/']}
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repositories: prefix
      repositoryPrefix: mirror
      anonymous: true
      default: true
`
	res := h.checker.CheckProduct(t.Context(), h.load(doc))

	s := step(t, repoNamed(t, res, "vendor"), "can list repositories")
	if s.Status != StatusOK {
		t.Errorf("listing should succeed against this registry, got %s: %s", s.Status, s.Detail)
	}
}

// Enumerating with no filters is legitimate on a dedicated registry and a
// mistake on a shared one, and only the operator knows which. So it is not an
// error — it is a warning carrying the number that decides it.
func TestUnfilteredEnumerationWarnsWithTheCount(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage("otherteam/thing", "v1.0.0", fakeregistry.NewLayer("b"))
	h.reg.AddImage("someoneelse/app", "v1.0.0", fakeregistry.NewLayer("c"))

	doc := `
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      anonymous: true
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repositories: prefix
      repositoryPrefix: mirror
      anonymous: true
      default: true
`
	res := h.checker.CheckProduct(t.Context(), h.load(doc))

	s := step(t, repoNamed(t, res, "vendor"), "can list repositories")
	if s.Status != StatusWarning {
		t.Fatalf("an unfiltered enumeration should warn, got %s", s.Status)
	}
	if !strings.Contains(s.Detail, "3 repositories") {
		t.Errorf("the count is the fact that decides it; got %q", s.Detail)
	}
	if !strings.Contains(s.Hint, "repositoryFilters") {
		t.Errorf("the hint should name the fix, got %q", s.Hint)
	}
}

// Not checked when the source names its repositories — probing a permission
// the product does not need would report a failure that does not matter.
func TestCatalogNotCheckedWhenRepositoriesAreNamed(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage("suite/database", "v2.0.0", fakeregistry.NewLayer("b"))

	res := h.checker.CheckProduct(t.Context(), h.load(anonDoc))

	for _, r := range res.Repositories {
		for _, s := range r.Steps {
			if s.Name == "can list repositories" {
				t.Error("registry-listing permission must not be probed when repositories are named")
			}
		}
	}
}

// Anonymous access to a registry that requires auth must be reported as an
// auth problem with the fix, not as a generic failure.
func TestAnonymousAgainstAuthenticatedRegistry(t *testing.T) {
	h := newHarness(t, fakeregistry.WithBasicAuth("svc", "correct-horse"))
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))

	doc := strings.Replace(anonDoc, "[suite/core, suite/database]", "[suite/core]", 1)
	res := h.checker.CheckProduct(t.Context(), h.load(doc))

	r := repoNamed(t, res, "vendor")
	s := step(t, r, "authenticated")
	if s.Status != StatusFailed {
		t.Fatalf("anonymous against an authenticated registry must fail, got %s", s.Status)
	}
	if !strings.Contains(s.Hint, "credentialsRef") {
		t.Errorf("the hint should name the fix, got %q", s.Hint)
	}
}

// Results are ordered deterministically, so two runs of the same configuration
// are diffable.
func TestResultsAreStablyOrdered(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage("suite/database", "v2.0.0", fakeregistry.NewLayer("b"))
	p := h.load(anonDoc)

	first := h.checker.CheckProduct(t.Context(), p)
	for range 3 {
		next := h.checker.CheckProduct(t.Context(), p)
		if len(next.Repositories) != len(first.Repositories) {
			t.Fatal("result length changed between runs")
		}
		for i := range first.Repositories {
			if next.Repositories[i].Repository != first.Repositories[i].Repository {
				t.Fatalf("order changed between runs at %d: %q vs %q",
					i, next.Repositories[i].Repository, first.Repositories[i].Repository)
			}
		}
	}
}

// TestInsecureSkipVerifyIsReportedAsAWarning: the setting has to be impossible
// to leave switched on by accident, and the report is where an operator looks.
//
// The fake registry is plain HTTP, so nothing here exercises a handshake — the
// point is only that the configuration reaches the report. The handshake itself
// is covered in internal/registry/transport.
func TestInsecureSkipVerifyIsReportedAsAWarning(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))

	p := h.load(`
apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata: {name: p}
spec:
  sources:
    - name: vendor
      registry: REGISTRY_HOST
      repository: suite/core
      anonymous: true
      network:
        tls: {insecureSkipVerify: true}
  targets:
    - name: internal
      registry: REGISTRY_HOST
      repositories: prefix
      repositoryPrefix: mirror
      anonymous: true
      default: true
`)

	res := h.checker.CheckProduct(t.Context(), p)
	if res.Status != StatusWarning {
		t.Errorf("product status = %s, want WARNING", res.Status)
	}

	s := step(t, repoNamed(t, res, "vendor"), "certificate verification")
	if s.Status != StatusWarning {
		t.Errorf("step status = %s, want WARNING", s.Status)
	}
	// The correction has to travel with the warning: this is exactly where an
	// operator chasing a negative-serial failure will read it.
	if !strings.Contains(s.Hint, "negative serial") {
		t.Errorf("the hint should say this does not fix negative serial numbers, got: %q", s.Hint)
	}
}

// The step must be ABSENT, not present-and-OK, when verification is on. A step
// that is always there trains people to stop reading it.
func TestNoCertificateStepWhenVerificationIsOn(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage("suite/database", "v2.0.0", fakeregistry.NewLayer("b"))

	res := h.checker.CheckProduct(t.Context(), h.load(anonDoc))
	for _, r := range res.Repositories {
		for _, s := range r.Steps {
			if s.Name == "certificate verification" {
				t.Errorf("%s: unexpected certificate step %+v", r.Name, s)
			}
		}
	}
}

// TestManifestFetchIsProbed is the step that separates "listing works" from
// "discovery will work". A registry can serve tags/list in milliseconds and
// stall every manifest GET behind it — observed in the field, through a proxy
// that inspects response bodies.
func TestManifestFetchIsProbed(t *testing.T) {
	h := newHarness(t)
	h.reg.AddImage("suite/core", "v1.0.0", fakeregistry.NewLayer("a"))
	h.reg.AddImage("suite/database", "v2.0.0", fakeregistry.NewLayer("b"))

	res := h.checker.CheckProduct(t.Context(), h.load(anonDoc))

	src := repoNamed(t, res, "vendor")
	s := step(t, src, "can fetch a manifest")
	if s.Status != StatusOK {
		t.Errorf("step = %s: %s", s.Status, s.Detail)
	}

	// A target is not probed: it has nothing to read until the first push, and
	// a failure there would be noise on every correctly-configured destination.
	for _, r := range res.Repositories {
		if r.Role != string(product.RoleTarget) {
			continue
		}
		for _, st := range r.Steps {
			if st.Name == "can fetch a manifest" {
				t.Errorf("targets must not be manifest-probed, got %+v", st)
			}
		}
	}
}
