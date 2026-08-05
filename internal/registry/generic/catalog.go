package generic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// maxCatalogResponseBytes bounds one page of `/v2/_catalog`.
const maxCatalogResponseBytes = 8 << 20 // 8 MiB

// Catalog is a REGISTRY-scoped client, as opposed to Repository which is
// scoped to one repository path.
//
// It exists only for `/v2/_catalog`. Separate from Repository for a reason that
// is easy to miss: the catalog endpoint needs the token scope
// `registry:catalog:*`, not `repository:<name>:pull`. Reusing a repository
// client would request the wrong scope, get a token the registry then refuses
// the request with, and produce a 401 that reads like a credentials problem
// rather than a client bug.
type Catalog struct {
	registryHost string
	scheme       string
	client       *http.Client
}

// CatalogConfig describes a registry-scoped client.
type CatalogConfig struct {
	Registry  string
	Transport transport.Config
	PlainHTTP bool
}

// NewCatalog builds a registry-scoped client.
func NewCatalog(cfg CatalogConfig) (*Catalog, error) {
	if cfg.Registry == "" {
		return nil, fmt.Errorf("registry host is required")
	}

	tcfg := cfg.Transport
	tcfg.Registry = cfg.Registry
	// Deliberately no Repository: this client is not scoped to one.
	tcfg.Repository = ""
	tcfg.Scope = "registry:catalog:*"

	client, err := transport.New(tcfg)
	if err != nil {
		return nil, fmt.Errorf("build transport for %s: %w", cfg.Registry, err)
	}

	scheme := "https"
	if cfg.PlainHTTP {
		scheme = "http"
	}
	return &Catalog{registryHost: cfg.Registry, scheme: scheme, client: client}, nil
}

// CatalogFromClientConfig builds a Catalog from the backend-neutral config.
func CatalogFromClientConfig(c registry.ClientConfig) CatalogConfig {
	full := FromClientConfig(c)
	return CatalogConfig{
		Registry:  c.Registry,
		Transport: full.Transport,
		PlainHTTP: c.PlainHTTP,
	}
}

// Registry returns the host.
func (c *Catalog) Registry() string { return c.registryHost }

// ListRepositories returns one page of repository paths plus the next token.
//
// Pagination is the same RFC 8288 Link scheme as tags/list. Registries vary in
// how faithfully they implement it — which is one of the reasons catalog
// enumeration is opt-in rather than the default way to find repositories
// (docs/design/07 §2).
func (c *Catalog) ListRepositories(ctx context.Context, last string, limit int) ([]string, string, error) {
	if limit <= 0 {
		limit = defaultPageSize
	}

	endpoint := c.scheme + "://" + c.registryHost + "/v2/_catalog"
	q := url.Values{}
	q.Set("n", itoa(limit))
	if last != "" {
		q.Set("last", last)
	}
	endpoint += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", c.wrap("list repositories", 0, nil, transportError(err))
	}
	defer closeBody(resp)

	if resp.StatusCode != http.StatusOK {
		// 401/403 here is the common case on a vendor registry: the credential
		// is good for pulling a named repository and not for enumerating the
		// whole registry. Classified so the caller can say that plainly rather
		// than reporting a generic failure.
		return nil, "", c.wrap("list repositories", resp.StatusCode, resp,
			registry.ClassifyStatus(resp.StatusCode))
	}

	var payload struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogResponseBytes)).Decode(&payload); err != nil {
		return nil, "", c.wrap("list repositories", resp.StatusCode, nil, registry.ErrMalformedResponse)
	}

	return payload.Repositories, nextFromLink(resp.Header.Get("Link")), nil
}

// ListAllRepositories pages through the whole catalog, up to max entries.
//
// Bounded because a catalog is registry-wide: on a shared internal registry it
// legitimately contains every other team's repositories, and an unbounded walk
// would page through all of them before the filters got a chance to reject
// them.
func (c *Catalog) ListAllRepositories(ctx context.Context, maxEntries int) ([]string, error) {
	if maxEntries <= 0 {
		maxEntries = 1000
	}

	var all []string
	seen := map[string]bool{}
	last := ""

	const maxPages = 100
	for page := 0; page < maxPages; page++ {
		repos, next, err := c.ListRepositories(ctx, last, defaultPageSize)
		if err != nil {
			return nil, err
		}

		for _, r := range repos {
			r = strings.Trim(r, "/")
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			all = append(all, r)
			if len(all) >= maxEntries {
				// Stop at the cap rather than erroring: a partial catalog is
				// still useful, and the caller reports the truncation.
				return all, nil
			}
		}

		// A cursor that does not advance means the registry is paginating
		// incorrectly; continuing would loop forever.
		if next == "" || next == last {
			return all, nil
		}
		last = next
	}
	return all, nil
}

// Ping verifies the registry answers at /v2/.
func (c *Catalog) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.scheme+"://"+c.registryHost+"/v2/", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return c.wrap("ping", 0, nil, transportError(err))
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusOK {
		return nil
	}
	return c.wrap("ping", resp.StatusCode, resp, registry.ClassifyStatus(resp.StatusCode))
}

func (c *Catalog) wrap(op string, status int, resp *http.Response, err error) error {
	e := &registry.Error{
		Op:         op,
		Repository: c.registryHost,
		StatusCode: status,
		Err:        err,
	}
	if resp != nil {
		e.RetryAfter = retryAfter(resp)
		e.Detail = registryErrorDetail(resp)
	}
	return e
}
