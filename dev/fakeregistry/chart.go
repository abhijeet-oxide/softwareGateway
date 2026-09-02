package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"strings"
)

// A real Helm chart, for the local deployment.
//
// # Why the fake registry serves genuine chart bytes
//
// Everything else here can be filler: a transfer moves bytes and does not read
// them, and a comparison aligns digests. Compliance is different - it renders
// the chart. A layer of random bytes under a chart media type would exercise
// exactly one code path, the one that reports "not a gzip archive", and the
// feature would be undemonstrable on a laptop.
//
// So chart components get a small, real chart: `helm template` renders it, the
// engine judges it, and the interface shows findings a person can read.
//
// # Why the charts have deliberate defects
//
// A development estate where everything passes teaches nobody what a finding
// looks like, and it cannot catch a regression in the rendering path either -
// zero findings is what a broken renderer also produces. Each component gets a
// different mix, derived from its name so it is stable across releases and a
// comparison between two releases of one component is meaningful.

// chartTarball builds a gzipped chart for one component.
//
// The defects are chosen from the component's name hash, so the same component
// is always wrong in the same ways: a component that is compliant in one
// release and not in the next would make every comparison read as a regression.
func chartTarball(component, version string) []byte {
	sum := sha256.Sum256([]byte(component))
	seed := binary.BigEndian.Uint32(sum[:4])

	name := strings.ReplaceAll(strings.ToLower(component), " ", "-")
	name = strings.ReplaceAll(name, "/", "-")

	// Four independent defects, so different components fail different checks
	// and the interface's filters have something to filter.
	rootUser := seed%2 == 0
	noLimits := seed%3 == 0
	taggedImage := seed%5 < 2
	noPDB := seed%7 < 3
	// SCH-08 and SCH-09: the toleration block a chart picks up by copying
	// somebody else's chart - it tolerates the node running out of memory, and
	// it tolerates an unreachable node forever.
	badTolerations := seed%11 < 4

	files := map[string]string{
		name + "/Chart.yaml": fmt.Sprintf(
			"apiVersion: v2\nname: %s\ndescription: %s\ntype: application\nversion: %s\nappVersion: \"%s\"\n",
			name, component, semverOf(version), version),

		name + "/values.yaml": strings.Join([]string{
			"replicaCount: 3",
			"image:",
			"  repository: registry.mavenir.example.com/" + name,
			"  tag: \"" + version + "\"",
			"  digest: sha256:" + fmt.Sprintf("%064x", sum[:32]),
			"resources:",
			"  limits:",
			"    memory: 1Gi",
			"  requests:",
			"    cpu: 250m",
			"    memory: 512Mi",
			"",
		}, "\n"),

		name + "/templates/serviceaccount.yaml": tmpl(`
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ .Chart.Name }}
  labels:
    {{- include "labels" . | nindent 4 }}
automountServiceAccountToken: false
`),

		name + "/templates/service.yaml": tmpl(`
apiVersion: v1
kind: Service
metadata:
  name: {{ .Chart.Name }}
  labels:
    {{- include "labels" . | nindent 4 }}
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: {{ .Chart.Name }}
    app.kubernetes.io/instance: {{ .Release.Name }}
  ports:
    - name: https
      port: 8443
      targetPort: https
`),

		name + "/templates/_helpers.tpl": strings.TrimLeft(`
{{- define "labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: cnf
app.kubernetes.io/part-of: {{ .Chart.Name }}
app.kubernetes.io/managed-by: Helm
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}
`, "\n"),

		name + "/templates/deployment.yaml": deploymentTemplate(rootUser, noLimits, taggedImage, badTolerations),
	}
	if !noPDB {
		files[name+"/templates/pdb.yaml"] = tmpl(`
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ .Chart.Name }}
  labels:
    {{- include "labels" . | nindent 4 }}
spec:
  minAvailable: 2
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ .Chart.Name }}
      app.kubernetes.io/instance: {{ .Release.Name }}
`)
	}
	return tarGz(files)
}

// deploymentTemplate is where the defects live.
//
// Each is a real failure mode from the check catalogue rather than a synthetic
// one: a container running as root, a missing memory limit, an image pinned by
// tag instead of digest.
func deploymentTemplate(rootUser, noLimits, taggedImage, badTolerations bool) string {
	image := `{{ .Values.image.repository }}@{{ .Values.image.digest }}`
	if taggedImage {
		// SUP-01: a tag is a pointer and it can be moved.
		image = `{{ .Values.image.repository }}:{{ .Values.image.tag }}`
	}

	security := `
            runAsNonRoot: true
            runAsUser: 10001`
	if rootUser {
		// SEC-01: hard-coded in the template, so the probe reports it `fixed`
		// and the finding is the vendor's to act on.
		security = `
            runAsNonRoot: false
            runAsUser: 0`
	}

	resources := `
          resources:
            requests:
              cpu: {{ .Values.resources.requests.cpu }}
              memory: {{ .Values.resources.requests.memory }}
            limits:
              memory: {{ .Values.resources.limits.memory }}`
	if noLimits {
		// RES-02: a leaking container takes the node with it.
		resources = `
          resources:
            requests:
              cpu: {{ .Values.resources.requests.cpu }}
              memory: {{ .Values.resources.requests.memory }}`
	}

	// The tolerations a conformant chart writes: nothing at all for the
	// node-pressure taints, and a bound on the two it does tolerate.
	tolerations := `
      tolerations:
        - key: node.kubernetes.io/not-ready
          operator: Exists
          effect: NoExecute
          tolerationSeconds: 300
        - key: node.kubernetes.io/unreachable
          operator: Exists
          effect: NoExecute
          tolerationSeconds: 300`
	if badTolerations {
		// SCH-08: scheduled onto a node that has already said it is out of
		// memory, with nothing anywhere saying why. SCH-09: and bound to an
		// unreachable node until somebody notices it is gone.
		tolerations = `
      tolerations:
        - key: node.kubernetes.io/memory-pressure
          operator: Exists
          effect: NoSchedule
        - key: node.kubernetes.io/unreachable
          operator: Exists
          effect: NoExecute`
	}

	return tmpl(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Chart.Name }}
  labels:
    {{- include "labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  progressDeadlineSeconds: 600
  selector:
    matchLabels:
      app.kubernetes.io/name: {{ .Chart.Name }}
      app.kubernetes.io/instance: {{ .Release.Name }}
  template:
    metadata:
      labels:
        {{- include "labels" . | nindent 8 }}
    spec:
      serviceAccountName: {{ .Chart.Name }}
      automountServiceAccountToken: false
      terminationGracePeriodSeconds: 45` + tolerations + `
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      topologySpreadConstraints:
        - topologyKey: topology.kubernetes.io/zone
          maxSkew: 1
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: {{ .Chart.Name }}
        - topologyKey: kubernetes.io/hostname
          maxSkew: 1
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels:
              app.kubernetes.io/name: {{ .Chart.Name }}
      containers:
        - name: main
          image: "` + image + `"
          ports:
            - name: https
              containerPort: 8443
            - name: metrics
              containerPort: 9464` + resources + `
          readinessProbe:
            httpGet:
              path: /readyz
              port: https
              scheme: HTTPS
            timeoutSeconds: 2
            periodSeconds: 5
            failureThreshold: 3
          livenessProbe:
            httpGet:
              path: /livez
              port: https
              scheme: HTTPS
            timeoutSeconds: 2
            periodSeconds: 10
            failureThreshold: 6
          securityContext:` + security + `
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            seccompProfile:
              type: RuntimeDefault
            capabilities:
              drop: [ALL]
`)
}

func tmpl(s string) string { return strings.TrimLeft(s, "\n") }

func tarGz(files map[string]string) []byte {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	// Deterministic order, so the same chart produces the same digest on every
	// run of the seed - which is what makes a re-seeded estate comparable with
	// the one it replaced.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range names {
		body := files[n]
		if err := tw.WriteHeader(&tar.Header{
			Name: n, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			log.Fatalf("chart tar header %s: %v", n, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			log.Fatalf("chart tar write %s: %v", n, err)
		}
	}
	if err := tw.Close(); err != nil {
		log.Fatalf("chart tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		log.Fatalf("chart gzip close: %v", err)
	}
	return buf.Bytes()
}

// semverOf turns a release tag into something Helm will accept as a chart
// version.
//
// Helm validates chart.metadata.version as semver and refuses to render
// otherwise, and real vendor tags are not semver: "24.Q4.2" and "orb_23.8.1076"
// are both ordinary. The digits are kept so the version still identifies the
// release, and appVersion carries the tag verbatim.
func semverOf(tag string) string {
	parts := strings.Split(tag, ".")
	out := make([]string, 0, 3)
	for _, p := range parts {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, p)
		if digits == "" {
			digits = "0"
		}
		out = append(out, strings.TrimLeft(digits, "0")+"")
		if len(out) == 3 {
			break
		}
	}
	for i, p := range out {
		if p == "" {
			out[i] = "0"
		}
	}
	for len(out) < 3 {
		out = append(out, "0")
	}
	return strings.Join(out, ".")
}
