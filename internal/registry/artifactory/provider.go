package artifactory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
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
	client *XrayClient
	// pace is how hard to push THIS scanner, and it outlives one sync on
	// purpose. A release synced at 2am against a busy Xray teaches the pacer
	// what that Xray can answer; the next release should not have to learn it
	// again from a sixty-second timeout. See pacer.go.
	pace *pacer
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
	return &XrayProvider{
		client: client,
		pace:   newPacer(settings.Concurrency, client.BatchSize()),
	}, nil
}

// Name implements security.Provider.
func (p *XrayProvider) Name() string { return providerName }

// Enabled implements security.Provider.
func (p *XrayProvider) Enabled() bool { return true }

// Ping checks Xray answers, for the deep health check.
func (p *XrayProvider) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

// Scan implements security.Provider.
//
// # Parallel, in batches, and both of them adaptive
//
// A release in this system is 157 images, sometimes 260. Asking Xray about them
// one at a time is that many sequential round trips for information Xray can
// return in a few seconds, so requests are batched and several are in flight.
//
// What changed is that neither number is a constant any more. A batch of fifty
// is right for an Xray with headroom and catastrophic for one that is busy: the
// request times out after a minute, and until now every one of the six
// outstanding batches discovered that independently, then split itself and
// discovered it again. One real sync spent thirteen of its fourteen minutes
// logging the same sentence twenty-four times.
//
// So the size of a batch and the number in flight are decided by a pacer shared
// across every request to this scanner (pacer.go), work is handed out by a queue
// rather than partitioned up front (queue.go), and a batch that times out is
// SPLIT BACK ONTO THE QUEUE rather than retried in series inside the slot it
// already holds. The first timeout still costs a minute. The twentieth does not
// happen, because by then the batch is twelve.
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
				Message:     unsupportedMessage(ref),
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
		security.ReportStage(opts.Progress, security.StageScanning, 0, 0)
		return reports, nil
	}

	sort.Ints(queryable)
	security.ReportStage(opts.Progress, security.StageScanning, 0, len(queryable))

	// What is about to happen, in a sentence, at INFO.
	//
	// This was written at warning level, so a sync doing exactly what it should
	// opened its transcript with an amber line - and a reader who learns that
	// the normal case looks like a problem stops reading the ones that are.
	opening := fmt.Sprintf("Fetching JFrog Xray for %d images, %d per request.",
		len(queryable), p.pace.Batch())
	if skipped := len(refs) - len(queryable); skipped > 0 {
		opening += fmt.Sprintf(
			" %d other artifacts in this release are charts, signatures or files, which Xray does not scan.",
			skipped)
	}
	security.ReportInfo(opts.Progress, opening)

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
	record := func(batch []int, res summaryResult, err error) {
		mu.Lock()
		defer mu.Unlock()
		for _, i := range batch {
			reports[i] = p.reportFor(refs[i], res, err, opts.Detail, now)
			if reports[i].Status == security.StatusUnavailable {
				failed++
			}
			// The scanner's own body for this image, handed to whoever asked to
			// keep it. Emitted HERE rather than re-fetched at export time,
			// because re-fetching is the fifteen-minute sync all over again for
			// a download somebody expects to be instant.
			if opts.Sink != nil {
				if raw, ok := res.Raw[checksumOf(refs[i].Digest)]; ok && len(raw) > 0 {
					opts.Sink.Document(security.Document{
						Artifact:    refs[i],
						Kind:        security.DocumentVulnerabilities,
						Provider:    providerName,
						ContentType: "application/json",
						Payload:     append([]byte(nil), raw...),
						Available:   true,
						FetchedAt:   now,
						SourceBytes: len(raw),
					})
				}
			}
		}
		done += len(batch)
		requests++
		security.ReportStage(opts.Progress, security.StageScanning, done, len(queryable))
		if failed > 0 {
			security.ReportStage(opts.Progress, security.StageFailing, failed, len(queryable))
		}

		// One line in the transcript that MOVES, rather than nothing at all.
		//
		// The bar said "fetching 96 of 157" and the log said nothing between
		// "sync started" and whatever went wrong twelve minutes later, so a
		// reader opening the log mid-sync could not tell a slow sync from a
		// stuck one. Recorded as a replacement, so thirty updates are one line
		// carrying the current number rather than one line saying "(x30)".
		security.ReportProgress(opts.Progress, fmt.Sprintf(
			"Retrieved scan results for %d of %d images.", done, len(queryable)))
	}

	queue := newBatchQueue(queryable)
	// A cancelled sync must wake the workers waiting on the queue, or each one
	// blocks on a condition nobody will ever signal - a goroutine per worker,
	// held for the life of the process.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			queue.Close()
		case <-stop:
		}
	}()

	// One worker per slot the operator allowed. They are cheap and idle
	// whenever the pacer has withheld their slot, which is exactly the shape
	// wanted: the ceiling is what an operator configured, and how much of it is
	// used is what the scanner has earned.
	var wg sync.WaitGroup
	for range p.pace.maxInFlight {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.work(ctx, queue, refs, record, opts.Progress)
		}()
	}
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return reports, err
	}

	if batch, inFlight, shrinks, _ := p.pace.Settled(); shrinks > 0 {
		// Said once, at the end, and only when the pacer actually had to move.
		// This is the line that answers "why did that take eleven minutes" -
		// and without it the only evidence is a wall clock and a shrug.
		security.ReportInfo(opts.Progress, fmt.Sprintf(
			"JFrog Xray was slow for this release. Requests settled at %d images each, "+
				"%d at a time, and it took %d requests in total.",
			batch, inFlight, requests))
	} else {
		security.ReportInfo(opts.Progress, fmt.Sprintf(
			"JFrog Xray answered %d requests without slowing down.", requests))
	}

	p.separateMissingFromUnindexed(ctx, reports, opts.Progress)
	return reports, nil
}

// work is one worker: take a batch at the size the pacer currently allows, send
// it, and hand back whatever needs retrying.
//
// The slot is held across the whole request and released before the next Take,
// so a worker blocked on the pacer is not also holding a queue slot open - the
// deadlock that shape produces is a queue that will not hand out work because
// every worker is waiting for a slot held by a worker waiting for work.
func (p *XrayProvider) work(
	ctx context.Context,
	queue *batchQueue,
	refs []security.ArtifactRef,
	record func([]int, summaryResult, error),
	progress security.Progress,
) {
	for {
		if ctx.Err() != nil {
			return
		}
		batch := queue.Take(p.pace.Batch())
		if len(batch) == 0 {
			return
		}
		if err := p.pace.Acquire(ctx); err != nil {
			// A cancelled sync. The batch is abandoned deliberately: its
			// artifacts keep whatever they had, and Scan returns the context's
			// error so the caller knows this run did not finish.
			queue.Done()
			return
		}
		left, right := p.fetchBatch(ctx, refs, batch, record, progress)
		p.pace.Release()
		queue.Done(left, right)
	}
}

// separateMissingFromUnindexed turns Xray's one sentence back into two facts.
//
// Xray answers "Artifact doesn't exist or not indexed/cached in Xray" for both
// an image it has not looked at and an image that is not in the repository at
// all. The first is a scan waiting to happen; the second is a TRANSFER waiting
// to happen, and reporting it as a scanning gap sends somebody to the wrong
// team. Artifactory knows which, so it is asked - for the ones Xray declined.
//
// # In bulk, and why that matters here more than anywhere else
//
// This phase is asked about every image Xray would not answer for, which on a
// release that has not been replicated yet is EVERY image. Per image, that was
// 260 round trips six at a time - the only part of a sync whose request count
// scaled with the number of artifacts rather than with the number of batches,
// and it ran in exactly the situation somebody is already waiting on a slow
// answer. One AQL query answers a hundred, so a release costs three requests
// and they still go out in parallel.
//
// The per-image search remains for accounts AQL is closed to. It is the same
// question asked the slow way, not a degraded answer.
func (p *XrayProvider) separateMissingFromUnindexed(
	ctx context.Context, reports []security.Report, progress security.Progress,
) {
	var pending []int
	for i := range reports {
		if reports[i].Status == security.StatusNotScanned && reports[i].Artifact.Digest != "" {
			pending = append(pending, i)
		}
	}
	if len(pending) == 0 {
		return
	}

	var (
		mu       sync.Mutex
		missing  int
		failed   bool
		perImage []int
	)

	// found reports what one chunk learned. Called from several goroutines.
	record := func(indexes []int, stored map[string]bool) {
		mu.Lock()
		defer mu.Unlock()
		for _, i := range indexes {
			if stored[checksumOf(reports[i].Artifact.Digest)] {
				reports[i].Message = notIndexedYetMessage
				continue
			}
			reports[i].Missing = true
			reports[i].Message = notInRepositoryMessage
			missing++
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.pace.InFlight())

	for _, chunk := range chunkIndexes(pending, ChecksumQueryLimit) {
		chunk := chunk
		g.Go(func() error {
			digests := make([]string, 0, len(chunk))
			for _, i := range chunk {
				digests = append(digests, reports[i].Artifact.Digest)
			}
			stored, err := p.client.StoredChecksums(gctx, digests)
			switch {
			case err == nil:
				record(chunk, stored)
			case errors.Is(err, errBatchUnsupported), errors.Is(err, errBatchInconclusive):
				// Answerable, just not in bulk - either this account cannot use
				// AQL, or the answer may have been truncated and its silences
				// cannot be trusted. Queued rather than probed here so the
				// fallback runs at the same bounded concurrency instead of one
				// request per already-running goroutine.
				mu.Lock()
				perImage = append(perImage, chunk...)
				mu.Unlock()
			default:
				// The probe is an enrichment. Losing it costs the distinction,
				// never the report, so the message stays as Xray phrased it.
				mu.Lock()
				failed = true
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()

	if len(perImage) > 0 {
		fallbackMissing, fallbackFailed := p.probeEachImage(ctx, reports, perImage)
		missing += fallbackMissing
		failed = failed || fallbackFailed
	}

	switch {
	case failed:
		security.ReportWarning(progress,
			"Artifactory did not say which of the unscanned images it holds, "+
				"so none of them are reported as missing.")
	case missing > 0:
		// A warning, and the one worth having: the fix is a TRANSFER, owned by
		// somebody other than whoever owns scanning.
		security.ReportWarning(progress, fmt.Sprintf(
			"%d images are not in the JFrog repository yet, so there is nothing there to scan. "+
				"Transfer this release, then sync again.", missing))
	default:
		security.ReportInfo(progress, fmt.Sprintf(
			"Checked %d unscanned images against the JFrog repository: all of them are there, "+
				"waiting to be indexed.", len(pending)))
	}
}

// probeEachImage is the fallback: one search per image, still bounded.
//
// Only reached where AQL is closed to this account. Kept because the
// distinction it draws - "not transferred" against "not indexed" - is the
// difference between a job for the replication team and one for whoever owns
// scanning, and losing it on a locked-down platform would be a worse trade than
// the requests it costs.
func (p *XrayProvider) probeEachImage(
	ctx context.Context, reports []security.Report, pending []int,
) (missing int, failed bool) {
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.pace.InFlight())

	for _, i := range pending {
		i := i
		g.Go(func() error {
			stored, err := p.client.StoresChecksum(gctx, reports[i].Artifact.Digest)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = true
				return nil
			}
			if !stored {
				reports[i].Missing = true
				reports[i].Message = notInRepositoryMessage
				missing++
			} else {
				reports[i].Message = notIndexedYetMessage
			}
			return nil
		})
	}
	_ = g.Wait()
	return missing, failed
}

// chunkIndexes splits a list of positions into runs of at most `size`.
func chunkIndexes(indexes []int, size int) [][]int {
	if size <= 0 {
		size = 1
	}
	out := make([][]int, 0, (len(indexes)+size-1)/size)
	for start := 0; start < len(indexes); start += size {
		end := start + size
		if end > len(indexes) {
			end = len(indexes)
		}
		out = append(out, indexes[start:end])
	}
	return out
}

const (
	notInRepositoryMessage = "This image is not in the JFrog repository. Transfer the release there, " +
		"then sync again."
	notIndexedYetMessage = "This image is in the JFrog repository. JFrog Xray has not indexed it yet."
)

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
// So a batch that TIMES OUT is halved. What changed is what happens to the
// halves: they are RETURNED, for the caller to put back on the queue, instead
// of being re-sent here one after the other. That recursion was correct and it
// serialized the recovery - a batch needing four splits paid four sixty-second
// timeouts in a row inside one slot, while the other workers had nothing to do.
// Returned, the halves are picked up by whichever worker is free, in parallel,
// inside the same global allowance the semaphore enforces.
//
// The failure also teaches the pacer, so the NEXT batch any worker takes is
// already smaller. That is the difference between paying the discovery once and
// paying it twenty-four times.
//
// Only a TIMEOUT is worth splitting. A 401 will refuse the halves too, and
// retrying it fifty times is a way to get an account locked rather than an
// answer, so every other failure is recorded as it stands.
func (p *XrayProvider) fetchBatch(
	ctx context.Context,
	refs []security.ArtifactRef,
	batch []int,
	record func([]int, summaryResult, error),
	progress security.Progress,
) (left, right []int) {
	checksums := make([]string, 0, len(batch))
	for _, i := range batch {
		checksums = append(checksums, refs[i].Digest)
	}

	res, err := p.client.ArtifactSummary(ctx, checksums)
	if err == nil {
		p.pace.Win()
		record(batch, res, nil)
		return nil, nil
	}

	// The pacer learns from a failure the smaller batch might fix, and from a
	// refusal it will not. A 401 told it to back off would slow every later
	// sync against a scanner that is perfectly healthy and simply does not know
	// us.
	if split := worthSplitting(ctx, err); split {
		p.pace.Setback(isRateLimited(err))
	}

	if len(batch) == 1 || !worthSplitting(ctx, err) {
		record(batch, res, err)
		return nil, nil
	}

	half := len(batch) / 2
	// What this MEANS, not how many retries it took.
	//
	// It said "JFrog Xray timed out on 13 artifacts. Retrying as two smaller
	// requests", and on a struggling scanner that arrived twenty-four times.
	// Twenty-four tellings of one situation, each naming an internal mechanism,
	// none of them saying the only thing a reader wants: is this sync going to
	// finish, and is anything being lost? Nothing is - the images are re-asked
	// about - and the honest headline is that the requests got smaller.
	security.ReportWarningUpdate(progress, fmt.Sprintf(
		"JFrog Xray is responding slowly, reducing batch size from "+
			"%d images to %d at a time. Failed ones are retried automatically.",
		p.pace.InFlight(), p.pace.Batch()))
	return batch[:half], batch[half:]
}

// isRateLimited reports whether a failure is the scanner saying "too many at
// once" rather than "too much at once".
//
// The two want opposite corrections - fewer requests in flight against a
// smaller request - and answering one with the other makes a sync slower
// without making it quieter.
func isRateLimited(err error) bool {
	var re *registry.Error
	if !errors.As(err, &re) {
		return false
	}
	if re.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return registry.ClassOf(re.Err) == registry.ClassRateLimited
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
	// Asked FIRST, and of the whole chain. A deadline reaches here dressed as
	// whatever noticed it - a *url.Error from the transport, a read error from
	// the JSON decoder - and the class switch below answered "no" to both. That
	// is why a release could lose two hundred artifacts to timeouts without a
	// single retry: the splitting was correct and simply never reached.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// A body that stopped arriving part way through. Same cause as a deadline -
	// a scanner struggling with the size of the answer - and the same remedy.
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
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
	res summaryResult,
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
	artifact, ok := res.Found[key]
	if !ok {
		r.Status = security.StatusNotScanned
		r.Message = notIndexedMessage(res.NotIndexed[key])
		return r
	}

	r.Status = security.StatusScanned
	r.ScannedAt = parseXrayTime(artifact.General.ScanTime)
	findings, malware := normalizeArtifact(artifact)
	r.Findings = findings
	// Malware is carried beside the findings rather than among them. A
	// malicious package is not a backlog item to grade against ninety thousand
	// others; it is a release that does not ship, and burying it in a table
	// sorted by severity is how it ships anyway.
	r.Malware = malware
	r.Recount()
	// A counts-only request keeps the arithmetic and drops the rows. That is
	// what a package listing's vulnerability column needs, and shipping a
	// megabyte of findings to render a number is the difference between a
	// listing that opens and one that does not.
	if !detail {
		r.Findings = nil
		r.Malware = nil
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
