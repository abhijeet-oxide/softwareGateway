package artifactory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	if skipped := len(refs) - len(queryable); skipped > 0 {
		security.ReportNote(opts.Progress, fmt.Sprintf(
			"%d artifacts are signatures or attestations, which Xray does not scan.", skipped))
	}

	var (
		mu       sync.Mutex
		done     int
		failed   int
		requests int
	)

	// record commits one batch's outcome and reports where the sync has got to.
	//
	// Counting FAILURES as well as progress, because "142 of 258" tells a
	// watcher the work is moving and nothing else. On a scanner that is timing
	// out, the number that matters is the one going up beside it.
	record := func(batch []int, found map[string]xrayArtifact, notIndexed map[string]string, err error) {
		mu.Lock()
		defer mu.Unlock()
		for _, i := range batch {
			reports[i] = p.reportFor(refs[i], found, notIndexed, err, opts.Detail, now)
			if reports[i].Status == security.StatusUnavailable {
				failed++
			}
		}
		done += len(batch)
		requests++
		security.ReportStage(opts.Progress, security.StageFetching, done, len(queryable))
		if failed > 0 {
			security.ReportStage(opts.Progress, security.StageFailing, failed, len(queryable))
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency)

	for _, batch := range batches {
		batch := batch
		g.Go(func() error {
			p.fetchBatch(gctx, refs, batch, record, opts.Progress)
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

// fetchBatch asks Xray about one batch, and SPLITS it rather than losing it.
//
// # The failure this exists for
//
// A real release is 258 images. Asked in batches of fifty, a busy Xray answered
// six requests with `context deadline exceeded` and 209 artifacts came back
// unavailable - four fifths of a release reported as unknown because a handful
// of images were slow to summarise. The batch is an optimisation, and an
// optimisation that loses the answer is not one.
//
// So a batch that TIMES OUT is halved and both halves are retried, down to a
// single artifact. The cost is bounded: a batch of fifty that fails entirely
// costs at most ninety-nine requests, and one that fails because of a single
// slow image costs about a dozen and returns the other forty-nine. The
// alternative - a smaller batch for everybody - pays the round trips on every
// sync against a scanner that is coping perfectly well.
//
// Only a TIMEOUT is worth splitting. A 401 will refuse the halves too, and
// retrying it fifty times is a way to get an account locked rather than an
// answer, so every other failure is recorded as it stands.
func (p *XrayProvider) fetchBatch(
	ctx context.Context,
	refs []security.ArtifactRef,
	batch []int,
	record func([]int, map[string]xrayArtifact, map[string]string, error),
	progress security.Progress,
) {
	checksums := make([]string, 0, len(batch))
	for _, i := range batch {
		checksums = append(checksums, refs[i].Digest)
	}

	found, notIndexed, err := p.client.ArtifactSummary(ctx, checksums)
	if err == nil || len(batch) == 1 || !worthSplitting(ctx, err) {
		record(batch, found, notIndexed, err)
		return
	}

	half := len(batch) / 2
	security.ReportNote(progress, fmt.Sprintf(
		"JFrog Xray timed out on %d artifacts; asking about them in two smaller requests.", len(batch)))

	// Sequentially, not in parallel. The scanner has just told us it is
	// struggling, and answering that by doubling the requests in flight is how
	// a slow Xray becomes an unreachable one.
	p.fetchBatch(ctx, refs, batch[:half], record, progress)
	p.fetchBatch(ctx, refs, batch[half:], record, progress)
}

// worthSplitting reports whether a failure might succeed on a smaller batch.
//
// Timeouts and rate limits might: both are about how much was asked for at
// once. A refused credential, a missing endpoint or a malformed answer will
// not, and retrying them per artifact turns one clear error into fifty.
func worthSplitting(ctx context.Context, err error) bool {
	// A cancelled sync is not a slow scanner. Splitting here would fire two
	// more doomed requests per level for the whole tree.
	if ctx.Err() != nil {
		return false
	}
	var re *registry.Error
	if errors.As(err, &re) {
		// A gateway timeout is the scanner's own way of saying the request was
		// too big to finish, and it arrives as a 504 rather than as a client
		// deadline - so it classifies as "unavailable" and would otherwise be
		// written off. 408 is the same statement from the other side.
		switch re.StatusCode {
		case http.StatusGatewayTimeout, http.StatusRequestTimeout:
			return true
		}
		switch registry.ClassOf(re.Err) {
		case registry.ClassTimeout, registry.ClassRateLimited:
			return true
		default:
			return false
		}
	}
	// An unclassified transport error is most often a deadline that never
	// reached the classifier, so it gets one split rather than being written
	// off - the recursion bottoms out at a single artifact either way.
	return errors.Is(err, context.DeadlineExceeded)
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

// describeXrayFailure says what went wrong in a sentence a person can act on.
//
// # Why the raw error is not good enough
//
// It was, and it read like this in a tooltip:
//
//	JFrog Xray could not be reached. POST /xray/api/v1/summary/artifact
//	artifact.example.com: Post "https://artifact.example.com/xray/api/v1/
//	summary/artifact": context deadline exceeded
//
// Three restatements of one URL and a Go phrase, for a reader who wants to know
// whether their scanner is down, their credential is wrong, or the request was
// simply too big. Each of those has a different fix and none of them is in that
// string.
func describeXrayFailure(err error) string {
	var re *registry.Error
	if errors.As(err, &re) {
		switch registry.ClassOf(re.Err) {
		case registry.ClassAuth:
			return "JFrog Xray refused the credential. " +
				detailOr(re, "The repository credential is valid for the registry but has no Xray read permission.")
		case registry.ClassRateLimited:
			return "JFrog Xray is rate limiting these requests. " +
				detailOr(re, "Lower coordinator.security.concurrency, or sync again when it is quieter.")
		case registry.ClassNotFound:
			return "JFrog Xray is not answering at this address. " +
				detailOr(re, "Xray may not be installed on this platform, or the docker host is a subdomain and xrayEndpoint needs to name the platform URL.")
		case registry.ClassTimeout:
			// The one people actually hit, and the one whose raw form says
			// least. Names the bound so it can be changed, and says what the
			// platform already did about it.
			return "JFrog Xray did not answer in time, even after the request was split into smaller ones. " +
				"It is reachable but slow for these artifacts - raise coordinator.security.requestTimeout, " +
				"or sync again when it is under less load."
		case registry.ClassUnavailable:
			if re.StatusCode == http.StatusGatewayTimeout || re.StatusCode == http.StatusRequestTimeout {
				return "JFrog Xray timed out on these artifacts, even after the request was split into smaller ones. " +
					"Raise coordinator.security.requestTimeout, or sync again when it is under less load."
			}
			return "JFrog Xray returned an error. " + detailOr(re, "Check the Xray service on this JFrog platform.")
		case registry.ClassMalformed:
			// It answered. Saying it could not be reached sends somebody to
			// check a network path that is demonstrably fine, when what is in
			// front of them is a proxy's error page, an SSO login form, or an
			// Xray whose response shape this build does not know.
			return "JFrog Xray answered with something this Coordinator could not read. " +
				detailOr(re, "Something between here and Xray may be returning an error page or a login form "+
					"in place of the summary - check xrayEndpoint and any proxy on the path.")
		}
		return "JFrog Xray could not be reached. " + detailOr(re, "Check the network path from this Coordinator to the JFrog platform.")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "JFrog Xray did not answer in time, even after the request was split into smaller ones. " +
			"Raise coordinator.security.requestTimeout, or sync again when it is under less load."
	}
	return "JFrog Xray could not be reached: " + err.Error()
}

func detailOr(e *registry.Error, fallback string) string {
	if e.Detail != "" {
		return e.Detail
	}
	return fallback
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
