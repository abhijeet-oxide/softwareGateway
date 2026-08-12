package calibrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/oci"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
)

// The source half: find real blobs, then read them as hard as the level says.

// Sampling bounds.
const (
	// maxSamples is how many distinct blobs the probe rotates through.
	//
	// More than one because a single blob read repeatedly can be served from a
	// cache — the registry's, a CDN's, or a caching proxy's — and would then
	// measure the cache rather than the path. Eight is enough that the working
	// set exceeds anything an incidental cache holds, without walking the whole
	// catalogue to find them.
	maxSamples = 8
	// minSampleBytes skips blobs too small to measure throughput with. A 2 KB
	// config blob measures the round trip, which is already measured separately
	// and much better.
	minSampleBytes = 256 << 10
	// maxTagsListed is how many tags are fetched before choosing which to open.
	//
	// Registries return tags in lexical order, so the FIRST ones are the oldest
	// spellings and frequently the smallest — an early release, a placeholder,
	// a signature. Listing a page and choosing from it beats taking whatever
	// came back first.
	maxTagsListed = 100
	// maxTagsOpened bounds the search within one repository.
	//
	// Twelve rather than a handful because the tags most likely to be opened
	// first are the ones least worth opening: a release is published alongside
	// its signature and attestation, those sort adjacently, and a budget of
	// five can be spent entirely on three-kilobyte PKCS#7 blobs before reaching
	// the release itself. deprioritiseAccessories handles the common spellings;
	// the wider budget handles the ones it does not know.
	maxTagsOpened = 12
	// maxRepositoriesTried bounds the search across repositories. A product
	// spanning forty of them should not be walked end to end to find a blob.
	maxRepositoriesTried = 12

	// warmupBudget bounds the connection-and-token phase that precedes each
	// level. Sixty seconds: the whole point is a path slow enough that the
	// measurement budget cannot absorb its setup, so this has to be generous or
	// it recreates the problem it exists to remove.
	warmupBudget = 60 * time.Second

	// maxSearchDepth is how far down a manifest tree the search will go.
	//
	// Four, matching the transfer walk. It has to be more than one, and
	// assuming one was the bug: a bundle is an index of INDEXES — the ORB lists
	// component images, each of which is a multi-platform index, and the layers
	// live under those. One level of descent lands on a component index, finds
	// no layers because an index has none, and concludes a repository full of
	// gigabytes contains nothing worth measuring.
	maxSearchDepth = 4
	// maxChildrenPerLevel bounds the fan-out. An index listing sixty components
	// does not need all sixty opened to find a layer.
	maxChildrenPerLevel = 8
)

// sample is one blob the read probe will fetch.
type sample struct {
	digest registry.Digest
	size   int64
}

// collectSamples finds real blobs to read, trying each candidate repository
// until one yields something worth timing.
//
// # Why it walks rather than picks
//
// A source spanning forty repositories has no single "the" repository, and the
// first one declared is as likely as not to be a stub. That is not
// hypothetical: a run against a real product measured
// `cfx-5000-product/aaa` — one tag, no blob over 256 KiB — and reported the
// whole source as unmeasurable while thirty-nine repositories of real content
// sat next to it.
//
// Real content, not a synthetic object, and that is the point: the numbers have
// to describe the path a transfer takes, including whatever the registry does
// with layers of this size. It also means no write ever happens to a source.
func collectSamples(
	ctx context.Context, cfg registry.ClientConfig, candidates []string,
) ([]sample, string, error) {
	if len(candidates) == 0 && cfg.Repository != "" {
		candidates = []string{cfg.Repository}
	}
	if len(candidates) == 0 {
		return nil, "", fmt.Errorf("no repository to read from")
	}

	var (
		tried []string
		// best is the largest thing found so far that did NOT clear the size
		// floor, kept as a fallback. A repository of small blobs is a poor
		// sample and a usable one; refusing to measure it at all — which is
		// what this did — leaves somebody with a size threshold they cannot
		// see and no way to act on it.
		best      []sample
		bestRepo  string
		bestBytes int64
	)

	for i, repository := range candidates {
		if ctx.Err() != nil || i >= maxRepositoriesTried {
			break
		}
		tried = append(tried, repository)

		attempt := cfg
		attempt.Repository = repository
		found, err := samplesIn(ctx, attempt)
		if err != nil || len(found) == 0 {
			continue
		}
		if found[0].size >= minSampleBytes {
			return found, repository, nil
		}
		if found[0].size > bestBytes {
			best, bestRepo, bestBytes = found, repository, found[0].size
		}
	}

	if len(best) > 0 {
		// Nothing anywhere cleared the floor, so measure the biggest there is
		// and let the report say the sample was small. A number with a caveat
		// beats a refusal.
		return best, bestRepo, nil
	}
	return nil, "", fmt.Errorf(
		"no blob at all could be read from %s. Name one with --source-repository",
		describeTried(tried, len(candidates)))
}

// samplesIn looks for readable blobs in one repository.
func samplesIn(ctx context.Context, cfg registry.ClientConfig) ([]sample, error) {
	client, err := repositoryClient(withConnections(cfg, 2))
	if err != nil {
		return nil, err
	}

	tags, _, err := client.ListTags(ctx, "", maxTagsListed)
	if err != nil {
		return nil, fmt.Errorf("list tags on %s: %w", cfg.Repository, err)
	}
	if len(tags) == 0 {
		return nil, fmt.Errorf("repository %s has no tags", cfg.Repository)
	}

	seen := map[registry.Digest]bool{}
	var found []sample

	for i, tag := range preferredTags(tags) {
		if ctx.Err() != nil || i >= maxTagsOpened {
			break
		}
		for _, b := range blobsUnderTag(ctx, client, tag) {
			if seen[b.digest] {
				continue
			}
			seen[b.digest] = true
			found = append(found, b)
		}
		// Enough blobs over the floor to fill the rotation: stop opening tags.
		// Small ones are kept as well, because a repository that has only those
		// is still measurable and the caller decides what to do about it.
		if countOver(found, minSampleBytes) >= maxSamples {
			break
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no blob found in %s", cfg.Repository)
	}

	// Largest first, so a short budget spends itself on bytes rather than on
	// re-establishing a request every few hundred milliseconds.
	sort.Slice(found, func(i, j int) bool { return found[i].size > found[j].size })
	if len(found) > maxSamples {
		found = found[:maxSamples]
	}
	return found, nil
}

// preferredTags orders a tag list: newest-looking first, accessories last.
//
// Two heuristics, and stated as such — they change which blobs get timed, never
// whether the measurement is honest.
//
// Registries serve tags in lexical order and the spec guarantees nothing about
// it, so "the first tag" is an arbitrary choice that reliably lands on the
// OLDEST spelling — an early release, a placeholder, a `latest` pointing at
// something small. Reversing gets the other end of the same ordering, which for
// every version scheme met so far is the most recent release.
//
// Reversing alone makes the second problem worse, which is why both are here. A
// release is published alongside its signature and attestation; those sort
// adjacently to it and, for the two conventions this system already knows
// about, sort AFTER it — so newest-first puts a 3 KB PKCS#7 blob at the front
// of the queue. Signatures are still opened, last, because a repository that
// holds nothing else is still measurable.
func preferredTags(tags []string) []string {
	reversed := make([]string, 0, len(tags))
	for i := len(tags) - 1; i >= 0; i-- {
		reversed = append(reversed, tags[i])
	}

	out := make([]string, 0, len(reversed))
	var accessories []string
	for _, tag := range reversed {
		if looksLikeAccessory(tag) {
			accessories = append(accessories, tag)
			continue
		}
		out = append(out, tag)
	}
	return append(out, accessories...)
}

// looksLikeAccessory recognises a tag that carries a signature or attestation
// rather than a release.
//
// Two conventions, both already documented elsewhere in this system: cosign's
// `sha256-<digest>.sig` / `.att` / `.sbom` tag schema, and the `signature_` /
// `signed_` prefixes NEAR publishes (internal/vendors/near). Deliberately a
// NAME test and nothing more — it only reorders a search, so a tag it
// misjudges costs one extra fetch and a tag it misses is caught by the size
// filter behind it.
func looksLikeAccessory(tag string) bool {
	lower := strings.ToLower(tag)
	if strings.HasPrefix(lower, "sha256-") &&
		(strings.HasSuffix(lower, ".sig") || strings.HasSuffix(lower, ".att") ||
			strings.HasSuffix(lower, ".sbom")) {
		return true
	}
	return strings.HasPrefix(lower, "signature_") || strings.HasPrefix(lower, "signed_")
}

// describeTried names the repositories the search covered, without printing
// forty of them.
func describeTried(tried []string, total int) string {
	switch {
	case len(tried) == 0:
		return "any repository"
	case len(tried) == 1:
		return tried[0]
	case len(tried) < total:
		return fmt.Sprintf("%d of %d repositories (%s, …)", len(tried), total, tried[0])
	default:
		return fmt.Sprintf("any of %d repositories (%s, …)", total, tried[0])
	}
}

// blobsUnderTag returns the layers reachable from one tag.
//
// An index states the size of its child MANIFESTS, not of the layers beneath
// them, so reaching actual content means descending — and descending as far as
// it takes, which is the part this got wrong. A bundle is an index of INDEXES:
// the ORB lists its components, each component is a multi-platform index, and
// the layers are a level below that. Stopping after one descent landed on a
// component index, found no layers because an index has none, and reported a
// repository holding gigabytes as having nothing to measure.
//
// Errors are swallowed: this is a search for something to measure, and a tag
// that will not resolve simply is not it — the caller reports "nothing found"
// once, rather than one failure per tag.
func blobsUnderTag(ctx context.Context, client *generic.Repository, tag string) []sample {
	desc, err := client.ResolveTag(ctx, tag)
	if err != nil {
		return nil
	}
	return searchForBlobs(ctx, client, desc, 0)
}

// searchForBlobs descends a manifest tree until it finds layers.
//
// # Why not oci.Walk
//
// The transfer walk fetches the WHOLE tree — every manifest, to a bounded depth
// — because a transfer needs every one of them. This needs a handful of blobs
// and can stop at the first manifest that has any, so it goes depth-first and
// returns as soon as it has something. On an ORB that is two or three requests
// against a tree of sixty artifacts, and the difference is a calibration that
// starts in a second rather than a minute.
func searchForBlobs(
	ctx context.Context, client *generic.Repository, desc registry.Descriptor, depth int,
) []sample {
	if depth > maxSearchDepth || ctx.Err() != nil {
		return nil
	}

	tree, err := oci.FetchRoot(ctx, client, desc)
	if err != nil || len(tree.Artifacts) == 0 {
		return nil
	}
	if out := samplesOf(tree.Artifacts[0]); len(out) > 0 {
		return out
	}

	// An index. Its children are listed but not fetched, so open them in turn
	// until one yields layers — of its own, or from further down.
	children := tree.Artifacts[1:]
	for i, child := range children {
		if i >= maxChildrenPerLevel {
			break
		}
		if out := searchForBlobs(ctx, client, child.Descriptor, depth+1); len(out) > 0 {
			return out
		}
	}
	return nil
}

// countOver is how many samples clear the size floor.
func countOver(samples []sample, floor int64) int {
	n := 0
	for _, s := range samples {
		if s.size >= floor {
			n++
		}
	}
	return n
}

func samplesOf(a oci.Artifact) []sample {
	out := make([]sample, 0, len(a.Blobs))
	for _, b := range a.Blobs {
		if b.Descriptor.Size > 0 {
			out = append(out, sample{digest: b.Descriptor.Digest, size: b.Descriptor.Size})
		}
	}
	return out
}

// probeRead measures aggregate read throughput at one concurrency.
//
// Every stream reads whole blobs from the rotating sample set and throws the
// bytes away. Discarding is what makes this a measurement of the PATH: writing
// them anywhere would fold a disk into a number that is supposed to be about
// the network, and invariant I5 says bytes never touch worker disk in any case.
func probeRead(
	ctx context.Context, cfg registry.ClientConfig, samples []sample,
	streams int, budget time.Duration,
) LevelResult {
	res := LevelResult{Concurrency: streams}

	client, err := repositoryClient(cfg)
	if err != nil {
		res.Errors++
		res.FirstError = err.Error()
		return res
	}

	// Setup happens BEFORE the clock starts, and this is the difference between
	// a measurement and a table of zeroes.
	//
	// Each level builds a fresh client — deliberately, so a level does not
	// inherit the previous one's warm sockets — which means its first request
	// pays a proxy CONNECT, a TLS handshake, a token exchange and a blob
	// resolve before a single byte of payload moves. On a path with a 900ms
	// round trip that is five round trips, more than the whole default budget,
	// so every level's first request was still in flight when the deadline
	// fired and every level reported nothing at all.
	warmUp(ctx, client, samples[0].digest, streams)

	runCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var (
		bytes     atomic.Int64
		requests  atomic.Int64
		failures  atomic.Int64
		throttled atomic.Int64
		next      atomic.Int64

		mu    sync.Mutex
		ttfbs []time.Duration
		first string
	)
	note := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == "" {
			first = err.Error()
		}
	}

	started := time.Now()
	var wg sync.WaitGroup
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, chunkSize)

			for runCtx.Err() == nil {
				s := samples[int(next.Add(1)-1)%len(samples)]

				requestStarted := time.Now()
				rc, err := client.FetchBlob(runCtx, s.digest)
				if err != nil {
					// A deadline reached mid-request is the probe ending, not
					// the registry failing. Counting it as an error would make
					// every level report one failure per stream.
					if runCtx.Err() != nil {
						return
					}
					countFailure(err, &failures, &throttled)
					note(err)
					continue
				}

				mu.Lock()
				ttfbs = append(ttfbs, time.Since(requestStarted))
				mu.Unlock()
				requests.Add(1)

				// CopyBuffer rather than Copy so the accounting granularity is
				// the buffer: a blob abandoned at the deadline still counts
				// every byte that actually crossed the link.
				n, _ := io.CopyBuffer(counter{&bytes}, rc, buf)
				_ = n
				_ = rc.Close()
			}
		}()
	}
	wg.Wait()

	res.Duration = workingTime(started, budget)
	res.Bytes = bytes.Load()
	res.Requests = int(requests.Load())
	res.Errors = int(failures.Load())
	res.Throttled = int(throttled.Load())
	res.TTFB = median(ttfbs)
	res.FirstError = describeEmptyLevel(first, res.Requests, res.Errors, budget)
	res.Rate = ratePerSecond(res.Bytes, res.Duration)
	if streams > 0 {
		res.PerStream = res.Rate / float64(streams)
	}
	return res
}

// warmUp opens the connections and exchanges the token before timing starts.
//
// One cheap request per stream, in parallel, so the pool the measurement runs
// against is already established. A HEAD rather than a GET: it proves the
// connection, the TLS session and the credential without moving payload that
// would then have to be excluded from the numbers.
//
// Failures are ignored. This is preparation, not a check — whatever is wrong
// will fail again inside the measured window, where it is counted and reported.
func warmUp(
	ctx context.Context, client *generic.Repository, digest registry.Digest, streams int,
) {
	// A generous ceiling of its own, because the thing being worked around is
	// precisely a path too slow for the measurement budget.
	warmCtx, cancel := context.WithTimeout(ctx, warmupBudget)
	defer cancel()

	var wg sync.WaitGroup
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.StatBlob(warmCtx, digest)
		}()
	}
	wg.Wait()
}

// describeEmptyLevel turns a silently empty level into a stated one.
//
// A level that completed no request and recorded no error rendered as a row of
// dashes with no explanation anywhere — which is what a five-second budget on a
// path with a one-second round trip produced, and it read as the probe being
// broken rather than as the budget being too short for the link.
func describeEmptyLevel(first string, requests, errors int, budget time.Duration) string {
	if first != "" || requests > 0 || errors > 0 {
		return first
	}
	return fmt.Sprintf(
		"no request finished within the %s budget — the link is slower than the "+
			"measurement window. Raise --budget", budget)
}

// counter is an io.Writer that only counts, so a copy can be measured without
// the bytes going anywhere.
type counter struct{ n *atomic.Int64 }

func (c counter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// countFailure records an error, separating "we are being throttled" from
// everything else.
//
// The separation matters more than the count. A registry answering 429 has
// stated its own limit, and no throughput measurement outranks that: the
// correct response is to configure requestsPerSecond, not to find the
// concurrency at which the throttling hurts least.
func countFailure(err error, failures, throttled *atomic.Int64) {
	failures.Add(1)
	switch {
	case errors.Is(err, registry.ErrRateLimited):
		throttled.Add(1)
	case errors.Is(err, registry.ErrUnavailable):
		// 503 under load is the same message in a different envelope: some
		// registries shed load rather than rate-limit it.
		throttled.Add(1)
	}
}

func ratePerSecond(bytes int64, d time.Duration) float64 {
	if d <= 0 || bytes <= 0 {
		return 0
	}
	return float64(bytes) / d.Seconds()
}
