// Package jfrog promotes between two repositories of one Artifactory, using
// JFrog's own promotion endpoint.
//
// See docs/design/22-promotion.md §5.
//
// # Why this plugin exists at all
//
// Our engine already promotes correctly, and within one registry it is already
// fast: every blob is relocated by cross-repository mount, so a 45 GB
// promotion moves no bytes (docs/design/05 §4.2, and
// internal/transfer/promote_test.go proves it). What it still costs is TALK. A
// 260-artifact release is a manifest walk, a plan, several thousand job rows,
// and a mount request per blob per destination repository - tens of thousands
// of round trips to relocate content the registry could relocate in one call
// per name.
//
// JFrog exposes exactly that call. `POST /api/docker/{repo}/v2/promote` moves
// one image - manifest, layers and all - between two repositories of the same
// Artifactory, server-side, atomically from a client's point of view. A
// release becomes one call per NAME rather than one per blob, and the whole
// planner is skipped.
//
// # copy: true, and it is not a default worth trusting
//
// JFrog's promote MOVES by default. `copy: false` deletes the image from the
// source repository, which for a promotion means lab is emptied the moment
// production is filled - the single most destructive thing this system could
// do, silently, on a successful-looking request.
//
// So `copy` is set explicitly on every call, it is never read from
// configuration, and there is no option to turn it off. A "move" mode would be
// a footgun with no user: nobody promoting lab to production wants lab
// emptied, and somebody who genuinely does has `transferctl` and the registry.
//
// # What it does not do
//
// It does not decide WHETHER to promote, what the destination is, or whether
// the operator may. Those are the requester's (internal/transfer/request.go),
// and this plugin sees a hop that has already been resolved and authorised.
package jfrog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/promote"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// Name is the registered plugin name. It reaches promotion rows, the API and
// the audit trail, so it does not change.
const Name = "jfrog"

// Option keys read from an endpoint's Options map.
//
// Strings rather than fields on promote.Endpoint, because a plugin's own
// configuration must not require editing a shared struct - see the Options
// comment there.
const (
	// OptionRepositoryKey is the Artifactory repository key, when it cannot be
	// derived from the configured repository path.
	OptionRepositoryKey = "jfrogRepositoryKey"
	// OptionEndpoint is the JFrog PLATFORM base URL, when the docker host is
	// not it.
	OptionEndpoint = "jfrogEndpoint"
)

const (
	// promotePath is the Artifactory Docker promotion endpoint. `{repo}` is
	// the SOURCE repository key.
	promotePath = "/artifactory/api/docker/%s/v2/promote"

	// defaultTimeout bounds one promotion call.
	//
	// Generous. A promotion is a metadata operation in the common case and
	// returns in well under a second, but Artifactory materialises the copy
	// synchronously and a release with a few hundred large layers on a busy
	// instance genuinely takes minutes for one call. Timing that out would
	// leave the promotion half done AND report a failure, which is the worst
	// of both.
	defaultTimeout = 10 * time.Minute

	retryAttempts  = 3
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 4 * time.Second

	// tagsListPath lists what a docker repository actually holds. `{repo}` is
	// the source repository key, `{name}` the docker repository path.
	tagsListPath = "/artifactory/api/docker/%s/v2/%s/tags/list"
)

// errAmbiguousBadRequest marks a 400 whose body named no cause beyond the
// status itself. Observed against packageType=oci/federated repositories when
// a promote request is scoped to one tag (`tag`/`targetTag`); classify cannot
// turn that into a better message, so promoteOne investigates instead of
// merely reporting it.
var errAmbiguousBadRequest = errors.New("Artifactory gave no further diagnosis")

func init() { promote.Register(Name, New) }

// Promoter promotes between two repository keys of one JFrog platform.
type Promoter struct {
	cfg promote.Config
	log *slog.Logger

	// endpoint, srcKey and dstKey are derived at construction so Claim can
	// answer without doing it twice, and so a derivation failure is a REASON
	// rather than an error - a hop this plugin cannot address is not a fault,
	// it is somebody else's hop.
	endpoint string
	srcKey   string
	dstKey   string
	// why is the derivation's complaint, empty when everything resolved.
	why string
}

// New builds a promoter for one hop.
//
// It never fails: everything it could complain about is a reason for Claim to
// decline rather than an error, because the chain constructs every registered
// plugin in order to ask. See promote.Constructor.
func New(cfg promote.Config) (promote.Promoter, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	p := &Promoter{cfg: cfg, log: log}

	var err error
	if p.endpoint, err = platformEndpoint(cfg.Origin, cfg.OriginClient); err != nil {
		p.why = err.Error()
		return p, nil
	}
	if p.srcKey, err = repositoryKey(cfg.Origin); err != nil {
		p.why = err.Error()
		return p, nil
	}
	if p.dstKey, err = repositoryKey(cfg.Destination); err != nil {
		p.why = err.Error()
		return p, nil
	}
	return p, nil
}

// Name implements promote.Promoter.
func (p *Promoter) Name() string { return Name }

// Claim decides whether this hop is JFrog's to carry.
//
// Configuration only, and every branch produces a sentence: an operator whose
// fast path is unavailable should learn WHICH of these was not true, because
// three of the four are configuration mistakes worth fixing and the fourth is
// a fact about the estate.
func (p *Promoter) Claim(h promote.Hop) promote.Verdict {
	no := func(format string, args ...any) promote.Verdict {
		return promote.Verdict{Promoter: Name, Reason: fmt.Sprintf(format, args...)}
	}

	// 1. Both ends must be JFrog. A target configured `generic` against an
	//    Artifactory host is deliberately NOT claimed: the configuration says
	//    what this deployment intends to speak, and inferring JFrog from a
	//    hostname would make the fast path depend on a DNS name.
	if !isJFrog(h.Origin.RegistryType) || !isJFrog(h.Destination.RegistryType) {
		return no("%s is type %s, not jfrog.", notJFrog(h), orGeneric(typeOfNotJFrog(h)))
	}

	// 2. One Artifactory. This is the real constraint and the reason the fast
	//    path exists: promotion is an internal relocation, and there is
	//    nothing internal about two hosts.
	// The hosts themselves are already on screen - the origin on the "From"
	// line, the destination on this row - so naming them again is a sentence
	// the reader has to parse to learn what they can already see.
	if !sameHost(h.Origin.Registry, h.Destination.Registry) {
		return no("A different Artifactory from %s.", h.Origin.Name)
	}

	// 3. Both paths have to name a repository key. The full stop is added
	//    here rather than carried in the error: this is a sentence shown to a
	//    reader, and a Go error does not end in punctuation.
	if p.why != "" {
		return no("%s.", p.why)
	}

	// 4. A hop that does not move is not a hop. Same key AND same path means
	//    the two targets are the same place under two names, which the
	//    requester already refuses by repository row - this catches the case
	//    where two rows resolve to one JFrog coordinate.
	if strings.EqualFold(p.srcKey, p.dstKey) &&
		sameRepository(h.Origin.Repository, h.Destination.Repository) {
		return no("%s and %s are the same JFrog repository (%s).",
			h.Origin.Name, h.Destination.Name, p.srcKey)
	}

	return promote.Verdict{
		Promoter: Name,
		Claimed:  true,
		Reason: fmt.Sprintf("Same Artifactory as %s. JFrog relocates %s server-side.",
			h.Origin.Name, plural(len(h.Names), "tag")),
	}
}

// Promote carries the hop out, one name at a time.
//
// Sequential rather than concurrent, deliberately. Each call is a server-side
// relocation that Artifactory performs synchronously against its own storage,
// and firing two hundred of them at once at one instance is how a promotion
// turns into a 429 storm and finishes slower than the serial version. The
// whole operation is already seconds in the case that matters.
func (p *Promoter) Promote(ctx context.Context, h promote.Hop) (promote.Outcome, error) {
	out := promote.Outcome{
		Promoter: Name,
		Detail:   fmt.Sprintf("%s -> %s on %s", p.srcKey, p.dstKey, hostOf(p.endpoint)),
	}
	if p.why != "" {
		return out, fmt.Errorf("jfrog promotion: %s", p.why)
	}

	client, err := p.httpClient()
	if err != nil {
		return out, err
	}

	// Tags this HOP intends to publish at each source path, keyed by that
	// path. Consulted only if a tag-scoped call comes back an undiagnosed 400
	// (see promoteOne): a repository-level fallback moves every tag a source
	// holds, and this is what proves that doing so publishes nothing the hop
	// did not already ask for.
	intended := map[string]map[string]bool{}
	for _, n := range h.Names {
		src := joinPath(trimKey(h.Origin.Repository, p.srcKey), n.Repository)
		if intended[src] == nil {
			intended[src] = map[string]bool{}
		}
		intended[src][n.Tag] = true
	}

	for _, n := range h.Names {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}

		src := joinPath(trimKey(h.Origin.Repository, p.srcKey), n.Repository)
		dst := joinPath(trimKey(h.Destination.Repository, p.dstKey), n.Repository)
		if src == "" || dst == "" {
			return out, fmt.Errorf(
				"jfrog promotion: %s:%s resolves to an empty docker repository path"+
					" (source %q, destination %q)", n.Repository, n.Tag,
				h.Origin.Repository, h.Destination.Repository)
		}

		if err := p.promoteOne(ctx, client, src, dst, n.Tag, intended[src]); err != nil {
			return out, fmt.Errorf("promote %s:%s to %s/%s: %w", src, n.Tag, p.dstKey, dst, err)
		}
		out.Promoted++
	}

	return out, nil
}

// promoteOne issues one promotion, scoped to one tag.
//
// Some Artifactory versions answer a bare, undiagnosed 400 to a tag-scoped
// request against a packageType=oci or federated repository - see
// errAmbiguousBadRequest. For THAT failure only, promoteOne checks what
// srcRepo actually holds (tagsOutsideHop) and, if every tag there is already
// something `allowed` names, retries as a repository-level promotion, which
// this Artifactory does accept. It never does that check for any other
// failure, and never retries when srcRepo holds a tag `allowed` does not -
// promoting the whole repository would carry that tag along uninvited.
func (p *Promoter) promoteOne(
	ctx context.Context, client *http.Client, srcRepo, dstRepo, tag string, allowed map[string]bool,
) error {
	err := p.doPromote(ctx, client, srcRepo, dstRepo, tag, tag)
	if err == nil || !errors.Is(err, errAmbiguousBadRequest) {
		return err
	}

	extra, lerr := p.tagsOutsideHop(ctx, client, srcRepo, allowed)
	if lerr != nil {
		return fmt.Errorf("%w (checked whether a repository-level promotion would be "+
			"safe, and could not: %s)", err, lerr)
	}
	if len(extra) > 0 {
		return fmt.Errorf(
			"%w\n%s cannot be promoted for just %q on this Artifactory, and a repository-"+
				"level promotion is not safe here either: %s also holds %s, which this "+
				"promotion did not ask to publish", err, srcRepo, tag, srcRepo, strings.Join(extra, ", "))
	}

	// Every tag srcRepo holds is one this hop already intends to publish, so
	// promoting the repository as a whole publishes nothing extra.
	if err2 := p.doPromote(ctx, client, srcRepo, dstRepo, "", ""); err2 != nil {
		return fmt.Errorf("%s (repository-level fallback also failed: %w)", err, err2)
	}
	return nil
}

// doPromote issues one call to the promote endpoint. tag and targetTag empty
// means "every tag srcRepo holds" - see promoteOne for when that is safe.
func (p *Promoter) doPromote(
	ctx context.Context, client *http.Client, srcRepo, dstRepo, tag, targetTag string,
) error {
	body, err := json.Marshal(promoteRequest{
		TargetRepo: p.dstKey,

		DockerRepository:       srcRepo,
		TargetDockerRepository: dstRepo,

		Tag:       tag,
		TargetTag: targetTag,

		// NOT configurable, and never false. See the package comment: the
		// default is a MOVE, and a move would empty lab the instant production
		// was filled.
		Copy: true,
	})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	u := p.endpoint + fmt.Sprintf(promotePath, url.PathEscape(p.srcKey))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	// Basic, not the OCI token exchange. This is the Artifactory REST API
	// rather than the distribution endpoint, and presenting a bearer token
	// obtained from the docker token realm gets a 401 that reads like a
	// credential problem. An access token in the password field works
	// unchanged, which is what most deployments actually use.
	if p.cfg.OriginClient.Username != "" || p.cfg.OriginClient.Password != "" {
		req.SetBasicAuth(p.cfg.OriginClient.Username, p.cfg.OriginClient.Password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return classify(resp, u, p.srcKey)
}

// tagsOutsideHop lists what srcRepo actually holds and reports any tag
// `allowed` does not name - the check that makes the repository-level
// fallback in promoteOne safe rather than merely convenient.
func (p *Promoter) tagsOutsideHop(
	ctx context.Context, client *http.Client, srcRepo string, allowed map[string]bool,
) ([]string, error) {
	u := p.endpoint + fmt.Sprintf(tagsListPath, url.PathEscape(p.srcKey), srcRepo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if p.cfg.OriginClient.Username != "" || p.cfg.OriginClient.Password != "" {
		req.SetBasicAuth(p.cfg.OriginClient.Username, p.cfg.OriginClient.Password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classify(resp, u, p.srcKey)
	}

	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode tag list: %w", err)
	}

	var extra []string
	for _, t := range payload.Tags {
		if !allowed[t] {
			extra = append(extra, t)
		}
	}
	sort.Strings(extra)
	return extra, nil
}

// promoteRequest is the body of POST /api/docker/{repo}/v2/promote.
type promoteRequest struct {
	TargetRepo string `json:"targetRepo"`

	DockerRepository       string `json:"dockerRepository"`
	TargetDockerRepository string `json:"targetDockerRepository,omitempty"`

	Tag       string `json:"tag,omitempty"`
	TargetTag string `json:"targetTag,omitempty"`

	Copy bool `json:"copy"`
}

// classify turns a failed response into an error of the shared vocabulary, so
// the retry policy and the operator-facing message both key off the same
// classification every other registry call uses.
func classify(resp *http.Response, url, repoKey string) error {
	detail := strings.TrimSpace(readMessage(resp.Body))
	if detail == "" {
		detail = resp.Status
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("%s (%w)", detail, registry.ErrUnauthorized)
	case http.StatusForbidden:
		// The single most common real failure, and worth naming precisely: the
		// credential can pull from lab and push to production over the docker
		// endpoint, and still lack the Artifactory permission that promotion
		// needs. "Forbidden" alone sends people to look at the wrong thing.
		return fmt.Errorf(
			"%s - the credential can reach the repositories but is not permitted to "+
				"promote between them; it needs delete on the source and deploy on the "+
				"destination in Artifactory (%w)", detail, registry.ErrForbidden)
	case http.StatusNotFound:
		// The single most confusing failure on this path, because it has two
		// completely different causes that read identically. The image really
		// is absent - or the URL is wrong, which on JFrog means the repository
		// key was derived from a path that does not contain it. Naming the URL
		// is what separates them: an operator who sees a key they do not
		// recognise knows to set `jfrogRepositoryKey`, and one who sees the
		// right key knows to look for the image.
		return fmt.Errorf(
			"%s\nasked %s, so this is either a missing image or the wrong Artifactory "+
				"repository key (%q, derived from `repository` unless `jfrogRepositoryKey` "+
				"is set) (%w)",
			detail, url, repoKey, registry.ErrNotFound)
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return fmt.Errorf(
			"this Artifactory does not serve the Docker promotion endpoint: %s (%w)",
			detail, registry.ErrUnsupported)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%s (%w)", detail, registry.ErrRateLimited)
	case http.StatusBadRequest:
		// Artifactory's OWN JSON, and it names no cause beyond the status - seen
		// against packageType=oci and federated repositories, some versions of
		// which reject a request scoped to one tag (`tag`/`targetTag`). Naming
		// that here rather than only in promoteOne is what makes a Claim-time
		// dry run and a failure a person reads later say the same thing; the
		// caller decides whether trying the repository-level fallback is safe.
		if strings.EqualFold(detail, "bad request") {
			return fmt.Errorf(
				"%s: %w; commonly seen on packageType=oci or federated repositories, "+
					"which some Artifactory versions refuse to promote one tag at a time",
				detail, errAmbiguousBadRequest)
		}
		return errors.New(detail)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%s (%w)", detail, registry.ErrUnavailable)
	}
	return errors.New(detail)
}

// readMessage pulls Artifactory's own explanation out of a failure.
//
// Artifactory answers errors as `{"errors":[{"status":403,"message":"..."}]}`,
// and the message is nearly always the actual diagnosis. Falling back to the
// raw body matters too: a corporate proxy returning an HTML error page is a
// completely different problem, and swallowing it leaves an operator with a
// status code and nothing else.
func readMessage(r io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(r, 8<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var payload struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		msgs := make([]string, 0, len(payload.Errors)+1)
		for _, e := range payload.Errors {
			if e.Message != "" {
				msgs = append(msgs, e.Message)
			}
		}
		if payload.Message != "" {
			msgs = append(msgs, payload.Message)
		}
		if len(msgs) > 0 {
			return strings.Join(msgs, "; ")
		}
	}
	return strings.TrimSpace(string(raw))
}

// httpClient builds the transport.
//
// The SAME stack the artifact path uses - TLS trust, proxy, timeouts, retry,
// rate limiting - with no registry credentials attached, because the OCI token
// exchange is the wrong authentication for this endpoint. Identical reasoning
// to internal/registry/artifactory/xray.go, and the two must stay that way: a
// promotion that reached JFrog by a different route from the transfers would
// be a second thing to configure and the second thing to go stale.
func (p *Promoter) httpClient() (*http.Client, error) {
	c := p.cfg.OriginClient
	if c.Username == "" && c.Password == "" {
		return nil, fmt.Errorf(
			"jfrog promotion needs a credential for %s; the target is configured anonymous (%w)",
			p.cfg.Origin.Registry, registry.ErrUnauthorized)
	}

	client, err := transport.New(transport.Config{
		Registry:              hostOf(p.endpoint),
		CABundle:              c.CABundle,
		InsecureSkipVerify:    c.InsecureSkipVerify,
		HTTPSProxy:            c.HTTPSProxy,
		NoProxy:               c.NoProxy,
		DirectConnect:         c.DirectConnect,
		ConnectTimeout:        c.ConnectTimeout,
		ResponseHeaderTimeout: c.ResponseHeaderTimeout,
		UserAgent:             c.UserAgent,
		Logger:                p.log,
		ForceHTTP1:            true,

		MaxRetryAttempts: retryAttempts,
		RetryBaseDelay:   retryBaseDelay,
		RetryMaxDelay:    retryMaxDelay,
		RetryMaxElapsed:  defaultTimeout / 2,
	})
	if err != nil {
		return nil, fmt.Errorf("jfrog promotion: build transport: %w", err)
	}
	client.Timeout = defaultTimeout
	return client, nil
}

// ---------------------------------------------------------------------------
// Addressing
// ---------------------------------------------------------------------------

// repositoryKey works out which Artifactory repository a target lives in.
//
// # Why this is a derivation and not a field
//
// JFrog serves Docker two ways, and only one of them puts the key in the path.
// A repository-path deployment addresses `acme.jfrog.io/docker-prod/nokia/orbs`
// - the first segment IS the repository key. A subdomain deployment addresses
// `acme-docker-prod.jfrog.io/nokia/orbs`, where the key is in the HOSTNAME and
// the path has none.
//
// There is no reliable way to tell those apart from a hostname, so the
// derivation covers the common case and configuration covers the other -
// exactly the arrangement xray.go's endpoint resolution uses, and for the same
// reason. Getting it wrong is cheap to diagnose because the failure names the
// repository key it tried.
func repositoryKey(e promote.Endpoint) (string, error) {
	if key := strings.Trim(e.Options[OptionRepositoryKey], "/ "); key != "" {
		return key, nil
	}
	path := strings.Trim(strings.TrimSpace(e.Repository), "/")
	if path == "" {
		return "", fmt.Errorf(
			"%s has no Artifactory repository key. Set `jfrogRepositoryKey`", e.Name)
	}
	if i := strings.Index(path, "/"); i > 0 {
		return path[:i], nil
	}
	return path, nil
}

// platformEndpoint decides where the Artifactory REST API lives.
//
// Same problem and same answer as xray.go: on a repository-path deployment the
// platform base URL is the docker host, and on a subdomain deployment it is
// not. `jfrogEndpoint` settles it where the derivation cannot.
func platformEndpoint(e promote.Endpoint, c registry.ClientConfig) (string, error) {
	raw := strings.TrimSpace(e.Options[OptionEndpoint])
	if raw == "" {
		if e.Registry == "" {
			return "", fmt.Errorf("target %q names no registry host", e.Name)
		}
		raw = e.Registry
	}
	if !strings.Contains(raw, "://") {
		if c.PlainHTTP {
			raw = "http://" + raw
		} else {
			raw = "https://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("target %q has an unusable JFrog endpoint %q", e.Name, raw)
	}
	return strings.TrimSuffix(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// trimKey removes the repository key from the front of a configured path.
//
// The docker repository JFrog wants is the path WITHOUT its key - promoting
// `docker-prod/nokia/orbs` means repository key `docker-prod` and docker
// repository `nokia/orbs`. Leaving the key on produces
// `docker-prod/docker-prod/nokia/orbs`, which 404s in a way that reads like a
// missing image.
//
// On a subdomain deployment the key is not in the path at all, so nothing is
// trimmed and the whole path is the docker repository - which is why this
// compares rather than assuming.
func trimKey(configured, key string) string {
	path := strings.Trim(strings.TrimSpace(configured), "/")
	switch {
	case strings.EqualFold(path, key):
		return ""
	case len(path) > len(key)+1 &&
		strings.EqualFold(path[:len(key)], key) && path[len(key)] == '/':
		return path[len(key)+1:]
	default:
		return path
	}
}

func joinPath(base, rest string) string {
	base = strings.Trim(strings.TrimSpace(base), "/")
	rest = strings.Trim(strings.TrimSpace(rest), "/")
	switch {
	case base == "":
		return rest
	case rest == "":
		return base
	default:
		return base + "/" + rest
	}
}

// isJFrog recognises the two spellings configuration accepts.
//
// Compared here rather than through product.RegistryType.IsJFrog, so this
// plugin stays free of configuration types - see the promote.Endpoint comment
// on why a promoter must not import internal/product. The two spellings are
// fixed by that package's validation, so there is nothing here to drift.
func isJFrog(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "jfrog", "artifactory":
		return true
	default:
		return false
	}
}

func orGeneric(t string) string {
	if strings.TrimSpace(t) == "" {
		return "generic"
	}
	return t
}

// notJFrog names the END that is not declared JFrog, so the refusal points at
// the one target somebody has to edit rather than restating both.
func notJFrog(h promote.Hop) string {
	if !isJFrog(h.Origin.RegistryType) {
		return h.Origin.Name
	}
	return h.Destination.Name
}

func typeOfNotJFrog(h promote.Hop) string {
	if !isJFrog(h.Origin.RegistryType) {
		return h.Origin.RegistryType
	}
	return h.Destination.RegistryType
}

// plural renders a count with its noun, so a message never says "1 tags".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func sameRepository(a, b string) bool {
	return strings.EqualFold(strings.Trim(strings.TrimSpace(a), "/"),
		strings.Trim(strings.TrimSpace(b), "/"))
}

func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return endpoint
	}
	return u.Host
}
