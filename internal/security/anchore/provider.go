package anchore

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

// Settings is the operator-level half of the Anchore configuration: how hard
// to push it, and how long a sync will wait for it.
//
// Separate from Config, which says where it is, for the same reason the Xray
// path splits them: where a scanner lives is a deployment fact and how hard to
// push it is an operator's decision, and a product document should state
// neither.
type Settings struct {
	// Enabled is the switch. False builds a security.Disabled that answers
	// every request with a sentence saying so.
	Enabled bool

	// Registry and Repository are where Anchore pulls this release's images
	// from - the INTERNAL registry a release was replicated into, never the
	// vendor's. See PullString.
	Registry   string
	Repository string

	// Application and Version name the release in Anchore's own model: the
	// product's name and the release's version. Empty for either switches the
	// grouping off, and the provider then reads per-image results only - which
	// is a legitimate configuration for a deployment that has its own
	// application taxonomy and does not want ours.
	Application string
	Version     string

	// Concurrency caps requests in flight against Anchore. Zero uses
	// DefaultConcurrency.
	Concurrency int
	// AnalysisWait is how long a sync waits for images it submitted. Zero uses
	// DefaultAnalysisWait; negative means do not wait at all, which is right
	// for a deployment that submits on transfer and syncs later.
	AnalysisWait time.Duration
	// PollInterval is how often a waiting sync re-asks. Zero uses the default.
	PollInterval time.Duration

	// SBOMFormat is which flavour of SBOM to fetch: spdx-json, cyclonedx-json
	// or native-json. Empty uses SPDX, which is what the person pressing
	// "download SBOM" is overwhelmingly about to send to somebody else.
	SBOMFormat string

	// Submit says whether this sync may submit images Anchore does not have.
	//
	// True by default and worth being able to turn off: an estate that
	// registers its images with Anchore by its own pipeline wants this
	// platform to READ Anchore, not to add to it, and a sync that submitted
	// anyway would duplicate their registration under our annotations.
	Submit bool
}

// Provider implements security.Provider over Anchore Enterprise.
type Provider struct {
	client   *Client
	settings Settings

	// version is the Anchore Application Version this release maps to, resolved
	// once per provider and reused.
	//
	// Cached because a provider is built per (product, repository) and every
	// scan of every release through it wants the same lookup - and because the
	// find-or-create is three requests that must not be repeated per batch.
	versionOnce sync.Once
	version     Version
	versionErr  error
}

// New builds the provider for one release's worth of configuration.
//
// Returns security.Disabled when Anchore is switched off, rather than a nil the
// caller must remember to check. The nil check is the one that gets forgotten,
// and the failure mode of forgetting it is a release that renders as clean.
func New(cfg Config, settings Settings) (security.Provider, error) {
	if !settings.Enabled {
		return security.Disabled{
			ProviderName: ProviderName,
			Reason:       "Anchore is not enabled for this repository.",
		}, nil
	}
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Provider{client: client, settings: settings}, nil
}

// Name implements security.Provider.
func (p *Provider) Name() string { return ProviderName }

// Enabled implements security.Provider.
func (p *Provider) Enabled() bool { return true }

// Ping checks Anchore answers, for the deep health check.
func (p *Provider) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

// Endpoint is the API base URL, so a caller can build a link into Anchore.
func (p *Provider) Endpoint() string { return p.client.Endpoint() }

// VersionURL is where a person opens this release in Anchore, once the
// grouping has been resolved. Empty before the first scan, or where the
// deployment has switched grouping off.
func (p *Provider) VersionURL() string {
	if p.version.VersionID == "" {
		return ""
	}
	return p.version.URL(p.client.Endpoint())
}

// Scan implements security.Provider.
//
// # The four phases, and why they are in this order
//
//  1. TAKE STOCK. One request asks Anchore what it already knows about every
//     image in the release. A re-synced release is overwhelmingly images that
//     are already analysed, and this is what makes that case cost one request
//     rather than one per image.
//  2. SUBMIT what is missing. Only what Anchore has never been told about; a
//     resubmission would be a no-op at best and a forced re-analysis at worst.
//  3. WAIT for what is analysing, up to a bound, saying what it is waiting for.
//     A first sync is entirely this phase, and a sync that skipped it would
//     report a whole release as unscanned every single time it was first run.
//  4. READ, in parallel, and GROUP: the per-image findings, and the
//     application version that makes the release one thing in Anchore.
//
// Grouping is last on purpose. It associates only images that reached
// `analyzed`, so it has to follow the wait - and it is the phase whose failure
// costs the least, because per-image findings are already in hand.
func (p *Provider) Scan(
	ctx context.Context, refs []security.ArtifactRef, opts security.ScanOptions,
) ([]security.Report, error) {
	if len(refs) == 0 {
		return nil, nil
	}

	now := time.Now().UTC()
	reports := make([]security.Report, len(refs))

	// Artifacts Anchore cannot have an opinion about are answered locally and
	// never reach the wire.
	var queryable []int
	for i, ref := range refs {
		switch {
		case !scannable(ref):
			reports[i] = security.Report{
				Artifact: ref, Status: security.StatusUnsupported, Provider: ProviderName,
				Message:     unsupportedMessage(ref),
				RetrievedAt: now,
			}
		case strings.TrimSpace(ref.Digest) == "":
			reports[i] = security.Report{
				Artifact: ref, Status: security.StatusUnavailable, Provider: ProviderName,
				Message:     "This artifact has no digest, so Anchore cannot be asked about it.",
				RetrievedAt: now,
			}
		default:
			queryable = append(queryable, i)
		}
	}
	if len(queryable) == 0 {
		security.ReportStage(opts.Progress, security.StageFetching, 0, 0)
		return reports, nil
	}

	// Every queryable image needs an internal registry path for Anchore to
	// pull from. Answered here rather than at submission time so a release that
	// has not been replicated yet says so once per image with an action in it,
	// instead of failing a hundred and fifty POSTs.
	pullable := make([]int, 0, len(queryable))
	for _, i := range queryable {
		if _, err := PullString(withLocation(refs[i], p.settings)); err != nil {
			reports[i] = security.Report{
				Artifact: refs[i], Status: security.StatusNotScanned, Provider: ProviderName,
				Missing:     true,
				Message:     notReplicatedMessage,
				RetrievedAt: now,
			}
			continue
		}
		pullable = append(pullable, i)
	}
	if len(pullable) == 0 {
		security.ReportWarning(opts.Progress,
			"None of this release's images are in the registry Anchore pulls from. "+
				"Transfer the release, then sync again.")
		return reports, nil
	}

	security.ReportStage(opts.Progress, security.StageResolving, 0, len(pullable))
	opening := fmt.Sprintf("Asking Anchore about %d images.", len(pullable))
	if skipped := len(refs) - len(pullable); skipped > 0 {
		opening += fmt.Sprintf(
			" %d other artifacts in this release are charts, signatures, files or images that are"+
				" not in the registry Anchore pulls from.", skipped)
	}
	security.ReportInfo(opts.Progress, opening)

	digests := make([]string, 0, len(pullable))
	for _, i := range pullable {
		digests = append(digests, refs[i].Digest)
	}

	// Phase 1 and 2: what Anchore has, and what it has to be told about.
	known, err := p.client.GetImages(ctx, digests)
	if err != nil {
		// A list that will not load is not a release with no findings. Every
		// image is reported unavailable with the reason, and the sync's own
		// rule then refuses to record that as a clean release.
		message := describeFailure(err)
		for _, i := range pullable {
			reports[i] = security.Report{
				Artifact: refs[i], Status: security.StatusUnavailable, Provider: ProviderName,
				Message: message, RetrievedAt: now,
			}
		}
		return reports, nil
	}

	known = p.submitMissing(ctx, refs, pullable, known, opts.Progress)

	// Phase 3: wait for what is being analysed.
	known = p.awaitAnalysis(ctx, digests, known, opts.Progress)

	// Phase 4a: read what is analysed.
	p.readFindings(ctx, refs, pullable, known, reports, now, opts)

	// Phase 4b: the release, as one thing in Anchore.
	p.groupRelease(ctx, refs, pullable, known, opts)

	if err := ctx.Err(); err != nil {
		return reports, err
	}
	return reports, nil
}

const notReplicatedMessage = "This image is not in the registry Anchore pulls from. " +
	"Transfer the release there, then sync again."

// submitMissing tells Anchore about images it has never seen.
func (p *Provider) submitMissing(
	ctx context.Context, refs []security.ArtifactRef, pullable []int,
	known map[string]ImageRecord, progress security.Progress,
) map[string]ImageRecord {
	var missing []int
	for _, i := range pullable {
		if rec := known[refs[i].Digest]; !rec.Known {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return known
	}
	if !p.settings.Submit {
		// A deliberate configuration, said out loud. An operator who switched
		// submission off should see the consequence named rather than a
		// release that is quietly a third scanned.
		security.ReportWarning(progress, fmt.Sprintf(
			"%d images are not registered with Anchore, and this deployment does not submit images. "+
				"Register them in Anchore, then sync again.", len(missing)))
		return known
	}

	security.ReportInfo(progress, fmt.Sprintf(
		"%d images are new to Anchore and are being submitted for analysis.", len(missing)))
	security.ReportStage(progress, StageSubmitting, 0, len(missing))

	var (
		mu     sync.Mutex
		done   int
		failed int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency())
	for _, i := range missing {
		g.Go(func() error {
			rec, err := p.client.Submit(gctx, withLocation(refs[i], p.settings))
			mu.Lock()
			defer mu.Unlock()
			done++
			if err != nil {
				failed++
				known[refs[i].Digest] = ImageRecord{Digest: refs[i].Digest, Detail: describeFailure(err)}
			} else {
				known[rec.Digest] = rec
			}
			security.ReportStage(progress, StageSubmitting, done, len(missing))
			return nil
		})
	}
	_ = g.Wait()

	if failed > 0 {
		// The one failure worth naming its own cause: Anchore refusing to pull
		// is almost always its registry credential rather than anything here.
		security.ReportWarning(progress, fmt.Sprintf(
			"Anchore would not accept %d of %d images for analysis. "+
				"Check that Anchore has a registry configured for %s.",
			failed, len(missing), p.settings.Registry))
	}
	return known
}

// StageSubmitting counts images being registered with Anchore.
//
// Its own stage rather than folding into "fetching", because it is a different
// wait with a different remedy: fetching is slow because a scanner is busy, and
// this is slow because a release has never been submitted - which the reader
// wants to see happening rather than infer from a bar that has not moved.
const StageSubmitting = "submitting"

// StageAnalysing counts images Anchore has finished analysing.
const StageAnalysing = "analysing"

// awaitAnalysis waits for images Anchore is still working on.
//
// # Why a sync waits at all, and why it gives up
//
// A first sync submits everything and finds nothing analysed, so a sync that
// read immediately would report the release as unscanned and leave the reader
// with no way to know that pressing Sync again in five minutes is exactly
// right. Waiting turns that into one press.
//
// It gives up at AnalysisWait because the alternative is holding a claim - and
// therefore blocking every later sync of this release - for as long as
// somebody's Anchore backlog takes. What has finished is recorded; what has not
// is reported as still analysing, with the sentence that says what to do.
func (p *Provider) awaitAnalysis(
	ctx context.Context, digests []string, known map[string]ImageRecord, progress security.Progress,
) map[string]ImageRecord {
	wait := p.settings.AnalysisWait
	if wait == 0 {
		wait = DefaultAnalysisWait
	}
	if wait < 0 {
		return known
	}
	interval := p.settings.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}

	pending := func() []string {
		var out []string
		for _, d := range digests {
			if rec := known[d]; rec.Known && !rec.Terminal() {
				out = append(out, d)
			}
		}
		return out
	}

	waiting := pending()
	if len(waiting) == 0 {
		return known
	}

	analysed := len(digests) - len(waiting)
	security.ReportStage(progress, StageAnalysing, analysed, len(digests))
	security.ReportInfo(progress, fmt.Sprintf(
		"Anchore is analysing %d of %d images. Waiting up to %s for it to finish.",
		len(waiting), len(digests), wait.Round(time.Minute)))

	deadline := time.Now().Add(wait)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return known
		case <-ticker.C:
		}

		fresh, err := p.client.GetImages(ctx, digests)
		if err != nil {
			// A poll that failed is not an analysis that failed. Reported as a
			// line that UPDATES rather than a new one each time, so a slow
			// Anchore is one situation in the transcript rather than forty.
			security.ReportWarningUpdate(progress,
				"Anchore did not answer while waiting for analysis to finish: "+describeFailure(err))
			if time.Now().After(deadline) {
				return known
			}
			continue
		}
		for d, rec := range fresh {
			if rec.Known {
				known[d] = rec
			}
		}

		waiting = pending()
		analysed = len(digests) - len(waiting)
		security.ReportStage(progress, StageAnalysing, analysed, len(digests))
		if len(waiting) == 0 {
			security.ReportInfo(progress, "Anchore has finished analysing every image in this release.")
			return known
		}
		security.ReportProgress(progress, fmt.Sprintf(
			"Anchore has analysed %d of %d images.", analysed, len(digests)))

		if time.Now().After(deadline) {
			// Not an error, and phrased so nobody reads it as one. The findings
			// for everything that finished are about to be recorded, and the
			// remedy is one button.
			security.ReportWarning(progress, fmt.Sprintf(
				"Anchore is still analysing %d images after %s. Their findings are not in this sync; "+
					"sync again in a few minutes to pick them up.",
				len(waiting), wait.Round(time.Minute)))
			return known
		}
	}
}

// readFindings retrieves each analysed image's vulnerabilities, in parallel.
func (p *Provider) readFindings(
	ctx context.Context, refs []security.ArtifactRef, pullable []int,
	known map[string]ImageRecord, reports []security.Report, now time.Time,
	opts security.ScanOptions,
) {
	security.ReportStage(opts.Progress, security.StageFetching, 0, len(pullable))

	var (
		mu     sync.Mutex
		done   int
		failed int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency())

	for _, i := range pullable {
		g.Go(func() error {
			ref := refs[i]
			rec := known[ref.Digest]
			report := p.reportFor(gctx, ref, rec, now, opts)

			mu.Lock()
			defer mu.Unlock()
			reports[i] = report
			done++
			if report.Status == security.StatusUnavailable {
				failed++
				security.ReportStage(opts.Progress, security.StageFailing, failed, len(pullable))
			}
			security.ReportStage(opts.Progress, security.StageFetching, done, len(pullable))
			security.ReportProgress(opts.Progress, fmt.Sprintf(
				"Retrieved Anchore results for %d of %d images.", done, len(pullable)))
			return nil
		})
	}
	_ = g.Wait()
}

// reportFor decides what one artifact's report says.
//
// The three-way split is the same one the Xray provider makes and for the same
// reason: "Anchore answered and found nothing", "Anchore has not analysed this"
// and "Anchore would not answer" are three different facts that all produce an
// empty finding list, and merging any two of them puts an unscanned image on a
// page that says the release is clean.
func (p *Provider) reportFor(
	ctx context.Context, ref security.ArtifactRef, rec ImageRecord,
	now time.Time, opts security.ScanOptions,
) security.Report {
	r := security.Report{Artifact: ref, Provider: ProviderName, RetrievedAt: now}

	switch {
	case !rec.Known:
		r.Status = security.StatusNotScanned
		r.Message = "Anchore has no record of this image."
		if rec.Detail != "" {
			r.Message = "Anchore would not accept this image for analysis: " + rec.Detail
			r.Status = security.StatusUnavailable
		}
		return r
	case rec.Status == AnalysisFailed:
		// A terminal failure inside Anchore. Unavailable rather than
		// not-scanned, because nobody is going to fix it by waiting: the image
		// needs looking at, and the two states send a reader to different
		// places.
		r.Status = security.StatusUnavailable
		r.Message = "Anchore could not analyse this image."
		if rec.Detail != "" {
			r.Message += " " + rec.Detail
		}
		return r
	case rec.Status != AnalysisAnalyzed:
		r.Status = security.StatusNotScanned
		r.Message = "Anchore has not finished analysing this image yet. Sync again in a few minutes."
		return r
	}

	res, raw, err := p.client.ImageVulnerabilities(ctx, ref.Digest, opts.Refresh)
	if err != nil {
		r.Status = security.StatusUnavailable
		r.Message = describeFailure(err)
		return r
	}

	r.Status = security.StatusScanned
	r.ScannedAt = rec.AnalyzedAt.Ptr()
	r.Findings = normalizeImage(res)
	r.Recount()

	// The scanner's own body, handed to whoever asked to keep it. Emitted here
	// rather than re-fetched at export time, because re-fetching is the whole
	// sync again for a download somebody expects to be instant.
	if opts.Sink != nil && len(raw) > 0 {
		opts.Sink.Document(security.Document{
			Artifact: ref, Kind: security.DocumentVulnerabilities, Provider: ProviderName,
			ContentType: "application/json",
			Payload:     append([]byte(nil), raw...),
			Available:   true, FetchedAt: now, SourceBytes: len(raw),
		})
	}

	// A counts-only request keeps the arithmetic and drops the rows, which is
	// what a package listing's vulnerability column needs.
	if !opts.Detail {
		r.Findings = nil
	}
	return r
}

// groupRelease makes the release one thing in Anchore: an Application Version
// with this release's analysed images associated to it.
//
// # Why a failure here does not fail the scan
//
// Because the findings are already in hand. Grouping is what makes the release
// legible in ANCHORE's interface and what unlocks the release-level
// vulnerability report; losing it costs a link and a second view, not a
// release's security posture. So every failure in this phase is a note in the
// transcript and nothing more.
func (p *Provider) groupRelease(
	ctx context.Context, refs []security.ArtifactRef, pullable []int,
	known map[string]ImageRecord, opts security.ScanOptions,
) {
	if p.settings.Application == "" || p.settings.Version == "" {
		return
	}

	version, err := p.resolveVersion(ctx)
	if err != nil {
		security.ReportWarning(opts.Progress,
			"This release's images were scanned, but Anchore would not group them under an "+
				"application version: "+describeFailure(err))
		return
	}

	analysed := make([]string, 0, len(pullable))
	for _, i := range pullable {
		if known[refs[i].Digest].Analyzed() {
			analysed = append(analysed, refs[i].Digest)
		}
	}
	if len(analysed) == 0 {
		return
	}
	sort.Strings(analysed)

	rec, err := p.client.AssociateImages(ctx, version, analysed)
	if err != nil {
		security.ReportWarning(opts.Progress,
			"Anchore's application version could not be read back, so it is not known whether this "+
				"release's images are grouped under it: "+describeFailure(err))
		return
	}

	switch {
	case rec.Complete():
		security.ReportInfo(opts.Progress, fmt.Sprintf(
			"All %d analysed images are grouped under Anchore application %q version %q.",
			rec.Matched, version.ApplicationName, version.VersionName))
	default:
		// The partial case, named as the guide insists: a version holding three
		// quarters of a release reports three quarters of the truth, and a
		// reader must not take its release-level report for the whole release.
		security.ReportWarning(opts.Progress, fmt.Sprintf(
			"%d of %d images are grouped under Anchore application %q version %q. "+
				"Its release-level report covers only those.",
			rec.Matched, rec.Expected, version.ApplicationName, version.VersionName))
	}
	if len(rec.Unexpected) > 0 {
		security.ReportInfo(opts.Progress, fmt.Sprintf(
			"Anchore's %q version %q also holds %d images this release did not put there. "+
				"They are left alone.",
			version.ApplicationName, version.VersionName, len(rec.Unexpected)))
	}
}

// resolveVersion finds or creates the Application Version once per provider.
func (p *Provider) resolveVersion(ctx context.Context) (Version, error) {
	p.versionOnce.Do(func() {
		p.version, p.versionErr = p.client.FindOrCreateVersion(
			ctx, p.settings.Application, p.settings.Version)
	})
	return p.version, p.versionErr
}

func (p *Provider) concurrency() int {
	if p.settings.Concurrency > 0 {
		return p.settings.Concurrency
	}
	return DefaultConcurrency
}

// withLocation puts the internal registry path onto an artifact reference.
//
// A release's own artifacts carry the SOURCE registry - the vendor's - because
// that is where they were discovered. Anchore cannot reach it, and the copy it
// can reach is the one this provider was configured against. The substitution
// happens here rather than in the caller so that every route into this package
// - scan, submit, document - names the same image.
func withLocation(ref security.ArtifactRef, settings Settings) security.ArtifactRef {
	out := ref
	if settings.Registry != "" {
		out.Registry = settings.Registry
	}
	if settings.Repository != "" {
		out.Repository = joinRepository(settings.Repository, ref)
	}
	return out
}

// joinRepository decides the repository path inside the internal registry.
//
// The configured repository is the ROOT a product's releases land under, and a
// release's artifacts each live at their own path beneath it. Where the
// artifact's own repository already begins with that root - which it does once
// a transfer has recorded where it landed - it is used unchanged; otherwise the
// root is prepended to the artifact's own name.
func joinRepository(root string, ref security.ArtifactRef) string {
	root = strings.Trim(strings.TrimSpace(root), "/")
	own := strings.Trim(strings.TrimSpace(ref.Repository), "/")
	switch {
	case own == "":
		if ref.Name == "" {
			return root
		}
		return root + "/" + strings.Trim(ref.Name, "/")
	case root == "", own == root, strings.HasPrefix(own, root+"/"):
		return own
	default:
		return root + "/" + own
	}
}

// scannable reports whether Anchore can have an opinion about an artifact.
//
// Container images only. A Helm chart, a cosign signature and an attestation
// are not things Anchore declines to scan - they are things it has nothing to
// scan in, and counting them as unscanned coverage would permanently pin every
// release below 100%.
func scannable(ref security.ArtifactRef) bool {
	switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
	case "image", "index", "":
		break
	default:
		return false
	}
	mt := strings.ToLower(ref.MediaType)
	switch {
	case strings.Contains(mt, "helm"), strings.Contains(mt, "chart"):
		return false
	case strings.Contains(mt, "cosign"), strings.Contains(mt, "signature"):
		return false
	case strings.Contains(mt, "in-toto"), strings.Contains(mt, "attestation"):
		return false
	}
	return true
}

func unsupportedMessage(ref security.ArtifactRef) string {
	switch strings.ToLower(strings.TrimSpace(ref.Kind)) {
	case "chart":
		return "Anchore scans container images; this is a Helm chart."
	case "signature":
		return "Anchore scans container images; this is a signature."
	case "file":
		return "Anchore scans container images; this is a file."
	default:
		return "Anchore scans container images; this artifact is not one."
	}
}

// describeFailure says what went wrong in a sentence a person can act on.
//
// # Why the raw error is not good enough
//
// Because it names a URL and a Go phrase, and the reader wants to know whether
// their Anchore is down, their credential is wrong, or their Anchore cannot
// reach the registry. Each of those has a different fix and none of them is in
// the string.
func describeFailure(err error) string {
	var re *registry.Error
	if errors.As(err, &re) {
		switch registry.ClassOf(re.Err) {
		case registry.ClassAuth:
			return "Anchore refused the credential. " + detailOr(re,
				"Check the username and password (or API key) configured for Anchore, and that the "+
					"account has permission to read images.")
		case registry.ClassRateLimited:
			return "Anchore is rate limiting these requests. " + detailOr(re,
				"Lower coordinator.security.anchore.concurrency, or sync again when it is quieter.")
		case registry.ClassNotFound:
			return "Anchore is not answering at this address. " + detailOr(re,
				"Check coordinator.security.anchore.endpoint - it must be the API host, and the "+
					"deployment must be Anchore Enterprise 5.x.")
		case registry.ClassTimeout:
			return "Anchore did not answer in time. " + detailOr(re,
				"It is reachable but slow - raise coordinator.security.anchore.requestTimeout, or "+
					"sync again when it is under less load.")
		case registry.ClassUnavailable:
			if re.StatusCode == http.StatusGatewayTimeout || re.StatusCode == http.StatusRequestTimeout {
				return "Anchore timed out on this request. Raise " +
					"coordinator.security.anchore.requestTimeout, or sync again when it is under less load."
			}
			return "Anchore returned an error. " + detailOr(re, "Check the Anchore services.")
		case registry.ClassMalformed:
			// It answered. Saying it could not be reached sends somebody to
			// check a network path that is demonstrably fine, when what is in
			// front of them is a proxy's error page or an SSO login form.
			return "Anchore answered with something this Coordinator could not read. " + detailOr(re,
				"Something between here and Anchore may be returning an error page or a login form "+
					"in place of the response.")
		}
		return "Anchore could not be reached. " + detailOr(re,
			"Check the network path from this Coordinator to Anchore.")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Anchore did not answer in time. Raise coordinator.security.anchore.requestTimeout."
	}
	return "Anchore could not be reached: " + err.Error()
}

func detailOr(e *registry.Error, fallback string) string {
	if e.Detail != "" {
		return e.Detail
	}
	return fallback
}

var _ security.Provider = (*Provider)(nil)
