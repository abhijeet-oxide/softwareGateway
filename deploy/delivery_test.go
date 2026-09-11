package deploy

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

// TestEveryDependencyIsWaitedFor is the ordering guarantee, made checkable.
//
// The rule this enforces: a workload that cannot do its job without something
// else must WAIT for that thing in an init container, not start and fail.
//
// The difference matters more than it sounds. A pod that starts without its
// dependency reaches CrashLoopBackOff, which looks identical whether it is
// waiting for its database or genuinely broken - so it trains everybody to
// ignore the one pod state that should never be ignored, and it makes a
// restart count meaningless. A pod waiting in an init container sits in
// `Init:0/1`, logs what it is waiting for, and has a restart count of zero
// that still means something.
//
// The list is explicit rather than derived. A dependency is a fact about what
// a process does at startup - ZITADEL's sign-in screens read their token from a
// file and exit if it is absent; the Coordinator migrates a schema - and there
// is nothing in a manifest to infer that from. So adding a workload means
// adding a line here and saying what it needs, which is the review this test
// is really for.
func TestEveryDependencyIsWaitedFor(t *testing.T) {
	want := map[string][]string{
		// Migrates the schema at startup: a database that is not there is a
		// crash, not a degraded start.
		"coordinator": {"wait-for-database"},
		// Refuses an unmigrated database. The pre-install hook has already
		// migrated it; this covers a failover in progress.
		"zitadel": {"wait-for-database"},
		// Reads its service-user token from a file AND EXITS if it is absent,
		// which is the crash loop this whole design removes.
		"zitadel-login": {"wait-for-zitadel", "wait-for-login-token"},
		// Proxies both upstreams and serves nothing of its own, so starting
		// early means 502 on the one address every sign-in goes through.
		"zitadel-proxy": {"wait-for-zitadel", "wait-for-login-screens"},
		// Soft: it renders the page that explains an outage, so it must come up
		// during one. The waits order a first install; they do not gate it.
		"web": {"wait-for-coordinator", "wait-for-oidc-client"},
		// Soft: the data plane must not depend on the control plane being up
		// first (docs/design/27 section 6).
		"worker": {"wait-for-coordinator"},
		// Drives the management API; there is nothing to seed without it.
		"seed": {"wait-for-zitadel"},
		// Talks to nothing. Listed with an empty set so this test says so
		// rather than being silent about it.
		"cerbos": {},
	}

	rendered := renderChart(t)
	seen := map[string]bool{}

	for _, doc := range rendered {
		component, _ := nested(doc, "metadata", "labels", "app.kubernetes.io/component")
		kind, _ := doc["kind"].(string)
		if kind != "Deployment" && kind != "Job" {
			continue
		}
		expected, tracked := want[component]
		if !tracked {
			continue
		}
		seen[component] = true

		var got []string
		if inits, ok := nestedSlice(doc, "spec", "template", "spec", "initContainers"); ok {
			for _, c := range inits {
				m, _ := c.(map[string]any)
				name, _ := m["name"].(string)
				if strings.HasPrefix(name, "wait-for-") {
					got = append(got, name)
				}
			}
		}
		if !slices.Equal(got, expected) {
			t.Errorf("%s waits for %v, but this test says it must wait for %v.\n"+
				"\nIf the dependency really changed, change the list in this test and say why in the\n"+
				"comment beside it. If it did not, the workload starts before something it needs and\n"+
				"will reach CrashLoopBackOff to find out - which is the state this chart exists to\n"+
				"never produce.\n", component, got, expected)
		}
	}

	for component := range want {
		if !seen[component] {
			t.Errorf("no Deployment or Job rendered with component %q, so its waits were never "+
				"checked. Either it was renamed or it stopped being deployed; this test cannot "+
				"tell which, and both need a person.", component)
		}
	}
}

// TestTheDatabaseIsNotInTheChart guards the decision the ordered startup rests
// on.
//
// The ZITADEL migration is a Helm pre-install hook, which means Helm WAITS for
// it before creating any pod - so no pod is ever created against an unmigrated
// schema. That only works because the database already exists when the chart is
// installed: a pre-install hook in a chart that also deploys its own database
// waits forever for a Postgres that Helm has not created yet.
//
// So the day somebody adds a Postgres workload back into this chart, the
// ordering silently becomes a deadlock on first install. This fails instead.
func TestTheDatabaseIsNotInTheChart(t *testing.T) {
	for _, doc := range renderChart(t) {
		kind, _ := doc["kind"].(string)
		if kind != "Deployment" && kind != "StatefulSet" {
			continue
		}
		containers, _ := nestedSlice(doc, "spec", "template", "spec", "containers")
		for _, c := range containers {
			m, _ := c.(map[string]any)
			image, _ := m["image"].(string)
			if strings.Contains(image, "postgres") {
				name, _ := nested(doc, "metadata", "name")
				t.Errorf("%s/%s runs %s: this chart must not deploy a database.\n"+
					"\nTwo things break. A `helm rollback` gains the ability to reach the one thing here\n"+
					"that cannot be recreated. And the ZITADEL migration can no longer be a pre-install\n"+
					"hook - it would wait for a Postgres that Helm has not created yet - so ordered\n"+
					"startup becomes a deadlock on every fresh install.\n"+
					"\nThe database belongs in deploy/environments/<env>/database, applied by the Flux\n"+
					"layer this chart's layer depends on.\n", kind, name, image)
			}
		}
	}
}

// A NOTE ON `go test` CACHING, because it will fool somebody.
//
// The tests in this file read YAML through helm and kustomize, which are
// SUBPROCESSES. Go's test cache tracks files the test process itself opens; it
// cannot see what a subprocess read. So editing a manifest and re-running gives
// a cached `ok` that proves nothing.
//
// Use `go test -count=1 ./deploy/...` whenever you are checking that one of
// these tests fails for a change you just made. CI is unaffected - a fresh
// runner has no cache.

// renderChart runs `helm template` with the chart's defaults and returns the
// objects. It skips rather than fails when helm is absent: the CI job that
// matters installs it, and a developer without helm should not be stopped by
// `go test ./...`.
func renderChart(t *testing.T) []map[string]any {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH; the chart job in CI covers this")
	}
	chart := filepath.Join(repoRoot, "deploy", "charts", "software-gateway")
	if _, err := os.Stat(filepath.Join(chart, "files", "config", "config.yaml")); err != nil {
		t.Skip("the chart is not staged; run `task chart:stage`")
	}

	cmd := exec.Command(helm, "template", "swgw", chart,
		"--set", "identity.masterkey.value=0123456789abcdef0123456789abcdef",
		"--set", "identity.rootPassword.value=test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	var docs []map[string]any
	for _, raw := range strings.Split(string(out), "\n---\n") {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || doc["kind"] == nil {
			continue
		}
		docs = append(docs, doc)
	}
	if len(docs) == 0 {
		t.Fatal("helm template produced no objects")
	}
	return docs
}

func nested(doc map[string]any, path ...string) (string, bool) {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[p]
		if !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	return s, ok
}

func nestedSlice(doc map[string]any, path ...string) ([]any, bool) {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	s, ok := cur.([]any)
	return s, ok
}

// TestNothingIsPulledFromThePublicInternet is the air-gap invariant.
//
// This product ships into estates with no egress, and the way that breaks is
// never the obvious way. Nobody forgets the coordinator's image. What gets
// forgotten is nginx, or the node image an init container uses for three
// seconds, or the PostgreSQL image CloudNativePG pulls only during a failover -
// and a cluster missing that last one does not fail when it is deployed, it
// fails when the primary dies, which is the worst possible moment to discover
// that a mirror was never configured.
//
// So every image reference that a DEPLOYED environment produces is checked, and
// the check is that it names the internal registry rather than that it does not
// name a list of public ones. A denylist of hostnames would pass the first
// registry nobody thought of.
func TestNothingIsPulledFromThePublicInternet(t *testing.T) {
	for _, env := range []string{"lab", "prod"} {
		t.Run(env, func(t *testing.T) {
			values, err := chartstage.EnvironmentValues(repoRoot, env)
			if err != nil {
				t.Fatalf("%v", err)
			}
			var v struct {
				Image struct {
					Registry string `json:"registry"`
				} `json:"image"`
			}
			if err := yaml.Unmarshal(values, &v); err != nil {
				t.Fatalf("parse %s values: %v", env, err)
			}
			registry := v.Image.Registry
			if registry == "" {
				t.Fatalf("%s sets no image.registry, so this environment pulls this product's "+
					"own images from wherever their path points", env)
			}

			for _, ref := range imageRefs(t, env, values) {
				if !strings.HasPrefix(ref.image, registry+"/") {
					t.Errorf("%s: %s pulls %q, which does not come from %s.\n"+
						"\nAn estate with no egress cannot start this pod, and nothing here would say so\n"+
						"until it tried. Route it through the mirror - images.mirror in the chart, or the\n"+
						"reference itself for anything outside it.\n", env, ref.where, ref.image, registry)
				}
			}
		})
	}
}

type imageRef struct{ where, image string }

// imageRefs collects every image a deployed environment produces: the rendered
// chart, the CloudNativePG Cluster beside it, and the operator that runs it.
// The last two are not Helm and would be missed by rendering alone - which is
// exactly where the forgettable ones live.
func imageRefs(t *testing.T, env string, values []byte) []imageRef {
	t.Helper()
	var refs []imageRef

	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH; the chart job in CI covers this")
	}
	chart := filepath.Join(repoRoot, "deploy", "charts", "software-gateway")
	if _, err := os.Stat(filepath.Join(chart, "files", "config", "config.yaml")); err != nil {
		t.Skip("the chart is not staged; run `task chart:stage`")
	}
	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, values, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(helm, "template", "swgw", chart, "--values", valuesFile).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s: %v\n%s", env, err, out)
	}
	for _, m := range imageLine.FindAllStringSubmatch(string(out), -1) {
		refs = append(refs, imageRef{where: "the chart", image: m[1]})
	}

	// The database and its operator, which are plain manifests in Flux layers
	// rather than Helm. GLOBBED, not listed: a file moved between overlays must
	// not quietly take its images out of this test's sight, which is exactly
	// what a hardcoded path would have done the first time the operator was
	// split into a base and two scopes.
	var manifests []string
	manifests = append(manifests, filepath.Join(repoRoot, "deploy", "environments", env, "database", "cluster.yaml"))
	err = filepath.WalkDir(filepath.Join(repoRoot, "deploy", "flux", "platform"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".yaml") {
			manifests = append(manifests, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/flux/platform: %v", err)
	}

	for _, f := range manifests {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range manifestImage.FindAllStringSubmatch(string(b), -1) {
			refs = append(refs, imageRef{where: filepath.Base(f), image: m[1]})
		}
	}

	if len(refs) == 0 {
		t.Fatal("found no image references at all, so this test is not testing anything")
	}
	return refs
}

var (
	// Indented `image:` only, so a `#` comment that happens to contain the word
	// is not read as a reference.
	imageLine = regexp.MustCompile(`(?m)^\s+image:\s+(\S+:\S+)\s*$`)
	// `imageName:` for a CloudNativePG Cluster, `repository:` for the operator
	// chart's values - the two spellings the non-Helm manifests use.
	manifestImage = regexp.MustCompile(`(?m)^\s+(?:imageName|repository):\s+(\S+/\S+)\s*$`)
)

// TestEveryOperatorScopeBuildsAndKeepsOneName guards the switch that decides
// where the database operator runs.
//
// Two things have to hold, and neither is visible by reading one file.
//
// EVERY ARRANGEMENT MUST BUILD. The overlays differ by a namespace and a
// patched value, which is exactly the kind of difference that rots: a field
// renamed in the base, a patch path that no longer resolves, and the scope
// nobody uses in CI is broken on the day somebody needs it.
//
// EVERY BOOTSTRAP MUST PRODUCE `platform-operators`. That name is what
// deploy/environments/<env>/layers.yaml depends on. If a scope produced a
// differently named Kustomization, choosing it would leave every environment
// waiting on a dependency that will never exist - and waiting is exactly what
// that arrangement is designed to do, so it would wait quietly and forever.
func TestEveryOperatorScopeBuildsAndKeepsOneName(t *testing.T) {
	kustomize, err := exec.LookPath("kustomize")
	if err != nil {
		t.Skip("kustomize is not on PATH; the chart job in CI covers this")
	}

	bootstraps, err := filepath.Glob(filepath.Join(repoRoot, "deploy", "flux", "platform", "bootstrap", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	nested, err := filepath.Glob(filepath.Join(repoRoot, "deploy", "flux", "platform", "bootstrap", "*", "*", "kustomization.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	bootstraps = append(bootstraps, nested...)
	if len(bootstraps) < 2 {
		t.Fatalf("found %d bootstrap arrangements; there should be one per scope", len(bootstraps))
	}

	for _, k := range bootstraps {
		dir := filepath.Dir(k)
		scope, _ := filepath.Rel(filepath.Join(repoRoot, "deploy", "flux", "platform", "bootstrap"), dir)
		t.Run(filepath.ToSlash(scope), func(t *testing.T) {
			out, err := exec.Command(kustomize, "build", dir).CombinedOutput()
			if err != nil {
				t.Fatalf("kustomize build %s: %v\n%s", scope, err, out)
			}

			var operatorLayer string
			for _, raw := range strings.Split(string(out), "\n---\n") {
				var doc map[string]any
				if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
					continue
				}
				kind, _ := doc["kind"].(string)
				if kind != "Kustomization" {
					continue
				}
				name, _ := nested2(doc, "metadata", "name")
				path, _ := nested2(doc, "spec", "path")
				operatorLayer = path
				if name != "platform-operators" {
					t.Errorf("this arrangement creates a Kustomization named %q, not "+
						"\"platform-operators\".\n"+
						"\ndeploy/environments/<env>/layers.yaml depends on that exact name. A scope that\n"+
						"produces a different one leaves every environment waiting on a dependency that\n"+
						"will never exist - quietly, because waiting is what it is designed to do.\n", name)
				}
			}
			if operatorLayer == "" {
				t.Fatal("no Kustomization was produced, so this scope installs no operator at all")
			}

			// And the layer it points at must build too, with exactly one
			// operator in it: the CRDs and admission webhooks are cluster-scoped
			// singletons, so a second would fight the first over both.
			layer := filepath.Join(repoRoot, filepath.FromSlash(strings.TrimPrefix(operatorLayer, "./")))
			out, err = exec.Command(kustomize, "build", layer).CombinedOutput()
			if err != nil {
				t.Fatalf("kustomize build %s (named by %s): %v\n%s", operatorLayer, scope, err, out)
			}
			releases := 0
			for _, raw := range strings.Split(string(out), "\n---\n") {
				var doc map[string]any
				if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
					continue
				}
				if kind, _ := doc["kind"].(string); kind != "HelmRelease" {
					continue
				}
				releases++

				// THE SCOPES MUST BE MUTUALLY EXCLUSIVE, and this pair is what
				// makes them so. The two scopes put the operator's HelmRelease
				// in different namespaces, so they are different objects and
				// the layer - which has prune off, because pruning an operator
				// can take its CRDs and every database with them - will not
				// remove the one it stopped pointing at.
				//
				// Helm keys a release by (releaseName, storageNamespace).
				// Identical in every scope means the second one cannot install:
				// it fails loudly instead of succeeding into two operators that
				// both report healthy while fighting over one admission
				// webhook. Drift here would restore that silent failure.
				name, _ := nested2(doc, "spec", "releaseName")
				storage, _ := nested2(doc, "spec", "storageNamespace")
				if name != "cloudnative-pg" || storage != "cnpg-system" {
					t.Errorf("this scope installs Helm release %q in storage namespace %q; every "+
						"scope must use (cloudnative-pg, cnpg-system).\n"+
						"\nThat pair is the only thing stopping a scope change from leaving TWO operators\n"+
						"running - each reconciling the same cluster-scoped admission webhooks to point at\n"+
						"itself, each reporting healthy, and nothing saying so. See\n"+
						"deploy/flux/platform/operators/README.md, \"Changing scope after a deployment\".\n",
						name, storage)
				}
			}
			if releases != 1 {
				t.Errorf("%s renders %d operator HelmReleases; there must be exactly one.\n"+
					"\nCloudNativePG's CRDs and admission webhooks are cluster-scoped singletons. Two\n"+
					"operators reconcile the same webhook configuration to point at themselves, and\n"+
					"the loser's databases are admitted - or rejected - by the winner's webhook.\n",
					operatorLayer, releases)
			}
		})
	}
}

// nested2 is nested() for documents decoded by this file's own loop.
func nested2(doc map[string]any, path ...string) (string, bool) { return nested(doc, path...) }
