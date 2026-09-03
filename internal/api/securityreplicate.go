package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/abhijeet-oxide/softwareGateway/internal/security"
	"github.com/abhijeet-oxide/softwareGateway/internal/store"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// Replicating a release to a scanner that has to be TOLD about it.
//
// # Why this is a request rather than a job
//
// Because it is short and its duration is ours. A sync is minutes against
// somebody else's analysis queue, so it is a claim, a goroutine and a progress
// endpoint. This is a submission per image at a bounded concurrency plus three
// calls for the application version - seconds against a responsive Anchore, a
// couple of minutes against a slow one. Held open, the user who pressed the
// button finds out whether it worked before the page finishes reloading, which
// is the whole reason not to make it a job.
//
// # Why it is not folded into the sync
//
// It was, and the operational reality broke it: analysis takes as long as it
// takes, so a sync that submitted and then waited had its duration decided by
// somebody else's queue. See internal/security/replicate.go.

// SecurityReplicator registers a release with a scanner.
//
// A consumer-defined interface like SecuritySyncer beside it: four calls, not
// the provider resolver and the credential store behind them.
type SecurityReplicator interface {
	// StartReplicate claims the release and registers it in the background,
	// returning as soon as the claim is decided.
	StartReplicate(ctx context.Context, req security.ReplicateRequest) error
	// ReplicationProgress is the live position of a run this replica is doing.
	// A miss is ordinary: the work may be elsewhere, or may have finished, and
	// the caller falls back to the position stored on the row.
	ReplicationProgress(packageID int64, provider string) (security.ProgressSnapshot, bool)
	// CancelReplication stops a run, wherever it is running.
	CancelReplication(ctx context.Context, packageID int64, provider string) (bool, error)
	// Registrable reports whether a scanner has to be told about artifacts at
	// all, so the interface knows whether to offer the button.
	Registrable(ctx context.Context, scope security.Scope) bool
}

// SecurityRegistrationStore serves the stored registration state.
//
// Separate from SecurityReplicator because the two fail differently: running a
// replication needs a reachable scanner and this needs only the database, so a
// release's registration state stays readable while Anchore is down - which is
// exactly when somebody is looking at it.
type SecurityRegistrationStore interface {
	ForPackage(ctx context.Context, packageID int64) ([]store.RegistrationRow, error)
	ForPackages(ctx context.Context, ids []int64) (map[int64][]store.RegistrationRow, error)
}

// handleReplicatePackageSecurity serves POST
// /api/v1/products/{product}/packages/{package}:replicateSecurity.
func (s *Server) handleReplicatePackageSecurity(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.SecurityReplicate == nil {
		Error(w, r, v1.CodeUnavailable,
			"replicating releases to a scanner is not configured on this Coordinator")
		return
	}

	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	target := s.securityTargetFor(r.Context(), productName, pkg)
	if !target.Available {
		Error(w, r, v1.CodeFailedPrecondition, target.Reason)
		return
	}

	var body v1.ReplicateSecurityRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	// Only the scanners that have to be told. A request naming Xray - which
	// indexes a repository - asks for something that does not exist, and the
	// honest answer names the scanners that do rather than pretending to run.
	scopes := s.registrableScopes(r.Context(), target, body.Provider)
	if len(scopes) == 0 {
		Error(w, r, v1.CodeFailedPrecondition,
			"no scanner configured for this release needs its images registered. "+
				"JFrog Xray indexes the repository, so there is nothing to replicate to it.")
		return
	}

	artifacts := s.securityArtifactsFor(productName, pkg, r.Context())
	if len(artifacts) == 0 {
		Error(w, r, v1.CodeFailedPrecondition,
			"this release has not been analysed yet, so there is nothing to replicate: analyse it first")
		return
	}

	out := v1.ReplicateSecurityResponse{
		Product: productName,
		Package: packageReferenceOf(pkg),
	}

	for _, scope := range scopes {
		err := s.deps.SecurityReplicate.StartReplicate(r.Context(), security.ReplicateRequest{
			PackageID: pkg.ID,
			Scope:     scope,
			Release:   releaseRefFor(productName, pkg),
			Artifacts: artifacts,
		})
		switch {
		case errors.Is(err, store.ErrRegistrationInFlight):
			// Not a failure. The thing the caller wanted is already happening,
			// and the panel it is about to poll will show it - so this answers
			// with the running state rather than a 409 that reads as a refusal.
			out.Started = false
		case errors.Is(err, security.ErrNotRegistrable):
			continue
		case err != nil:
			Error(w, r, v1.CodeUnavailable, err.Error())
			return
		default:
			out.Started = true
		}
	}

	// The state as it now stands, including the run's first position, so the
	// page has something to draw before its first poll comes back.
	out.Registrations = s.registrationsFor(r.Context(), target, pkg.ID)
	WriteJSON(w, r, http.StatusAccepted, out)
}

// handleCancelPackageReplication serves POST
// /api/v1/products/{product}/packages/{package}:cancelSecurityReplication.
//
// The counterpart of stopping a sync or a compliance check, and it exists for
// the same reason: a run against an unreachable Anchore holds a claim for its
// whole timeout, and a reader who can see it is stuck should be able to end it
// rather than wait out a window sized for the worst case.
func (s *Server) handleCancelPackageReplication(w http.ResponseWriter, r *http.Request) {
	productName := chi.URLParam(r, "product")
	ref := chi.URLParam(r, "package")

	if !s.productExists(w, r, productName) {
		return
	}
	if s.deps.Packages == nil || s.deps.SecurityReplicate == nil {
		Error(w, r, v1.CodeUnavailable,
			"replicating releases to a scanner is not configured on this Coordinator")
		return
	}
	pkg, ok := s.resolvePackage(w, r, productName, ref)
	if !ok {
		return
	}

	var body v1.ReplicateSecurityRequest
	if err := decodeOptionalJSON(r, &body); err != nil {
		Error(w, r, v1.CodeInvalidArgument, err.Error())
		return
	}

	target := s.securityTargetFor(r.Context(), productName, pkg)
	out := v1.ReplicateSecurityResponse{
		Product: productName,
		Package: packageReferenceOf(pkg),
	}
	for _, scope := range s.registrableScopes(r.Context(), target, body.Provider) {
		stopped, err := s.deps.SecurityReplicate.CancelReplication(
			r.Context(), pkg.ID, scope.Provider)
		if err != nil {
			s.internal(w, r, "stop security replication", err)
			return
		}
		// False is not a failure: the run finished between the reader deciding
		// to stop it and the request arriving.
		out.Stopped = out.Stopped || stopped
	}
	out.Registrations = s.registrationsFor(r.Context(), target, pkg.ID)
	WriteJSON(w, r, http.StatusOK, out)
}

// registrableScopes is the scanners for this release that have to be told about
// its images, narrowed to one where the caller asked for one.
//
// A name this release has no such scanner for is IGNORED rather than refused,
// the same rule the sync applies to the same parameter: a stale browser tab
// must not turn a button into an error.
func (s *Server) registrableScopes(
	ctx context.Context, target securityTarget, wanted string,
) []security.Scope {
	var out []security.Scope
	for _, scope := range target.scopes() {
		if wanted != "" && scope.Provider != wanted {
			continue
		}
		if s.deps.SecurityReplicate.Registrable(ctx, scope) {
			out = append(out, scope)
		}
	}
	if len(out) == 0 && wanted != "" {
		// The named scanner is not registrable. Fall back to every one that is,
		// rather than refusing outright.
		return s.registrableScopes(ctx, target, "")
	}
	return out
}

// registrationsFor is what each scanner holds for a release, for the page.
//
// Reads the STORE, never a scanner: this is drawn on every open of a Security
// tab, and asking Anchore three questions to decide whether to draw a notice
// would put a third system's availability on the critical path of a page that
// otherwise reads one database.
func (s *Server) registrationsFor(
	ctx context.Context, target securityTarget, packageID int64,
) []v1.SecurityRegistration {
	if s.deps.SecurityRegistrations == nil {
		return nil
	}

	// Which scanners NEED registering. Without this a release would carry an
	// entry for Xray, and the interface would offer a button that cannot do
	// anything for a scanner that does not need it.
	registrable := map[string]bool{}
	if s.deps.SecurityReplicate != nil {
		for _, scope := range target.scopes() {
			if s.deps.SecurityReplicate.Registrable(ctx, scope) {
				registrable[scope.Provider] = true
			}
		}
	}
	if len(registrable) == 0 {
		return nil
	}

	stored := map[string]store.RegistrationRow{}
	if rows, err := s.deps.SecurityRegistrations.ForPackage(ctx, packageID); err == nil {
		for _, row := range rows {
			stored[row.Provider] = row
		}
	} else {
		// A notice that will not load costs the button, never the page.
		s.deps.Logger.Warn("security: could not read the replication state",
			"package", packageID, "error", err)
	}

	out := make([]v1.SecurityRegistration, 0, len(registrable))
	for _, scope := range target.scopes() {
		if !registrable[scope.Provider] {
			continue
		}
		reg := toAPIRegistration(scope.Provider, stored[scope.Provider], target)
		// The LIVE position wins over the stored one where this Coordinator is
		// the replica doing the work: the row is rewritten on a heartbeat, so
		// what it holds is up to one beat old and what is in memory is now.
		if reg.State == string(security.RegistrationRunning) {
			if progress, ok := s.deps.SecurityReplicate.ReplicationProgress(
				packageID, scope.Provider); ok {
				reg.Progress = toAPIReplicationProgress(
					scope.Provider, progress, target.Registry, true)
			}
		}
		out = append(out, reg)
	}
	return out
}
