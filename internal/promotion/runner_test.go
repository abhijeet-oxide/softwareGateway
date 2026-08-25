package promotion

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/promote"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
)

// The runner's whole job is to be RECOVERABLE and HONEST: finish what an
// interrupted Coordinator started, and settle on what the registry serves
// rather than on what it said.
//
// Everything here is driven through the real store against SQLite, because
// resumability is a property of the rows rather than of the code path - a
// runner tested against an in-memory fake would prove nothing about the thing
// that actually has to survive a restart.

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	labRepo  = "docker-lab/nokia"
	prodRepo = "docker-prod/nokia"
	rootTag  = "23.5.0"
	rootPath = "orbs/cfx"
	rootDgst = "sha256:aaaa"
)

type fixture struct {
	t      *testing.T
	store  *store.Promotions
	db     store.Store
	runner *Runner

	promoter *recordingPromoter
	reader   *fakeReader
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	s, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "promotion.db"),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := store.Migrate(t.Context(), s, nil); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seed(t, s)

	promoter := &recordingPromoter{claims: true}
	reader := &fakeReader{digests: map[string]string{
		"jfrog-prod|" + rootPath + "|" + rootTag: rootDgst,
	}}

	f := &fixture{
		t: t, db: s, store: store.NewPromotions(s),
		promoter: promoter, reader: reader,
	}
	// The double is what the plugin registry resolves for the duration of this
	// test. Registered ONCE from init below, exactly as a real plugin is - so
	// what runs here is the real chain, not a seam that exists for tests.
	current = promoter

	f.runner = NewRunner(NewService(loadedProducts{}, nil, nil), f.store,
		store.NewPackages(s), reader, nil)
	return f
}

// open records a promotion the expander would have claimed.
func (f *fixture) open() {
	f.t.Helper()
	if err := f.store.Open(f.t.Context(), "t1", "fake", []store.PromotionName{
		{Repository: rootPath, Tag: rootTag, Digest: rootDgst},
		{Repository: rootPath + "/nginx", Tag: "1.2.3", Digest: "sha256:bbbb"},
		{Repository: rootPath + "/redis", Tag: "7.0", Digest: "sha256:cccc"},
	}); err != nil {
		f.t.Fatalf("open promotion: %v", err)
	}
}

func (f *fixture) transferState() string {
	f.t.Helper()
	var state string
	if err := f.db.DB().QueryRowContext(f.t.Context(),
		`SELECT state FROM transfers WHERE id = 't1'`).Scan(&state); err != nil {
		f.t.Fatal(err)
	}
	return state
}

func (f *fixture) promotion() store.Promotion {
	f.t.Helper()
	pm, err := f.store.ForTransfer(f.t.Context(), "t1")
	if err != nil {
		f.t.Fatalf("read promotion: %v", err)
	}
	return pm
}

func seed(t *testing.T, s store.Store) {
	t.Helper()
	for _, stmt := range []string{
		`INSERT INTO products (id,name,config_hash,config) VALUES (1,'nokia','h','{}')`,
		`INSERT INTO repositories (id,product_id,role,name,registry_host,repository_path,registry_type)
		 VALUES (1,1,'target','jfrog-lab','acme.jfrog.io','` + labRepo + `','artifactory'),
		        (2,1,'target','jfrog-prod','acme.jfrog.io','` + prodRepo + `','artifactory')`,
		`INSERT INTO packages (id,product_id,source_repo_id,tag,manifest_digest,media_type)
		 VALUES (1,1,1,'` + rootTag + `','` + rootDgst + `','application/json')`,
		`INSERT INTO transfer_requests (id,product_id,package_id,operation,source_repo_id,idempotency_key)
		 VALUES ('r1',1,1,'promote',1,'k1')`,
		`INSERT INTO transfers (id,request_id,package_id,source_repo_id,target_repo_id,state)
		 VALUES ('t1','r1',1,1,2,'planning')`,
	} {
		if _, err := s.DB().ExecContext(t.Context(), stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}
}

// ---------------------------------------------------------------------------
// Doubles
// ---------------------------------------------------------------------------

// recordingPromoter claims everything and remembers what it was asked to do.
type recordingPromoter struct {
	mu    sync.Mutex
	names []string
	// failOn is the name that fails, as "<repository>:<tag>". Empty means none.
	failOn string
	// claims is what Claim answers. False only for the placeholder the plugin
	// hands out when no test has set one up.
	claims bool
}

func (p *recordingPromoter) Name() string { return "fake" }

func (p *recordingPromoter) Claim(promote.Hop) promote.Verdict {
	return promote.Verdict{Promoter: "fake", Claimed: p.claims, Reason: "mine"}
}

func (p *recordingPromoter) Promote(_ context.Context, h promote.Hop) (promote.Outcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, n := range h.Names {
		key := n.Repository + ":" + n.Tag
		if key == p.failOn {
			return promote.Outcome{}, errors.New("Artifactory said no")
		}
		p.names = append(p.names, key)
	}
	return promote.Outcome{Promoter: "fake", Promoted: len(h.Names)}, nil
}

func (p *recordingPromoter) promoted() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.names...)
}

// fakeReader is the destination, as verification sees it.
type fakeReader struct {
	digests map[string]string
	err     error
}

func (r *fakeReader) ResolveAt(
	_ context.Context, _, target, path, tag string,
) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	got, ok := r.digests[target+"|"+path+"|"+tag]
	if !ok {
		return "", registry.ErrNotFound
	}
	return got, nil
}

// current is the promoter the registered plugin hands out.
//
// A package-level handle rather than a constructor argument, because a plugin
// is built by the registry from a Config and has no other way to reach a test.
// These tests do not run in parallel, which is what keeps it a fixture rather
// than a race.
var current *recordingPromoter

func init() {
	promote.Register("fake", func(promote.Config) (promote.Promoter, error) {
		if current == nil {
			// Claiming nothing is the honest answer outside a test that set
			// one up, and it keeps this registration from deciding what any
			// other test in the package resolves to.
			return &recordingPromoter{claims: false}, nil
		}
		return current, nil
	})
}

// loadedProducts is the two targets, without a configuration loader.
type loadedProducts struct{}

func (loadedProducts) Get(name string) (*product.Product, bool) {
	if name != "nokia" {
		return nil, false
	}
	p := &product.Product{}
	p.Metadata.Name = "nokia"
	p.Spec.Targets = []product.Target{
		{Name: "jfrog-lab", Registry: "acme.jfrog.io", Repository: labRepo,
			Type: product.RegistryJFrog, Anonymous: true},
		{Name: "jfrog-prod", Registry: "acme.jfrog.io", Repository: prodRepo,
			Type: product.RegistryJFrog, Anonymous: true},
	}
	return p, true
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestNothingToDoIsNotWork(t *testing.T) {
	f := newFixture(t)
	did, err := f.runner.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if did {
		t.Error("an empty tick must report that it did nothing, or the loop never backs off")
	}
}

func TestEveryNameIsPublishedAndTheTransferSucceeds(t *testing.T) {
	f := newFixture(t)
	f.open()

	if did, err := f.runner.Tick(t.Context()); err != nil || !did {
		t.Fatalf("Tick: did=%v err=%v", did, err)
	}

	// One call per name, in the order the tree gave - the root first, so a
	// promotion interrupted part way has published a consistent prefix.
	want := []string{
		rootPath + ":" + rootTag,
		rootPath + "/nginx:1.2.3",
		rootPath + "/redis:7.0",
	}
	if got := f.promoter.promoted(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("promoted %v, want %v", got, want)
	}

	if state := f.transferState(); state != "succeeded" {
		t.Errorf("transfer is %q, want succeeded", state)
	}
	if pm := f.promotion(); pm.State != "succeeded" || pm.NamesDone != 3 {
		t.Errorf("promotion is %s with %d of %d done", pm.State, pm.NamesDone, pm.NamesTotal)
	}
}

// The point of recording names as they land. A promotion interrupted half way
// through a 260-name release must re-issue what is LEFT, not the whole thing.
func TestAResumedPromotionOnlyPublishesWhatIsLeft(t *testing.T) {
	f := newFixture(t)
	f.open()

	f.promoter.failOn = rootPath + "/nginx:1.2.3"
	if _, err := f.runner.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if state := f.transferState(); state != "failed" {
		t.Fatalf("transfer is %q, want failed", state)
	}
	if got := f.promoter.promoted(); len(got) != 1 {
		t.Fatalf("published %v, want only the root", got)
	}

	// Somebody retries: the expander reopens the promotion, and the second
	// run picks up at the name that failed.
	f.promoter.failOn = ""
	f.open()
	if _, err := f.runner.Tick(t.Context()); err != nil {
		t.Fatalf("Tick again: %v", err)
	}

	if state := f.transferState(); state != "succeeded" {
		t.Errorf("transfer is %q, want succeeded", state)
	}
	// The root was published ONCE. Re-issuing it would be correct but wasteful,
	// and on a real release it is two hundred calls to discover nothing.
	got := f.promoter.promoted()
	if count(got, rootPath+":"+rootTag) != 1 {
		t.Errorf("the root was published %d times: %v", count(got, rootPath+":"+rootTag), got)
	}
	if len(got) != 3 {
		t.Errorf("published %v, want three names in total", got)
	}
}

// A promotion is settled on what the registry SERVES. A 200 says Artifactory
// did something, not that it did this.
func TestAMissingRootFailsThePromotionHoweverItWasReported(t *testing.T) {
	f := newFixture(t)
	f.open()
	f.reader.digests = map[string]string{} // the destination serves nothing

	if _, err := f.runner.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if state := f.transferState(); state != "failed" {
		t.Fatalf("transfer is %q, want failed", state)
	}
	if reason := f.promotion().LastError; !strings.Contains(reason, "not present") {
		t.Errorf("the failure must say the release is not there; got %q", reason)
	}
}

// The one failure that would otherwise be invisible: the promotion is reported
// as done and the tag resolves to the PREVIOUS version. Nobody notices until a
// cluster pulls it.
func TestADifferentDigestAtTheDestinationIsAFailureNotADivergence(t *testing.T) {
	f := newFixture(t)
	f.open()
	f.reader.digests["jfrog-prod|"+rootPath+"|"+rootTag] = "sha256:something-else"

	if _, err := f.runner.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if state := f.transferState(); state != "failed" {
		// Deliberately NOT `diverged`: that belongs to a mirror following an
		// upstream tag that moved, and this is a copy we asked for.
		t.Fatalf("transfer is %q, want failed", state)
	}
	if reason := f.promotion().LastError; !strings.Contains(reason, "was not promoted") {
		t.Errorf("the failure must say the release did not arrive; got %q", reason)
	}
}

// An unreachable destination is not a wrong one. The promotion is left open so
// the next tick can verify again rather than being condemned for a network
// blip on the last step.
func TestAnUnreadableDestinationDoesNotCondemnThePromotion(t *testing.T) {
	f := newFixture(t)
	f.open()
	f.reader.err = errors.New("connection refused")

	if _, err := f.runner.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	// It settles as failed - a failed transfer is visible and retryable, which
	// is the whole reason the runner does not loop silently - but the names
	// stay recorded, so the retry costs nothing.
	if state := f.transferState(); state != "failed" {
		t.Fatalf("transfer is %q, want failed", state)
	}
	if pm := f.promotion(); pm.NamesDone != 3 {
		t.Errorf("%d of %d names recorded; a verification failure must not undo them",
			pm.NamesDone, pm.NamesTotal)
	}

	f.reader.err = nil
	f.open()
	if _, err := f.runner.Tick(t.Context()); err != nil {
		t.Fatalf("Tick again: %v", err)
	}
	if state := f.transferState(); state != "succeeded" {
		t.Errorf("transfer is %q, want succeeded", state)
	}
	if got := len(f.promoter.promoted()); got != 3 {
		t.Errorf("%d names were published in total; the retry re-issued work", got)
	}
}

func count(in []string, want string) int {
	n := 0
	for _, s := range in {
		if s == want {
			n++
		}
	}
	return n
}

var _ transfer.Promotion = (*Service)(nil)
