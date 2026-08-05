package product

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Secret holds a credential value that must never be printed.
//
// See docs/design/02-configuration.md section 5.5. Secret values must not
// appear in config dumps, logs, API responses, audit records or error
// messages. Rather than relying on every call site to remember that, this type
// makes leaking one impossible through the ordinary paths: String, GoString,
// Format, MarshalJSON and LogValue all return the redaction.
//
// That means even a careless slog.Any("cfg", cfg) or fmt.Sprintf("%v", cfg)
// is safe. Reading the actual value requires calling Reveal, which is easy to
// spot in review.
type Secret struct {
	value string
}

// NewSecret wraps a credential value.
func NewSecret(v string) Secret { return Secret{value: v} }

// Redacted is what every formatting path yields.
const Redacted = "[REDACTED]"

// Reveal returns the underlying value. Call sites are deliberately greppable.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.value == "" }

func (s Secret) String() string   { return Redacted }
func (s Secret) GoString() string { return Redacted }

// Format captures %v, %s, %q, %#v and every other verb.
func (s Secret) Format(f fmt.State, verb rune) {
	_, _ = f.Write([]byte(Redacted))
	_ = verb
}

// MarshalJSON prevents a secret reaching an API response or an audit record.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// LogValue is what slog uses, so structured logs are safe too.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// Credentials is a resolved registry username and password.
type Credentials struct {
	Username string
	Password Secret
}

// SecretResolver reads secrets from projected volume mounts.
//
// Volume mounts rather than the Kubernetes API: no client-go, no RBAC (in
// particular no cluster-wide Secret read permission), no API-server load, and
// the same code path works in local development against a plain directory.
// See docs/design/02 section 3.
type SecretResolver struct {
	dir string
}

// NewSecretResolver reads from dir, laid out as <dir>/<secretName>/<key>.
func NewSecretResolver(dir string) *SecretResolver { return &SecretResolver{dir: dir} }

// Dir returns the root directory.
func (r *SecretResolver) Dir() string { return r.dir }

// Value reads one key from one secret.
func (r *SecretResolver) Value(secretName, key string) (Secret, error) {
	if secretName == "" {
		return Secret{}, fmt.Errorf("secretName is empty")
	}
	if key == "" {
		return Secret{}, fmt.Errorf("key is empty for secret %q", secretName)
	}
	// Reject traversal: a crafted config must not read outside the mount.
	if strings.ContainsAny(secretName, `/\`) || strings.ContainsAny(key, `/\`) {
		return Secret{}, fmt.Errorf("secret %q key %q: path separators are not permitted", secretName, key)
	}

	path := filepath.Join(r.dir, secretName, key)
	b, err := os.ReadFile(path) //nolint:gosec // path is validated above
	if err != nil {
		if os.IsNotExist(err) {
			return Secret{}, fmt.Errorf("secret %q key %q not found at %s", secretName, key, path)
		}
		return Secret{}, fmt.Errorf("read secret %q key %q: %w", secretName, key, err)
	}

	// Trailing newlines are near-universal in mounted files and hand-created
	// dev fixtures, and a credential with a stray \n fails auth in a way that
	// is genuinely hard to diagnose.
	return NewSecret(strings.TrimRight(string(b), "\r\n")), nil
}

// Exists reports whether a secret key is present, without reading it.
// Used by validation so a missing reference is caught at load rather than at
// the first registry call.
func (r *SecretResolver) Exists(secretName, key string) bool {
	if strings.ContainsAny(secretName, `/\`) || strings.ContainsAny(key, `/\`) {
		return false
	}
	info, err := os.Stat(filepath.Join(r.dir, secretName, key))
	return err == nil && !info.IsDir()
}

// Credentials resolves a CredentialsRef.
func (r *SecretResolver) Credentials(ref CredentialsRef) (Credentials, error) {
	user, err := r.Value(ref.SecretName, ref.UsernameKeyOrDefault())
	if err != nil {
		return Credentials{}, err
	}
	pass, err := r.Value(ref.SecretName, ref.PasswordKeyOrDefault())
	if err != nil {
		return Credentials{}, err
	}
	// The username is not secret and appears in logs for diagnostics.
	return Credentials{Username: user.Reveal(), Password: pass}, nil
}
