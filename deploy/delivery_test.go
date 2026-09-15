package deploy

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
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
					"\nThe database is the `database` layer: a CloudNativePG Cluster, installed as its\n"+
					"own HelmRelease before this one. See deploy/flux/software/base.\n", kind, name, image)
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

	cmd := exec.CommandContext(t.Context(), helm, "template", "swgw", chart,
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
	instances, err := chartstage.Instances(repoRoot)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(instances) == 0 {
		t.Fatal("no instances under deploy/flux/instances, so this test is not testing anything")
	}

	for _, instance := range instances {
		t.Run(instance, func(t *testing.T) {
			values, err := chartstage.InstanceValues(repoRoot, instance)
			if err != nil {
				t.Fatalf("%v", err)
			}
			var v struct {
				Images struct {
					Registry string `json:"registry"`
				} `json:"images"`
			}
			if err := yaml.Unmarshal(values, &v); err != nil {
				t.Fatalf("parse %s values: %v", instance, err)
			}
			registry := v.Images.Registry
			if registry == "" {
				t.Fatalf("%s sets no images.registry, so this instance pulls this product's "+
					"own images from wherever their path points", instance)
			}

			for _, ref := range imageRefs(t, instance, values) {
				if !strings.HasPrefix(ref.image, registry+"/") {
					t.Errorf("%s: %s pulls %q, which does not come from %s.\n"+
						"\nAn estate with no egress cannot start this pod, and nothing here would say so\n"+
						"until it tried. Route it through the mirror - images.mirror in the chart, or the\n"+
						"reference itself for anything outside it.\n", instance, ref.where, ref.image, registry)
				}
			}
		})
	}
}

type imageRef struct{ where, image string }

// imageRefs collects every image an instance produces, from BOTH layers of the
// chart. The database one matters most and is the easiest to miss: CloudNativePG
// pulls its PostgreSQL image during a failover, so a cluster missing that mirror
// does not fail when it is deployed - it fails when the primary dies.
func imageRefs(t *testing.T, instance string, values []byte) []imageRef {
	t.Helper()

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

	var refs []imageRef
	for _, layer := range []string{"application", "database"} {
		args := []string{"template", "swgw", chart, "--values", valuesFile,
			"--set", "layers.application=" + boolFor(layer, "application"),
			"--set", "layers.database=" + boolFor(layer, "database")}
		out, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template %s (%s layer): %v\n%s", instance, layer, err, out)
		}
		for _, m := range imageLine.FindAllStringSubmatch(string(out), -1) {
			refs = append(refs, imageRef{where: "the " + layer + " layer", image: m[1]})
		}
		for _, m := range manifestImage.FindAllStringSubmatch(string(out), -1) {
			refs = append(refs, imageRef{where: "the " + layer + " layer", image: m[1]})
		}
	}

	if len(refs) == 0 {
		t.Fatal("found no image references at all, so this test is not testing anything")
	}
	return refs
}

func boolFor(layer, want string) string {
	if layer == want {
		return "true"
	}
	return "false"
}

var (
	// Indented `image:` only, so a `#` comment that happens to contain the word
	// is not read as a reference.
	imageLine = regexp.MustCompile(`(?m)^\s+image:\s+(\S+:\S+)\s*$`)
	// `imageName:` is what a CloudNativePG Cluster calls the same thing.
	manifestImage = regexp.MustCompile(`(?m)^\s+imageName:\s+(\S+/\S+)\s*$`)
)

// TestFluxSchemaMatchesTheChart keeps the copy an editor validates an instance's
// values against identical to the one Helm enforces.
//
// A stale copy is worse than no copy: it accepts a key the chart will refuse,
// which is exactly the mistake the schema exists to catch, found one layer later.
func TestFluxSchemaMatchesTheChart(t *testing.T) {
	chart, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(chartstage.ChartSchema)))
	if err != nil {
		t.Fatalf("%v", err)
	}
	copied, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(chartstage.SchemaCopy)))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !bytes.Equal(bytes.ReplaceAll(chart, []byte("\r\n"), []byte("\n")),
		bytes.ReplaceAll(copied, []byte("\r\n"), []byte("\n"))) {
		t.Errorf("%s is not the chart's schema.\n\nRun:\n\n    task chart:stage\n", chartstage.SchemaCopy)
	}
}

// TestEveryFluxDirectoryBuilds is the cheapest test in this file and catches the
// most.
//
// The Flux tree is three layers of kustomize - clusters, instances, software -
// and the ones nobody is currently deploying are the ones that rot: a renamed
// field in `software/base`, a `resources` path that no longer resolves, a patch
// target that matches nothing. None of that is visible by reading one file, and
// all of it fails as a reconciliation error in a cluster rather than on a pull
// request.
func TestEveryFluxDirectoryBuilds(t *testing.T) {
	kustomize, err := exec.LookPath("kustomize")
	if err != nil {
		t.Skip("kustomize is not on PATH; the chart job in CI covers this")
	}

	root := filepath.Join(repoRoot, "deploy", "flux")
	var dirs []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "kustomization.yaml" {
			dirs = append(dirs, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk deploy/flux: %v", err)
	}
	if len(dirs) < 6 {
		t.Fatalf("found %d kustomizations under deploy/flux; the tree has more than that, so "+
			"this test is looking in the wrong place", len(dirs))
	}

	for _, dir := range dirs {
		rel, _ := filepath.Rel(root, dir)
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), kustomize, "build", dir).CombinedOutput()
			if err != nil {
				t.Fatalf("kustomize build: %v\n%s", err, out)
			}
		})
	}
}

// TestEveryInstanceReadsOneValuesFile is the property the whole Flux layout
// exists for, made checkable.
//
// Both HelmReleases must read the SAME ConfigMap. The day one of them stops -
// a copied file, a renamed generator, an inline `values:` block that grew
// past the two `layers` keys - the database and the application can disagree
// about which database they mean, and the symptom is a coordinator talking to
// an empty schema.
func TestEveryInstanceReadsOneValuesFile(t *testing.T) {
	kustomize, err := exec.LookPath("kustomize")
	if err != nil {
		t.Skip("kustomize is not on PATH; the chart job in CI covers this")
	}
	instances, err := chartstage.Instances(repoRoot)
	if err != nil {
		t.Fatalf("%v", err)
	}

	for _, instance := range instances {
		t.Run(instance, func(t *testing.T) {
			dir := filepath.Join(repoRoot, filepath.FromSlash(chartstage.InstanceDir(instance)))
			out, err := exec.CommandContext(t.Context(), kustomize, "build", dir).CombinedOutput()
			if err != nil {
				t.Fatalf("kustomize build: %v\n%s", err, out)
			}

			sources := map[string]int{}
			generated := ""
			releases := 0
			for _, raw := range strings.Split(string(out), "\n---\n") {
				var doc map[string]any
				if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
					continue
				}
				switch kind, _ := doc["kind"].(string); kind {
				case "ConfigMap":
					generated, _ = nested(doc, "metadata", "name")
				case "HelmRelease":
					releases++
					from, _ := nestedSlice(doc, "spec", "valuesFrom")
					if len(from) != 1 {
						name, _ := nested(doc, "metadata", "name")
						t.Errorf("%s reads %d valuesFrom entries; it must read exactly one, so "+
							"there is one place to look.", name, len(from))
						continue
					}
					m, _ := from[0].(map[string]any)
					name, _ := m["name"].(string)
					sources[name]++
				}
			}

			if releases != 2 {
				t.Fatalf("%d HelmReleases; an instance is the database layer and the application "+
					"layer, and nothing else", releases)
			}
			if len(sources) != 1 {
				t.Fatalf("the two layers read %d different values sources: %v.\n"+
					"\nThey must read one, or they can disagree about the database they share.\n",
					len(sources), sources)
			}
			for name := range sources {
				if name != generated {
					t.Errorf("the HelmReleases read ConfigMap %q but this instance generates %q, "+
						"so nothing in Git supplies their values.", name, generated)
				}
			}
		})
	}
}

// instanceDials are the settings an instance may state even when the value is
// the chart's default.
//
// They are the knobs an operator is EXPECTED to turn, and for those, seeing the
// current value in the instance file is the point - somebody sizing a lab reads
// `worker: {replicas: 2}` and changes the 2. Hiding them because they happen to
// match the chart today would make the file answer "what is different" at the
// cost of answering "what can I change", and the second question is the one
// somebody has at 09:00 on their first day.
//
// Everything NOT on this list must differ, which is where the rule below earns
// its place: a resource request or a probe threshold pinned at today's default
// is a decision nobody made, and it diverges silently the day the chart moves.
var instanceDials = []string{
	"coordinator.replicas",
	"worker.replicas",
	"web.replicas",
	"cerbos.replicas",
	"identity.zitadel.replicas",
	"identity.login.replicas",
	"identity.proxy.replicas",
	"identity.sso.enabled",
	// The chart's values for these three are PLACEHOLDERS that exist so
	// `sso.enabled: true` renders at all. Matching one is not agreement with a
	// default - it is an instance that has not been pointed at a directory yet,
	// and the line has to stay visible for the person who will.
	"identity.sso.issuer",
	"identity.sso.clientId",
	"identity.sso.existingSecret",
	"identity.bootstrapAdmin.username",
	"database.cluster.instances",
	"database.cluster.storage.size",
	"database.cluster.walStorage.size",
	"database.cluster.synchronousReplicas",
	"database.cluster.backup.enabled",
	"logLevel.coordinator",
	"logLevel.worker",
}

// TestNoInstanceRestatesAChartDefault keeps one rule true: the chart holds every
// default, and an instance's values file holds what differs plus the dials.
//
// A restated default that is not a dial is not harmless. It reads as a decision
// somebody made for this deployment, so the next person changing the chart's
// default changes it everywhere except the places that silently pinned the old
// one - and the divergence is invisible until something behaves differently in
// one namespace.
func TestNoInstanceRestatesAChartDefault(t *testing.T) {
	var defaults map[string]any
	b, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "charts", "software-gateway", "values.yaml"))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if err := yaml.Unmarshal(b, &defaults); err != nil {
		t.Fatalf("parse the chart's values: %v", err)
	}

	instances, err := chartstage.Instances(repoRoot)
	if err != nil {
		t.Fatalf("%v", err)
	}

	for _, instance := range instances {
		t.Run(instance, func(t *testing.T) {
			raw, err := chartstage.InstanceValues(repoRoot, instance)
			if err != nil {
				t.Fatalf("%v", err)
			}
			var values map[string]any
			if err := yaml.Unmarshal(raw, &values); err != nil {
				t.Fatalf("parse values: %v", err)
			}
			for _, restated := range sameAsDefault(values, defaults, "") {
				if slices.Contains(instanceDials, restated.path) {
					continue
				}
				t.Errorf("%s states %s: %v, which is already the chart's default.\n"+
					"\nDelete the line. If it is a knob an operator is meant to turn, add it to\n"+
					"instanceDials in this test and say so - that list is the difference between a\n"+
					"value somebody chose and one nobody did.\n",
					instance, restated.path, restated.value)
			}
		})
	}
}

type restatement struct {
	path  string
	value any
}

// sameAsDefault returns the dotted paths an instance sets to the value the chart
// already has. Maps are walked; anything else is compared whole, because a list
// that happens to equal the default is still a restatement.
func sameAsDefault(values, defaults map[string]any, prefix string) []restatement {
	var found []restatement
	for key, got := range values {
		want, ok := defaults[key]
		if !ok {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		gotMap, gotIsMap := got.(map[string]any)
		wantMap, wantIsMap := want.(map[string]any)
		if gotIsMap && wantIsMap {
			found = append(found, sameAsDefault(gotMap, wantMap, path)...)
			continue
		}
		if reflect.DeepEqual(got, want) {
			found = append(found, restatement{path: path, value: got})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })
	return found
}

// TestEachInstanceNamesItsNamespaceOnce is the other half of the rule that one
// file holds what a deployment differs in.
//
// The namespace is not a Helm value - kustomize stamps it on everything the
// instance applies, and it renames the Namespace object too - so it lives in
// `kustomization.yaml` rather than in `values/values.yaml`. Written twice it is
// a deployment whose Secrets land in one namespace and whose pods start in
// another, which fails as ImagePullBackOff naming neither.
//
// So: exactly one Namespace, named by the transformer, with everything else
// inside it, and the literal written nowhere else in the directory.
// namespaceLookalikes are keys whose value may equal the namespace without
// being one. An object-name prefix that matches the namespace is ordinary -
// `fullnameOverride: swgw` in namespace `swgw` is what most deployments write.
var namespaceLookalikes = []string{"nameOverride", "fullnameOverride"}

// scalarEquals reports whether any scalar anywhere in a decoded document is
// exactly want, ignoring the keys above.
func scalarEquals(node any, want string) bool {
	switch v := node.(type) {
	case string:
		return v == want
	case map[string]any:
		for key, child := range v {
			if slices.Contains(namespaceLookalikes, key) {
				continue
			}
			if scalarEquals(child, want) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if scalarEquals(child, want) {
				return true
			}
		}
	}
	return false
}

func TestEachInstanceNamesItsNamespaceOnce(t *testing.T) {
	kustomize, err := exec.LookPath("kustomize")
	if err != nil {
		t.Skip("kustomize is not on PATH; the chart job in CI covers this")
	}
	instances, err := chartstage.Instances(repoRoot)
	if err != nil {
		t.Fatalf("%v", err)
	}

	for _, instance := range instances {
		t.Run(instance, func(t *testing.T) {
			dir := filepath.Join(repoRoot, filepath.FromSlash(chartstage.InstanceDir(instance)))

			kfile := filepath.Join(dir, "kustomization.yaml")
			b, err := os.ReadFile(kfile) // #nosec G304 -- instance passed checkSegment.
			if err != nil {
				t.Fatalf("%v", err)
			}
			var k struct {
				Namespace string `json:"namespace"`
			}
			if err := yaml.Unmarshal(b, &k); err != nil {
				t.Fatalf("parse kustomization.yaml: %v", err)
			}
			if k.Namespace == "" {
				t.Fatal("kustomization.yaml sets no namespace, so nothing says where this " +
					"instance is deployed")
			}

			out, err := exec.CommandContext(t.Context(), kustomize, "build", dir).CombinedOutput()
			if err != nil {
				t.Fatalf("kustomize build: %v\n%s", err, out)
			}
			namespaces := 0
			for _, raw := range strings.Split(string(out), "\n---\n") {
				var doc map[string]any
				if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || doc["kind"] == nil {
					continue
				}
				name, _ := nested(doc, "metadata", "name")
				if kind, _ := doc["kind"].(string); kind == "Namespace" {
					namespaces++
					if name != k.Namespace {
						t.Errorf("the Namespace is called %q but the transformer says %q",
							name, k.Namespace)
					}
					continue
				}
				if got, ok := nested(doc, "metadata", "namespace"); ok && got != k.Namespace {
					t.Errorf("%s %s is in namespace %q, not %q", doc["kind"], name, got, k.Namespace)
				}
			}
			if namespaces != 1 {
				t.Errorf("%d Namespace objects; an instance is one namespace", namespaces)
			}

			// And no other file in the instance states it as a VALUE. Compared as
			// whole scalars rather than as text: `swgw` is a substring of
			// `swgw-db-backup`, and a test that cried wolf about that would be
			// turned off within a week.
			err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || filepath.Base(path) == "kustomization.yaml" {
					return err
				}
				if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
					return nil
				}
				b, err := os.ReadFile(path) // #nosec G304 -- walked from the instance directory.
				if err != nil {
					return err
				}
				var doc any
				if err := yaml.Unmarshal(b, &doc); err != nil {
					return nil
				}
				if scalarEquals(doc, k.Namespace) {
					rel, _ := filepath.Rel(repoRoot, path)
					t.Errorf("%s states the namespace %q.\n\n"+
						"It belongs in kustomization.yaml and nowhere else - kustomize stamps it on\n"+
						"everything, including the Namespace object's own name.\n",
						filepath.ToSlash(rel), k.Namespace)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", instance, err)
			}
		})
	}
}

// TestMirrorPathsMatchTheChart keeps `task images:mirror` honest.
//
// It prints where every third-party image has to be copied to, and the chart
// decides where it will be pulled from. Those are two implementations of one
// rule - Go's mirroredPath and the chart's swgw.mirroredImage - and a drift
// between them is a set of copy commands that produce paths no pod asks for.
// The symptom is ImagePullBackOff on a registry that visibly has the image.
func TestMirrorPathsMatchTheChart(t *testing.T) {
	instances, err := chartstage.Instances(repoRoot)
	if err != nil {
		t.Fatalf("%v", err)
	}

	for _, instance := range instances {
		t.Run(instance, func(t *testing.T) {
			want, mirror, err := chartstage.ImagesToMirror(repoRoot, instance)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if mirror == "" {
				t.Skip("this instance pulls from upstream, so nothing is mirrored")
			}

			values, err := chartstage.InstanceValues(repoRoot, instance)
			if err != nil {
				t.Fatalf("%v", err)
			}
			// imageRefs renders both layers, which is where all six appear.
			rendered := map[string]bool{}
			for _, ref := range imageRefs(t, instance, values) {
				rendered[ref.image] = true
			}

			for _, img := range want {
				if !rendered[img.Destination] {
					t.Errorf("images.%s is copied to %q, and no pod pulls that.\n"+
						"\nThe chart's swgw.mirroredImage and chartstage.mirroredPath disagree, so the copy\n"+
						"commands produce a path nothing asks for - which reads as ImagePullBackOff on a\n"+
						"registry that visibly has the image.\n", img.Key, img.Destination)
				}
			}
		})
	}
}

// refusals are configurations the chart must not render.
//
// EACH ONE COMES UP GREEN AND DOES NOT WORK, which is the only kind worth
// failing a render for. They lived in a shell loop in ci.yml until three of
// them silently stopped testing anything: the values they set were renamed,
// `helm template` then succeeded for the wrong reason, and the loop reported
// "the chart accepted X" - correctly, but three releases too late.
//
// Beside the chart they cannot drift. A renamed value breaks this test in the
// same commit that renames it.
var refusals = []struct {
	what string
	set  []string
}{
	{"a release that renders neither layer", []string{
		"layers.application=false"}},
	{"a database layer with no cluster to name", []string{
		"layers.application=false", "layers.database=true", "database.cluster.name="}},
	{"an external database nobody said where to find", []string{
		"database.cluster.name="}},
	{"a masterkey that is not 32 bytes", []string{
		"identity.masterkey.value=short"}},
	// The chart ships placeholders so that `sso.enabled: true` renders, but an
	// operator who CLEARS one has said something different from leaving it
	// alone, and gets told rather than a seed Job that never starts.
	{"single sign-on with the client secret cleared", []string{
		"identity.sso.existingSecret="}},
	{"single sign-on with the issuer cleared", []string{
		"identity.sso.issuer="}},
	{"the bootstrap password shortcut with single sign-on on", []string{
		"identity.bootstrapAdmin.password=hunter2"}},
	{"an Ingress routing on an IP literal", []string{
		"access.expose.type=ingress", "access.webUrl=http://10.0.0.1"}},
	{"a browser URL with a path on it", []string{
		"access.webUrl=http://gateway.example.com/ui"}},
	{"a node port outside the node port range", []string{
		"access.expose.type=nodePort"}},
	{"a pull secret that no inventory entry produces", []string{
		"secrets.backend=vault", "secrets.registryPullSecret.enabled=true",
		"secrets.registryPullSecret.from=nothing-declares-this"}},
	{"an internal registry with no mirror for the third-party images", []string{
		"images.registry=registry.example.internal"}},
}

// TestTheChartRefusesWhatCannotWork renders each of the above and requires the
// render to fail.
func TestTheChartRefusesWhatCannotWork(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not on PATH; the chart job in CI covers this")
	}
	chart := filepath.Join(repoRoot, "deploy", "charts", "software-gateway")
	if _, err := os.Stat(filepath.Join(chart, "files", "config", "config.yaml")); err != nil {
		t.Skip("the chart is not staged; run `task chart:stage`")
	}

	for _, r := range refusals {
		t.Run(r.what, func(t *testing.T) {
			args := []string{"template", "t", chart}
			for _, s := range r.set {
				args = append(args, "--set", s)
			}
			out, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
			if err == nil {
				t.Errorf("the chart accepted %s.\n\n%s\n"+
					"\nEither a validation rule was lost, or the values this case sets were renamed\n"+
					"and it is no longer testing what its name says.\n", r.what, out)
			}
		})
	}
}

// TestEnterpriseWorkflowsTrackTheirOriginals is the cost of keeping a second
// copy of each pipeline, made cheap enough to be worth paying.
//
// .github/workflows/*_enterprise.yml.disabled are the same pipelines written
// for a network with an IP allow list, allow-listed actions and no egress; the
// README beside them says why one file with switches was not the answer. The
// failure a copy has is silent: a job is added to ci.yml, nobody opens the copy
// for a year, and the enterprise repository has been running a pipeline that
// never built the thing the job was added to check.
//
// Job NAMES only. What a job runs on, what it is gated by, and which of its
// steps fetch from a mirror are exactly what the two files exist to disagree
// about; that a job is there at all is not.
func TestEnterpriseWorkflowsTrackTheirOriginals(t *testing.T) {
	const suffix = "_enterprise.yml.disabled"

	dir := filepath.Join(repoRoot, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the workflows directory: %v", err)
	}

	jobs := func(path string) []string {
		b, err := os.ReadFile(path) // #nosec G304 -- path is built from the workflow directory.
		if err != nil {
			t.Fatalf("read %s: %v", filepath.Base(path), err)
		}
		var wf struct {
			Jobs map[string]struct{} `json:"jobs"`
		}
		if err := yaml.Unmarshal(b, &wf); err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), err)
		}
		out := make([]string, 0, len(wf.Jobs))
		for name := range wf.Jobs {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}

	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		public := strings.TrimSuffix(e.Name(), suffix) + ".yml"
		if _, err := os.Stat(filepath.Join(dir, public)); err != nil {
			t.Errorf("%s has no %s to track.\n"+
				"\nEither the pipeline was renamed and its enterprise copy was not, or the copy\n"+
				"outlived the pipeline and is now the only place its jobs are written down.\n",
				e.Name(), public)
			continue
		}
		checked++

		want, got := jobs(filepath.Join(dir, public)), jobs(filepath.Join(dir, e.Name()))
		for _, name := range want {
			if !slices.Contains(got, name) {
				t.Errorf("%s has the job %q and %s does not.\n"+
					"\nAdd it to the copy, or - if it cannot work in that network - keep it there\n"+
					"disabled by a repository variable, the way the security copy does, so the\n"+
					"reason is written down where the next person looks.\n",
					public, name, e.Name())
			}
		}
		for _, name := range got {
			if !slices.Contains(want, name) {
				t.Errorf("%s has the job %q and %s does not.\n"+
					"\nA job in the enterprise copy alone is a job nothing here tests. Either it\n"+
					"belongs in the pipeline both networks run, or it was left behind by a rename.\n",
					e.Name(), name, public)
			}
		}
	}

	if checked == 0 {
		t.Fatalf("found no *%s beside the workflows - either they were deleted, or "+
			"this test is looking in the wrong place", suffix)
	}
}
