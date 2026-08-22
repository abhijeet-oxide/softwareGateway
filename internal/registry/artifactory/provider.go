package artifactory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// XrayProvider implements security.Provider over JFrog Xray.
//
// The only implementation of that interface today, and the reason the interface
// exists anyway is stated on it: the alternative is a core platform that speaks
// Xray's JSON, and a second scanner that is a rewrite rather than a package.
type XrayProvider struct {
	client      *XrayClient
	concurrency int
}

// NewXrayProvider builds the provider for one JFrog repository.
//
// Returns security.Disabled when the repository has not switched Xray on. A
// disabled provider is a real object that answers every request with a disabled
// report, rather than a nil the caller must remember to check - the nil check is
// the one that gets forgotten, and the failure mode of forgetting it is a
// release that renders as clean.
func NewXrayProvider(cfg XrayConfig, settings XraySettings) (security.Provider, error) {
	if !settings.Enabled {
		return security.Disabled{
			ProviderName: providerName,
			Reason:       "JFrog Xray is not enabled for this repository.",
		}, nil
	}

	client, err := NewXrayClient(cfg)
	if err != nil {
		return nil, err
	}
	concurrency := settings.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	return &XrayProvider{client: client, concurrency: concurrency}, nil
}

// Name implements security.Provider.
func (p *XrayProvider) Name() string { return providerName }

// Enabled implements security.Provider.
func (p *XrayProvider) Enabled() bool { return true }

// Ping checks Xray answers, for the deep health check.
func (p *XrayProvider) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

// Scan implements security.Provider.
//
// # Parallel, in batches, bounded
//
// A release in this system is 157 images. Asking Xray about them one at a time
// is 157 sequential round trips against an endpoint that answers in hundreds of
// milliseconds, which is a minute of staring at a spinner for information Xray
// could return in a few seconds.
//
// So: batched (one call asks about fifty artifacts) and parallel (a few calls
// in flight). Both bounds matter and they bound different things. The batch
// bounds the blast radius of one failure - a failed call costs fifty artifacts'
// results, not a release's. The concurrency bounds what we do to Xray, which is
// rate-limited and whose summary endpoint is expensive server-side; sixty
// parallel calls is not ten times faster than six, it is a 429 storm.
//
// # Why a failed batch is not an error
//
// It becomes fifty StatusUnavailable reports. One image Xray would not answer
// for must not lose the other hundred, and - critically - must not silently
// become an image with no findings.
func (p *XrayProvider) Scan(ctx context.Context, refs []security.ArtifactRef, opts security.ScanOptions) ([]security.Report, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	now := time.Now().UTC()
	reports := make([]security.Report, len(refs))

	// Artifacts Xray cannot have an opinion about are answered locally and
	// never reach the wire.
	var queryable []int
	for i, ref := range refs {
		if !scannable(ref) {
			reports[i] = security.Report{
				Artifact:    ref,
				Status:      security.StatusUnsupported,
				Provider:    providerName,
				Message:     "JFrog Xray does not scan this kind of artifact.",
				RetrievedAt: now,
			}
			continue
		}
		if checksumOf(ref.Digest) == "" {
			reports[i] = security.Report{
				Artifact:    ref,
				Status:      security.StatusUnavailable,
				Provider:    providerName,
				Message:     "This artifact has no digest, so JFrog Xray cannot be asked about it.",
				RetrievedAt: now,
			}
			continue
		}
		queryable = append(queryable, i)
	}

	if len(queryable) == 0 {
		security.ReportStage(opts.Progress, security.StageFetching, 0, 0)
		return reports, nil
	}

	batches := batchIndexes(queryable, p.client.BatchSize())
	security.ReportStage(opts.Progress, security.StageFetching, 0, len(queryable))
	security.ReportNote(opts.Progress, fmt.Sprintf(
		"Asking JFrog Xray about %d artifacts in %d requests.", len(queryable), len(batches)))

	var (
		mu   sync.Mutex
		done int
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency)

	for _, batch := range batches {
		batch := batch
		g.Go(func() error {
			checksums := make([]string, 0, len(batch))
			for _, i := range batch {
				checksums = append(checksums, refs[i].Digest)
			}

			found, notIndexed, err := p.client.ArtifactSummary(gctx, checksums)

			mu.Lock()
			defer mu.Unlock()
			for _, i := range batch {
				reports[i] = p.reportFor(refs[i], found, notIndexed, err, opts.Detail, now)
			}
			done += len(batch)
			security.ReportStage(opts.Progress, security.StageFetching, done, len(queryable))
			return nil
		})
	}

	// No batch returns an error, by construction: a per-artifact failure is a
	// report. An error here can only be a cancelled context, and that one IS
	// worth returning - the caller asked us to stop.
	if err := g.Wait(); err != nil {
		return reports, err
	}
	if err := ctx.Err(); err != nil {
		return reports, err
	}
	return reports, nil
}

// reportFor decides what one artifact's report says.
//
// The three-way split here is the single most important piece of logic in the
// package. "Xray answered and found nothing", "Xray has never looked at this"
// and "Xray would not answer" are three different facts that all produce an
// empty finding list, and merging any two of them puts an unscanned image on a
// page that says the release is clean.
func (p *XrayProvider) reportFor(
	ref security.ArtifactRef,
	found map[string]xrayArtifact,
	notIndexed map[string]string,
	callErr error,
	detail bool,
	now time.Time,
) security.Report {
	r := security.Report{Artifact: ref, Provider: providerName, RetrievedAt: now}

	if callErr != nil {
		r.Status = security.StatusUnavailable
		r.Message = describeXrayFailure(callErr)
		return r
	}

	key := checksumOf(ref.Digest)
	artifact, ok := found[key]
	if !ok {
		r.Status = security.StatusNotScanned
		r.Message = notIndexedMessage(notIndexed[key])
		return r
	}

	r.Status = security.StatusScanned
	r.ScannedAt = parseXrayTime(artifact.General.ScanTime)
	findings := normalizeArtifact(artifact)
	r.Findings = findings
	r.Recount()
	// A counts-only request keeps the arithmetic and drops the rows. That is
	// what a package listing's vulnerability column needs, and shipping a
	// megabyte of findings to render a number is the difference between a
	// listing that opens and one that does not.
	if !detail {
		r.Findings = nil
	}
	return r
}

// describeXrayFailure says what went wrong in words with an action in them.
func describeXrayFailure(err error) string {
	var re *registry.Error
	if ok := asRegistryError(err, &re); ok {
		switch registry.ClassOf(re.Err) {
		case registry.ClassAuth:
			return "JFrog Xray refused the repository credential. " + detailOr(re, "Check that the credential has Xray read permission.")
		case registry.ClassRateLimited:
			return "JFrog Xray is rate limiting this request. " + detailOr(re, "Try again shortly.")
		case registry.ClassNotFound:
			return "JFrog Xray did not answer at this endpoint. " + detailOr(re, "Check the Xray endpoint configured for this repository.")
		}
		return "JFrog Xray could not be reached. " + detailOr(re, re.Error())
	}
	return "JFrog Xray could not be reached: " + err.Error()
}

func detailOr(e *registry.Error, fallback string) string {
	if e.Detail != "" {
		return e.Detail
	}
	return fallback
}

// asRegistryError is errors.As without the import cycle of a helper package.
func asRegistryError(err error, target **registry.Error) bool {
	for err != nil {
		if e, ok := err.(*registry.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// batchIndexes splits indexes into batches of at most size, deterministically.
func batchIndexes(idx []int, size int) [][]int {
	if size <= 0 {
		size = DefaultBatchSize
	}
	sorted := append([]int(nil), idx...)
	sort.Ints(sorted)

	var out [][]int
	for start := 0; start < len(sorted); start += size {
		end := start + size
		if end > len(sorted) {
			end = len(sorted)
		}
		out = append(out, sorted[start:end])
	}
	return out
}

// PathFor builds the Artifactory storage path of an artifact's manifest, for
// deployments where Xray indexed by path rather than by a checksum we hold.
//
// Not used by the checksum lookup and kept because it is the documented escape
// hatch: `<repoKey>/<image path>/<tag>/manifest.json` is where a Docker
// manifest lives in Artifactory, and an operator debugging a "not indexed"
// answer needs to be able to see the path we would have asked about.
func (p *XrayProvider) PathFor(ref security.ArtifactRef) string {
	repoKey := p.client.repoKey
	if repoKey == "" || ref.Repository == "" {
		return ""
	}
	path := strings.TrimPrefix(ref.Repository, repoKey+"/")
	tag := ref.Tag
	if tag == "" {
		tag = checksumOf(ref.Digest)
	}
	return fmt.Sprintf("%s/%s/%s/manifest.json", repoKey, path, tag)
}

var _ security.Provider = (*XrayProvider)(nil)
