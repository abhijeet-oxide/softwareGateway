package compliance_test

import (
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// A rendered stream shaped like the ones helm actually produces: a source
// marker, two objects in one stream, sequences at both indentation styles, and
// a field that is absent from one container and present in the next.
const rendered = `---
# Source: app/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: 3
  template:
    spec:
      tolerations:
      - key: node.kubernetes.io/memory-pressure
        operator: Exists
        effect: NoSchedule
      - key: node.kubernetes.io/unreachable
        operator: Exists
        effect: NoExecute
        tolerationSeconds: 300
      containers:
        - name: main
          image: registry.example/app:1.0
          resources:
            requests:
              memory: 512Mi
        - name: sidecar
          image: registry.example/sidecar:1.0
          resources:
            limits:
              memory: 1Gi
---
# Source: app/templates/service.yaml
apiVersion: v1
kind: Service
metadata:
  name: app
spec:
  ports:
    - port: 8080
`

// lineOf is the 1-based line a snippet is on, so the expectations below read as
// the text rather than as arithmetic somebody has to redo when a line moves.
func lineOf(t *testing.T, snippet string) int {
	t.Helper()
	for i, l := range strings.Split(strings.TrimSuffix(rendered, "\n"), "\n") {
		if strings.Contains(l, snippet) {
			return i + 1
		}
	}
	t.Fatalf("no line contains %q", snippet)
	return 0
}

func TestLocusLineResolvesAPathToItsLine(t *testing.T) {
	deployment := lineOf(t, "kind: Deployment")

	cases := []struct {
		locus   string
		snippet string
	}{
		{"spec.replicas", "replicas: 3"},
		{"metadata.name", "name: app"},
		{"spec.template.spec.tolerations", "tolerations:"},
		// A sequence whose dashes sit at the key's own indentation.
		{"spec.template.spec.tolerations[1].tolerationSeconds", "tolerationSeconds: 300"},
		// And one whose dashes are indented under it.
		{"spec.template.spec.containers[0].name", "- name: main"},
		{"spec.template.spec.containers[1].name", "- name: sidecar"},
		{"spec.template.spec.containers[1].resources.limits.memory", "memory: 1Gi"},
	}
	for _, c := range cases {
		t.Run(c.locus, func(t *testing.T) {
			want := lineOf(t, c.snippet)
			got := compliance.LocusLine([]byte(rendered), deployment, c.locus)
			if got != want {
				t.Fatalf("LocusLine(%q) = %d, want %d (%q)", c.locus, got, want, c.snippet)
			}
		})
	}
}

// The half of every run that is about something ABSENT. A locus that resolves
// to nothing must say so, because a highlight on an arbitrary line is worse
// than no highlight: it is a claim about the document that is false.
func TestLocusLineReportsWhatItCannotFind(t *testing.T) {
	deployment := lineOf(t, "kind: Deployment")
	for _, locus := range []string{
		// Absent from container 0, and PRESENT on container 1 - the case a
		// search that ignored block boundaries would get wrong.
		"spec.template.spec.containers[0].resources.limits.memory",
		"spec.template.spec.containers[0].securityContext.runAsNonRoot",
		"spec.template.spec.affinity",
		"spec.template.spec.containers[7].name",
		"", "   ",
	} {
		if got := compliance.LocusLine([]byte(rendered), deployment, locus); got != 0 {
			t.Errorf("LocusLine(%q) = %d, want 0", locus, got)
		}
	}
}

// A locus absent from THIS object must not match the same key in the next one.
// Both objects here have `metadata.name`; a Service's ports are only on the
// Service.
func TestLocusLineStaysInsideItsOwnDocument(t *testing.T) {
	deployment := lineOf(t, "kind: Deployment")
	if got := compliance.LocusLine([]byte(rendered), deployment, "spec.ports"); got != 0 {
		t.Fatalf("the Deployment's spec.ports resolved to line %d in the Service below it", got)
	}

	service := lineOf(t, "kind: Service")
	want := lineOf(t, "ports:")
	if got := compliance.LocusLine([]byte(rendered), service, "spec.ports"); got != want {
		t.Fatalf("the Service's spec.ports = %d, want %d", got, want)
	}
}

// `tolerations[]` is the shape a check writes when it means "any of them".
// There is no one item to point at, so the key is the honest answer.
func TestLocusLineWithAnEmptyIndexStopsAtTheKey(t *testing.T) {
	deployment := lineOf(t, "kind: Deployment")
	want := lineOf(t, "tolerations:")
	got := compliance.LocusLine([]byte(rendered), deployment,
		"spec.template.spec.tolerations[].tolerationSeconds")
	if got != want {
		t.Fatalf("got %d, want the tolerations key at %d", got, want)
	}
}

// A key must match exactly. `runAsUser` is a prefix of `runAsUserName`, and a
// value containing a colon is not a key.
func TestLocusLineMatchesWholeKeys(t *testing.T) {
	doc := []byte(strings.Join([]string{
		"kind: Pod",
		"spec:",
		"  note: run: this is a value",
		"  runAsUserName: bob",
		"  runAsUser: 1000",
	}, "\n"))
	if got := compliance.LocusLine(doc, 1, "spec.runAsUser"); got != 5 {
		t.Fatalf("got line %d, want 5 - the exact key, not its prefix", got)
	}
	if got := compliance.LocusLine(doc, 1, "spec.run"); got != 0 {
		t.Fatalf("got line %d for a key that only appears inside a value", got)
	}
}

func TestSliceCentresOnTheFocusAndNumbersHonestly(t *testing.T) {
	doc := compliance.RenderedDoc{Chart: "app", Content: []byte(rendered)}
	focus := lineOf(t, "tolerationSeconds: 300")
	object := lineOf(t, "kind: Deployment")

	ex := doc.Slice(compliance.AnchorFor([]byte(rendered), object,
		"spec.template.spec.tolerations[1].tolerationSeconds"), 3)
	if ex.StartLine != focus-3 {
		t.Fatalf("StartLine = %d, want %d", ex.StartLine, focus-3)
	}
	if len(ex.Lines) != 7 {
		t.Fatalf("got %d lines, want 7", len(ex.Lines))
	}
	// The numbering has to agree with the download, or a line number quoted
	// out of the excerpt points at nothing.
	if got := ex.Lines[focus-ex.StartLine]; !strings.Contains(got, "tolerationSeconds: 300") {
		t.Fatalf("the focus line is %q, not the line it claims to be", got)
	}
	if ex.TotalLines != len(strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")) {
		t.Fatalf("TotalLines = %d", ex.TotalLines)
	}
}

// With no focus line the window leads with the object, because "here is the
// object your field is missing from" is the whole of what can be shown.
func TestSliceWithoutAFocusLeadsWithTheObject(t *testing.T) {
	doc := compliance.RenderedDoc{Chart: "app", Content: []byte(rendered)}
	object := lineOf(t, "kind: Deployment")

	ex := doc.Slice(compliance.Anchor{ObjectLine: object}, 5)
	if ex.FocusLine != 0 {
		t.Fatalf("FocusLine = %d, want 0", ex.FocusLine)
	}
	if ex.StartLine > object {
		t.Fatalf("StartLine %d is past the object at %d", ex.StartLine, object)
	}
	if ex.StartLine+len(ex.Lines)-1 <= object {
		t.Fatal("the window ends before the object it is supposed to be showing")
	}
}

func TestSliceClampsToTheDocument(t *testing.T) {
	doc := compliance.RenderedDoc{Content: []byte("a\nb\nc\n")}
	ex := doc.Slice(compliance.Anchor{ObjectLine: 2, FocusLine: 2}, 50)
	if ex.StartLine != 1 || len(ex.Lines) != 3 {
		t.Fatalf("StartLine = %d, %d lines; want 1 and 3", ex.StartLine, len(ex.Lines))
	}
	if ex.TotalLines != 3 {
		t.Fatalf("TotalLines = %d, want 3 - a trailing newline is not a line", ex.TotalLines)
	}

	empty := compliance.RenderedDoc{}
	if got := empty.Slice(compliance.Anchor{ObjectLine: 1}, 5); len(got.Lines) != 0 || got.TotalLines != 0 {
		t.Fatalf("an empty document produced %d lines", len(got.Lines))
	}
}

func TestRenderedDocKey(t *testing.T) {
	if got := (compliance.RenderedDoc{Chart: "app"}).Key(); got != "app" {
		t.Fatalf("Key() = %q", got)
	}
	if got := (compliance.RenderedDoc{SourceFile: "manifests/a.yaml"}).Key(); got != "manifests/a.yaml" {
		t.Fatalf("Key() = %q", got)
	}
}

// The absent-field case, which is half of every run. The path stops at the
// container, so the window shows THAT container rather than the top of a
// Deployment the reader then has to scroll through.
func TestAnchorForFallsBackToTheDeepestPartThatExists(t *testing.T) {
	object := lineOf(t, "kind: Deployment")
	a := compliance.AnchorFor([]byte(rendered), object,
		"spec.template.spec.containers[0].resources.limits.memory")

	if a.FocusLine != 0 {
		t.Fatalf("FocusLine = %d for a field that is not there", a.FocusLine)
	}
	if want := lineOf(t, "requests:") - 1; a.NearLine != want {
		t.Fatalf("NearLine = %d, want the container's own resources block at %d", a.NearLine, want)
	}

	ex := compliance.RenderedDoc{Content: []byte(rendered)}.Slice(a, 4)
	if ex.StartLine != a.NearLine-4 {
		t.Fatalf("the window starts at %d, not centred on the anchor at %d", ex.StartLine, a.NearLine)
	}
}

// An index the document does not have resolves to nothing at all below the
// collection, and the collection is as far as the path goes.
func TestAnchorForAnIndexThatDoesNotExist(t *testing.T) {
	object := lineOf(t, "kind: Deployment")
	a := compliance.AnchorFor([]byte(rendered), object, "spec.template.spec.containers[7].name")
	if a.FocusLine != 0 {
		t.Fatalf("FocusLine = %d for container 7 of 2", a.FocusLine)
	}
	if want := lineOf(t, "containers:"); a.NearLine != want {
		t.Fatalf("NearLine = %d, want the containers key at %d", a.NearLine, want)
	}
}
