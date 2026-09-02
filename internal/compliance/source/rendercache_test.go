package source_test

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/render"
	"github.com/abhijeet-oxide/softwareGateway/internal/compliance/source"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
)

// A cache that can return a different answer from the work it replaces is not a
// cache, it is a bug with a hit rate. This file is the proof that it cannot.

// memCache is a compliance.RenderStore in a map, and it counts what the
// preparer asked it for.
type memCache struct {
	entries map[string]compliance.CachedRender
	lookups int
	stores  int
	// readOnly refuses writes, for the test that says a cache that cannot be
	// written does not fail a run.
	readOnly bool
	// blind refuses reads, for the same reason on the other side.
	blind bool
}

func newMemCache() *memCache {
	return &memCache{entries: map[string]compliance.CachedRender{}}
}

func (m *memCache) LookupRenders(_ context.Context, keys []string) (map[string]compliance.CachedRender, error) {
	m.lookups++
	if m.blind {
		return nil, os.ErrPermission
	}
	out := map[string]compliance.CachedRender{}
	for _, k := range keys {
		if e, ok := m.entries[k]; ok {
			out[k] = e
		}
	}
	return out, nil
}

func (m *memCache) StoreRenders(_ context.Context, renders []compliance.CachedRender) error {
	m.stores++
	if m.readOnly {
		return os.ErrPermission
	}
	for _, r := range renders {
		m.entries[r.Key()] = r
	}
	return nil
}

// countingBlobs answers like `blobs` and records how many times the registry was
// actually asked - which is the number a cache hit has to leave at zero.
type countingBlobs struct {
	inner blobs
	reads int
}

func (c *countingBlobs) ReadBlob(ctx context.Context, product string, pkg store.PackageRow, digest string) (io.ReadCloser, error) {
	c.reads++
	return c.inner.ReadBlob(ctx, product, pkg, digest)
}

// A chart with enough shape that its rendered output has line numbers, several
// objects, and values worth reading back.
func cacheChart(t *testing.T) []byte {
	t.Helper()
	return tarball(t, map[string]string{
		"web/Chart.yaml": "apiVersion: v2\nname: web\nversion: 4.2.1\nappVersion: \"1.9\"\n",
		"web/values.yaml": "replicaCount: 2\nimage:\n  repository: registry.example/web\n" +
			"  tag: \"1.9\"\nprobes:\n  enabled: true\n",
		"web/templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Chart.Name }}
spec:
  replicas: {{ .Values.replicaCount }}
  template:
    spec:
      containers:
        - name: main
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
`,
		"web/templates/service.yaml": `apiVersion: v1
kind: Service
metadata:
  name: {{ .Chart.Name }}
spec:
  ports:
    - port: 8080
`,
	})
}

func preparerWith(cache compliance.RenderStore, b source.BlobReader, candidates []store.ChartCandidate) *source.Preparer {
	return &source.Preparer{
		Fetcher:     source.Fetcher{Blobs: b},
		Packages:    stubLookup{candidates: candidates},
		Probe:       true,
		RenderCache: cache,
	}
}

// THE TEST. The second run of the same release must produce byte-identical
// resources, addresses, line numbers and chart metadata - having contacted no
// registry and run no helm.
func TestACacheHitProducesTheSameReleaseAsAMiss(t *testing.T) {
	helmOrSkipHere(t)

	body := cacheChart(t)
	candidates := []store.ChartCandidate{{
		Digest: "sha256:artifact", LayerDigest: "sha256:layer", LayerCount: 1,
		Ref: "charts/web", MediaType: "application/vnd.oci.image.manifest.v1+json",
		ConfigMediaType: "application/vnd.cncf.helm.config.v1+json",
	}}

	cache := newMemCache()
	registry := &countingBlobs{inner: blobs{"sha256:layer": body}}
	p := preparerWith(cache, registry, candidates)

	first, firstProbe, cleanup1, err := p.Prepare(context.Background(),
		compliance.Request{Product: "acme", Release: "1.0", Digest: "sha256:pkg"},
		compliance.NopReporter{})
	if cleanup1 != nil {
		defer cleanup1()
	}
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if registry.reads != 1 {
		t.Fatalf("the first run read the registry %d times, want 1", registry.reads)
	}
	if len(cache.entries) != 2 {
		t.Fatalf("the first run cached %d renders, want 2 (baseline and probe)", len(cache.entries))
	}

	readsAfterFirst := registry.reads
	second, secondProbe, cleanup2, err := p.Prepare(context.Background(),
		compliance.Request{Product: "acme", Release: "1.0", Digest: "sha256:pkg"},
		compliance.NopReporter{})
	if cleanup2 != nil {
		defer cleanup2()
	}
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	// The point of keying on the layer digest: a hit needs no bytes at all.
	if registry.reads != readsAfterFirst {
		t.Errorf("the second run read the registry %d more times; a cache hit must need "+
			"no bytes from the vendor", registry.reads-readsAfterFirst)
	}

	// And the answer is the same answer.
	if len(second.Resources) != len(first.Resources) {
		t.Fatalf("cached run has %d resources, fresh run had %d",
			len(second.Resources), len(first.Resources))
	}
	for i := range first.Resources {
		a, b := first.Resources[i], second.Resources[i]
		if a.Address != b.Address {
			t.Errorf("resource %d address differs:\n fresh  %+v\n cached %+v", i, a.Address, b.Address)
		}
		if !reflect.DeepEqual(a.Object, b.Object) {
			t.Errorf("resource %d object differs", i)
		}
	}
	if len(second.Charts) != 1 {
		t.Fatalf("cached run has %d charts", len(second.Charts))
	}
	fresh, cached := first.Charts[0], second.Charts[0]
	for _, f := range []struct {
		name string
		a, b string
	}{
		{"name", fresh.Name, cached.Name},
		{"version", fresh.Version, cached.Version},
		{"appVersion", fresh.AppVersion, cached.AppVersion},
		{"digest", fresh.Digest, cached.Digest},
		{"ref", fresh.Ref, cached.Ref},
		{"renderStatus", fresh.RenderStatus, cached.RenderStatus},
	} {
		if f.a != f.b {
			t.Errorf("chart %s: fresh %q, cached %q", f.name, f.a, f.b)
		}
	}
	// A check may read `chart.values`, so a hit that dropped them would make
	// that check behave differently than on a miss.
	if !reflect.DeepEqual(fresh.Values, cached.Values) {
		t.Errorf("chart values differ:\n fresh  %+v\n cached %+v", fresh.Values, cached.Values)
	}

	// The evidence - the manifests a finding is shown against - is the same
	// text, so an excerpt's line numbers mean the same thing either way.
	if len(second.Rendered) != len(first.Rendered) {
		t.Fatalf("cached run kept %d documents, fresh kept %d",
			len(second.Rendered), len(first.Rendered))
	}
	for i := range first.Rendered {
		if string(first.Rendered[i].Content) != string(second.Rendered[i].Content) {
			t.Errorf("kept document %d differs between a fresh render and a cached one", i)
		}
	}

	// Determinacy survives: the probe render is cached too, so the second run
	// can still tell a value the chart fixes from one a site can override.
	if firstProbe == nil || secondProbe == nil {
		t.Fatal("a determiner was not produced")
	}
	// The probe render is cached too, so the second run can still tell a value
	// the chart fixes from one a site can override. Asked of a real subject,
	// because "usable" is not a thing a Determiner reports - what it reports is
	// a determinacy, and `unknown` everywhere is what a lost probe looks like.
	if len(second.Resources) > 0 {
		subj := compliance.Subject{Resource: &second.Resources[0]}
		if got := secondProbe.Determinacy(subj, "spec.replicas"); got == compliance.DeterminacyUnknown {
			t.Error("determinacy was lost on the cached run, so every finding would report " +
				"as unknown - which is a different answer")
		}
	}
}

// A vendor who republishes the same version with different bytes must not be
// served the old answer.
func TestDifferentBytesAreADifferentKey(t *testing.T) {
	helmOrSkipHere(t)

	cache := newMemCache()
	first := cacheChart(t)
	// The same chart name and the same version, with an object added - which is
	// exactly what a vendor republishing 4.2.1 with a fix looks like.
	second := tarball(t, map[string]string{
		"web/Chart.yaml":                "apiVersion: v2\nname: web\nversion: 4.2.1\nappVersion: \"1.9\"\n",
		"web/values.yaml":               "replicaCount: 2\n",
		"web/templates/deployment.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n",
		"web/templates/service.yaml":    "apiVersion: v1\nkind: Service\nmetadata:\n  name: web\n",
		"web/templates/extra.yaml":      "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: extra\n",
	})

	run := func(layerDigest string, body []byte) int {
		candidates := []store.ChartCandidate{{
			Digest: "sha256:artifact", LayerDigest: layerDigest, LayerCount: 1, Ref: "charts/web",
			ConfigMediaType: "application/vnd.cncf.helm.config.v1+json",
		}}
		p := preparerWith(cache, blobs{layerDigest: body}, candidates)
		rel, _, cleanup, err := p.Prepare(context.Background(),
			compliance.Request{Product: "acme", Release: "1.0"}, compliance.NopReporter{})
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		return len(rel.Resources)
	}

	a := run("sha256:first", first)
	b := run("sha256:second", second)
	if a == b {
		t.Fatalf("both renders produced %d resources; the fixture cannot detect a stale hit", a)
	}
	// And re-running the first digest still gets the FIRST answer.
	if again := run("sha256:first", first); again != a {
		t.Fatalf("re-running the first chart produced %d resources, want %d", again, a)
	}
}

// Changing a render input invalidates every entry, because every entry was
// produced under the old one.
func TestChangingARenderInputMissesTheCache(t *testing.T) {
	helmOrSkipHere(t)

	body := cacheChart(t)
	candidates := []store.ChartCandidate{{
		Digest: "sha256:artifact", LayerDigest: "sha256:layer", LayerCount: 1, Ref: "charts/web",
		ConfigMediaType: "application/vnd.cncf.helm.config.v1+json",
	}}
	cache := newMemCache()

	warm := preparerWith(cache, blobs{"sha256:layer": body}, candidates)
	_, _, cleanup, err := warm.Prepare(context.Background(),
		compliance.Request{Product: "acme"}, compliance.NopReporter{})
	if cleanup != nil {
		cleanup()
	}
	if err != nil {
		t.Fatal(err)
	}
	warmed := len(cache.entries)

	// A different Kubernetes version gates different template blocks, so its
	// output is a different answer and must be a different key.
	registry := &countingBlobs{inner: blobs{"sha256:layer": body}}
	other := preparerWith(cache, registry, candidates)
	other.Helm = render.Helm{KubeVersion: "1.28.0"}
	_, _, cleanup2, err := other.Prepare(context.Background(),
		compliance.Request{Product: "acme"}, compliance.NopReporter{})
	if cleanup2 != nil {
		cleanup2()
	}
	if err != nil {
		t.Fatal(err)
	}
	if registry.reads == 0 {
		t.Error("a run under a different kubeVersion was served from the cache; its entries " +
			"were produced by a renderer that is no longer the one in use")
	}
	if len(cache.entries) <= warmed {
		t.Error("the run under new inputs recorded nothing, so the next one pays again")
	}
}

// A cache is an optimisation. Neither half of it failing may cost an answer.
func TestACacheThatDoesNotWorkDoesNotFailTheRun(t *testing.T) {
	helmOrSkipHere(t)

	body := cacheChart(t)
	candidates := []store.ChartCandidate{{
		Digest: "sha256:artifact", LayerDigest: "sha256:layer", LayerCount: 1, Ref: "charts/web",
		ConfigMediaType: "application/vnd.cncf.helm.config.v1+json",
	}}

	for _, tc := range []struct {
		name  string
		cache *memCache
	}{
		{"unreadable", &memCache{entries: map[string]compliance.CachedRender{}, blind: true}},
		{"unwritable", &memCache{entries: map[string]compliance.CachedRender{}, readOnly: true}},
		{"absent", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cache compliance.RenderStore
			if tc.cache != nil {
				cache = tc.cache
			}
			p := preparerWith(cache, blobs{"sha256:layer": body}, candidates)
			rel, _, cleanup, err := p.Prepare(context.Background(),
				compliance.Request{Product: "acme"}, compliance.NopReporter{})
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				t.Fatalf("the run failed over the cache: %v", err)
			}
			if len(rel.Resources) == 0 {
				t.Error("the run produced nothing")
			}
		})
	}
}

func TestRenderInputsDigestDistinguishesEveryInput(t *testing.T) {
	base := compliance.RenderInputs{
		HelmVersion: "v3.16.3", KubeVersion: "1.30.0",
		APIVersions: []string{"a/v1", "b/v1"},
		ReleaseName: "sgw-compliance", Namespace: "sgw-compliance",
	}
	seen := map[string]string{base.Digest(): "base"}

	variants := map[string]compliance.RenderInputs{
		"helm":       {HelmVersion: "v3.17.0", KubeVersion: "1.30.0", APIVersions: []string{"a/v1", "b/v1"}, ReleaseName: "sgw-compliance", Namespace: "sgw-compliance"},
		"kube":       {HelmVersion: "v3.16.3", KubeVersion: "1.28.0", APIVersions: []string{"a/v1", "b/v1"}, ReleaseName: "sgw-compliance", Namespace: "sgw-compliance"},
		"apis":       {HelmVersion: "v3.16.3", KubeVersion: "1.30.0", APIVersions: []string{"a/v1"}, ReleaseName: "sgw-compliance", Namespace: "sgw-compliance"},
		"release":    {HelmVersion: "v3.16.3", KubeVersion: "1.30.0", APIVersions: []string{"a/v1", "b/v1"}, ReleaseName: "other", Namespace: "sgw-compliance"},
		"namespace":  {HelmVersion: "v3.16.3", KubeVersion: "1.30.0", APIVersions: []string{"a/v1", "b/v1"}, ReleaseName: "sgw-compliance", Namespace: "other"},
		"apiJoinAmb": {HelmVersion: "v3.16.3", KubeVersion: "1.30.0", APIVersions: []string{"a/v1b", "/v1"}, ReleaseName: "sgw-compliance", Namespace: "sgw-compliance"},
	}
	for name, in := range variants {
		d := in.Digest()
		if other, clash := seen[d]; clash {
			t.Errorf("%s collides with %s: two different renderers share a cache key", name, other)
		}
		seen[d] = name
	}

	// Order must not matter; content must.
	reordered := base
	reordered.APIVersions = []string{"b/v1", "a/v1"}
	if reordered.Digest() != base.Digest() {
		t.Error("reordering apiVersions changed the key, so an unchanged renderer misses its own cache")
	}
}

func TestRenderKeyIsEmptyWithoutItsParts(t *testing.T) {
	if compliance.RenderKey("", compliance.VariantBase, "inputs") != "" {
		t.Error("a key was produced without a chart digest")
	}
	if compliance.RenderKey("sha256:a", compliance.VariantBase, "") != "" {
		t.Error("a key was produced without render inputs")
	}
	if compliance.RenderKey("sha256:a", compliance.VariantBase, "in") ==
		compliance.RenderKey("sha256:a", compliance.VariantProbe, "in") {
		t.Error("the baseline and the probe share a key")
	}
}

// helmOrSkipHere skips when there is no helm, because these tests are about
// what rendering produces.
func helmOrSkipHere(t *testing.T) {
	t.Helper()
	h := render.Helm{}.WithDefaults()
	if _, err := h.Version(context.Background()); err != nil {
		t.Skipf("helm is not available: %v", err)
	}
}
