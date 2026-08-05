// Package preflight answers a different question from health.
//
//	health      Is the SERVICE working? Coordinator up, database reachable,
//	            configuration parsed. Fast, cheap, safe to poll constantly, and
//	            its API sibling backs /readyz.
//
//	preflight   Is the CONFIGURATION correct and usable? Can we reach this
//	            registry, does this credential work, does it grant the
//	            permissions this product needs. Slow, makes real network calls
//	            to third parties, run on demand.
//
// They are deliberately separate. Folding registry probes into health would
// mean a vendor's outage makes OUR service look unhealthy — and since the same
// machinery backs readiness, a vendor's bad afternoon would pull our pods out
// of service. It would also make health unable to answer the question it
// exists for: an operator seeing "DEGRADED" could not tell whether the fault
// was ours or someone else's.
//
// See docs/design/09 §9.1.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/generic"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// Status is one check's outcome.
type Status string

const (
	StatusOK      Status = "OK"
	StatusFailed  Status = "FAILED"
	StatusWarning Status = "WARNING"
	// StatusSkipped means the check did not apply, or an earlier one failed
	// and made it meaningless. A skipped check is reported rather than hidden:
	// "we did not check" and "it passed" must not look the same.
	StatusSkipped Status = "SKIPPED"
)

// Step is one probe against one repository.
type Step struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Latency time.Duration
}

// RepositoryResult is every check for one configured repository.
type RepositoryResult struct {
	Product    string
	Name       string
	Role       string
	Registry   string
	Repository string
	Status     Status
	Steps      []Step
}

// ProductResult is every repository of one product.
type ProductResult struct {
	Product      string
	Status       Status
	Repositories []RepositoryResult
}

// Checker probes configured repositories.
type Checker struct {
	secrets *product.SecretResolver
	// timeout bounds ONE repository's checks. A vendor that accepts a
	// connection and never answers must not hold the whole report.
	timeout time.Duration
	// concurrency bounds parallel probes, so checking a product with forty
	// repositories does not open forty connections to one registry at once —
	// which is the behaviour a rate limiter exists to prevent.
	concurrency int
}

// NewChecker builds a preflight checker.
//
// The per-repository timeout is derived from the transport budget in
// probeTransport rather than picked: 2 attempts of (5s connect + 10s waiting
// for headers) is 30 seconds of legitimate work, plus retry backoff. A 15s
// budget — what this used to be — cut off a probe that was still within its own
// retry policy, so a registry behind a slow proxy reported a timeout the
// transport had not yet reached.
func NewChecker(secrets *product.SecretResolver) *Checker {
	return &Checker{secrets: secrets, timeout: 45 * time.Second, concurrency: 8}
}

// CheckProduct probes every repository a product declares.
func (c *Checker) CheckProduct(ctx context.Context, p *product.Product) ProductResult {
	res := ProductResult{Product: p.Metadata.Name, Status: StatusOK}

	type job struct {
		name       string
		role       string
		registry   string
		repository string
		anonymous  bool
		creds      *product.CredentialsRef
		network    *product.Network
		limits     product.RateLimits
		catalog    bool
		filtered   bool
	}

	// A disabled product is not probed at all: there is nothing to diagnose
	// about configuration that is deliberately not running, and reporting
	// failures for it would bury the products that ARE running.
	if !p.IsEnabled() {
		res.Status = StatusSkipped
		return res
	}

	var jobs []job

	for _, s := range p.Spec.Sources {
		if !s.IsEnabled() {
			continue
		}
		if s.EnumeratesRepositories() {
			// A source that names no repositories finds them from the catalog,
			// so the thing to probe is the registry itself and whether this
			// credential may enumerate it.
			jobs = append(jobs, job{
				name: s.Name, role: string(product.RoleSource), registry: s.Registry,
				anonymous: s.Anonymous, creds: s.CredentialsRef, network: s.Network,
				limits: s.RateLimits, catalog: true,
				filtered: len(s.Discovery.RepositoryFilters.Include) > 0 ||
					len(s.Discovery.RepositoryFilters.Exclude) > 0,
			})
			continue
		}
		for _, repo := range s.DeclaredRepositories() {
			jobs = append(jobs, job{
				name: s.Name, role: string(product.RoleSource), registry: s.Registry,
				repository: repo, anonymous: s.Anonymous, creds: s.CredentialsRef,
				network: s.Network, limits: s.RateLimits,
			})
		}
	}
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() {
			continue
		}
		jobs = append(jobs, job{
			name: t.Name, role: string(product.RoleTarget), registry: t.Registry,
			repository: t.Repository, anonymous: t.Anonymous, creds: t.CredentialsRef,
			network: t.Network, limits: t.RateLimits,
		})
	}

	results := make([]RepositoryResult, len(jobs))
	sem := make(chan struct{}, c.concurrency)
	var wg sync.WaitGroup

	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			jobCtx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()

			results[i] = c.checkRepository(jobCtx, p, repoSpec{
				Name: j.name, Role: j.role, Registry: j.registry, Repository: j.repository,
				Anonymous: j.anonymous, Credentials: j.creds, Network: j.network,
				RateLimits: j.limits, Catalog: j.catalog, Filtered: j.filtered,
			})
		}(i, j)
	}
	wg.Wait()

	// Stable order, so two runs of the same configuration produce diffable
	// output.
	sort.SliceStable(results, func(a, b int) bool {
		if results[a].Role != results[b].Role {
			return results[a].Role < results[b].Role
		}
		if results[a].Name != results[b].Name {
			return results[a].Name < results[b].Name
		}
		return results[a].Repository < results[b].Repository
	})

	res.Repositories = results
	for _, r := range results {
		switch r.Status {
		case StatusFailed:
			res.Status = StatusFailed
		case StatusWarning:
			if res.Status != StatusFailed {
				res.Status = StatusWarning
			}
		case StatusOK, StatusSkipped:
		}
	}
	return res
}

// repoSpec is one repository to probe, flattened from a source or a target.
type repoSpec struct {
	Name        string
	Role        string
	Registry    string
	Repository  string
	Anonymous   bool
	Credentials *product.CredentialsRef
	Network     *product.Network
	RateLimits  product.RateLimits
	// Catalog means this source enumerates its repositories.
	Catalog bool
	// Filtered means discovery.repositoryFilters is set. Used to warn about an
	// unfiltered enumeration, which is legitimate on a dedicated registry and a
	// mistake on a shared one.
	Filtered bool
}

// checkRepository runs the probes for one repository, in dependency order.
//
// Ordered deliberately, each step gated on the previous: there is no point
// asking whether a credential grants read access when the host does not
// resolve, and reporting four failures for one cause is noise an operator has
// to work through.
func (c *Checker) checkRepository(ctx context.Context, p *product.Product, spec repoSpec) RepositoryResult {
	res := RepositoryResult{
		Product:    p.Metadata.Name,
		Name:       spec.Name,
		Role:       spec.Role,
		Registry:   spec.Registry,
		Repository: spec.Repository,
		Status:     StatusOK,
	}

	// 1. Credentials resolve from the projected secret volume.
	cfg, step := c.resolveCredentials(p, spec)
	res.Steps = append(res.Steps, step)
	if step.Status == StatusFailed {
		res.Status = StatusFailed
		res.Steps = append(res.Steps,
			skipped("reachable", "credentials could not be resolved"),
			skipped("authenticated", "credentials could not be resolved"))
		return res
	}

	// 1a. Certificate verification, if it has been turned off.
	//
	// Reported on every run, whether or not anything else passes. This is the
	// setting that gets switched on during an incident to unblock a release and
	// is then still on a year later; the only defence is that it is impossible
	// to look at this report and not see it.
	if cfg.InsecureSkipVerify {
		res.Steps = append(res.Steps, Step{
			Name: "certificate verification", Status: StatusWarning,
			Detail: "DISABLED by network.tls.insecureSkipVerify",
			Hint: "the connection is encrypted but not authenticated: anything that can " +
				"intercept it can serve different bytes and we would not know. Prefer " +
				"network.caBundleRef. Note this does NOT fix \"x509: negative serial " +
				"number\" — that needs tls.allowNegativeSerialNumbers",
		})
	}

	// 2. The registry answers /v2/, and the credential is accepted.
	//
	// One probe covers both: the Distribution spec makes /v2/ the
	// authentication challenge point, so a 200 means reachable AND
	// authenticated, and the error class separates the two failures.
	client, err := generic.New(generic.Config{
		Registry:   spec.Registry,
		Repository: repositoryOrPlaceholder(spec.Repository),
		PlainHTTP:  isLoopback(spec.Registry),
		Transport:  probeTransport(cfg),
	})
	if err != nil {
		res.Status = StatusFailed
		res.Steps = append(res.Steps, Step{
			Name: "reachable", Status: StatusFailed, Detail: err.Error()})
		return res
	}

	started := time.Now()
	pingErr := client.Ping(ctx)
	latency := time.Since(started)

	switch {
	case pingErr == nil:
		res.Steps = append(res.Steps,
			Step{Name: "reachable", Status: StatusOK, Latency: latency},
			Step{Name: "authenticated", Status: StatusOK,
				Detail: credentialDescription(spec)})

	case registry.ClassOf(pingErr) == registry.ClassAuth:
		res.Status = StatusFailed
		res.Steps = append(res.Steps,
			Step{Name: "reachable", Status: StatusOK, Latency: latency},
			Step{Name: "authenticated", Status: StatusFailed,
				Detail: pingErr.Error(),
				Hint:   authHint(spec)})
		res.Steps = append(res.Steps, skipped(readStepName(spec.Role), "authentication failed"))
		return res

	default:
		res.Status = StatusFailed
		res.Steps = append(res.Steps,
			Step{Name: "reachable", Status: StatusFailed, Latency: latency,
				Detail: pingErr.Error(), Hint: reachabilityHint(pingErr, spec)},
			skipped("authenticated", "the registry could not be reached"),
			skipped(readStepName(spec.Role), "the registry could not be reached"))
		return res
	}

	// 3. Permissions the product actually needs.
	if spec.Repository != "" {
		read := c.checkRead(ctx, client, spec)
		res.Steps = append(res.Steps, read)

		// 3a. And then actually FETCH one, because listing tags and reading a
		// manifest are different requests and a registry can serve one and not
		// the other. Observed in the field: `tags/list` returned in
		// milliseconds while every manifest GET through the same proxy timed
		// out awaiting response headers, and preflight said the source was
		// healthy. Discovery then failed on every tag.
		if read.Status == StatusOK && spec.Role == string(product.RoleSource) {
			res.Steps = append(res.Steps, c.checkManifest(ctx, client, spec))
		}
	}
	if spec.Catalog {
		res.Steps = append(res.Steps, c.checkCatalog(ctx, cfg, spec))
	}
	if spec.Role == string(product.RoleTarget) {
		// Write access cannot be verified without writing, and preflight must
		// not leave artefacts in a production registry. Reported as skipped
		// with the reason, rather than silently omitted — "we did not check"
		// and "it passed" must not look the same.
		res.Steps = append(res.Steps, Step{
			Name: "writable", Status: StatusSkipped,
			Detail: "not verified",
			Hint: "confirming write access requires an upload, which preflight will not " +
				"leave behind in your registry. The first transfer exercises it (M3)",
		})
	}

	for _, s := range res.Steps {
		if s.Status == StatusFailed {
			res.Status = StatusFailed
		} else if s.Status == StatusWarning && res.Status != StatusFailed {
			res.Status = StatusWarning
		}
	}
	return res
}

// resolveCredentials reads the secret and builds a client config.
func (c *Checker) resolveCredentials(p *product.Product, spec repoSpec) (registry.ClientConfig, Step) {
	cfg := registry.ClientConfig{
		Registry:  spec.Registry,
		UserAgent: "softwaregateway-preflight",
		PlainHTTP: isLoopback(spec.Registry),
	}

	limits := spec.RateLimits.WithDefaults()
	cfg.RequestsPerSecond = limits.RequestsPerSecond
	cfg.Burst = limits.Burst
	cfg.MaxConnections = 4

	// The CA bundle and proxy come from the product, overridden per repository.
	// Checking them here is the point: a private CA that does not load is a
	// TLS failure at the first scan otherwise, a long way from its cause.
	if err := applyNetwork(&cfg, p, spec.Network, c.secrets); err != nil {
		return cfg, Step{Name: "credentials", Status: StatusFailed, Detail: err.Error(),
			Hint: "the CA bundle or proxy settings could not be read"}
	}

	switch {
	case spec.Anonymous:
		return cfg, Step{Name: "credentials", Status: StatusOK, Detail: "anonymous"}

	case spec.Credentials == nil:
		return cfg, Step{Name: "credentials", Status: StatusFailed,
			Detail: "neither credentialsRef nor anonymous: true",
			Hint:   "add a credentialsRef, or set anonymous: true if the registry is public"}

	default:
		creds, err := c.secrets.Credentials(*spec.Credentials)
		if err != nil {
			return cfg, Step{Name: "credentials", Status: StatusFailed, Detail: err.Error(),
				Hint: "the secret is projected at " + c.secrets.Dir() + "/" +
					spec.Credentials.SecretName + "/, one file per key"}
		}
		cfg.Username = creds.Username
		cfg.Password = creds.Password.Reveal()
		return cfg, Step{Name: "credentials", Status: StatusOK,
			Detail: "resolved from secret " + spec.Credentials.SecretName +
				" as user " + creds.Username}
	}
}

// checkRead verifies the credential can actually list tags.
//
// Reachability and authentication are not enough: a token can be valid and
// still carry no pull scope for this repository, which fails at the first scan
// rather than here unless we ask.
func (c *Checker) checkRead(ctx context.Context, client *generic.Repository, spec repoSpec) Step {
	started := time.Now()
	tags, _, err := client.ListTags(ctx, "", 1)
	latency := time.Since(started)

	switch {
	case err == nil && len(tags) > 0:
		return Step{Name: readStepName(spec.Role), Status: StatusOK, Latency: latency,
			Detail: fmt.Sprintf("readable (%s…)", tags[0])}

	case err == nil:
		// Reachable, authorised, and genuinely empty. Not a failure — a vendor
		// repository awaiting its first release is a normal state — but worth
		// flagging, because an empty repository is also what a typo'd path
		// looks like.
		return Step{Name: readStepName(spec.Role), Status: StatusWarning, Latency: latency,
			Detail: "readable, but the repository has no tags",
			Hint:   "correct if the vendor has not published yet; otherwise check the repository path"}

	case errors.Is(err, registry.ErrNotFound) && spec.Role == string(product.RoleTarget):
		// A target repository does not exist until something is pushed to it.
		// Reporting that as a failure would make every correctly-configured new
		// destination look broken before its first transfer.
		return Step{Name: readStepName(spec.Role), Status: StatusOK, Latency: latency,
			Detail: "does not exist yet",
			Hint:   "normal for a destination: the registry creates it on the first push"}

	case errors.Is(err, registry.ErrNotFound):
		// For a SOURCE it is a typo, and it is the most common one there is.
		// Distinguishing it from an empty repository is the reason the client
		// reports 404 rather than flattening it into an empty list.
		return Step{Name: readStepName(spec.Role), Status: StatusFailed, Latency: latency,
			Detail: "repository does not exist",
			Hint:   "check the repository path; the registry host and credential are fine"}

	case registry.ClassOf(err) == registry.ClassAuth:
		return Step{Name: readStepName(spec.Role), Status: StatusFailed, Latency: latency,
			Detail: err.Error(),
			Hint: "the credential authenticated but is not authorised to pull this " +
				"repository — the scope is usually per repository"}

	default:
		return Step{Name: readStepName(spec.Role), Status: StatusFailed, Latency: latency,
			Detail: err.Error()}
	}
}

// checkManifest resolves one tag and fetches its manifest.
//
// The step that turns "the source looks fine but discovery finds nothing" into
// a specific answer. Discovery does one HEAD and one GET per tag, plus more for
// the artifact tree; if either is slow or refused, every scan fails after a
// listing that looked perfectly healthy.
//
// Exactly one tag, so this stays a probe rather than a scan.
func (c *Checker) checkManifest(ctx context.Context, client *generic.Repository, spec repoSpec) Step {
	const name = "can fetch a manifest"

	tags, _, err := client.ListTags(ctx, "", 1)
	if err != nil || len(tags) == 0 {
		// checkRead already reported whatever went wrong here.
		return skipped(name, "no tag was available to try")
	}
	tag := tags[0]

	started := time.Now()
	desc, err := client.ResolveTag(ctx, tag)
	if err != nil {
		return Step{Name: name, Status: StatusFailed, Latency: time.Since(started),
			Detail: fmt.Sprintf("HEAD manifests/%s failed: %s", tag, err.Error()),
			Hint:   manifestHint(err)}
	}

	if _, _, err := client.FetchManifest(ctx, string(desc.Digest)); err != nil {
		// Split from the HEAD deliberately. A registry — or a proxy that
		// inspects bodies — can answer HEAD promptly and stall on the GET, and
		// reporting them as one step would hide which of the two is broken.
		return Step{Name: name, Status: StatusFailed, Latency: time.Since(started),
			Detail: fmt.Sprintf("HEAD %s succeeded, GET manifests/%s failed: %s",
				tag, desc.Digest.Short(), err.Error()),
			Hint: manifestHint(err)}
	}

	return Step{Name: name, Status: StatusOK, Latency: time.Since(started),
		Detail: fmt.Sprintf("%s resolved to %s and the body was readable", tag, desc.Digest.Short())}
}

// manifestHint names the setting that addresses a manifest-fetch failure.
func manifestHint(err error) string {
	switch {
	case errors.Is(err, registry.ErrTimeout):
		return "discovery does this for EVERY tag, so a slow manifest fetch fails every " +
			"scan. Raise network.timeouts.responseHeader, and if the traffic goes " +
			"through an inspecting proxy, check network.proxy — a proxy that scans " +
			"response bodies can answer HEAD quickly and stall on GET"
	case registry.ClassOf(err) == registry.ClassAuth:
		return "the credential can list tags but not read manifests, which some " +
			"registries scope separately"
	default:
		return ""
	}
}

// checkCatalog verifies repositoryDiscovery can enumerate the registry.
func (c *Checker) checkCatalog(ctx context.Context, cfg registry.ClientConfig, spec repoSpec) Step {
	cat, err := generic.NewCatalog(generic.CatalogConfig{
		Registry: spec.Registry, PlainHTTP: isLoopback(spec.Registry),
		Transport: probeTransport(cfg),
	})
	if err != nil {
		return Step{Name: "can enumerate repositories", Status: StatusFailed, Detail: err.Error()}
	}

	started := time.Now()
	// Deliberately over the cap a scan would use, so the report can say how
	// much an unfiltered source would actually adopt — the fact worth acting
	// on, rather than a blanket rule that filters are mandatory.
	repos, err := cat.ListAllRepositories(ctx, 500)
	latency := time.Since(started)

	switch {
	case err == nil && !spec.Filtered && len(repos) > 1:
		return Step{Name: "can list repositories", Status: StatusWarning, Latency: latency,
			Detail: fmt.Sprintf("%d repositories on this registry, and no repositoryFilters", len(repos)),
			Hint: "every one of them will be scanned. Correct if this registry is " +
				"dedicated to this product; on a shared registry add " +
				"discovery.repositoryFilters.include to scope it"}

	case err == nil:
		return Step{Name: "can list repositories", Status: StatusOK, Latency: latency,
			Detail: fmt.Sprintf("%d repositories visible", len(repos))}

	case registry.ClassOf(err) == registry.ClassAuth:
		// The most common failure for a source that names no repositories, and
		// the one whose raw error is least informative.
		return Step{Name: "can list repositories", Status: StatusFailed, Latency: latency,
			Detail: "the registry refused to list its repositories",
			Hint: "this credential is scoped to pulling named repositories, not to " +
				"enumerating the registry — common for vendor-issued credentials. " +
				"Name them under `repositories:` instead"}

	case errors.Is(err, registry.ErrNotFound):
		return Step{Name: "can list repositories", Status: StatusFailed, Latency: latency,
			Detail: "the registry does not implement /v2/_catalog",
			Hint:   "name the repositories under `repositories:` instead"}

	default:
		return Step{Name: "can list repositories", Status: StatusFailed, Latency: latency,
			Detail: err.Error()}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func skipped(name, why string) Step {
	return Step{Name: name, Status: StatusSkipped, Detail: why}
}

func readStepName(role string) string {
	if role == string(product.RoleTarget) {
		return "readable"
	}
	return "can list tags"
}

// repositoryOrPlaceholder gives the client a path when the source has none.
//
// A source using repositoryDiscovery names no repository, but the client needs
// one to build a URL. Only /v2/ is requested with it, which ignores the path.
func repositoryOrPlaceholder(repo string) string {
	if repo == "" {
		return "_"
	}
	return repo
}

func credentialDescription(spec repoSpec) string {
	if spec.Anonymous {
		return "anonymous access accepted"
	}
	return "credential accepted"
}

func authHint(spec repoSpec) string {
	if spec.Anonymous {
		return "the registry requires authentication; add a credentialsRef"
	}
	return "the username or password in secret " + spec.Credentials.SecretName +
		" was rejected. Check for a trailing newline in the file — it is the " +
		"most common cause and the hardest to see"
}

func reachabilityHint(err error, spec repoSpec) string {
	switch {
	case errors.Is(err, registry.ErrTimeout):
		return "the host accepted a connection but did not answer in time — " +
			"check a proxy or firewall between here and " + spec.Registry
	case registry.ClassOf(err) == registry.ClassUnavailable:
		return "check the registry host is correct and reachable from this cluster; " +
			"if it uses a private CA, set network.caBundleRef"
	default:
		return ""
	}
}

func isLoopback(host string) bool {
	h := host
	if i := lastColon(h); i > 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "[::1]" || h == "::1"
}

func lastColon(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return i
		}
		if s[i] == ']' {
			return -1
		}
	}
	return -1
}

// probeTransport uses a short, shallow retry policy.
//
// Preflight is interactive: an operator waiting on the answer wants it in
// seconds. The production schedule — eight attempts with backoff — would make
// an unreachable host take two minutes to report, which is the difference
// between a useful diagnostic and one nobody runs.
func probeTransport(cfg registry.ClientConfig) transport.Config {
	return transport.Config{
		Username:              cfg.Username,
		Password:              cfg.Password,
		RequestsPerSecond:     cfg.RequestsPerSecond,
		Burst:                 cfg.Burst,
		MaxConnections:        4,
		CABundle:              cfg.CABundle,
		HTTPSProxy:            cfg.HTTPSProxy,
		NoProxy:               cfg.NoProxy,
		DirectConnect:         cfg.DirectConnect,
		InsecureSkipVerify:    cfg.InsecureSkipVerify,
		ConnectTimeout:        5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		UserAgent:             cfg.UserAgent,
		ForceHTTP1:            true,
		MaxRetryAttempts:      2,
		RetryBaseDelay:        200 * time.Millisecond,
		RetryMaxDelay:         time.Second,
	}
}

// applyNetwork merges the repository's network settings over the product's.
func applyNetwork(
	cfg *registry.ClientConfig, p *product.Product, override *product.Network,
	secrets *product.SecretResolver,
) error {
	networks := []product.Network{p.Spec.Network}
	if override != nil {
		networks = append(networks, *override)
	}

	for _, n := range networks {
		if n.CABundleRef != nil {
			key := n.CABundleRef.Key
			if key == "" {
				key = product.DefaultCABundleKey
			}
			bundle, err := secrets.Value(n.CABundleRef.SecretName, key)
			if err != nil {
				return fmt.Errorf("caBundleRef: %w", err)
			}
			cfg.CABundle = []byte(bundle.Reveal())
		}
		if n.Proxy != nil {
			switch {
			case n.Proxy.Direct:
				// Clears any inherited proxy. Without this the only way to say
				// "everything through the corporate proxy except this one
				// registry" is to repeat the host in noProxy at every level.
				cfg.HTTPSProxy = ""
				cfg.NoProxy = nil
				cfg.DirectConnect = true
			case n.Proxy.HTTPSProxy != "":
				cfg.HTTPSProxy = n.Proxy.HTTPSProxy
				cfg.DirectConnect = false
			}
			if len(n.Proxy.NoProxy) > 0 && !n.Proxy.Direct {
				cfg.NoProxy = n.Proxy.NoProxy
			}
		}
		if n.TLS.SetsSkipVerify() {
			cfg.InsecureSkipVerify = n.TLS.SkipsVerify()
		}
		if d := time.Duration(n.Timeouts.Connect); d > 0 {
			cfg.ConnectTimeout = d
		}
		if d := time.Duration(n.Timeouts.ResponseHeader); d > 0 {
			cfg.ResponseHeaderTimeout = d
		}
	}
	return nil
}
