package replication

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
)

var applyTime = time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)

func mirrorTarget() product.Target {
	return product.Target{
		Name: "ocp-prod", Registry: "quay.apps.ocp.example.com",
		Repository: "platform-prod/vendor-a-platform", Type: product.RegistryQuay,
		Replication: &product.Replication{
			Mode: product.ReplicationMirror,
			Mirror: &product.MirrorConfig{
				From:          "jfrog-store",
				Tags:          []string{"v3.*", "sha256-*.sig"},
				Interval:      product.Duration(6 * time.Hour),
				Robot:         "platform-prod+swgw-mirror",
				SkopeoTimeout: product.Duration(300 * time.Second),
				Network: &product.MirrorNetwork{
					Proxy: &product.MirrorProxy{
						HTTPSProxy: "http://proxy:3128",
						NoProxy:    []string{"a.example.com", "b.example.com"},
					},
				},
			},
		},
	}
}

func resolveMirror(t *testing.T) *Desired {
	t.Helper()
	d, err := Resolve(mirrorTarget(), "artifactory.example.com/docker-local/vendor-a",
		Credentials{Username: "reader", Password: "s3cret"}, applyTime)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return d
}

// Everything the two sides name differently, converted in one place.
func TestResolveTranslatesToQuaysVocabulary(t *testing.T) {
	d := resolveMirror(t)

	if d.Namespace != "platform-prod" || d.Name != "vendor-a-platform" {
		t.Fatalf("coordinates = %s/%s", d.Namespace, d.Name)
	}
	m := d.Mirror
	if m.SyncInterval != 21600 {
		t.Fatalf("6h must become 21600 seconds, got %d", m.SyncInterval)
	}
	if m.SkopeoTimeoutInterval != 300 {
		t.Fatalf("skopeo timeout = %d", m.SkopeoTimeoutInterval)
	}
	if m.RootRule.RuleKind != quay.RuleKindTagGlobCSV {
		t.Fatalf("rule kind = %q", m.RootRule.RuleKind)
	}
	// A list in our schema because a list is reviewable; a comma-joined string
	// on the wire because that is what Quay takes.
	if got := m.ExternalRegistryConfig.Proxy.NoProxy; got != "a.example.com,b.example.com" {
		t.Fatalf("noProxy = %q", got)
	}
	if m.ExtRef != "artifactory.example.com/docker-local/vendor-a" {
		t.Fatalf("external reference = %q", m.ExtRef)
	}
	if m.ExternalRegistryPassword != "s3cret" {
		t.Fatal("the upstream credential must be sent; Quay needs it to pull")
	}
}

// Quay requires a start date and our configuration usually does not give one.
func TestStartDateDefaultsToApplyTime(t *testing.T) {
	d := resolveMirror(t)
	if d.Mirror.SyncStartDate != "2026-09-01T02:00:00Z" {
		t.Fatalf("sync start date = %q", d.Mirror.SyncStartDate)
	}

	tgt := mirrorTarget()
	tgt.Replication.Mirror.StartAt = "2027-01-01T00:00:00Z"
	d2, err := Resolve(tgt, "up.example.com/x", Credentials{}, applyTime)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if d2.Mirror.SyncStartDate != "2027-01-01T00:00:00Z" {
		t.Fatalf("a configured startAt must win, got %q", d2.Mirror.SyncStartDate)
	}
}

func TestResolveRejectsNonQuayRepositoryShape(t *testing.T) {
	tgt := mirrorTarget()
	tgt.Repository = "flat"
	if _, err := Resolve(tgt, "up.example.com/x", Credentials{}, applyTime); err == nil {
		t.Fatal("a repository with no organization must be rejected")
	}
}

func TestUpstreamResolution(t *testing.T) {
	p := &product.Product{Spec: product.Spec{Targets: []product.Target{
		{Name: "jfrog-store", Registry: "artifactory.example.com", Repository: "docker-local/vendor-a"},
		mirrorTarget(),
	}}}
	got, err := ResolveUpstream(p, p.Spec.Targets[1])
	if err != nil {
		t.Fatalf("ResolveUpstream: %v", err)
	}
	if got != "artifactory.example.com/docker-local/vendor-a" {
		t.Fatalf("upstream = %q", got)
	}

	// externalReference is taken as written.
	tgt := mirrorTarget()
	tgt.Replication.Mirror.From = ""
	tgt.Replication.Mirror.ExternalReference = "elsewhere.example.com/path"
	got, err = ResolveUpstream(p, tgt)
	if err != nil || got != "elsewhere.example.com/path" {
		t.Fatalf("ResolveUpstream = (%q, %v)", got, err)
	}
}

// ---------------------------------------------------------------------------
// Hashing: the whole basis of drift detection
// ---------------------------------------------------------------------------

// The start date defaults to the apply time, so including it would make every
// hash differ from the last and report drift on a document nobody had touched.
func TestConfigHashIgnoresApplyTime(t *testing.T) {
	a := resolveMirror(t)
	later, err := Resolve(mirrorTarget(), "artifactory.example.com/docker-local/vendor-a",
		Credentials{Username: "reader", Password: "s3cret"}, applyTime.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if a.ConfigHash() != later.ConfigHash() {
		t.Fatal("two applies of the same configuration must hash identically")
	}
}

func TestConfigHashChangesWithConfiguration(t *testing.T) {
	base := resolveMirror(t).ConfigHash()
	for name, mutate := range map[string]func(*product.Target){
		"tags":     func(t *product.Target) { t.Replication.Mirror.Tags = []string{"v4.*"} },
		"interval": func(t *product.Target) { t.Replication.Mirror.Interval = product.Duration(time.Hour) },
		"robot":    func(t *product.Target) { t.Replication.Mirror.Robot = "platform-prod+other" },
		"proxy": func(t *product.Target) {
			t.Replication.Mirror.Network.Proxy.HTTPSProxy = "http://other:3128"
		},
		"enabled": func(t *product.Target) { off := false; t.Replication.Mirror.Enabled = &off },
	} {
		tgt := mirrorTarget()
		mutate(&tgt)
		d, err := Resolve(tgt, "artifactory.example.com/docker-local/vendor-a", Credentials{Username: "reader"}, applyTime)
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		if d.ConfigHash() == base {
			t.Fatalf("changing %s must change the config hash", name)
		}
	}
}

// Quay never returns a stored password, so a rotation is only detectable from
// what we sent. Hashing it means the value itself is never stored.
func TestSecretHashDetectsRotation(t *testing.T) {
	a := resolveMirror(t)
	rotated, err := Resolve(mirrorTarget(), "artifactory.example.com/docker-local/vendor-a",
		Credentials{Username: "reader", Password: "NEW"}, applyTime)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if a.SecretHash == rotated.SecretHash {
		t.Fatal("a rotated password must change the secret hash")
	}
	if strings.Contains(a.SecretHash, "s3cret") {
		t.Fatal("the hash must not contain the value")
	}
	if a.ConfigHash() != rotated.ConfigHash() {
		t.Fatal("a password change must not move the CONFIG hash; that is what keeps the two questions separate")
	}
}

// ---------------------------------------------------------------------------
// Drift
// ---------------------------------------------------------------------------

func observedFrom(spec *quay.MirrorSpec) *quay.MirrorState {
	return &quay.MirrorState{
		IsEnabled: spec.IsEnabled, ExtRef: spec.ExtRef,
		ExternalRegistryUsername: spec.ExternalRegistryUsername,
		SyncInterval:             spec.SyncInterval,
		RobotUsername:            spec.RobotUsername,
		RootRule:                 spec.RootRule,
		ExternalRegistryConfig:   spec.ExternalRegistryConfig,
		SkopeoTimeoutInterval:    spec.SkopeoTimeoutInterval,
		RawSyncStatus:            "SUCCESS",
		// Deliberately different from what we sent: Quay normalises it, and
		// comparing it would report permanent drift.
		SyncStartDate: "2020-01-01T00:00:00Z",
	}
}

func TestNoDriftWhenRegistryAgrees(t *testing.T) {
	d := resolveMirror(t)
	got := DiffMirror(d.Mirror, observedFrom(d.Mirror), "MIRROR", d.SecretHash, d.SecretHash)
	if got.Drifted() {
		t.Fatalf("expected no drift, got %s (%v)", got.Summary(), got.Fields)
	}
	if got.Summary() != "in sync" {
		t.Fatalf("summary = %q", got.Summary())
	}
}

// The case the feature exists to catch: somebody edited the mirror in the
// Quay UI.
func TestDriftWhenSomebodyEditsQuayByHand(t *testing.T) {
	d := resolveMirror(t)
	obs := observedFrom(d.Mirror)
	obs.RootRule.RuleValue = []string{"v3.*"} // signatures dropped by hand
	obs.SyncInterval = 3600

	got := DiffMirror(d.Mirror, obs, "MIRROR", d.SecretHash, d.SecretHash)
	if !got.Drifted() {
		t.Fatal("expected drift")
	}
	fields := map[string]FieldDiff{}
	for _, f := range got.Fields {
		fields[f.Field] = f
	}
	if _, ok := fields["tags"]; !ok {
		t.Fatalf("tags must be reported, got %v", got.Fields)
	}
	iv, ok := fields["interval"]
	if !ok {
		t.Fatalf("interval must be reported, got %v", got.Fields)
	}
	// Rendered in the reader's units, not Quay's seconds.
	if iv.Want != "6h0m0s" || iv.Got != "1h0m0s" {
		t.Fatalf("interval diff must render as durations, got want=%q got=%q", iv.Want, iv.Got)
	}
}

// Quay does not promise to return globs in the order they were sent.
func TestReorderedTagsAreNotDrift(t *testing.T) {
	d := resolveMirror(t)
	obs := observedFrom(d.Mirror)
	obs.RootRule.RuleValue = []string{"sha256-*.sig", "v3.*"}

	if got := DiffMirror(d.Mirror, obs, "MIRROR", d.SecretHash, d.SecretHash); got.Drifted() {
		t.Fatalf("a reordered list is not drift, got %v", got.Fields)
	}
}

// The classic reconciler bug: comparing against a redacted password and
// reporting permanent, unfixable drift until people stop reading the signal.
func TestRedactedPasswordIsNotDrift(t *testing.T) {
	d := resolveMirror(t)
	obs := observedFrom(d.Mirror)
	// Quay returns the username and nothing for the password.
	got := DiffMirror(d.Mirror, obs, "MIRROR", d.SecretHash, d.SecretHash)
	for _, f := range got.Fields {
		if strings.Contains(strings.ToLower(f.Field), "password") {
			t.Fatalf("a password must never appear in a diff: %v", f)
		}
	}
	if got.CredentialsRotated {
		t.Fatal("an unchanged credential must not read as rotated")
	}
}

func TestNeverAppliedDoesNotReportRotation(t *testing.T) {
	d := resolveMirror(t)
	got := DiffMirror(d.Mirror, observedFrom(d.Mirror), "MIRROR", "", d.SecretHash)
	if got.CredentialsRotated {
		t.Fatal("with no previous apply there is nothing to have rotated from")
	}
}

// A mirror configuration on a NORMAL repository is inert: it looks configured
// and syncs nothing.
func TestWrongRepositoryStateIsDrift(t *testing.T) {
	d := resolveMirror(t)
	got := DiffMirror(d.Mirror, observedFrom(d.Mirror), "NORMAL", d.SecretHash, d.SecretHash)
	if !got.StateWrong || !got.Drifted() {
		t.Fatal("a repository that is not in MIRROR state is drift")
	}
	if !strings.Contains(got.Summary(), "NORMAL") {
		t.Fatalf("summary should name the actual state, got %q", got.Summary())
	}
}

func TestUnreadStateIsNotReportedAsWrong(t *testing.T) {
	d := resolveMirror(t)
	if got := DiffMirror(d.Mirror, observedFrom(d.Mirror), "", d.SecretHash, d.SecretHash); got.StateWrong {
		t.Fatal("not having read the state is not the same as the state being wrong")
	}
}

func TestAbsentMirrorIsItsOwnAnswer(t *testing.T) {
	d := resolveMirror(t)
	got := DiffMirror(d.Mirror, nil, "NORMAL", "", d.SecretHash)
	if !got.Absent || len(got.Fields) != 0 {
		t.Fatalf("an unconfigured mirror is Absent, not a list of field differences: %+v", got)
	}
	if !strings.Contains(got.Summary(), "not configured") {
		t.Fatalf("summary = %q", got.Summary())
	}
}

func TestPendingIsSeparateFromDrift(t *testing.T) {
	if Pending("abc", "abc") {
		t.Fatal("an unchanged configuration is not pending")
	}
	if !Pending("abc", "def") {
		t.Fatal("a changed configuration is pending an apply")
	}
}

// ---------------------------------------------------------------------------
// Plan and Apply
// ---------------------------------------------------------------------------

type fakeQuay struct {
	repo    *quay.Repository
	mirror  *quay.MirrorState
	cache   *quay.ProxyCache
	calls   []string
	syncing bool

	failValidate error
}

func (f *fakeQuay) record(s string) { f.calls = append(f.calls, s) }

func (f *fakeQuay) GetRepository(_ context.Context, ns, name string) (*quay.Repository, error) {
	f.record("GetRepository")
	if f.repo == nil {
		return nil, &registry.Error{Err: registry.ErrNotFound}
	}
	return f.repo, nil
}

func (f *fakeQuay) SetRepositoryState(_ context.Context, ns, name string, s quay.RepositoryState) error {
	f.record("SetRepositoryState:" + string(s))
	if f.repo == nil {
		f.repo = &quay.Repository{}
	}
	f.repo.State = string(s)
	return nil
}

func (f *fakeQuay) GetMirror(_ context.Context, ns, name string) (*quay.MirrorState, error) {
	f.record("GetMirror")
	if f.mirror == nil {
		return nil, &registry.Error{Err: registry.ErrNotFound}
	}
	return f.mirror, nil
}

func (f *fakeQuay) PutMirror(_ context.Context, ns, name string, spec quay.MirrorSpec) error {
	f.record("PutMirror")
	f.mirror = observedFrom(&spec)
	return nil
}

func (f *fakeQuay) SyncNow(context.Context, string, string) (bool, error) {
	f.record("SyncNow")
	if f.syncing {
		return true, nil
	}
	f.syncing = true
	return false, nil
}

func (f *fakeQuay) CancelSync(context.Context, string, string) (bool, error) {
	f.record("CancelSync")
	was := f.syncing
	f.syncing = false
	return was, nil
}

func (f *fakeQuay) GetProxyCache(_ context.Context, org string) (*quay.ProxyCache, error) {
	f.record("GetProxyCache")
	if f.cache == nil {
		return nil, &registry.Error{Err: registry.ErrNotFound}
	}
	return f.cache, nil
}

func (f *fakeQuay) CreateProxyCache(_ context.Context, org string, spec quay.ProxyCache) error {
	f.record("CreateProxyCache")
	f.cache = &spec
	return nil
}

func (f *fakeQuay) DeleteProxyCache(_ context.Context, org string) error {
	f.record("DeleteProxyCache")
	if f.cache == nil {
		return &registry.Error{Err: registry.ErrNotFound}
	}
	f.cache = nil
	return nil
}

func (f *fakeQuay) ValidateProxyCache(_ context.Context, org string, spec quay.ProxyCache) error {
	f.record("ValidateProxyCache")
	return f.failValidate
}

// The plan a human approves has to say the destructive part out loud.
func TestPlanNamesTheDestructiveStep(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	d := resolveMirror(t)

	p, err := PlanApply(context.Background(), api, d, Applied{})
	if err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	if !p.Destructive {
		t.Fatal("flipping a repository into MIRROR state is destructive and must be marked")
	}
	joined := strings.Join(p.Steps, "\n")
	if !strings.Contains(joined, "READ-ONLY") {
		t.Fatalf("the plan must say what MIRROR state does, got:\n%s", joined)
	}
	if !strings.Contains(joined, d.Mirror.RobotUsername) {
		t.Fatalf("the plan must name who will still be able to push, got:\n%s", joined)
	}
}

func TestPlanIsNoOpWhenRegistryAlreadyAgrees(t *testing.T) {
	d := resolveMirror(t)
	api := &fakeQuay{
		repo:   &quay.Repository{State: "MIRROR"},
		mirror: observedFrom(d.Mirror),
	}
	p, err := PlanApply(context.Background(), api, d, Applied{SecretHash: d.SecretHash})
	if err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	if !p.NoOp || p.Destructive {
		t.Fatalf("expected a no-op plan, got %+v", p)
	}
}

// Narrowing a glob deletes tags at the destination and looks harmless in a diff.
func TestNarrowingTagsIsMarkedDestructive(t *testing.T) {
	d := resolveMirror(t)
	obs := observedFrom(d.Mirror)
	obs.RootRule.RuleValue = []string{"*"}
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: obs}

	p, err := PlanApply(context.Background(), api, d, Applied{SecretHash: d.SecretHash})
	if err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	if !p.Destructive {
		t.Fatal("a change to the tag glob converges content and must be marked destructive")
	}
}

func TestPlanForCopyTargetDoesNothing(t *testing.T) {
	d := &Desired{Target: "lab", Mode: product.ReplicationCopy}
	p, err := PlanApply(context.Background(), &fakeQuay{}, d, Applied{})
	if err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	if !p.NoOp {
		t.Fatal("a copy target has nothing to apply")
	}
}

// State before configuration: a mirror written to a NORMAL repository is inert.
func TestApplySetsStateBeforeWritingConfiguration(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	d := resolveMirror(t)

	applied, err := Apply(context.Background(), api, d, applyTime)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var order []string
	for _, c := range api.calls {
		if strings.HasPrefix(c, "SetRepositoryState") || c == "PutMirror" {
			order = append(order, c)
		}
	}
	if len(order) < 2 || order[0] != "SetRepositoryState:MIRROR" || order[1] != "PutMirror" {
		t.Fatalf("call order = %v", order)
	}
	if applied.ConfigHash != d.ConfigHash() || applied.SecretHash != d.SecretHash {
		t.Fatal("a successful apply must record what was sent, so drift can be computed against it")
	}
}

func TestApplyIsIdempotentAgainstItself(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}
	d := resolveMirror(t)

	applied, err := Apply(context.Background(), api, d, applyTime)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	p, err := PlanApply(context.Background(), api, d, applied)
	if err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	if !p.NoOp {
		t.Fatalf("applying twice must be a no-op the second time, got %v", p.Steps)
	}
}

// A proxy cache that cannot reach its upstream fails when a pod pulls. Better
// to find out during the apply.
func TestApplyValidatesProxyCacheBeforeStoringIt(t *testing.T) {
	api := &fakeQuay{failValidate: &registry.Error{Err: registry.ErrUnavailable, Detail: "upstream unreachable"}}
	d, err := Resolve(product.Target{
		Name: "dev-cache", Registry: "quay.example.com", Repository: "dev-cache/vendor-a",
		Type: product.RegistryQuay,
		Replication: &product.Replication{Mode: product.ReplicationProxy, Proxy: &product.ProxyCacheConfig{
			Organization: "dev-cache", UpstreamRegistry: "artifactory.example.com",
		}},
	}, "", Credentials{}, applyTime)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if _, err := Apply(context.Background(), api, d, applyTime); err == nil {
		t.Fatal("expected the apply to fail validation")
	}
	for _, c := range api.calls {
		if c == "CreateProxyCache" {
			t.Fatal("nothing may be stored after validation failed")
		}
	}
}

// Quay has no proxy-cache update, so a change is a delete and a create — and
// between them the cache is empty. The plan has to say so.
func TestProxyCacheReplacementIsExplained(t *testing.T) {
	api := &fakeQuay{cache: &quay.ProxyCache{UpstreamRegistry: "old.example.com", ExpirationS: 3600}}
	d, err := Resolve(product.Target{
		Name: "dev-cache", Registry: "quay.example.com", Repository: "dev-cache/vendor-a",
		Type: product.RegistryQuay,
		Replication: &product.Replication{Mode: product.ReplicationProxy, Proxy: &product.ProxyCacheConfig{
			Organization: "dev-cache", UpstreamRegistry: "artifactory.example.com",
		}},
	}, "", Credentials{}, applyTime)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	p, err := PlanApply(context.Background(), api, d, Applied{})
	if err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	joined := strings.Join(p.Steps, "\n")
	if !strings.Contains(joined, "deleted and recreated") || !p.Destructive {
		t.Fatalf("the plan must explain the empty window, got:\n%s", joined)
	}
}

func TestRequestSyncReportsAlreadyRunning(t *testing.T) {
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, syncing: true}
	d := resolveMirror(t)

	res, err := RequestSync(context.Background(), api, d, applyTime)
	if err != nil {
		t.Fatalf("RequestSync: %v", err)
	}
	if !res.AlreadyRunning {
		t.Fatal("a sync already in progress satisfies the request rather than failing it")
	}
}

func TestSyncOnNonMirrorTargetIsRefusedByName(t *testing.T) {
	d := &Desired{Target: "lab", Mode: product.ReplicationCopy}
	if _, err := RequestSync(context.Background(), &fakeQuay{}, d, applyTime); err == nil {
		t.Fatal("a copy target has no sync to request")
	} else if !strings.Contains(err.Error(), "lab") || !strings.Contains(err.Error(), "copy") {
		t.Fatalf("the error must name the target and its mode, got %v", err)
	}
}

func TestObserveReportsQuaysOwnStatusVerbatim(t *testing.T) {
	d := resolveMirror(t)
	obs := observedFrom(d.Mirror)
	obs.RawSyncStatus = "QUIESCING"
	api := &fakeQuay{repo: &quay.Repository{State: "MIRROR"}, mirror: obs}

	got, err := Observe(context.Background(), api, d, Applied{SecretHash: d.SecretHash})
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got.SyncStatus != quay.SyncUnknown || got.SyncStatusRaw != "QUIESCING" {
		t.Fatalf("status = %q / raw %q", got.SyncStatus, got.SyncStatusRaw)
	}
}

// Observe is called on every reload and from a metrics sweep, so it must never
// write. This is the property that makes continuous DETECTION acceptable when
// continuous reconciliation is not.
func TestObserveNeverWrites(t *testing.T) {
	d := resolveMirror(t)
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}

	if _, err := Observe(context.Background(), api, d, Applied{}); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	for _, c := range api.calls {
		if !strings.HasPrefix(c, "Get") {
			t.Fatalf("Observe performed a mutating call: %s (all: %v)", c, api.calls)
		}
	}
}

func TestPlanNeverWrites(t *testing.T) {
	d := resolveMirror(t)
	api := &fakeQuay{repo: &quay.Repository{State: "NORMAL"}}

	if _, err := PlanApply(context.Background(), api, d, Applied{}); err != nil {
		t.Fatalf("PlanApply: %v", err)
	}
	for _, c := range api.calls {
		if !strings.HasPrefix(c, "Get") {
			t.Fatalf("a dry run performed a mutating call: %s (all: %v)", c, api.calls)
		}
	}
}
