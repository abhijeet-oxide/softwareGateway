package calibrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
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
	// maxTagsOpened bounds the search within one repository. A repository whose
	// newest tags are all signatures should not turn calibration into a scan.
	maxTagsOpened = 5
	// maxRepositoriesTried bounds the search across repositories. A product
	// spanning forty of them should not be walked end to end to find a blob.
	maxRepositoriesTried = 12
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

	var tried []string
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
		return found, repository, nil
	}

	return nil, "", fmt.Errorf(
		"no blob of at least %s found in %s. Name one with --source-repository",
		humanSize(minSampleBytes), describeTried(tried, len(candidates)))
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
		if ctx.Err() != nil || len(found) >= maxSamples || i >= maxTagsOpened {
			break
		}
		for _, b := range blobsUnderTag(ctx, client, tag) {
			if seen[b.digest] || b.size < minSampleBytes {
				continue
			}
			seen[b.digest] = true
			found = append(found, b)
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no blob of at least %s in %s",
			humanSize(minSampleBytes), cfg.Repository)
	}

	// Largest first, so a short budget spends itself on bytes rather than on
	// re-establishing a request every few hundred milliseconds.
	sort.Slice(found, func(i, j int) bool { return found[i].size > found[j].size })
	if len(found) > maxSamples {
		found = found[:maxSamples]
	}
	return found, nil
}

// preferredTags orders a tag list newest-looking first.
//
// Registries serve tags in lexical order and the spec guarantees nothing about
// it, so "the first tag" is an arbitrary choice that reliably lands on the
// OLDEST spelling — an early release, a placeholder, a `latest` pointing at
// something small. Reversing gets the other end of the same ordering, which for
// every version scheme met so far is the most recent release and therefore the
// most representative of what a transfer will actually move.
//
// A heuristic, and stated as one: it changes which blobs get timed, never
// whether the measurement is honest.
func preferredTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for i := len(tags) - 1; i >= 0; i-- {
		out = append(out, tags[i])
	}
	return out
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

// blobsUnderTag returns the layers one tag references, descending one level
// into an index.
//
// An index states the size of its child MANIFESTS, not of the layers beneath
// them, so a bundle's root yields nothing readable and one descent is required
// to reach actual content. Errors are swallowed: this is a search for something
// to measure, and a tag that will not resolve simply is not it — the caller
// reports "nothing found" once, rather than one failure per tag.
func blobsUnderTag(ctx context.Context, client *generic.Repository, tag string) []sample {
	desc, err := client.ResolveTag(ctx, tag)
	if err != nil {
		return nil
	}
	tree, err := oci.FetchRoot(ctx, client, desc)
	if err != nil || len(tree.Artifacts) == 0 {
		return nil
	}

	if out := samplesOf(tree.Artifacts[0]); len(out) > 0 {
		return out
	}

	// The root was an index. Its children are listed but not fetched, so walk
	// into the first one that is a manifest rather than another index.
	for _, child := range tree.Artifacts[1:] {
		sub, err := oci.FetchRoot(ctx, client, child.Descriptor)
		if err != nil || len(sub.Artifacts) == 0 {
			continue
		}
		if out := samplesOf(sub.Artifacts[0]); len(out) > 0 {
			return out
		}
	}
	return nil
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
		return res
	}

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
	res.FirstError = first
	res.Rate = ratePerSecond(res.Bytes, res.Duration)
	if streams > 0 {
		res.PerStream = res.Rate / float64(streams)
	}
	return res
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
