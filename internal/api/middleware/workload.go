package middleware

import (
	"net/http"
	"strings"
)

// The data plane's own identity, and the fence around it.
//
// A worker authenticates like everything else here: it presents a bearer token
// the Coordinator verifies against the identity provider's keys. What is
// different is what that token is allowed to reach.
//
// The worker plane hands out work and accepts its results. A credential that
// can call it can take every job in the queue and report each one finished,
// which is a denial of service on the thing this system exists to do. That is
// the WORST a stolen worker credential should be able to manage, and it is
// only bounded if the same credential cannot also read the audit trail,
// request transfers or enumerate products it was never handed work for.
//
// Nothing else is enforced here yet: the human routes remain open to any
// authenticated caller, as they were before this file existed. That is a gap
// and it is named in docs/design/24. This closes the half of it that is new,
// which is the credential this release introduces, rather than pretending to
// close the half that is older than it.

// WorkerPlane reports the routes a worker calls, and no others.
//
// Spelled as paths rather than as route patterns because it runs BEFORE
// routing: chi has not matched a pattern yet, so there is nothing to ask.
// Kept beside PublicPaths, in the same shape, for the same reason - a list of
// exempt or privileged routes must be one greppable function, not a condition
// spread over the router.
//
// The method matters. `GET /api/v1/workers` is the fleet listing, which is a
// person looking at a screen; `POST /api/v1/workers/{id}` is a worker saying
// it is alive. Same path, opposite sides of this fence.
func WorkerPlane(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	switch {
	case p == "/api/v1/jobs:lease":
		return true
	case strings.HasPrefix(p, "/api/v1/jobs/"):
		// The custom methods on one job: complete, fail, progress. The verb is
		// split by the handler rather than by the router, so it is part of the
		// path here.
		return true
	case strings.HasPrefix(p, "/api/v1/workers/"):
		return true
	}
	return false
}

// WorkloadOnly reports an identity that holds the work action and nothing
// else, which is what a machine account in the data plane is granted.
//
// Deliberately derived from the GRANTS rather than from a role name or a flag
// on the token. A deployment can rename the role, and a second workload will
// exist eventually; what makes something confined is what it was granted, not
// what it is called.
//
// An identity with no grants at all is not a workload. That is a person whose
// roles have not been set up yet, and answering them with "you are a machine
// and may only lease jobs" would be both wrong and impossible to act on.
func (i Identity) WorkloadOnly() bool {
	if len(i.Grants) == 0 {
		return false
	}
	for _, g := range i.Grants {
		if g.Action != ActionWork {
			return false
		}
	}
	return true
}

// Confine keeps the data plane inside the worker plane, and everybody else
// out of it.
//
// Two rules, one place:
//
//   - a request to the worker plane must hold the work action. With
//     authentication disabled the anonymous identity holds admin and passes,
//     so a deployment that has not switched authentication on behaves exactly
//     as it did before.
//   - a request to anything else must not come from an identity that holds
//     ONLY that action.
//
// deny writes the refusal. It is passed in rather than imported so this
// package keeps knowing nothing about the API's error format.
func Confine(deny func(http.ResponseWriter, *http.Request, string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := IdentityFrom(r.Context())
			if WorkerPlane(r) {
				// Scoped to the caller's OWN tenant, never to the estate. A
				// grant carries the tenant it was made in, and an estate-wide
				// question is the strictest one there is - see Scope.covers -
				// so asking it here would refuse the very identity that was
				// granted the role.
				if !id.Can(ActionWork, Scope{Tenant: id.Tenant}) {
					deny(w, r, "This endpoint is the data plane's. It is reached by a worker "+
						"holding the worker role, not by a user session.")
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			if id.WorkloadOnly() {
				deny(w, r, "This credential may lease and report work, and nothing else.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
