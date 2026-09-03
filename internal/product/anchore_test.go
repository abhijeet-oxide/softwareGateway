package product

import "testing"

// The rule that is not cosmetic: a product naming its own Anchore must name its
// own credential, or the deployment's would be sent to whatever host it named.
func TestAnchoreEndpointWithoutCredentialsIsRefused(t *testing.T) {
	errs := validateAnchore("spec.anchore", &Anchore{
		Endpoint: "https://customer-anchore.example.com",
	}, nil)
	if len(errs) == 0 {
		t.Fatal("a product naming a different Anchore with no credential of its own was accepted")
	}
	found := false
	for _, e := range errs {
		if e.Field == "spec.anchore.credentialsRef" {
			found = true
		}
	}
	if !found {
		t.Errorf("the error did not name credentialsRef: %+v", errs)
	}
}

// The reverse is a real configuration: the same Anchore, a different account or
// a different service account.
func TestAnchoreAccountOnlyOverrideIsAccepted(t *testing.T) {
	if errs := validateAnchore("spec.anchore", &Anchore{Account: "network-eng"}, nil); len(errs) != 0 {
		t.Errorf("an account-only override was rejected: %+v", errs)
	}
	if errs := validateAnchore("spec.anchore", &Anchore{
		CredentialsRef: &CredentialsRef{SecretName: "anchore-neteng"},
	}, nil); len(errs) != 0 {
		t.Errorf("a credential-only override was rejected: %+v", errs)
	}
}

// A block that overrides nothing reads as configuration and is not.
func TestEmptyAnchoreBlockIsReported(t *testing.T) {
	if errs := validateAnchore("spec.anchore", &Anchore{}, nil); len(errs) == 0 {
		t.Error("an empty override block was accepted silently")
	}
	if errs := validateAnchore("spec.anchore", nil, nil); len(errs) != 0 {
		t.Errorf("an absent block was reported: %+v", errs)
	}
}

// Each field falls back independently, so changing one thing costs one line.
func TestAnchoreFieldsFallBackIndependently(t *testing.T) {
	p := Product{Spec: Spec{Anchore: &Anchore{Account: "network-eng"}}}
	if got := p.AnchoreEndpoint("https://anchore.example.com"); got != "https://anchore.example.com" {
		t.Errorf("endpoint = %q, want the deployment's", got)
	}
	if got := p.AnchoreAccount("default"); got != "network-eng" {
		t.Errorf("account = %q, want the product's", got)
	}
	if _, ok := p.AnchoreCredentials(); ok {
		t.Error("a product with no credentialsRef reported one")
	}

	own := Product{Spec: Spec{Anchore: &Anchore{
		Endpoint:       "https://customer.example.com",
		CredentialsRef: &CredentialsRef{SecretName: "customer-anchore"},
	}}}
	if got := own.AnchoreEndpoint("https://anchore.example.com"); got != "https://customer.example.com" {
		t.Errorf("endpoint = %q, want the product's", got)
	}
	if got := own.AnchoreAccount("default"); got != "default" {
		t.Errorf("account = %q, want the deployment's", got)
	}
	creds, ok := own.AnchoreCredentials()
	if !ok || creds.SecretName != "customer-anchore" {
		t.Errorf("credentials = %+v, %v", creds, ok)
	}
	// The keys default exactly as a source's or a target's do - the whole
	// point of reusing CredentialsRef.
	if creds.UsernameKeyOrDefault() != "username" || creds.PasswordKeyOrDefault() != "password" {
		t.Error("the credential keys do not default like every other credentialsRef")
	}
}

// A product with no Anchore block at all is the common case and inherits
// everything.
func TestNoAnchoreBlockInheritsEverything(t *testing.T) {
	p := Product{}
	if got := p.AnchoreEndpoint("https://anchore.example.com"); got != "https://anchore.example.com" {
		t.Errorf("endpoint = %q", got)
	}
	if got := p.AnchoreAccount("acct"); got != "acct" {
		t.Errorf("account = %q", got)
	}
}
