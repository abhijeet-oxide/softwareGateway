package anchore

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// Replicating a release to Anchore: the images are registered, the application
// and version are created, and the call returns without waiting for analysis.
//
// See internal/security/registration.go for why this is a separate act from a
// sync. The short version: analysis takes as long as it takes, and a sync whose
// duration is set by somebody else's queue is a sync nobody can rely on.

// StageDiscovering counts the release's artifacts that Anchore can be told
// about at all - the images, as opposed to the charts and files it has nothing
// to scan in.
const StageDiscovering = "discovering"

// StageSubmitting counts images being registered with Anchore.
//
// Its own stage rather than folding into "fetching", because it is a different
// wait with a different remedy: fetching is slow because a scanner is busy, and
// this is slow because a release has never been submitted - which the reader
// wants to see happening rather than infer from a bar that has not moved.
const StageSubmitting = "submitting"

// StageAssociating counts images being attached to the application version.
const StageAssociating = "associating"

// Register implements security.Registrar.
//
// # The four steps, and why the second no longer asks first
//
//  1. DISCOVER which of the release's artifacts Anchore can pull.
//  2. REPLICATE every one of them. Anchore is TOLD about all of them, and it
//     decides what that means: an image it does not have it pulls, and an image
//     it has it leaves alone.
//  3. FIND OR CREATE the application.
//  4. FIND OR CREATE the version, and attach the images to it.
//
// Step 2 used to list Anchore's whole image collection first and submit only
// what was missing. That was wrong twice over. It made a release's replication
// depend on a listing of every image in the account - an unbounded response
// that gets slower as the estate grows, and whose failure aborted the run
// before a single image had been offered, which is exactly the "Anchore could
// not be reached" that made a working Anchore look broken. And it duplicated a
// decision Anchore already makes: submission by digest is idempotent there, so
// re-offering an image it holds costs one request and changes nothing.
//
// Step 4 not waiting for analysis is a deliberate departure from the
// integration guide, which says to associate only analysed images so a version
// never reports less than it appears to. That is true and the trade goes the
// other way: association is what makes the release EXIST in Anchore's own
// interface, and deferring it until analysis finishes means the thing a person
// goes to Anchore to look at appears only once they no longer need it. Attached
// up front, the version fills in as its images finish, and this platform's own
// coverage numbers say how much of it is answerable yet.
func (p *Provider) Register(
	ctx context.Context, refs []security.ArtifactRef, opts security.RegisterOptions,
) (security.Registration, error) {
	reg := security.Registration{
		Provider: ProviderName,
		Failed:   map[string]string{},
		At:       time.Now().UTC(),
	}

	// 1. DISCOVER.
	pullable, unusable := p.registrable(refs)
	for ref, why := range unusable {
		reg.Failed[ref] = why
	}
	reg.Expected = len(pullable)
	security.ReportStage(opts.Progress, StageDiscovering, len(pullable), len(pullable))
	if len(pullable) == 0 {
		reg.Message = "None of this release's images are in the registry Anchore pulls from. " +
			"Transfer the release, then replicate it again."
		security.ReportWarning(opts.Progress, reg.Message)
		reg.Settle()
		return reg, nil
	}
	security.ReportInfo(opts.Progress, fmt.Sprintf(
		"Selected %d images in this release for replication to Anchore.", len(pullable)))

	// 2. REPLICATE.
	records := p.submit(ctx, pullable, &reg, opts.Progress)

	// 3 and 4. APPLICATION, VERSION, and the images attached to it - unless
	// Anchore accepted nothing, in which case there is nothing to attach and
	// the four requests to find or create a version would group an empty set.
	if len(records) == 0 {
		security.ReportWarning(opts.Progress,
			"No images were replicated, so no application version was created for this release.")
		reg.Settle()
		return reg, ctx.Err()
	}
	p.group(ctx, pullable, records, &reg, opts)

	reg.Analysed = countAnalysed(records)
	reg.Settle()
	return reg, ctx.Err()
}

// RegistrationFor implements security.Registrar: what Anchore holds right now,
// without changing anything.
//
// # Why this asks Anchore rather than reading our own record
//
// Because our record cannot know that somebody deleted the application in
// Anchore, or removed an image from it. The stored row is what a page renders
// without a round trip; this is what a reader presses to check it, and the two
// disagreeing is exactly the situation worth being able to see.
func (p *Provider) RegistrationFor(
	ctx context.Context, refs []security.ArtifactRef, opts security.RegisterOptions,
) (security.Registration, error) {
	reg := security.Registration{
		Provider: ProviderName,
		Failed:   map[string]string{},
		At:       time.Now().UTC(),
	}

	pullable, _ := p.registrable(refs)
	reg.Expected = len(pullable)
	if len(pullable) == 0 {
		reg.Settle()
		return reg, nil
	}

	digests := make([]string, 0, len(pullable))
	for _, ref := range pullable {
		digests = append(digests, ref.Digest)
	}
	known, err := p.client.GetImages(ctx, digests)
	if err != nil {
		return reg, fmt.Errorf("%s", describeFailure(err))
	}
	for _, d := range digests {
		if known[d].Known {
			reg.AlreadyKnown++
		}
	}
	reg.Analysed = countAnalysed(known)

	if p.settings.Grouping && opts.Release.Named() {
		version, err := p.resolveVersion(ctx, opts.Release)
		if err != nil {
			reg.Message = describeFailure(err)
			reg.Settle()
			return reg, nil
		}
		p.describeVersion(&reg, version)
		if held, err := p.client.listArtifacts(ctx, p.artifactsPath(version)); err == nil {
			for _, d := range digests {
				if _, ok := held[d]; ok {
					reg.Associated++
				}
			}
		}
	} else {
		// Grouping is off, so "registered" means submitted and nothing else.
		reg.Associated = reg.AlreadyKnown
	}
	reg.Settle()
	return reg, nil
}

// submissionLabel is what an in-flight image is called on the progress panel.
//
// The artifact's own name where it has one, and a short digest otherwise. The
// full pull string is a registry host, a repository path and seventy-one
// characters of hex, which at eleven pixels in a row of six chips is not read
// at a glance - and being read at a glance is the entire job of these.
func submissionLabel(ref security.ArtifactRef) string {
	name := strings.TrimSpace(ref.Name)
	tag := strings.TrimSpace(ref.Tag)
	if name != "" && tag != "" && !strings.HasSuffix(name, ":"+tag) {
		return name + ":" + tag
	}
	if name != "" {
		return name
	}
	if tag != "" {
		return tag
	}
	return shortDigest(ref.Digest)
}

// registrable splits a release's artifacts into the ones Anchore can be told
// about and the ones it cannot, with the reason.
//
// A chart, a signature or a file is not "failed" - Anchore has nothing to scan
// in one, so it is not expected either and never reaches the counts. An image
// with no internal registry path IS a failure, and the one whose remedy is a
// transfer rather than anything to do with Anchore.
func (p *Provider) registrable(
	refs []security.ArtifactRef,
) (pullable []security.ArtifactRef, unusable map[string]string) {
	unusable = map[string]string{}
	for _, ref := range refs {
		if !scannable(ref) {
			continue
		}
		located := withLocation(ref, p.settings)
		if _, err := PullString(located); err != nil {
			unusable[ref.Ref()] = notReplicatedMessage
			continue
		}
		pullable = append(pullable, located)
	}
	sort.Slice(pullable, func(i, j int) bool { return pullable[i].Digest < pullable[j].Digest })
	return pullable, unusable
}

// submit tells Anchore about the images it has never seen.
//
// # Idempotent, and visibly so
//
// Every image is offered, every time. Submission by digest is idempotent in
// Anchore - it returns the record it already holds rather than re-analysing -
// so asking first only moved the same decision to a slower place. A second
// press therefore costs one request per image and changes nothing, and the
// counts say so: "154 replicated, 154 already held" is how a reader sees that
// it ran and had nothing to do, where a silent no-op reads as a broken button.
//
// Returns what Anchore said about each image, which is where the analysis
// counts and the association list come from.
func (p *Provider) submit(
	ctx context.Context, refs []security.ArtifactRef,
	reg *security.Registration, progress security.Progress,
) map[string]ImageRecord {
	records := make(map[string]ImageRecord, len(refs))
	analysed := make([]string, 0)
	statuses := map[string]int{}

	if !p.settings.Submit {
		// A deliberate configuration, said out loud. An operator who switched
		// submission off should see the consequence named rather than a release
		// that is quietly a third registered.
		for _, ref := range refs {
			reg.Failed[ref.Ref()] = "This deployment does not submit images to Anchore. " +
				"Register it in Anchore, then replicate again."
		}
		security.ReportWarning(progress, fmt.Sprintf(
			"%d images are not registered with Anchore, and this deployment does not submit "+
				"images. Register them in Anchore, then replicate again.", len(refs)))
		return records
	}

	security.ReportStage(progress, StageSubmitting, 0, len(refs))
	security.ReportConcurrency(progress, p.concurrency())
	var (
		mu     sync.Mutex
		done   int
		failed int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency())
	for _, ref := range refs {
		g.Go(func() error {
			// The image's own name, up while it is in flight. A bar and a count
			// say how far; this is what says the run is alive at all when the
			// bar has not moved for thirty seconds, and it is the only thing
			// that tells a slow Anchore from a wedged one.
			label := submissionLabel(ref)
			security.ReportBegin(progress, label)
			defer security.ReportEnd(progress, label)

			rec, err := p.client.Submit(gctx, ref)
			mu.Lock()
			defer mu.Unlock()
			done++
			security.ReportStage(progress, StageSubmitting, done, len(refs))
			if err != nil {
				reason := describeFailure(err)
				reg.Failed[ref.Ref()] = reason
				if tag := TagString(ref); tag != "" {
					reg.Outcomes.Failed = append(reg.Outcomes.Failed, tag)
				}
				failed++
				security.ReportStage(progress, security.StageFailing, failed, len(refs))
				// ONE line for the whole class of refusal, updated in place.
				// A hundred and fifty images refused for one reason is one
				// fault, and printing its remedy paragraph once per image
				// buries every other line in the transcript.
				security.ReportWarningUpdate(progress, fmt.Sprintf(
					"%d images rejected by Anchore. %s", failed, reason))
				return nil
			}
			records[ref.Digest] = rec
			reg.Submitted++
			if tag := TagString(ref); tag != "" {
				reg.Outcomes.Replicated = append(reg.Outcomes.Replicated, tag)
			}
			statuses[rec.Status]++
			security.ReportStatuses(progress, statuses)
			// Anchore answers a submission with the record it now holds. A
			// brand-new one comes back not_analyzed; anything further along was
			// already there before this run, which is what makes a second press
			// visibly a no-op rather than apparently a hundred and fifty.
			if rec.Status != "" && rec.Status != AnalysisNotAnalyzed {
				reg.AlreadyKnown++
			}
			if rec.Analyzed() {
				analysed = append(analysed, submissionLabel(ref))
				if tag := TagString(ref); tag != "" {
					reg.Outcomes.Analysed = append(reg.Outcomes.Analysed, tag)
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	sort.Strings(reg.Outcomes.Replicated)
	sort.Strings(reg.Outcomes.Analysed)
	sort.Strings(reg.Outcomes.Failed)
	if len(analysed) > 0 {
		sort.Strings(analysed)
		const limit = 15
		shown := analysed
		if len(shown) > limit {
			shown = shown[:limit]
		}
		line := fmt.Sprintf("Anchore has already analysed %d images: %s.",
			len(analysed), strings.Join(shown, ", "))
		if len(analysed) > len(shown) {
			line += fmt.Sprintf(" %d additional images are already analysed.", len(analysed)-len(shown))
		}
		security.ReportInfo(progress, line)
	}

	switch {
	case reg.Submitted > 0 && reg.AlreadyKnown >= reg.Submitted:
		security.ReportInfo(progress, fmt.Sprintf(
			"All %d images were already registered with Anchore. No new images were pulled.",
			reg.Submitted))
	case reg.Submitted > 0:
		security.ReportInfo(progress, fmt.Sprintf(
			"Replicated %d images to Anchore; %d were already registered. Anchore pulls and "+
				"analyses on its own schedule. Sync this release to collect results.",
			reg.Submitted, reg.AlreadyKnown))
	default:
		// The one failure worth naming its own cause: Anchore rejecting every
		// image is almost always its registry configuration rather than
		// anything on this side.
		security.ReportWarning(progress, fmt.Sprintf(
			"Anchore rejected all %d images. Add %s to Anchore's registry list with credentials "+
				"that can read it, then replicate again.", len(refs), p.settings.Registry))
	}
	return records
}

// group makes the release one thing in Anchore: an application, a version under
// it, and this release's images attached to that version.
//
// Every image Anchore accepted is attached, whatever its analysis status. See
// Register for why that departs from the integration guide.
func (p *Provider) group(
	ctx context.Context, refs []security.ArtifactRef, records map[string]ImageRecord,
	reg *security.Registration, opts security.RegisterOptions,
) {
	if !p.settings.Grouping || !opts.Release.Named() {
		// Grouping is off. "Registered" then means replicated and nothing else,
		// and the count has to say so or a release would report itself
		// permanently partial. Not Submitted + AlreadyKnown: the second is a
		// subset of the first now that every image is offered, and adding them
		// reported twice as many images as the release has.
		reg.Associated = reg.Submitted
		return
	}

	// The panel's third stage opens here, before the round trips rather than
	// after them: finding or creating an application and a version is up to
	// four requests against Anchore, and a route still highlighting "Replicate
	// images" through all of them says the run is somewhere it is not.
	security.ReportStage(opts.Progress, StageAssociating, 0, len(refs))
	security.ReportInfo(opts.Progress, fmt.Sprintf(
		"Finding or creating Anchore application %q, version %q.",
		opts.Release.Product, opts.Release.Version))

	version, err := p.resolveVersion(ctx, opts.Release)
	if err != nil {
		reg.Message = "The images were replicated, but Anchore did not create the application " +
			"version for this release: " + describeFailure(err)
		security.ReportWarning(opts.Progress, reg.Message)
		// The submissions stand. Grouping is what makes the release legible in
		// Anchore's interface and what unlocks its release-level report; losing
		// it costs a link and a second view, not the analysis.
		reg.Associated = reg.Submitted
		return
	}
	p.describeVersion(reg, version)
	security.ReportInfo(opts.Progress, fmt.Sprintf(
		"Anchore application %q version %q is ready.",
		version.ApplicationName, version.VersionName))

	// Everything Anchore has a record of - which after replicating is
	// everything that did not fail.
	digests := make([]string, 0, len(refs))
	for _, ref := range refs {
		if records[ref.Digest].Known {
			digests = append(digests, ref.Digest)
		}
	}
	if len(digests) == 0 {
		return
	}
	sort.Strings(digests)

	security.ReportStage(opts.Progress, StageAssociating, 0, len(digests))
	rec, err := p.client.AssociateImages(ctx, version, digests)
	if err != nil {
		reg.Message = "Anchore's application version could not be read back, so it is not known " +
			"whether this release's images are grouped under it: " + describeFailure(err)
		security.ReportWarning(opts.Progress, reg.Message)
		return
	}
	security.ReportStage(opts.Progress, StageAssociating, rec.Matched, len(digests))

	reg.Associated = rec.Matched
	for digest, why := range rec.Failed {
		reg.Failed[digest] = why
	}

	switch {
	case rec.Complete():
		security.ReportInfo(opts.Progress, fmt.Sprintf(
			"All %d images are grouped under Anchore application %q version %q.",
			rec.Matched, version.ApplicationName, version.VersionName))
	default:
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

// describeVersion puts the scanner's own names and link onto a registration.
func (p *Provider) describeVersion(reg *security.Registration, version Version) {
	reg.Application = version.ApplicationName
	reg.ApplicationID = version.ApplicationID
	reg.Version = version.VersionName
	reg.VersionID = version.VersionID
	reg.URL = version.URL(p.client.Endpoint())
}

// artifactsPath is where a version's associations live.
func (p *Provider) artifactsPath(version Version) string {
	return fmt.Sprintf("/applications/%s/versions/%s/artifacts",
		pathEscape(version.ApplicationID), pathEscape(version.VersionID))
}

// countAnalysed is how many of the images Anchore holds it has finished with.
func countAnalysed(known map[string]ImageRecord) int {
	n := 0
	for _, rec := range known {
		if rec.Analyzed() {
			n++
		}
	}
	return n
}

// ensure the compiler holds this provider to the interface it claims.
var _ security.Registrar = (*Provider)(nil)
