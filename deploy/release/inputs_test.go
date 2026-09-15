package release_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/deploy/release"
)

// git runs a command in the throwaway repository, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestTheInputsDigestMovesOnlyForWhatChanged is the property the rerun decision
// rests on: a digest that moved when nothing changed rebuilds everything
// forever, and one that held still when something changed publishes a tag whose
// bits do not match its source.
func TestTheInputsDigestMovesOnlyForWhatChanged(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")

	for _, p := range []string{"cmd/coordinator", "cmd/worker", "internal", "pkg",
		"deploy/go", "deploy/certs", "deploy/web", "deploy/npm", "web",
		"deploy/build"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p, "f"), []byte("one"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"go.mod", "go.sum",
		"deploy/build/Dockerfile.coordinator", "deploy/build/Dockerfile.worker",
		"deploy/build/Dockerfile.web"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("one"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "one")

	// InputsDigest shells out to git in the process's own directory.
	t.Chdir(dir)

	before := map[string]string{}
	for _, c := range release.Components() {
		d, err := release.InputsDigest("HEAD", c)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		before[c.Name] = d
	}

	// A commit that changes nothing those paths hold.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "docs only")
	for _, c := range release.Components() {
		d, _ := release.InputsDigest("HEAD", c)
		if d != before[c.Name] {
			t.Errorf("%s moved on a commit that touched none of its inputs: every release would rebuild it", c.Name)
		}
	}

	// A change to web/ alone.
	if err := os.WriteFile(filepath.Join(dir, "web", "f"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "web")
	after := map[string]string{}
	for _, c := range release.Components() {
		after[c.Name], _ = release.InputsDigest("HEAD", c)
	}
	if after["web"] == before["web"] {
		t.Error("web did not move when web/ changed: the release would reuse an image that misses the change")
	}
	for _, name := range []string{"coordinator", "worker"} {
		if after[name] != before[name] {
			t.Errorf("%s moved when only web/ changed", name)
		}
	}

	// internal/ is shared, and both Go binaries must move with it.
	if err := os.WriteFile(filepath.Join(dir, "internal", "f"), []byte("three"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", "internal")
	for _, name := range []string{"coordinator", "worker"} {
		d, _ := release.InputsDigest("HEAD", componentNamed(t, name))
		if d == after[name] {
			t.Errorf("%s did not move when internal/ changed, and it is built from it", name)
		}
	}
}

func componentNamed(t *testing.T, name string) release.Component {
	t.Helper()
	for _, c := range release.Components() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no component named %q", name)
	return release.Component{}
}
