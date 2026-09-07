package authz

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Options configure the middleware.
type Options struct {
	// Verifier validates tokens. Nil means authentication is DISABLED and
	// every caller is Anonymous - useful for local development, never for a
	// deployment reachable from anywhere.
	Verifier *Verifier
	// Engine decides permissions. Nil means AllowAll.
	Engine Engine
	// Optional lets an unauthenticated request through as Anonymous instead of
	// being refused. Handlers must then check Identity.Authenticated().
	Optional bool
	// Logger records refusals. Refusals are logged, never their tokens.
	Logger *slog.Logger
	// ErrorWriter renders a refusal. Defaults to RFC 9457 problem details,
	// matching the rest of this API.
	ErrorWriter func(w http.ResponseWriter, r *http.Request, status int, detail string)
}

// Authenticate validates the bearer token and puts the Identity in the request
// context. It does NOT decide permissions: that is Require, per route, because
// a middleware cannot know which resource a handler is about to touch.
//
// Position it exactly where the no-op authenticator sits today:
//
//	RequestID -> Logging -> Tracing -> Metrics -> Recovery -> [Authenticate] -> Compress -> Handler
func Authenticate(opts Options) func(http.Handler) http.Handler {
	writeErr := opts.ErrorWriter
	if writeErr == nil {
		writeErr = writeProblem
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Authentication disabled: be explicit rather than absent.
			if opts.Verifier == nil {
				next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), Anonymous)))
				return
			}
			raw := BearerToken(r)
			if raw == "" {
				if opts.Optional {
					next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), Anonymous)))
					return
				}
				writeErr(w, r, http.StatusUnauthorized, "No bearer token was supplied.")
				return
			}
			id, err := opts.Verifier.Verify(r.Context(), raw)
			if err != nil {
				// The reason is logged, never returned: telling a caller which
				// check failed helps them forge the next attempt.
				log.WarnContext(r.Context(), "token rejected", "error", err, "path", r.URL.Path)
				writeErr(w, r, http.StatusUnauthorized, "The token is not valid.")
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), id)))
		})
	}
}

// ResourceFunc derives the resource a request acts on, usually from path
// variables. It runs per request, so it can read the product out of the URL.
type ResourceFunc func(r *http.Request) Resource

// Require gates a route on one action against the resource the request names.
//
//	r.With(authz.Require(eng, productFromPath, "request")).
//	  Post("/products/{product}/downloads", h.create)
func Require(engine Engine, resource ResourceFunc, action string, opts ...Options) func(http.Handler) http.Handler {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	writeErr := o.ErrorWriter
	if writeErr == nil {
		writeErr = writeProblem
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	if engine == nil {
		engine = AllowAll{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := FromContext(r.Context())
			res := resource(r)
			ok, err := Allowed(r.Context(), engine, id, res, action)
			if err != nil {
				// The engine is unreachable. Refuse: "cannot know" is not "yes".
				log.ErrorContext(r.Context(), "authorization check failed",
					"error", err, "action", action, "kind", res.Kind)
				writeErr(w, r, http.StatusServiceUnavailable,
					"Permissions cannot be checked right now.")
				return
			}
			if !ok {
				log.InfoContext(r.Context(), "refused",
					"subject", id.Subject, "tenant", id.Tenant,
					"action", action, "kind", res.Kind, "product", res.Product)
				writeErr(w, r, http.StatusForbidden,
					"You do not have permission to do this.")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeProblem renders RFC 9457 problem details, which is what this API already
// returns everywhere else.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="software-gateway"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":     "about:blank",
		"title":    http.StatusText(status),
		"status":   status,
		"detail":   detail,
		"instance": r.URL.Path,
	})
}

// WhoAmI reports the caller and what they hold. Mount it unauthenticated-safe:
// a caller must always be able to discover that they are anonymous.
func WhoAmI(w http.ResponseWriter, r *http.Request) {
	id := FromContext(r.Context())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"subject":       id.Subject,
		"authenticated": id.Authenticated(),
		"method":        id.Method,
		"tenant":        id.Tenant,
		"email":         id.Email,
		"name":          id.Name,
		"orgRoles":      id.OrgRoles,
		"products":      id.Products,
	})
}
