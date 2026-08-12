package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/api"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/queue"
	"github.com/abhijeet-oxide/softwareGateway/internal/regclient"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	"github.com/abhijeet-oxide/softwareGateway/internal/transfer"
	"github.com/abhijeet-oxide/softwareGateway/internal/worker"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// The whole stack, over the wire.
//
// internal/transfer's vertical slice deliberately removes the HTTP hop, so a
// failure there is unambiguously the engine or the queue. This puts it back:
// a real Coordinator router, a real worker Loop polling it over TCP, real
// registries at both ends, and a real product configuration file on disk that
// the worker reads to resolve its credentials.
//
// It is the test that would catch a DTO field that serializes wrong, a route
// registered under the wrong pattern, or a credential the worker cannot
// resolve from the reference the lease response carried — none of which the
// in-process test can see.

func TestPackageTransfersOverTheWorkerPlane(t *testing.T) {
	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New()
	t.Cleanup(dst.Close)

	h := newWireHarness(t, src, dst)

	pkg := h.seedPackage("v2.14.0")
	h.plan(pkg, "wire-transfer")

	// Run the worker until the transfer finishes, or fail with what it was
	// still waiting for. A bounded wait rather than an unbounded one: a stuck
	// queue should fail this test in seconds, not hang CI.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()

	h.waitForTransfer(ctx, "wire-transfer", "succeeded")
	cancel()
	<-done

	if got := dst.TagDigest(targetPath, pkg.Tag); got != pkg.ManifestDigest {
		t.Fatalf("destination tag %s resolves to %s, want %s", pkg.Tag, got, pkg.ManifestDigest)
	}
	if dst.UploadedBytes.Load() == 0 {
		t.Error("no bytes were uploaded to the destination")
	}
}

// ---------------------------------------------------------------------------

const (
	sourcePath = "vendor/suite"
	// targetBase is the target's configured `repository`: a PREFIX beneath
	// which the source's structure is reproduced.
	targetBase  = "mirror"
	targetPath  = targetBase + "/" + sourcePath
	productYAML = `apiVersion: softwaregateway.io/v1alpha1
kind: Product
metadata:
  name: vendor-a
spec:
  sources:
    - name: main
      registry: %s
      repositories: [%s]
      anonymous: true
  targets:
    - name: lab
      registry: %s
      repository: %s
      anonymous: true
`
)

type wireHarness struct {
	t        *testing.T
	src      *fakeregistry.Registry
	dst      *fakeregistry.Registry
	st       store.Store
	packages *store.Packages
	loop     *worker.Loop

	gate *coordinatorGate

	productID int64
	sourceID  int64
	targetID  int64
}

// coordinatorGate makes the control plane come and go.
//
// A worker's relationship with the Coordinator is a POLLING one, and every
// deployment gets the order wrong at least once: the worker starts first, or
// the Coordinator is rolled while workers are up. Neither may need an operator.
// This is what lets a test say so rather than the comments claiming it.
//
// 502 rather than a closed listener because the two are the same thing to the
// client — a non-nil error from the call — and a socket that can be reopened on
// the same address mid-test is a great deal of machinery for no extra coverage.
type coordinatorGate struct {
	up    atomic.Bool
	inner http.Handler
}

func (g *coordinatorGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.up.Load() {
		http.Error(w, "coordinator is not up yet", http.StatusBadGateway)
		return
	}
	g.inner.ServeHTTP(w, r)
}

func newWireHarness(t *testing.T, src, dst *fakeregistry.Registry) *wireHarness {
	t.Helper()

	st, err := store.Open(t.Context(), store.Config{
		Driver: store.DriverSQLite, DSN: filepath.Join(t.TempDir(), "wire.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.Migrate(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	packages := store.NewPackages(st)
	h := &wireHarness{t: t, src: src, dst: dst, st: st, packages: packages}

	res, err := st.DB().ExecContext(t.Context(),
		`INSERT INTO products (name, config_hash, config) VALUES ('vendor-a','h','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	h.productID, _ = res.LastInsertId()
	h.sourceID = h.repo("source", "main", src.Host(), sourcePath)
	h.targetID = h.repo("target", "lab", dst.Host(), targetBase)

	// A real Coordinator router, served over TCP — behind a gate, so a test can
	// take the control plane away and give it back.
	h.gate = &coordinatorGate{inner: api.NewServer(api.Deps{
		Logger:   log,
		Store:    st,
		Packages: packages,
		Queue:    queue.New(packages, 0, log),
	}).Handler()}
	h.gate.up.Store(true)

	server := httptest.NewServer(h.gate)
	t.Cleanup(server.Close)

	// A real product configuration file, read by the worker exactly as it
	// would read a projected ConfigMap. This is what resolves the credential
	// REFERENCE the lease response carries into an actual client.
	products := h.loadProducts()

	h.loop = worker.NewLoop(
		v1.NewClient(server.URL),
		regclient.NewClients(products, product.NewSecretResolver(t.TempDir()), "", log),
		worker.Options{
			WorkerID:          "worker-wire-1",
			MaxConcurrentJobs: 4,
			HeartbeatInterval: time.Second,
			ProgressInterval:  50 * time.Millisecond,
		},
		log,
	)
	return h
}

func (h *wireHarness) loadProducts() *product.Registry {
	h.t.Helper()

	dir := h.t.TempDir()
	body := fmt.Sprintf(productYAML,
		h.src.Host(), sourcePath, h.dst.Host(), targetBase)
	if err := os.WriteFile(filepath.Join(dir, "vendor-a.yaml"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}

	loaded, err := product.NewLoader(dir, product.NewSecretResolver(h.t.TempDir())).Load()
	if err != nil {
		h.t.Fatal(err)
	}
	for _, bad := range loaded.Invalid {
		h.t.Fatalf("the test's own product configuration is invalid: %s: %v", bad.File, bad.Err)
	}
	if len(loaded.Valid) == 0 {
		h.t.Fatal("no product loaded")
	}

	reg := product.NewRegistry()
	reg.Swap(loaded)
	return reg
}

func (h *wireHarness) repo(role, name, host, path string) int64 {
	h.t.Helper()

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.EnsureRepository(h.t.Context(), tx, h.productID, role, name,
		host, path, "generic", "config", "")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *wireHarness) seedPackage(tag string) store.PackageRow {
	h.t.Helper()

	shared := fakeregistry.NewLayer("a base layer shared by both platforms")
	amd := h.src.AddImage(sourcePath, "", shared, fakeregistry.NewLayer("amd64 payload"))
	arm := h.src.AddImage(sourcePath, "", shared, fakeregistry.NewLayer("arm64 payload"))

	raw, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": registry.MediaTypeOCIIndex,
		"manifests": []map[string]any{
			{"mediaType": registry.MediaTypeOCIManifest, "digest": amd, "size": 100},
			{"mediaType": registry.MediaTypeOCIManifest, "digest": arm, "size": 100},
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	root := h.src.AddManifest(sourcePath, tag, raw, registry.MediaTypeOCIIndex)

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	id, err := h.packages.InsertPackage(h.t.Context(), tx, store.PackageRow{
		ProductID: h.productID, SourceRepoID: h.sourceID, Tag: tag,
		ManifestDigest: root, MediaType: registry.MediaTypeOCIIndex, ArtifactCount: 3,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}

	row, err := h.packages.GetPackageByID(h.t.Context(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return row
}

func (h *wireHarness) plan(pkg store.PackageRow, transferID string) {
	h.t.Helper()

	if _, err := h.st.DB().ExecContext(h.t.Context(),
		`INSERT INTO transfer_requests (id, product_id, package_id, operation, source_repo_id, idempotency_key)
		 VALUES (?, ?, ?, 'replicate', ?, ?)`,
		"req-"+transferID, h.productID, pkg.ID, h.sourceID, "key-"+transferID); err != nil {
		h.t.Fatal(err)
	}

	tx, err := h.st.DB().BeginTx(h.t.Context(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := h.packages.CreateTransfer(h.t.Context(), tx, store.TransferRow{
		ID: transferID, RequestID: "req-" + transferID, PackageID: pkg.ID,
		SourceRepoID: h.sourceID, TargetRepoID: h.targetID, Priority: 50,
	}); err != nil {
		h.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		h.t.Fatal(err)
	}

	client, err := generic.New(generic.Config{
		Registry: h.src.Host(), Repository: sourcePath, PlainHTTP: true,
	})
	if err != nil {
		h.t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := transfer.NewPlanner(h.packages, 4, log).Plan(h.t.Context(), transfer.Request{
		TransferID: transferID, RequestID: "req-" + transferID,
		Package: pkg, SourceRepoID: h.sourceID, TargetRepoID: h.targetID,
		ProductID: h.productID, TargetName: "lab", TargetRegistry: h.dst.Host(),
		TargetRegistryType: "generic", TargetBasePath: targetBase,
		SourceRepository: sourcePath,
		Priority:         50, Source: client,
	}); err != nil {
		h.t.Fatal(err)
	}
}

// waitForTransfer polls until the transfer reaches a state, or the context
// expires — reporting what it was actually waiting on rather than just timing
// out, because "want succeeded, got running with 3 pending jobs" is a
// diagnosis and "timeout" is not.
func (h *wireHarness) waitForTransfer(ctx context.Context, id, want string) {
	h.t.Helper()

	for {
		var state string
		if err := h.st.DB().QueryRowContext(ctx,
			`SELECT state FROM transfers WHERE id = ?`, id).Scan(&state); err == nil {
			if state == want {
				return
			}
			if state == "failed" {
				h.t.Fatalf("transfer failed: %s", h.jobErrors(ctx, id))
			}
		}

		select {
		case <-ctx.Done():
			h.t.Fatalf("transfer never reached %q; outstanding work: %s",
				want, h.jobErrors(ctx, id))
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// jobErrors summarises what is left, for a failure message worth reading.
func (h *wireHarness) jobErrors(ctx context.Context, transferID string) string {
	rows, err := h.st.DB().QueryContext(context.WithoutCancel(ctx),
		`SELECT kind, state, COALESCE(last_error,'') FROM jobs WHERE transfer_id = ?
		  AND state NOT IN ('succeeded','skipped')`, transferID)
	if err != nil {
		return "could not read jobs: " + err.Error()
	}
	defer func() { _ = rows.Close() }()

	out := ""
	for rows.Next() {
		var kind, state, lastErr string
		if err := rows.Scan(&kind, &state, &lastErr); err != nil {
			break
		}
		out += "\n  " + kind + " " + state + " " + lastErr
	}
	if out == "" {
		return "none"
	}
	return out
}

// A worker started before the Coordinator, and a Coordinator restarted under a
// running worker. Neither may need an operator, and the deployment gets the
// order wrong at least once by definition.
//
// What must hold, and each of these has been assumed rather than asserted:
//
//   - the worker does not exit when the control plane is absent. It polls.
//   - it does not need restarting when the control plane returns.
//   - the work it could not lease is still there, and it finishes.
func TestAWorkerOutlivesACoordinatorThatIsNotThereYet(t *testing.T) {
	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New()
	t.Cleanup(dst.Close)

	h := newWireHarness(t, src, dst)
	pkg := h.seedPackage("v2.14.0")
	h.plan(pkg, "wire-late-coordinator")

	// The Coordinator is not up. The worker starts anyway.
	h.gate.up.Store(false)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()

	// Long enough for several failed lease attempts. The worker must still be
	// running: a control-plane blip that killed the fleet would turn a routine
	// restart into an outage.
	select {
	case err := <-done:
		t.Fatalf("the worker exited while the Coordinator was down: %v", err)
	case <-time.After(2 * time.Second):
	}
	if state := h.transferState(ctx); state == "succeeded" {
		t.Fatal("the transfer finished with the Coordinator down; the gate is not gating")
	}

	// The control plane arrives. Nothing restarts the worker.
	h.gate.up.Store(true)

	h.waitForTransfer(ctx, "wire-late-coordinator", "succeeded")
	cancel()
	<-done
}

// The other order: the Coordinator goes away under a worker that is running,
// and comes back. Jobs already in flight do not pass through the Coordinator,
// so they carry on; what is interrupted is only the leasing.
func TestAWorkerSurvivesACoordinatorRestartMidTransfer(t *testing.T) {
	src := fakeregistry.New()
	t.Cleanup(src.Close)
	dst := fakeregistry.New()
	t.Cleanup(dst.Close)

	h := newWireHarness(t, src, dst)
	pkg := h.seedPackage("v2.14.0")
	h.plan(pkg, "wire-coordinator-restart")

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.loop.Run(ctx) }()

	// Take it away mid-flight, leave it away long enough for leases to be
	// refused and heartbeats to fail, then give it back.
	time.Sleep(300 * time.Millisecond)
	h.gate.up.Store(false)

	select {
	case err := <-done:
		t.Fatalf("the worker exited when the Coordinator went away: %v", err)
	case <-time.After(2 * time.Second):
	}

	h.gate.up.Store(true)

	h.waitForTransfer(ctx, "wire-coordinator-restart", "succeeded")
	cancel()
	<-done
}

func (h *wireHarness) transferState(ctx context.Context) string {
	h.t.Helper()

	var state string
	_ = h.st.DB().QueryRowContext(ctx,
		`SELECT state FROM transfers LIMIT 1`).Scan(&state)
	return state
}
