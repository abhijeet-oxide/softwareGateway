package anchore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

	// Grouping says whether releases are mapped onto Anchore's
	// Application/Version model.
	//
	// The NAMES are not here: a provider is built per repository and a release
	// is not, so which application and version a scan is about arrives on the
	// scan (ScanOptions.Release). This is only the switch, for the deployment
	// that has its own application taxonomy in Anchore and does not want ours
	// beside it.
	Grouping bool

	// Concurrency caps requests in flight against Anchore. Zero uses
	// DefaultConcurrency.
	Concurrency int

	// SBOMFormat is which flavour of SBOM to fetch: spdx-json, cyclonedx-json
	// or native-json. Empty uses SPDX, which is what the person pressing
	// "download SBOM" is overwhelmingly about to send to somebody else.
	SBOMFormat string

	// Submit says whether this deployment may register images with Anchore.
	//
	// True by default and worth being able to turn off: an estate that
	// registers its images with Anchore by its own pipeline wants this platform
	// to READ Anchore, not to add to it, and replicating anyway would duplicate
	// their registration under our annotations.
	Submit bool
}

// Provider implements security.Provider over Anchore Enterprise.
type Provider struct {
	client   *Client
	settings Settings

	// versions caches the Application Version each release maps to.
	//
	// Keyed by release, because a provider is built per (product, repository)
	// and serves every release through it - and because the find-or-create is
	// three requests that must not be repeated per scan of the same release.
	//
	// Bounded by the number of releases a Coordinator syncs before it restarts,
	// which is small, and each entry is four strings.
	mu       sync.Mutex
	versions map[string]Version
	// latest is the last version resolved, so VersionURL has something to
	// return for a caller that has just scanned.
	latest Version
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
	return &Provider{client: client, settings: settings, versions: map[string]Version{}}, nil
}

// Name implements security.Provider.
func (p *Provider) Name() string { return ProviderName }

// Enabled implements security.Provider.
func (p *Provider) Enabled() bool { return true }

// Ping checks Anchore answers, for the deep health check.
func (p *Provider) Ping(ctx context.Context) error { return p.client.Ping(ctx) }

// Endpoint is the API base URL, so a caller can build a link into Anchore.
func (p *Provider) Endpoint() string { return p.client.Endpoint() }

// VersionURL is where a person opens the last-scanned release in Anchore.
// Empty before the first scan, or where the deployment has switched grouping
// off.
func (p *Provider) VersionURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.latest.VersionID == "" {
		return ""
	}
	return p.latest.URL(p.client.Endpoint())
}

// Scan implements security.Provider: READ ONLY.
//
// # Why this no longer submits or waits
//
// It did both, and the argument against is operational. Anchore analyses an
// image on its own schedule and nobody can promise a bound on it, so a sync
// that submitted and then waited had its duration set by somebody else's queue,
// held a claim on the release throughout, and reported the release as unscanned
// every time the wait ran out. Registration is now its own act (Register), and
// this is exactly as fast as reading.
//
// Two phases:
//
//  1. TAKE STOCK. One request asks Anchore what it knows about every image in
//     the release. A release of 157 images costs one request, not 157.
//  2. READ, in parallel, whatever has finished. An image Anchore has never been
//     told about is reported as not scanned with the sentence that names the
//     remedy - and the remedy is the Replicate button, not waiting.
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
		security.ReportStage(opts.Progress, security.StageScanning, 0, 0)
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

	// Read what Anchore has finished with. NOTHING here submits, and nothing
	// waits - see the header on internal/security/registration.go for why those
	// moved out. An image Anchore has never been told about is reported as such,
	// with the button that fixes it named.
	p.readFindings(ctx, refs, pullable, known, reports, now, opts)

	if err := ctx.Err(); err != nil {
		return reports, err
	}
	return reports, nil
}

const notReplicatedMessage = "This image is not in the registry Anchore pulls from. " +
	"Transfer the release there, then sync again."

// readFindings retrieves each analysed image's vulnerabilities, in parallel.
func (p *Provider) readFindings(
	ctx context.Context, refs []security.ArtifactRef, pullable []int,
	known map[string]ImageRecord, reports []security.Report, now time.Time,
	opts security.ScanOptions,
) {
	security.ReportStage(opts.Progress, security.StageScanning, 0, len(pullable))

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
			security.ReportStage(opts.Progress, security.StageScanning, done, len(pullable))
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
		// NOT a failure, and the message has to make that obvious.
		//
		// Since registering and reading became separate acts, an image Anchore
		// has never been told about is the ordinary state of a release nobody
		// has replicated yet - and the remedy is a button on this page, not a
		// scanner to go and investigate. A sentence that only said "no record"
		// sent the reader to Anchore to look for something that was never
		// there.
		r.Status = security.StatusNotScanned
		r.Message = "Anchore has no record of this image. Replicate this release to Anchore " +
			"to submit it for analysis."
		if rec.Detail != "" {
			r.Message = "Anchore rejected this image for analysis: " + rec.Detail
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
		// Registered and in Anchore's queue. Nothing here waits for it - see
		// the header on registration.go - so the honest thing to say is that
		// it is under way and that a later sync collects it.
		r.Status = security.StatusNotScanned
		r.Message = "Anchore is still analysing this image. Sync again later to collect the result."
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

// resolveVersion finds or creates one release's Application Version, once.
func (p *Provider) resolveVersion(ctx context.Context, release security.ReleaseRef) (Version, error) {
	key := release.Product + "|" + release.Version

	p.mu.Lock()
	cached, ok := p.versions[key]
	p.mu.Unlock()
	if ok {
		return cached, nil
	}

	// Deliberately OUTSIDE the lock. The find-or-create is three round trips
	// against somebody else's service, and holding a provider-wide mutex across
	// them would serialize every release syncing through this provider behind
	// the slowest one. Two syncs of the same release racing here is not a
	// problem: the create treats "already exists" as the success it is and both
	// end up with the same version.
	version, err := p.client.FindOrCreateVersion(ctx, release.Product, release.Version)
	if err != nil {
		return Version{}, err
	}

	p.mu.Lock()
	p.versions[key] = version
	p.latest = version
	p.mu.Unlock()
	return version, nil
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
	case "image":
		break
	case "":
		// Older rows have no classified kind. Admit only container image media
		// types; an OCI index is a release manifest, not an image Anchore can
		// analyse or pull by tag.
		mt := strings.ToLower(ref.MediaType)
		return strings.Contains(mt, "image.manifest") && !strings.Contains(mt, "image.index")
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
		// Anchore reporting that IT cannot pull, which is a different system's
		// problem from Anchore being unreachable and has a different remedy.
		// Checked before the class ladder because it arrives as a 400, and a
		// 400 is otherwise indistinguishable from an unsupported call.
		if pull := describePullFailure(re); pull != "" {
			return pull
		}
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
		case registry.ClassUnsupported:
			// A 4xx that is not auth, not missing and not rate limiting.
			// Anchore answered and rejected the request, so reporting it as
			// unreachable sends an operator to a network path that is fine.
			return "Anchore rejected the request. " + detailOr(re,
				"Check that the Anchore deployment is Enterprise 5.x; this Coordinator uses its v2 API.")
		}
		return "Anchore could not be reached. " + detailOr(re,
			"Check the network path from this Coordinator to Anchore.")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Anchore did not answer in time. Raise coordinator.security.anchore.requestTimeout."
	}
	if strings.Contains(strings.ToLower(err.Error()), "certificate signed by unknown authority") {
		return "Anchore certificate verification failed: " + err.Error() +
			" Set coordinator.security.anchore.insecureSkipVerify to true for this deployment, or configure the Anchore CA."
	}
	return "The Anchore request failed: " + err.Error()
}

// describePullFailure returns ANCHORE'S OWN WORDS for a failure to pull, and
// "" for anything else.
//
// # The failure, and why it was reported as the opposite of itself
//
// Anchore answers a submission it cannot honour with `400 cannot fetch image
// digest/manifest from registry`. It is saying that IT could not pull from the
// internal registry - a registry that is not in its own registry list, a
// credential it does not hold, a TLS chain it does not trust, or a host its
// network cannot route to.
//
// A 400 classifies as unsupported, which had no case, so this arrived as
// "Anchore could not be reached" - the exact inverse of what happened. Anchore
// was reached; it replied.
//
// # Why the remedy is not in this string
//
// Because this is EVIDENCE and a remedy is an interpretation, and the two get
// used differently: the scanner's sentence is what goes into a ticket and gets
// verified by whoever receives it, and the remedy is what the reader does next.
// Concatenated, the evidence was buried mid-paragraph and the whole thing was
// unquotable. The interface renders the remedy underneath - see
// pullRemedy in internal/api/securitywire.go, which can also name the registry.
func describePullFailure(re *registry.Error) string {
	if !isPullFailure(re.Detail) {
		return ""
	}
	return "Anchore rejected the image: " + strings.TrimSpace(re.Detail)
}

// isPullFailure recognises Anchore saying it could not pull.
//
// Exported through PullFailureDetail so the API layer can decide whether a
// STORED error warrants the registry remedy, without re-encoding this list.
func isPullFailure(detail string) bool {
	d := strings.ToLower(detail)
	switch {
	case strings.Contains(d, "cannot fetch image digest/manifest"),
		strings.Contains(d, "cannot fetch image"),
		strings.Contains(d, "failed to retrieve manifest"),
		strings.Contains(d, "error fetching"):
		return true
	}
	return false
}

// IsPullFailure reports whether a recorded failure is Anchore saying it could
// not pull the image from the registry.
func IsPullFailure(message string) bool { return isPullFailure(message) }

func detailOr(e *registry.Error, fallback string) string {
	if e.Detail != "" {
		return e.Detail
	}
	return fallback
}

var _ security.Provider = (*Provider)(nil)
