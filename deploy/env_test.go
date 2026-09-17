package deploy

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// ZITADEL's default password policy, from its own defaults.yaml.
//
// Restated here because nothing else in this repository can see it: the policy
// lives inside an image we pull, it is not configured in docker-compose.yml, so
// the defaults apply and a password that violates them is rejected at first
// boot.
//
// If compose ever sets ZITADEL_DEFAULTINSTANCE_PASSWORDCOMPLEXITYPOLICY_*,
// these constants have to move with it.
const (
	zitadelMinPasswordLength = 8
	zitadelPolicySource      = "ZITADEL DefaultInstance.PasswordComplexityPolicy"
)

// passwordVars are the variables in .env.example whose values become ZITADEL
// account passwords.
//
// Not every password in the file: POSTGRES_PASSWORD is a database credential
// and Postgres has no complexity policy, so holding it to one would be
// inventing a rule.
var passwordVars = []string{
	"ZITADEL_ROOT_PASSWORD",
	"BOOTSTRAP_ADMIN_PASSWORD",
}

// TestEveryPlaceholderPasswordSatisfiesZitadelsPolicy is the test for an
// afternoon somebody spent watching nothing happen.
//
// # The incident
//
// `.env.example` shipped `ZITADEL_ROOT_PASSWORD=change-me-root`. No upper case,
// no digit, no symbol. The documented first step of bringing the stack up is to
// copy that file to `.env`, so anybody who followed it got a password ZITADEL's
// default policy rejects.
//
// What makes it worth a test rather than a fix is HOW it fails. The account is
// created by the first-boot migration `03_default_instance`, which fails with
// InvalidArgument out of policy_password_complexity_model.go. ZITADEL exits,
// `restart: unless-stopped` brings it back, and it fails again - forever. Every
// service that waits on `zitadel: condition: service_healthy` - the
// Coordinator, the Worker, the interface - sits in `created` with nothing at
// all wrong in its own logs, and the stack simply never comes up.
//
// The compose file's own inline default is `RootPassw0rd!`, which is valid. So
// the stack worked for anybody who did NOT create a .env, and hung for everyone
// who followed the instructions. That divergence is what this test closes.
func TestEveryPlaceholderPasswordSatisfiesZitadelsPolicy(t *testing.T) {
	env := readEnvExample(t)

	for _, name := range passwordVars {
		value, ok := env[name]
		if !ok {
			t.Errorf("%s is not set in .env.example, but it is required to bring "+
				"the stack up", name)
			continue
		}
		if why := violatesZitadelPolicy(value); why != "" {
			t.Errorf("%s=%q %s.\n\n"+
				"%s rejects it, so the first-boot migration cannot create the\n"+
				"account. ZITADEL then exits and is restarted forever, and every\n"+
				"service waiting on its health check sits in `created` with nothing\n"+
				"wrong in its own logs - the stack never comes up and never says why.\n\n"+
				"Use a placeholder that is still obviously a placeholder and still\n"+
				"satisfies the policy - `Change-Me-Root-1!` reads as one and passes.",
				name, value, why, zitadelPolicySource)
		}
	}
}

// violatesZitadelPolicy returns why a password is rejected, or "" if it is not.
func violatesZitadelPolicy(pw string) string {
	var missing []string
	if len([]rune(pw)) < zitadelMinPasswordLength {
		missing = append(missing,
			fmt.Sprintf("is shorter than %d characters", zitadelMinPasswordLength))
	}
	for _, c := range []struct {
		what string
		has  func(rune) bool
	}{
		{"upper case letter", unicode.IsUpper},
		{"lower case letter", unicode.IsLower},
		{"digit", unicode.IsDigit},
		{"symbol", func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsSpace(r)
		}},
	} {
		if !strings.ContainsFunc(pw, c.has) {
			missing = append(missing, "has no "+c.what)
		}
	}
	return strings.Join(missing, ", and ")
}

// envValue is an uncommented assignment, with its value. Distinct from
// envAssignment in deploy_test.go, which matches the NAMES of both live and
// commented-out lines because that check is about documentation. This one needs
// the value of a line that is actually in effect.
var envValue = regexp.MustCompile(`^([A-Z0-9_]+)=(.*)$`)

// readEnvExample parses the uncommented assignments in .env.example.
func readEnvExample(t *testing.T) map[string]string {
	t.Helper()

	path := filepath.Join(repoRoot, ".env.example")
	f, err := os.Open(path) //nolint:gosec // a fixed path in this repository
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := envValue.FindStringSubmatch(line); m != nil {
			out[m[1]] = strings.Trim(m[2], `"'`)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatalf("%s parsed to nothing, so this test is checking an empty map", path)
	}
	return out
}
