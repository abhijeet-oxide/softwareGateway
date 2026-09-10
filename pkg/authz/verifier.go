package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Verifier validates bearer tokens against the issuer's public keys.
//
// The keys come from the issuer's JWKS endpoint, which is PUBLIC: no client
// id, no secret, no service account. go-oidc caches them and refetches on an
// unknown key id, so validation is offline after the first call. A brief
// issuer outage therefore cannot reject an already-authenticated request.
type Verifier struct {
	verifier *oidc.IDTokenVerifier
	// productRolePrefix distinguishes the two role tiers. A role key that
	// namespaces a product ("software-01:product-owner") is product tier;
	// anything else is org tier.
	orgRolePrefix string

	// tenant is the deployment's own. See Config.Tenant.
	tenant string

	// How the key set is actually fetched, kept so Health can ask the same
	// question Verify silently depends on. Verify only notices an unreachable
	// issuer once the cached keys age out and a token arrives that needs new
	// ones, which is hours after the network broke and nowhere near the change
	// that caused it.
	keysURL string
	client  *http.Client
}

// Config configures a Verifier.
type Config struct {
	// Issuer is the OIDC issuer URL, e.g. http://localhost:8090.
	Issuer string
	// Audience is the client or project id the token must be issued for.
	// Empty skips the check, which is only correct behind a trusted gateway.
	Audience string
	// Tenant is the ONE tenant this deployment serves. A token asserting any
	// other is refused, whatever roles it carries.
	//
	// This is a boundary, not a preference. An issuer that hosts more than one
	// organization signs tokens for all of them with the same keys, so
	// signature, issuer and expiry - everything else checked here - are
	// satisfied by a token from a tenant this deployment has nothing to do
	// with. Without this the only thing standing between that token and the
	// data is whether its bearer happens to hold a role of the same NAME,
	// which is a coincidence rather than a control: `org-admin` granted in
	// somebody else's organization is spelled exactly like `org-admin` here.
	//
	// Empty disables the check and is only correct where the issuer serves
	// exactly one tenant and always will.
	Tenant string
	// OrgRolePrefix marks tenant-wide roles. Default "org-".
	OrgRolePrefix string
	// HTTPClient reaches the issuer. Supply one with your CA pool when the
	// issuer is behind a corporate proxy or uses a private certificate.
	HTTPClient *http.Client
	// DiscoveryURL is where the discovery document and JWKS are actually
	// fetched from, when that address differs from Issuer.
	//
	// This is the normal case in a container network: the token says
	// `iss: http://localhost:8090` because that is where the BROWSER reached
	// the issuer, but this service must fetch the keys over the internal
	// network as http://zitadel:8080. The token is still validated against
	// Issuer; only the fetch address differs.
	DiscoveryURL string
	// SkipIssuerCheck stops the `iss` claim being compared at all. Prefer
	// DiscoveryURL, which keeps the check. Neither skips signature
	// verification.
	SkipIssuerCheck bool
	// HostHeader overrides the Host sent when fetching discovery and keys.
	//
	// Multi-tenant issuers - ZITADEL among them - select the instance from the
	// Host header and return 404 for one they do not recognise. Reached over
	// an internal network as `zitadel:8080` such a server refuses a request it
	// would answer on its public name, so the Host must be stated explicitly.
	// Without this the symptom is a 404 from a service that is healthy and
	// serving, which is a genuinely misleading half hour.
	HostHeader string
}

// NewVerifier discovers the issuer and prepares token validation.
//
// It performs one network call, to the discovery document, and fails fast if
// the issuer is unreachable: a service that starts with a broken auth
// configuration and only discovers it on the first request is a service that
// looks healthy while rejecting everyone.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("authz: Issuer is required")
	}
	client := cfg.HTTPClient
	if cfg.HostHeader != "" {
		base := http.DefaultTransport
		if client != nil && client.Transport != nil {
			base = client.Transport
		}
		c := *defaultHTTPClient()
		if client != nil {
			c = *client
		}
		c.Transport = hostRewriter{host: cfg.HostHeader, base: base}
		client = &c
	}
	if client != nil {
		ctx = oidc.ClientContext(ctx, client)
	}
	issuer := strings.TrimRight(cfg.Issuer, "/")

	// Where keys are actually fetched from.
	//
	// oidc.NewProvider's InsecureIssuerURLContext only redirects DISCOVERY.
	// The key set is then built from the jwks_uri INSIDE that document, which
	// is the issuer's public address - unreachable from inside a container
	// network. So the discovery document is read here, and its jwks_uri is
	// re-pointed at the address this service can actually reach. Everything
	// else about verification is unchanged: the signature is still checked
	// against the issuer's real keys and `iss` is still compared to Issuer.
	keysURL := ""
	if cfg.DiscoveryURL != "" {
		base := strings.TrimRight(cfg.DiscoveryURL, "/")
		doc, err := fetchDiscovery(ctx, client, base)
		if err != nil {
			return nil, fmt.Errorf("authz: discover issuer at %q: %w", base, err)
		}
		if doc.Issuer != issuer {
			return nil, fmt.Errorf("authz: issuer mismatch: token issuer configured as %q but %s reports %q",
				issuer, base, doc.Issuer)
		}
		keysURL, err = rehost(doc.JWKSURI, base)
		if err != nil {
			return nil, fmt.Errorf("authz: jwks_uri %q: %w", doc.JWKSURI, err)
		}
	} else {
		doc, err := fetchDiscovery(ctx, client, issuer)
		if err != nil {
			return nil, fmt.Errorf("authz: discover issuer at %q: %w", issuer, err)
		}
		keysURL = doc.JWKSURI
	}

	keySet := oidc.NewRemoteKeySet(ctx, keysURL)

	prefix := cfg.OrgRolePrefix
	if prefix == "" {
		prefix = "org-"
	}
	return &Verifier{
		verifier: oidc.NewVerifier(issuer, keySet, &oidc.Config{
			ClientID:          cfg.Audience,
			SkipClientIDCheck: cfg.Audience == "",
			SkipIssuerCheck:   cfg.SkipIssuerCheck,
		}),
		orgRolePrefix: prefix,
		tenant:        cfg.Tenant,
		keysURL:       keysURL,
		client:        client,
	}, nil
}

// Health reports whether the issuer's signing keys are still reachable.
//
// DIAGNOSTIC ONLY, and that is the whole point of where it is registered. A
// remote key set is cached, so an issuer that goes away breaks nothing for
// some time and then breaks everything at once when a key rotates. Making
// this gate readiness would do the opposite damage: a brief blip at the
// identity provider would deregister every replica of a service that is
// still verifying tokens perfectly well from cache.
//
// It fetches the SAME url the verifier fetches, through the SAME client, so a
// wrong DiscoveryURL, a Host header the issuer does not recognise or a
// proxy in the way all show up here rather than in a token failure hours later.
func (v *Verifier) Health(ctx context.Context) error {
	if v == nil || v.keysURL == "" {
		return fmt.Errorf("no key set configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.keysURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", v.keysURL, resp.Status)
	}
	var keys struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return fmt.Errorf("%s answered something other than a key set: %w", v.keysURL, err)
	}
	// An empty key set validates nothing, and answers 200 while doing it.
	if len(keys.Keys) == 0 {
		return fmt.Errorf("%s published no signing keys", v.keysURL)
	}
	return nil
}

// KeysURL is where this verifier fetches signing keys. For diagnostics.
func (v *Verifier) KeysURL() string { return v.keysURL }

// zitadelClaims is the subset of the token this package reads.
//
// ZITADEL emits roles under `urn:zitadel:iam:org:project:<projectID>:roles`,
// whose value is {roleKey: {orgID: orgDomain}}. The project id is opaque, so
// the ROLE KEY carries the product (see splitProductRole) and the org name is
// read off the value rather than needing a second lookup.
type zitadelClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	OrgName string `json:"urn:zitadel:iam:user:resourceowner:name"`
}

// Verify checks the token's signature, issuer, audience and expiry, and
// decodes it into an Identity.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("authz: %w", err)
	}
	var all map[string]json.RawMessage
	if err := tok.Claims(&all); err != nil {
		return Identity{}, fmt.Errorf("authz: decode claims: %w", err)
	}
	var known zitadelClaims
	_ = tok.Claims(&known)

	id := Identity{
		Subject:  known.Subject,
		Tenant:   known.OrgName,
		Email:    known.Email,
		Name:     known.Name,
		Products: map[string][]string{},
		Method:   "oidc",
	}
	if id.Subject == "" {
		id.Subject = tok.Subject
	}

	readRoles(all, &id)

	/* THE TENANT BOUNDARY, checked once, here, before any of this reaches a
	 * permission model that would compare role NAMES across organizations.
	 *
	 * Refused rather than downgraded to an identity holding nothing: a caller
	 * from another tenant is not a person with no permissions here, they are a
	 * caller this deployment has no relationship with, and the two want
	 * different answers and different log lines.
	 *
	 * A token that asserts NO tenant is refused by the same rule. It is the
	 * shape a caller has when the sign-in did not ask for the claim that
	 * carries it (`urn:zitadel:iam:user:resourceowner`), and "cannot tell" is
	 * not "belongs here" - a boundary that waves through what it cannot read
	 * is not one. */
	if err := wrongTenant(id.Tenant, v.tenant); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// wrongTenant reports a token that belongs to another tenant, or to none.
//
// Its own function so it can be tested without minting a signed token, and
// because the empty cases are the whole of it: an unset deployment tenant
// disables the boundary, and an unasserted token tenant must never be read as
// agreement.
func wrongTenant(have, want string) error {
	if want == "" {
		return nil
	}
	if strings.EqualFold(have, want) {
		return nil
	}
	if have == "" {
		return fmt.Errorf("authz: token asserts no tenant and this deployment serves %q "+
			"(the sign-in must request the urn:zitadel:iam:user:resourceowner scope)", want)
	}
	return fmt.Errorf("authz: token is for tenant %q, this deployment serves %q", have, want)
}

// readRoles decodes every role claim in the token onto the identity.
//
// ZITADEL emits the same role under MORE THAN ONE claim: one per project
// (`urn:zitadel:iam:org:project:<projectID>:roles`) and one flattened across
// all of them (`urn:zitadel:iam:org:project:roles`). Both match the shape this
// reads, which is deliberate - a deployment may have either - so the same
// grant arrives twice and has to be counted once. Left undeduplicated it
// reached the policy engine twice and reached the Settings page as
// "org-admin, org-admin", which reads as a misconfigured grant rather than as
// one role named twice.
func readRoles(all map[string]json.RawMessage, id *Identity) {
	seen := map[string]bool{}
	for claim, raw := range all {
		if !strings.HasPrefix(claim, "urn:zitadel:iam:org:project:") || !strings.HasSuffix(claim, ":roles") {
			continue
		}
		var roles map[string]map[string]string
		if err := json.Unmarshal(raw, &roles); err != nil {
			continue
		}
		for key, orgs := range roles {
			// The tenant is on the role itself, so a token that somehow
			// carried a grant from another org cannot be read as this one's.
			for _, domain := range orgs {
				if id.Tenant == "" {
					id.Tenant = strings.SplitN(domain, ".", 2)[0]
				}
				break
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			if product, role, ok := splitProductRole(key); ok {
				id.Products[product] = append(id.Products[product], role)
			} else {
				id.OrgRoles = append(id.OrgRoles, key)
			}
		}
	}
}

// BearerToken extracts the token from an Authorization header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// defaultHTTPClient is used when a caller supplies none.
func defaultHTTPClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

// hostRewriter sets the Host header on every outbound request. See
// Config.HostHeader.
type hostRewriter struct {
	host string
	base http.RoundTripper
}

func (h hostRewriter) RoundTrip(r *http.Request) (*http.Response, error) {
	// Clone: a RoundTripper must not modify the request it is given.
	r2 := r.Clone(r.Context())
	r2.Host = h.host
	return h.base.RoundTrip(r2)
}

// discoveryDoc is the part of .well-known/openid-configuration we need.
type discoveryDoc struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}

func fetchDiscovery(ctx context.Context, c *http.Client, base string) (discoveryDoc, error) {
	var doc discoveryDoc
	if c == nil {
		c = defaultHTTPClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		base+"/.well-known/openid-configuration", nil)
	if err != nil {
		return doc, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return doc, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return doc, fmt.Errorf("%s", resp.Status)
	}
	return doc, json.NewDecoder(resp.Body).Decode(&doc)
}

// rehost re-points a URL at the scheme and authority of base, keeping its path.
func rehost(raw, base string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Scheme, u.Host = b.Scheme, b.Host
	return u.String(), nil
}
