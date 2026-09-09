package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Resource is what an action is being attempted on.
type Resource struct {
	// Kind matches a Cerbos resource policy, e.g. "software_download".
	Kind string
	// ID is the specific object. "*" for a collection.
	ID string
	// Product scopes the resource. Empty means the resource is not
	// product-scoped, and only org-tier roles can reach it.
	Product string
	// Attr carries anything else the policy conditions on.
	Attr map[string]any
}

// Engine decides whether an identity may perform actions on a resource.
//
// An interface, not a struct, so a test can substitute a table of answers and
// so a deployment that outgrows Cerbos is one implementation away.
type Engine interface {
	Check(ctx context.Context, id Identity, res Resource, actions ...string) (map[string]bool, error)
}

// Cerbos calls a Cerbos PDP over its REST API.
type Cerbos struct {
	// Addr is the PDP base URL, e.g. http://cerbos:3592.
	Addr string
	// Tenant is the tenant this deployment serves, and it is what the resource
	// is labelled with.
	//
	// EVERY derived role in config/access/policies compares
	// `R.attr.tenant == P.attr.tenant`. Labelling the resource with the
	// PRINCIPAL's tenant made that comparison compare a value with itself:
	// always true, in every rule, for every caller - a tenancy condition
	// written eleven times and enforced nowhere. The resource belongs to this
	// deployment, so it is this deployment that says which tenant it is in.
	//
	// Empty keeps the old behaviour and the old tautology, for a deployment
	// that has not been told its tenant yet; the Coordinator says so at
	// startup rather than leaving it to be discovered.
	Tenant string
	HTTP   *http.Client
}

// NewCerbos builds a client with sane timeouts.
func NewCerbos(addr string) *Cerbos {
	return &Cerbos{Addr: strings.TrimRight(addr, "/"), HTTP: defaultHTTPClient()}
}

// Prober is an Engine that can report on its own reachability.
//
// Optional, and asserted for rather than folded into Engine: an engine that is
// a table of answers in a test has no reachability to report, and a health
// check is not a reason to make every implementation carry a method it cannot
// answer honestly.
type Prober interface {
	Health(ctx context.Context) error
}

// Health reports whether the PDP is serving.
//
// DIAGNOSTIC ONLY. A PDP that has gone away is not a reason to take a
// Coordinator out of service: Check already fails closed, so the service is
// already refusing what it cannot authorize, and pulling every replica out of
// the endpoints as well would turn a policy-engine blip into an outage with
// nothing left to serve the page that explains it.
func (c *Cerbos) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Addr+"/_cerbos/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cerbos at %s answered %s", c.Addr, resp.Status)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("cerbos at %s answered something other than a health document: %w", c.Addr, err)
	}
	// SERVING is Cerbos' own word, from the gRPC health protocol its REST
	// endpoint mirrors. Anything else is a PDP that is up and not ready, which
	// is a different fault from one that is not there.
	if body.Status != "SERVING" {
		return fmt.Errorf("cerbos at %s reports %s", c.Addr, body.Status)
	}
	return nil
}

type cerbosReq struct {
	RequestID string          `json:"requestId"`
	Principal cerbosPrincipal `json:"principal"`
	Resources []cerbosResEnt  `json:"resources"`
}
type cerbosPrincipal struct {
	ID    string         `json:"id"`
	Roles []string       `json:"roles"`
	Attr  map[string]any `json:"attr"`
}
type cerbosResEnt struct {
	Resource cerbosRes `json:"resource"`
	Actions  []string  `json:"actions"`
}
type cerbosRes struct {
	Kind string         `json:"kind"`
	ID   string         `json:"id"`
	Attr map[string]any `json:"attr"`
}
type cerbosResp struct {
	Results []struct {
		Actions map[string]string `json:"actions"`
	} `json:"results"`
}

// Check asks the PDP. A caller with no roles is refused without a call: an
// unauthenticated request is not a policy question.
func (c *Cerbos) Check(ctx context.Context, id Identity, res Resource, actions ...string) (map[string]bool, error) {
	roles := id.AllRoles()
	if len(roles) == 0 || len(actions) == 0 {
		return refuseAll(actions), nil
	}

	body, err := json.Marshal(cerbosReq{
		RequestID: "swgw",
		Principal: cerbosPrincipal{
			ID:    id.Subject,
			Roles: roles,
			Attr: map[string]any{
				"tenant":   id.Tenant,
				"products": id.ProductNames(),
			},
		},
		Resources: []cerbosResEnt{{
			Resource: cerbosRes{Kind: res.Kind, ID: orStar(res.ID), Attr: c.resourceAttr(id, res)},
			Actions:  actions,
		}},
	})
	if err != nil {
		return nil, err
	}

	decoded, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	if len(decoded.Results) == 0 {
		return refuseAll(actions), nil
	}
	out := map[string]bool{}
	for _, a := range actions {
		out[a] = decoded.Results[0].Actions[a] == "EFFECT_ALLOW"
	}
	return out, nil
}

// Allowed is the single-action form, which is what a handler almost always
// wants.
func Allowed(ctx context.Context, e Engine, id Identity, res Resource, action string) (bool, error) {
	m, err := e.Check(ctx, id, res, action)
	if err != nil {
		return false, err
	}
	return m[action], nil
}

func (c *Cerbos) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient()
}

func orStar(s string) string {
	if s == "" {
		return "*"
	}
	return s
}

// AllowAll is an Engine that permits everything. It exists so a deployment can
// run with authentication switched off and be OBVIOUS about it, rather than
// having the middleware quietly absent.
type AllowAll struct{}

func (AllowAll) Check(_ context.Context, _ Identity, _ Resource, actions ...string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, a := range actions {
		out[a] = true
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Asking about many resources at once
// ---------------------------------------------------------------------------

// Query is one resource and the actions to ask about it.
type Query struct {
	Resource Resource
	Actions  []string
}

// BatchEngine is an Engine that can answer several questions in one exchange.
//
// Optional, and asserted for rather than folded into Engine, for the same
// reason Prober is: a table of answers in a test has nothing to batch, and an
// interface method every implementation must carry in order for one of them to
// be faster is a tax on the ones that are not.
type BatchEngine interface {
	CheckMany(ctx context.Context, id Identity, queries []Query) ([]map[string]bool, error)
}

// CheckMany answers every query, in the order they were asked.
//
// # Why this exists
//
// Describing what a caller may do - the whole permission set an interface
// renders itself from - is forty questions, not one. Asked one at a time
// against a PDP that is a network hop away, that is forty round trips on the
// first read of every session. Cerbos takes a batch natively, so it is one.
//
// An engine with no batch of its own is asked in sequence, which is correct
// and slow rather than wrong.
func CheckMany(ctx context.Context, e Engine, id Identity, queries []Query) ([]map[string]bool, error) {
	if b, ok := e.(BatchEngine); ok {
		return b.CheckMany(ctx, id, queries)
	}
	out := make([]map[string]bool, 0, len(queries))
	for _, q := range queries {
		got, err := e.Check(ctx, id, q.Resource, q.Actions...)
		if err != nil {
			return nil, err
		}
		out = append(out, got)
	}
	return out, nil
}

// CheckMany asks the PDP about every resource in ONE call.
//
// Cerbos' CheckResources takes a list, decides each against the same
// principal, and answers in the same order - which is the whole reason the
// permission set can be resolved without making a session's first read
// quadratic in the size of the catalogue.
func (c *Cerbos) CheckMany(ctx context.Context, id Identity, queries []Query) ([]map[string]bool, error) {
	out := make([]map[string]bool, len(queries))
	roles := id.AllRoles()
	if len(roles) == 0 {
		for i, q := range queries {
			out[i] = refuseAll(q.Actions)
		}
		return out, nil
	}

	// Entries with no actions are answered here rather than sent: Cerbos
	// rejects a resource entry with an empty action list, which would fail the
	// whole batch over a question nobody asked.
	entries := make([]cerbosResEnt, 0, len(queries))
	index := make([]int, 0, len(queries))
	for i, q := range queries {
		if len(q.Actions) == 0 {
			out[i] = map[string]bool{}
			continue
		}
		entries = append(entries, cerbosResEnt{
			Resource: cerbosRes{
				Kind: q.Resource.Kind,
				ID:   orStar(q.Resource.ID),
				Attr: c.resourceAttr(id, q.Resource),
			},
			Actions: q.Actions,
		})
		index = append(index, i)
	}
	if len(entries) == 0 {
		return out, nil
	}

	body, err := json.Marshal(cerbosReq{
		RequestID: "swgw",
		Principal: cerbosPrincipal{
			ID:    id.Subject,
			Roles: roles,
			Attr: map[string]any{
				"tenant":   id.Tenant,
				"products": id.ProductNames(),
			},
		},
		Resources: entries,
	})
	if err != nil {
		return nil, err
	}

	decoded, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}
	for n, i := range index {
		if n >= len(decoded.Results) {
			// A PDP that answered about fewer resources than it was asked
			// about has not permitted the rest of them.
			out[i] = refuseAll(queries[i].Actions)
			continue
		}
		got := map[string]bool{}
		for _, a := range queries[i].Actions {
			got[a] = decoded.Results[n].Actions[a] == "EFFECT_ALLOW"
		}
		out[i] = got
	}
	return out, nil
}

// resourceAttr labels a resource for the policies. See Cerbos.Tenant: the
// tenant is the DEPLOYMENT's, never the caller's.
func (c *Cerbos) resourceAttr(id Identity, res Resource) map[string]any {
	attr := map[string]any{}
	for k, v := range res.Attr {
		attr[k] = v
	}
	attr["tenant"] = c.Tenant
	if c.Tenant == "" {
		attr["tenant"] = id.Tenant
	}
	if res.Product != "" {
		attr["product"] = res.Product
	}
	return attr
}

// post sends one prepared request to the PDP and decodes its answer.
func (c *Cerbos) post(ctx context.Context, body []byte) (cerbosResp, error) {
	var decoded cerbosResp
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.Addr+"/api/check/resources", bytes.NewReader(body))
	if err != nil {
		return decoded, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http().Do(req)
	if err != nil {
		// NEVER fail open. An unreachable policy engine means we cannot know
		// whether this is allowed, and "cannot know" is not "yes".
		return decoded, fmt.Errorf("authz: policy engine unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return decoded, fmt.Errorf("authz: policy engine returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return decoded, fmt.Errorf("authz: decode policy response: %w", err)
	}
	return decoded, nil
}

func refuseAll(actions []string) map[string]bool {
	out := map[string]bool{}
	for _, a := range actions {
		out[a] = false
	}
	return out
}

// CheckMany permits everything, in the shape the caller asked for.
func (AllowAll) CheckMany(_ context.Context, _ Identity, queries []Query) ([]map[string]bool, error) {
	out := make([]map[string]bool, len(queries))
	for i, q := range queries {
		got := map[string]bool{}
		for _, a := range q.Actions {
			got[a] = true
		}
		out[i] = got
	}
	return out, nil
}
