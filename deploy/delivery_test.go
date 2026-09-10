package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/abhijeet-oxide/softwareGateway/deploy/chartstage"
	"github.com/abhijeet-oxide/softwareGateway/deploy/secretsinv"
)

// repoRoot is the repository root, as seen from this package's directory.
const repoRoot = ".."

// TestEveryReferencedSecretIsDeclared is the credential half of the rule
// TestEveryProductHasAnOwner makes about people.
//
// A product names a credential with `credentialsRef.secretName`, and every
// runtime resolves it to <config dir>/secrets/<name>/<key>. Nothing used to say
// which secrets EXIST or where their values come from, so a product could name
// one that no operator would ever create - and the deployment came up healthy,
// loaded the product, marked it invalid for a missing file, and said so in a
// log line nobody was reading.
//
// config/secrets/secrets.yaml is that list, and this is what keeps it honest.
// The failure names the product, the reference and the two lines to add,
// because a test that says "invariant violated" costs the reader the same
// twenty minutes every time.
func TestEveryReferencedSecretIsDeclared(t *testing.T) {
	inv, err := secretsinv.Load(repoRoot)
	if err != nil {
		t.Fatalf("%v", err)
	}

	dir := filepath.Join(repoRoot, "config", "products")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read config/products: %v", err)
	}

	type ref struct{ file, where, name string }
	var missing []ref

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var doc struct {
			Spec struct {
				Sources []struct {
					Name           string `json:"name"`
					CredentialsRef *struct {
						SecretName string `json:"secretName"`
					} `json:"credentialsRef"`
				} `json:"sources"`
				Targets []struct {
					Name           string `json:"name"`
					CredentialsRef *struct {
						SecretName string `json:"secretName"`
					} `json:"credentialsRef"`
				} `json:"targets"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, s := range doc.Spec.Sources {
			if s.CredentialsRef != nil && !inv.Has(s.CredentialsRef.SecretName) {
				missing = append(missing, ref{e.Name(), "source " + s.Name, s.CredentialsRef.SecretName})
			}
		}
		for _, tg := range doc.Spec.Targets {
			if tg.CredentialsRef != nil && !inv.Has(tg.CredentialsRef.SecretName) {
				missing = append(missing, ref{e.Name(), "target " + tg.Name, tg.CredentialsRef.SecretName})
			}
		}
	}

	for _, m := range missing {
		t.Errorf("config/products/%s: %s names credentialsRef.secretName %q, which is not in %s.\n"+
			"\nNothing would create that Secret in a cluster, and there is no directory for it on a\n"+
			"laptop, so the product loads and takes itself out of service. Add it to the inventory\n"+
			"in the same change as the product:\n"+
			"\n  - name: %s\n    description: what it is for\n    keys: [username, password]\n    path: %s\n"+
			"\nDeclared today: %s\n",
			m.file, m.where, m.name, secretsinv.Path, m.name, m.name, strings.Join(inv.Names(), ", "))
	}
}

// TestStagedChartIsReproducible catches the one failure mode the staging step
// introduces: a chart packaged from a stale copy of config/.
//
// `helm package` takes a DIRECTORY and `.Files` cannot reach outside it, so
// config/ is copied into the chart before packaging (deploy/chartstage). That
// copy is not committed, which removes the "two files, one of them stale"
// problem - and replaces it with "the copy was made before the last edit". So
// this stages into a temporary directory and compares fingerprints: if a stage
// run right now would produce something different from what is on disk, the
// chart on disk is stale.
//
// It is a no-op on a clean checkout, where neither tree exists, and that is
// deliberate: the pipeline stages before it packages, so CI proves the fresh
// copy rather than a committed one.
func TestStagedChartIsReproducible(t *testing.T) {
	staged := filepath.Join(repoRoot, filepath.FromSlash(chartstage.ChartFilesDir))
	if _, err := os.Stat(staged); os.IsNotExist(err) {
		t.Skip("nothing staged; `task chart:stage` has not been run in this checkout")
	}

	have, err := chartstage.Fingerprint(staged)
	if err != nil {
		t.Fatalf("fingerprint the staged tree: %v", err)
	}

	// Stage into a copy of the repository's sources rather than into the repo:
	// a test that rewrites the tree it is testing is a test that hides the
	// thing it is looking for.
	tmp := t.TempDir()
	for _, s := range chartstage.Sources {
		if err := copyInto(filepath.Join(repoRoot, filepath.FromSlash(s.From)), filepath.Join(tmp, filepath.FromSlash(s.From))); err != nil {
			t.Fatalf("prepare %s: %v", s.From, err)
		}
	}
	if err := chartstage.Stage(tmp); err != nil {
		t.Fatalf("stage into a temporary root: %v", err)
	}
	want, err := chartstage.Fingerprint(filepath.Join(tmp, filepath.FromSlash(chartstage.ChartFilesDir)))
	if err != nil {
		t.Fatalf("fingerprint the fresh stage: %v", err)
	}

	if have != want {
		t.Errorf("the chart's staged files are not what config/ and deploy/ say today.\n"+
			"\nA chart packaged now would carry an older copy of the products, the people or the\n"+
			"policies than the repository holds. Run:\n\n    task chart:stage\n"+
			"\nstaged: %s\nfresh:  %s\n", have[:16], want[:16])
	}
}

func copyInto(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		b, err := os.ReadFile(from)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		return os.WriteFile(to, b, info.Mode().Perm())
	}
	return filepath.Walk(from, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, fi.Mode().Perm())
	})
}

// TestImagePathsCoverWhatTheDockerfilesRead guards the decision that makes a
// configuration-only release cost no restarts.
//
// .github/scripts/version.sh rebuilds images only when a commit touched one of
// CODE_PATHS. That is the whole mechanism: a release that adds a product ships
// a new chart pointing at the images already running, and no pod is replaced.
//
// It is also the mechanism's one failure mode. A path that an image is built
// from and that is NOT in that list means a real code change ships inside an
// image built before it - silently, with a green pipeline, and the symptom is
// a fix that "did not deploy". So the list is checked against what the
// Dockerfiles actually COPY.
func TestImagePathsCoverWhatTheDockerfilesRead(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(repoRoot, ".github", "scripts", "version.sh"))
	if err != nil {
		t.Fatalf("read version.sh: %v", err)
	}
	declared := codePaths(string(script))
	if len(declared) == 0 {
		t.Fatal("version.sh declares no CODE_PATHS; the pipeline would never rebuild an image")
	}

	dockerfiles, err := filepath.Glob(filepath.Join(repoRoot, "deploy", "build", "Dockerfile.*"))
	if err != nil || len(dockerfiles) == 0 {
		t.Fatalf("no Dockerfiles under deploy/build: %v", err)
	}

	for _, df := range dockerfiles {
		b, err := os.ReadFile(df)
		if err != nil {
			t.Fatalf("read %s: %v", df, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			code, _, _ := strings.Cut(line, "#")
			fields := strings.Fields(code)
			if len(fields) < 3 || !strings.EqualFold(fields[0], "COPY") {
				continue
			}
			// `COPY --from=<stage>` reads a path inside an earlier BUILD STAGE,
			// not the build context, so its source is a filesystem path in an
			// image and has nothing to do with what is in the repository.
			if strings.Contains(code, "--from=") {
				continue
			}
			for _, src := range fields[1 : len(fields)-1] {
				// Flags, and the whole-context copy, which is covered by
				// everything else in the list by definition.
				if strings.HasPrefix(src, "--") || src == "." {
					continue
				}
				src = strings.TrimSuffix(filepath.ToSlash(src), "/")
				if !covered(src, declared) {
					t.Errorf("%s copies %q, which no entry in version.sh's CODE_PATHS covers.\n"+
						"\nA change to it would not rebuild the images, so it would ship inside an image built\n"+
						"before it - with a green pipeline and no symptom until somebody asks why a fix did\n"+
						"not deploy. Add it to CODE_PATHS.\n\nCODE_PATHS today: %s\n",
						filepath.Base(df), src, strings.Join(declared, " "))
				}
			}
		}
	}
}

// codePaths pulls the CODE_PATHS array out of version.sh.
func codePaths(script string) []string {
	_, rest, ok := strings.Cut(script, "CODE_PATHS=(")
	if !ok {
		return nil
	}
	body, _, ok := strings.Cut(rest, ")")
	if !ok {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(body, "\n") {
		code, _, _ := strings.Cut(line, "#")
		paths = append(paths, strings.Fields(code)...)
	}
	return paths
}

// covered reports whether src sits under one of the declared paths. A copy from
// a build STAGE (--from=build) names a path inside the image and is skipped by
// the caller, so everything reaching here is a context path.
func covered(src string, declared []string) bool {
	for _, d := range declared {
		d = strings.TrimSuffix(filepath.ToSlash(d), "/")
		if src == d || strings.HasPrefix(src, d+"/") {
			return true
		}
	}
	return false
}
