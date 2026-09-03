package anchore

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
)

// Telling Anchore a release exists, and returning as soon as it has been told.
//
// See internal/security/registration.go for why this is a separate act from a
// sync. The short version: analysis takes as long as it takes, and a sync whose
// duration is set by somebody else's queue is a sync nobody can rely on.

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
// # The order, and why nothing in it waits
//
//  1. TAKE STOCK. One request asks Anchore what it already knows.
//  2. SUBMIT what it has never seen. Only that: a resubmission is a no-op at
//     best and a forced re-analysis at worst.
//  3. GROUP, IMMEDIATELY. Find or create the Application and the Version and
//     associate every submitted image - analysed or not.
//
// Step 3 not waiting for analysis is a deliberate departure from the
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

	pullable, unusable := p.registrable(refs)
	for ref, why := range unusable {
		reg.Failed[ref] = why
	}
	reg.Expected = len(pullable)
	if len(pullable) == 0 {
		reg.Message = "None of this release's images are in the registry Anchore pulls from. " +
			"Transfer the release, then replicate it again."
		reg.Settle()
		return reg, nil
	}

	security.ReportInfo(opts.Progress, fmt.Sprintf(
		"Registering %d images with Anchore.", len(pullable)))

	digests := make([]string, 0, len(pullable))
	for _, ref := range pullable {
		digests = append(digests, ref.Digest)
	}

	known, err := p.client.GetImages(ctx, digests)
	if err != nil {
		// Nothing can be decided without this. Unlike a per-image failure it
		// makes the whole request meaningless, so it is an error return.
		return reg, fmt.Errorf("%s", describeFailure(err))
	}

	p.submit(ctx, pullable, known, &reg, opts.Progress)
	p.group(ctx, pullable, known, &reg, opts)

	reg.Analysed = countAnalysed(known)
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
// A second press of the Replicate button should do nothing and SAY it did
// nothing. Images already known are counted rather than resubmitted, and the
// result carries both numbers - "submitted 0, already known 157" is how a
// reader sees that it ran and had nothing to do, where a silent no-op reads as
// a button that is broken.
func (p *Provider) submit(
	ctx context.Context, refs []security.ArtifactRef, known map[string]ImageRecord,
	reg *security.Registration, progress security.Progress,
) {
	var missing []security.ArtifactRef
	for _, ref := range refs {
		if known[ref.Digest].Known {
			reg.AlreadyKnown++
			continue
		}
		missing = append(missing, ref)
	}
	if len(missing) == 0 {
		security.ReportInfo(progress, fmt.Sprintf(
			"Anchore already holds all %d images. Nothing was submitted.", reg.AlreadyKnown))
		return
	}
	if !p.settings.Submit {
		// A deliberate configuration, said out loud. An operator who switched
		// submission off should see the consequence named rather than a release
		// that is quietly a third registered.
		for _, ref := range missing {
			reg.Failed[ref.Ref()] = "This deployment does not submit images to Anchore. " +
				"Register it in Anchore, then replicate again."
		}
		security.ReportWarning(progress, fmt.Sprintf(
			"%d images are not registered with Anchore, and this deployment does not submit "+
				"images. Register them in Anchore, then replicate again.", len(missing)))
		return
	}

	security.ReportStage(progress, StageSubmitting, 0, len(missing))
	var (
		mu   sync.Mutex
		done int
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(p.concurrency())
	for _, ref := range missing {
		g.Go(func() error {
			rec, err := p.client.Submit(gctx, ref)
			mu.Lock()
			defer mu.Unlock()
			done++
			security.ReportStage(progress, StageSubmitting, done, len(missing))
			if err != nil {
				reg.Failed[ref.Ref()] = describeFailure(err)
				return nil
			}
			known[ref.Digest] = rec
			reg.Submitted++
			return nil
		})
	}
	_ = g.Wait()

	switch {
	case reg.Submitted > 0:
		security.ReportInfo(progress, fmt.Sprintf(
			"Submitted %d images to Anchore for analysis. Analysis runs on Anchore's own "+
				"schedule; sync this release to collect results as they finish.", reg.Submitted))
	default:
		// The one failure worth naming its own cause: Anchore refusing to pull
		// is almost always its registry credential rather than anything here.
		security.ReportWarning(progress, fmt.Sprintf(
			"Anchore would not accept any of the %d images. Check that Anchore has a registry "+
				"configured for %s.", len(missing), p.settings.Registry))
	}
}

// group makes the release one thing in Anchore, immediately.
//
// Every image this run knows about is associated, whatever its analysis status.
// See Register for why that departs from the integration guide.
func (p *Provider) group(
	ctx context.Context, refs []security.ArtifactRef, known map[string]ImageRecord,
	reg *security.Registration, opts security.RegisterOptions,
) {
	if !p.settings.Grouping || !opts.Release.Named() {
		// Grouping is off. "Registered" then means submitted, and the counts
		// have to say so or a release would report itself permanently partial.
		reg.Associated = reg.Submitted + reg.AlreadyKnown
		return
	}

	version, err := p.resolveVersion(ctx, opts.Release)
	if err != nil {
		reg.Message = "This release's images were submitted, but Anchore would not group them " +
			"under an application version: " + describeFailure(err)
		security.ReportWarning(opts.Progress, reg.Message)
		// The submissions stand. Grouping is what makes the release legible in
		// Anchore's interface and what unlocks its release-level report; losing
		// it costs a link and a second view, not the analysis.
		reg.Associated = reg.Submitted + reg.AlreadyKnown
		return
	}
	p.describeVersion(reg, version)

	// Everything Anchore has a record of - which after submit is everything
	// that did not fail.
	digests := make([]string, 0, len(refs))
	for _, ref := range refs {
		if known[ref.Digest].Known {
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
