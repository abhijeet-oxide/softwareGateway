package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// Reusing a render, because rendering the same bytes twice cannot produce a
// different answer.
//
// # Why this is safe, which is the only interesting question
//
// A cache that can return a different answer from the work it replaces is not a
// cache, it is a bug with a hit rate. So the key names EVERY input that can
// change what `helm template` produces, and nothing else:
//
//	the chart          by its layer digest, which content-addresses the bytes
//	the renderer       helm's own version string
//	the cluster shape  kubeVersion and apiVersions, which gate whole template blocks
//	the release        releaseName and namespace, which appear in rendered names
//	the variant        baseline, or the perturbed render determinacy needs
//
// Every one of those is already recorded on a run, because rule 5 requires a
// finding to be re-derivable from them. That is not a coincidence: the set of
// things that make a run reproducible and the set of things that make its render
// cacheable are the same set. If a new input is ever added to the renderer, it
// belongs in RenderInputs and in the run's provenance in the same commit, and
// the compiler will not remind anyone - which is why this comment exists.
//
// # What it buys
//
// Two things, and the second is larger than the first.
//
// A re-check of an unchanged release renders nothing: 95 helm subprocesses
// become 95 map lookups. And because the key is the LAYER DIGEST, which is known
// from the release's recorded contents before anything is fetched, a hit also
// means the chart never has to be pulled out of the vendor's registry or
// unpacked. Most charts do not change between two releases of a product, so the
// second check of an orb is mostly cache, and the vendor's registry sees almost
// no traffic for it.
//
// # Why the chart's digest and not its name and version
//
// A vendor who republishes 4.2.1 with a fixed template has shipped different
// bytes under the same version, and a cache keyed by name and version would
// serve them the old answer forever. The digest cannot do that: different bytes
// are a different key, and the stale entry is simply never asked for again.

// RenderVariant distinguishes the two renders a run performs of each chart.
type RenderVariant string

const (
	// VariantBase is the chart at its own defaults - what the checks judge.
	VariantBase RenderVariant = "base"
	// VariantProbe is the perturbed render determinacy is measured against.
	// Its values are derived from the chart's own values.yaml, so they are a
	// function of the chart's bytes and need no separate key.
	VariantProbe RenderVariant = "probe"
)

// RenderInputs is everything about the RENDERER that can change its output.
//
// A struct rather than a string so that adding an input is a compile error at
// every construction site rather than a silently wrong key.
type RenderInputs struct {
	HelmVersion string
	KubeVersion string
	APIVersions []string
	ReleaseName string
	Namespace   string
}

// Digest is the stable identity of these inputs.
//
// Sorted and length-prefixed: `apiVersions: ["a", "bc"]` and `["ab", "c"]` are
// different render inputs and must not collide, which a naive join would allow.
func (in RenderInputs) Digest() string {
	versions := append([]string(nil), in.APIVersions...)
	sort.Strings(versions)

	h := sha256.New()
	write := func(label, value string) {
		_, _ = h.Write([]byte(label))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	write("helm", in.HelmVersion)
	write("kube", in.KubeVersion)
	write("release", in.ReleaseName)
	write("namespace", in.Namespace)
	for _, v := range versions {
		write("api", v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RenderKey identifies one cached render.
func RenderKey(chartDigest string, variant RenderVariant, inputs string) string {
	if chartDigest == "" || inputs == "" {
		return ""
	}
	h := sha256.Sum256([]byte(chartDigest + "\x00" + string(variant) + "\x00" + inputs))
	return hex.EncodeToString(h[:])
}

// CachedRender is one chart's rendered output, and the chart metadata that came
// out of the same bytes.
//
// The metadata is here because a hit must reproduce EVERYTHING loading the chart
// produced, not only its manifests: the chart's name and version appear on every
// finding's address, and a cache that returned the stream without them would
// produce findings addressed to an empty chart.
type CachedRender struct {
	ChartDigest  string
	Variant      RenderVariant
	InputsDigest string

	ChartName    string
	ChartVersion string
	AppVersion   string
	SubchartPath string
	// ValuesYAML is the chart's own values.yaml as shipped.
	//
	// Stored because a check can read `chart.values`, and a cache entry that
	// dropped them would make a custom check behave differently on a hit than
	// on a miss - which is precisely the failure this whole design is arranged
	// to prevent. No shipped check reads them today; that is not a reason to
	// build a cache that would break the first one that does.
	ValuesYAML []byte

	Manifests []byte
}

// Key is where this render is stored.
func (r CachedRender) Key() string {
	return RenderKey(r.ChartDigest, r.Variant, r.InputsDigest)
}

// RenderStore is the cache's persistence.
//
// An interface for the reason every other seam in this package is one: the
// rendering path has to be drivable from a test with no database, and the
// dependency points from the store to the domain rather than back.
//
// A nil RenderStore is a working configuration - it means "render everything" -
// so no caller needs a nil check and a deployment can turn the cache off
// without a code path of its own.
type RenderStore interface {
	// LookupRenders returns the entries present, keyed by RenderKey. Keys with
	// no entry are simply absent; a miss is not an error.
	LookupRenders(ctx context.Context, keys []string) (map[string]CachedRender, error)
	// StoreRenders records renders, replacing any entry with the same key.
	StoreRenders(ctx context.Context, renders []CachedRender) error
}

// RenderCache is the cache as the rendering path uses it.
//
// Wraps a possibly-nil store so the call sites read as straight line code, and
// counts hits and misses so a run can say how much of it was reused - which is
// the number somebody wants when a check that took four minutes last week takes
// twelve seconds today, and the number they want when it does not.
type RenderCache struct {
	Store RenderStore
	// Inputs is the renderer's identity for this run.
	Inputs RenderInputs

	inputsDigest string
	hits, misses int
}

// NewRenderCache prepares the cache for one run.
func NewRenderCache(store RenderStore, inputs RenderInputs) *RenderCache {
	return &RenderCache{Store: store, Inputs: inputs, inputsDigest: inputs.Digest()}
}

// Enabled reports whether anything will be reused or recorded.
func (c *RenderCache) Enabled() bool { return c != nil && c.Store != nil }

// InputsDigest identifies the renderer this cache is keyed against.
func (c *RenderCache) InputsDigest() string {
	if c == nil {
		return ""
	}
	return c.inputsDigest
}

// Lookup fetches every render already held for these chart digests.
//
// Both variants of every digest in one query, because the caller needs to know
// whether a chart can be skipped ENTIRELY - which it can only be when both the
// baseline and the probe are present. One round trip for a 95-chart release
// rather than 190.
func (c *RenderCache) Lookup(ctx context.Context, chartDigests []string) (map[string]CachedRender, error) {
	if !c.Enabled() || len(chartDigests) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(chartDigests)*2)
	for _, d := range chartDigests {
		for _, v := range []RenderVariant{VariantBase, VariantProbe} {
			if k := RenderKey(d, v, c.inputsDigest); k != "" {
				keys = append(keys, k)
			}
		}
	}
	return c.Store.LookupRenders(ctx, keys)
}

// Get reads one render out of a lookup result.
func (c *RenderCache) Get(found map[string]CachedRender, chartDigest string, v RenderVariant) (CachedRender, bool) {
	if !c.Enabled() || found == nil {
		return CachedRender{}, false
	}
	r, ok := found[RenderKey(chartDigest, v, c.inputsDigest)]
	return r, ok
}

// Hit and Miss record what a run reused, for the progress panel and the log.
func (c *RenderCache) Hit(n int) {
	if c != nil {
		c.hits += n
	}
}

func (c *RenderCache) Miss(n int) {
	if c != nil {
		c.misses += n
	}
}

// Stats is what was reused.
func (c *RenderCache) Stats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	return c.hits, c.misses
}

// Save records renders produced by this run.
//
// # Why a failure here is logged and not returned
//
// The renders are already in hand and the run is about to judge them. Failing
// the run because the CACHE could not be written would turn a slow next run
// into no result at all, which is a strictly worse outcome for the person
// waiting. The caller logs it; nothing about this run's answer changes.
func (c *RenderCache) Save(ctx context.Context, renders []CachedRender) error {
	if !c.Enabled() || len(renders) == 0 {
		return nil
	}
	keep := renders[:0]
	for _, r := range renders {
		if r.ChartDigest == "" || len(r.Manifests) == 0 {
			continue
		}
		r.InputsDigest = c.inputsDigest
		keep = append(keep, r)
	}
	if len(keep) == 0 {
		return nil
	}
	return c.Store.StoreRenders(ctx, keep)
}

// SummariseReuse is the sentence a run puts in its log.
func SummariseReuse(hits, misses int) string {
	total := hits + misses
	if total == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Reused ")
	b.WriteString(itoa(hits))
	b.WriteString(" of ")
	b.WriteString(itoa(total))
	b.WriteString(" chart renders from the render cache")
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
