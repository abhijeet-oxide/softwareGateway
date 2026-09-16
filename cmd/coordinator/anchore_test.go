package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/config"
	"github.com/abhijeet-oxide/softwareGateway/internal/product"
)

// A CREDENTIAL THAT IS NOT IN GIT MUST NOT STOP THE COORDINATOR.
//
// `coordinator.security.anchore.secretName` names a projected Secret. The NAME
// is configuration and belongs in Git; the VALUE is a credential and never
// does - so every checkout that is not a running deployment has the name and
// not the file behind it. That includes CI, which boots the Coordinator
// against the committed config to prove it starts (the smoke test in
// .github/workflows/ci.yml).
//
// So the absence of that file is the NORMAL case for this repository, not an
// error, and it must degrade to "Anchore is not available" rather than to a
// Coordinator that will not start. Anything else makes an operator's own
// scanner configuration unmergeable.
//
// This is a unit test on the resolution rather than a booted Coordinator
// because it is the resolution that decides: anchoreTuning is the only thing
// between the configured name and a credential nobody provisioned.
func TestAnchoreWithNoCredentialPresentDoesNotStopStartup(t *testing.T) {
	// A secrets directory that exists and holds nothing, which is exactly what
	// config/secrets/local is in a fresh checkout.
	dir := t.TempDir()
	resolver := product.NewSecretResolver(dir)

	tuning := anchoreTuning(config.AnchoreConfig{
		Endpoint:    "https://anchore.example.internal",
		SecretName:  "anchore-credentials",
		UsernameKey: "username",
		PasswordKey: "password",
	}, resolver, slog.New(slog.DiscardHandler))

	// Empty, because Anchore cannot be used without a credential - and empty
	// rather than a half-built client, which would fail later and further from
	// the cause.
	if tuning.Endpoint != "" || tuning.Username != "" || tuning.Password != "" {
		t.Errorf("a missing credential produced a usable Anchore tuning: %+v", tuning)
	}
}

// The same when the secrets directory does not exist at all, which is the
// other shape a checkout takes.
func TestAnchoreWithNoSecretsDirectoryDoesNotStopStartup(t *testing.T) {
	resolver := product.NewSecretResolver(filepath.Join(t.TempDir(), "absent"))

	tuning := anchoreTuning(config.AnchoreConfig{
		Endpoint:   "https://anchore.example.internal",
		SecretName: "anchore-credentials",
	}, resolver, slog.New(slog.DiscardHandler))

	if tuning.Endpoint != "" {
		t.Errorf("a missing secrets directory produced a usable Anchore tuning: %+v", tuning)
	}
}

// AND THE STANZA ITSELF MUST LOAD.
//
// The loader refuses configuration it does not recognise rather than ignoring
// it, so a mis-spelled path is a startup failure - which is the right
// behaviour and makes the exact spelling load-bearing. It is
// `coordinator.security.anchore`, not a top-level `security`, and this pins it
// so a reader who copies the stanza from here gets the one that works.
func TestTheAnchoreStanzaLoadsWhereTheSchemaPutsIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`
apiVersion: softwaregateway.io/v1alpha1
kind: SystemConfig
configDir: `+dir+`
coordinator:
  security:
    anchore:
      endpoint: https://anchore.example.internal
      secretName: anchore-credentials
      usernameKey: username
      passwordKey: password
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load a configuration carrying an anchore stanza: %v", err)
	}
	got := cfg.Coordinator.Security.Anchore
	if got.Endpoint != "https://anchore.example.internal" {
		t.Errorf("anchore endpoint = %q; the stanza did not land where the "+
			"schema expects it", got.Endpoint)
	}
	if got.SecretName != "anchore-credentials" {
		t.Errorf("anchore secretName = %q, want anchore-credentials", got.SecretName)
	}
}
