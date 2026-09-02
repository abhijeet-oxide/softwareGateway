package anchore

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// The Application / Version / artifact model, which is how a RELEASE becomes
// one thing in Anchore rather than a hundred and fifty unrelated images.
//
// # Why bother, when the vulnerabilities can be read per image
//
// Because Anchore is somebody else's interface too. A security team looking at
// Anchore directly should see "cfx-5000 25.7.2131" with its images under it,
// not a flat list of digests nobody can attribute. And because the
// application-version vulnerability endpoint aggregates differently from the
// per-image one - one entry per advisory with its matches, rather than one per
// (advisory, package) - which is a genuinely useful second view and the reason
// the integration guide insists both are collected.
//
// # Idempotency, which is the whole difficulty
//
// Every step here runs again on every sync, concurrently with a sync of a
// neighbouring release, possibly on a second Coordinator. So each one is a
// find-then-create that treats "already exists" as the success it is, and each
// one re-reads rather than trusting a write. The integration guide's rule -
// never assume a successful write proves the final state - is not pedantry: an
// association that silently did not happen produces an application version that
// reports a subset of a release as if it were the whole thing.

// Version is one release, as Anchore holds it.
type Version struct {
	ApplicationID   string
	ApplicationName string
	VersionID       string
	VersionName     string
}

// URL is where a person opens this version in Anchore's own interface.
func (v Version) URL(endpoint string) string {
	if v.ApplicationID == "" || v.VersionID == "" {
		return ""
	}
	// The UI lives at the platform root; the API base carries the version
	// prefix this has to drop.
	base := strings.TrimSuffix(endpoint, apiPrefix)
	return fmt.Sprintf("%s/applications/%s/versions/%s", base, v.ApplicationID, v.VersionID)
}

// FindOrCreateVersion resolves a release to an Anchore Application Version,
// creating whichever of the two does not exist yet.
//
// The names are the platform's own: the product's name is the Application and
// the release's version is the Version, exactly as the integration guide maps
// them. Nothing here invents an identifier, because the value of the mapping is
// that a person can find their release in Anchore by typing what they call it.
func (c *Client) FindOrCreateVersion(ctx context.Context, appName, versionName string) (Version, error) {
	appName = strings.TrimSpace(appName)
	versionName = strings.TrimSpace(versionName)
	if appName == "" || versionName == "" {
		return Version{}, fmt.Errorf(
			"anchore: an application name and a version name are both required to group a release")
	}

	app, err := c.findOrCreateApplication(ctx, appName)
	if err != nil {
		return Version{}, err
	}
	version, err := c.findOrCreateVersion(ctx, app.ApplicationID, versionName)
	if err != nil {
		return Version{ApplicationID: app.ApplicationID, ApplicationName: app.Name}, err
	}
	return Version{
		ApplicationID:   app.ApplicationID,
		ApplicationName: app.Name,
		VersionID:       version.ApplicationVersionID,
		VersionName:     version.VersionName,
	}, nil
}

// findOrCreateApplication resolves one application by name.
//
// # The ambiguity that is refused rather than guessed at
//
// Anchore documents application names as unique per account, and in a healthy
// estate the list contains at most one match. If it contains two - two accounts
// merged, a name that differs only in case on a build that does not fold it -
// this stops. Picking one silently would associate half a product's releases
// with one application and half with the other, and the symptom would be a
// release whose vulnerabilities are missing rather than an error anybody can
// act on.
func (c *Client) findOrCreateApplication(ctx context.Context, name string) (application, error) {
	found, err := c.findApplication(ctx, name)
	switch {
	case err != nil:
		return application{}, err
	case found != nil:
		return *found, nil
	}

	created := application{}
	err = c.do(ctx, http.MethodPost, "/applications", application{Name: name}, &created)
	switch {
	case err == nil && created.ApplicationID != "":
		return created, nil
	case err == nil, Conflict(err):
		// Created by somebody else between the list and the create - another
		// release of this product syncing at the same time, which is the
		// ordinary case on a product with several releases in flight. Re-read;
		// the winner's application is the one everybody wants.
		again, readErr := c.findApplication(ctx, name)
		if readErr != nil {
			return application{}, readErr
		}
		if again == nil {
			return application{}, fmt.Errorf(
				"anchore: application %q was reported as existing but cannot be found", name)
		}
		return *again, nil
	default:
		return application{}, err
	}
}

func (c *Client) findApplication(ctx context.Context, name string) (*application, error) {
	var apps []application
	if err := c.do(ctx, http.MethodGet, "/applications", nil, &apps); err != nil {
		return nil, err
	}
	var matches []application
	for _, a := range apps {
		if strings.EqualFold(strings.TrimSpace(a.Name), name) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return &matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, m.ApplicationID)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf(
			"anchore: %d applications are named %q (%s); resolve the duplicate in Anchore before syncing",
			len(matches), name, strings.Join(ids, ", "))
	}
}

func (c *Client) findOrCreateVersion(
	ctx context.Context, appID, name string,
) (applicationVersion, error) {
	path := "/applications/" + pathEscape(appID) + "/versions"

	found, err := c.findVersion(ctx, path, name)
	switch {
	case err != nil:
		return applicationVersion{}, err
	case found != nil:
		return *found, nil
	}

	created := applicationVersion{}
	err = c.do(ctx, http.MethodPost, path, applicationVersion{VersionName: name}, &created)
	switch {
	case err == nil && created.ApplicationVersionID != "":
		return created, nil
	case err == nil, Conflict(err):
		again, readErr := c.findVersion(ctx, path, name)
		if readErr != nil {
			return applicationVersion{}, readErr
		}
		if again == nil {
			return applicationVersion{}, fmt.Errorf(
				"anchore: version %q was reported as existing but cannot be found", name)
		}
		return *again, nil
	default:
		return applicationVersion{}, err
	}
}

func (c *Client) findVersion(ctx context.Context, path, name string) (*applicationVersion, error) {
	var versions []applicationVersion
	if err := c.do(ctx, http.MethodGet, path, nil, &versions); err != nil {
		return nil, err
	}
	for i := range versions {
		// EXACT, not case-folded. Application names are typed by people and
		// folding them is a kindness; version names are release identifiers
		// where `25.7.2131-RC1` and `25.7.2131-rc1` may genuinely be two
		// builds, and merging those would report one release's findings
		// against the other.
		if strings.TrimSpace(versions[i].VersionName) == name {
			return &versions[i], nil
		}
	}
	return nil, nil
}

// Reconciliation is what an association attempt actually produced.
//
// # Why this is a value and not a boolean
//
// Because a partial association is the interesting outcome and the one the
// integration guide spends a section on. A release whose images are three
// quarters associated has an application-level vulnerability report that is
// three quarters of the truth, and a sync that returned "ok" would let that be
// read as the whole release. Missing is the number that must reach the log.
type Reconciliation struct {
	// Expected is the images this release wanted associated.
	Expected int
	// Associated is how many Anchore reports on read-back.
	Associated int
	// Matched is the intersection: expected images that are actually there.
	Matched int
	// Missing is expected images Anchore does not have associated, sorted.
	Missing []string
	// Unexpected is associated images this release did not ask for, sorted.
	//
	// Not an error. A version associated by hand, or by an earlier release that
	// shared a name, legitimately holds images this sync does not know about -
	// and removing them would be this platform deleting somebody else's work.
	// Reported so it can be looked at.
	Unexpected []string
	// Failed maps a digest to why its association did not happen.
	Failed map[string]string
}

// Complete reports whether every expected image is associated.
func (r Reconciliation) Complete() bool { return len(r.Missing) == 0 && len(r.Failed) == 0 }

// AssociateImages associates analysed images with a version and reads the
// result back.
//
// # Only analysed images, and why that is not a limitation
//
// An image Anchore has not finished analysing has no vulnerabilities, so
// associating it adds a row to the application version that contributes
// nothing and reports the version as covering an image it cannot speak for.
// The integration guide says the same thing and it is right: associate what
// has been analysed, report the rest as pending, and let the NEXT sync - which
// is one button press away - pick them up. The caller decides what to do about
// a partial version; this reports it.
func (c *Client) AssociateImages(
	ctx context.Context, version Version, digests []string,
) (Reconciliation, error) {
	rec := Reconciliation{Expected: len(digests), Failed: map[string]string{}}
	if version.ApplicationID == "" || version.VersionID == "" {
		return rec, fmt.Errorf("anchore: no application version to associate images with")
	}
	path := fmt.Sprintf("/applications/%s/versions/%s/artifacts",
		pathEscape(version.ApplicationID), pathEscape(version.VersionID))

	// What is already there. Associating an image twice is at best a wasted
	// request and at worst a 409 that reads like a failure, and a re-synced
	// release is overwhelmingly images that are already associated.
	existing, err := c.listArtifacts(ctx, path)
	if err != nil {
		// Not fatal: the association below is idempotent on Anchore's side, so
		// a failed read costs requests rather than correctness. The read-back
		// afterwards is the one that must work.
		existing = map[string]string{}
	}

	for _, digest := range digests {
		if _, already := existing[digest]; already {
			continue
		}
		req := associationRequest{
			ArtifactType: "image",
			ArtifactKeys: map[string]string{"image_digest": digest},
		}
		if err := c.do(ctx, http.MethodPost, path, req, nil); err != nil && !Conflict(err) {
			rec.Failed[digest] = associationFailure(err)
		}
	}

	// THE READ-BACK. A successful write is not evidence of the final state -
	// see the type comment - and this is the only thing in the flow that can
	// tell a complete version from a plausible-looking partial one.
	actual, err := c.listArtifacts(ctx, path)
	if err != nil {
		return rec, err
	}
	rec.Associated = len(actual)

	wanted := make(map[string]bool, len(digests))
	for _, d := range digests {
		wanted[d] = true
		if _, ok := actual[d]; ok {
			rec.Matched++
		} else {
			rec.Missing = append(rec.Missing, d)
		}
	}
	for d := range actual {
		if !wanted[d] {
			rec.Unexpected = append(rec.Unexpected, d)
		}
	}
	sort.Strings(rec.Missing)
	sort.Strings(rec.Unexpected)
	return rec, nil
}

func (c *Client) listArtifacts(ctx context.Context, path string) (map[string]string, error) {
	var list artifactList
	if err := c.do(ctx, http.MethodGet, path+query("artifact_types", "image"), nil, &list); err != nil {
		return nil, err
	}
	return list.digests(), nil
}

// associationFailure is one association failure in a sentence.
func associationFailure(err error) string {
	if err == nil {
		return ""
	}
	return firstLine(err.Error())
}
