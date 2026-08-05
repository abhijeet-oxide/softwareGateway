package product

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretNeverLeaksThroughFormatting(t *testing.T) {
	// The point of the type: a careless %v or slog.Any cannot leak a
	// credential. Every formatting path must yield the redaction.
	s := NewSecret("hunter2")

	for _, got := range []string{
		s.String(),
		s.GoString(),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprint(s),
	} {
		if strings.Contains(got, "hunter2") {
			t.Fatalf("secret leaked: %q", got)
		}
		if !strings.Contains(got, Redacted) {
			t.Fatalf("expected redaction, got %q", got)
		}
	}
}

func TestSecretNeverLeaksThroughJSON(t *testing.T) {
	// Guards API responses and audit records.
	b, err := json.Marshal(struct {
		User string `json:"user"`
		Pass Secret `json:"pass"`
	}{"admin", NewSecret("hunter2")})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("secret leaked through JSON: %s", b)
	}
}

func TestSecretNeverLeaksThroughSlog(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	creds := Credentials{Username: "admin", Password: NewSecret("hunter2")}
	logger.Info("connecting", slog.Any("credentials", creds))

	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("secret leaked through slog: %s", buf.String())
	}
	if !strings.Contains(buf.String(), Redacted) {
		t.Fatalf("expected redaction in log output: %s", buf.String())
	}
}

func TestRevealReturnsTheValue(t *testing.T) {
	if got := NewSecret("hunter2").Reveal(); got != "hunter2" {
		t.Fatalf("got %q", got)
	}
}

func TestResolverReadsAndTrims(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Trailing newline is near-universal in mounted files and hand-created dev
	// fixtures; a credential with a stray \n fails auth in a way that is
	// genuinely hard to diagnose.
	if err := os.WriteFile(filepath.Join(dir, "creds", "password"), []byte("s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := NewSecretResolver(dir).Value("creds", "password")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reveal() != "s3cret" {
		t.Fatalf("got %q, want trimmed value", got.Reveal())
	}
}

func TestResolverRejectsPathTraversal(t *testing.T) {
	r := NewSecretResolver(t.TempDir())
	for _, tc := range []struct{ secret, key string }{
		{"../../etc", "passwd"},
		{"creds", "../../../etc/passwd"},
		{`..\..\windows`, "system32"},
	} {
		if _, err := r.Value(tc.secret, tc.key); err == nil {
			t.Fatalf("expected rejection for %q/%q", tc.secret, tc.key)
		}
		if r.Exists(tc.secret, tc.key) {
			t.Fatalf("Exists must also reject %q/%q", tc.secret, tc.key)
		}
	}
}

func TestResolverMissingSecretNamesThePath(t *testing.T) {
	r := NewSecretResolver("/etc/softwaregateway/secrets")
	_, err := r.Value("absent", "password")
	if err == nil {
		t.Fatal("expected an error")
	}
	// The operator needs to know where we looked.
	if !strings.Contains(err.Error(), "/etc/softwaregateway/secrets/absent/password") {
		t.Fatalf("error should name the expected path, got %v", err)
	}
}

func TestValidationChecksSecretExistence(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vendor-a-registry"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor-a-registry", "username"), []byte("u"), 0o600); err != nil {
		t.Fatal(err)
	}
	// password deliberately absent

	p := valid()
	p.Spec.Sources[0].Anonymous = false
	p.Spec.Sources[0].CredentialsRef = &CredentialsRef{SecretName: "vendor-a-registry"}

	err := p.Validate(NewSecretResolver(dir))
	e := requireField(t, err, "spec.sources[0].credentialsRef")
	if !strings.Contains(e.Message, "password") {
		t.Fatalf("should name the missing key, got %q", e.Message)
	}
}

func TestOfflineValidationSkipsSecretChecks(t *testing.T) {
	// `transferctl config validate` runs in CI where cluster Secrets are
	// legitimately unavailable; a nil resolver must not produce false errors.
	p := valid()
	p.Spec.Sources[0].Anonymous = false
	p.Spec.Sources[0].CredentialsRef = &CredentialsRef{SecretName: "vendor-a-registry"}

	if err := p.Validate(nil); err != nil {
		t.Fatalf("offline validation should not check secret existence: %v", err)
	}
}
