package cel

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The third part of the engine function library: the semantics a check needs in
// order to be right about a thing it would otherwise guess at.
//
// Every function here replaced an expression that was wrong in a way its author
// could not see. Each one names, in its comment, the wrong finding it removes -
// because "why does this function exist" is the question somebody will ask when
// they are tempted to inline it again.
func bindings3(idx *compliance.Index) map[string]impl {
	out := map[string]impl{}
	add := func(id string, fn impl) { out[id] = fn }

	// probeHandler() reduces a probe to WHAT IT ACTUALLY CALLS, discarding the
	// timing fields around it.
	//
	// The check that compares liveness against readiness used to compare the
	// probes field by field with text(). text() of a list is the empty string,
	// so two exec probes running completely different commands both rendered as
	// "" and compared equal - every container with two shell-based health
	// checks was reported as having identical probes, and the finding printed
	// " on " where the handler should have been. Roughly a third of that
	// check's findings were that bug.
	add("probehandler_dyn", func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.String("")
		}
		probe, ok := native(args[0]).(map[string]any)
		if !ok {
			return types.String("")
		}
		return types.String(probeHandler(probe))
	})

	// podField() reads a value from a workload's pod spec, wherever that kind
	// keeps it.
	//
	// A CronJob's pod spec is two levels deeper than everything else's. A check
	// reading "spec.template.spec.terminationGracePeriodSeconds" off a CronJob
	// finds nothing, and then either faults or silently concludes the field is
	// absent - a false finding on every scheduled job in the estate, from a
	// path that looks obviously correct.
	add("podfield_dyn_string", func(args ...ref.Val) ref.Val {
		if len(args) != 2 {
			return types.NullValue
		}
		spec, ok := podSpecOf(native(args[0]))
		if !ok {
			return types.NullValue
		}
		v, found := compliance.Lookup(spec, scalarString(native(args[1])))
		if !found || v == nil {
			return types.NullValue
		}
		return types.DefaultTypeAdapter.NativeToValue(v)
	})

	// securityValue() resolves a security setting the way the kubelet does:
	// container first, then pod, then nothing - and returns it as the text a
	// manifest wrote, so a check compares against "0" or "false" without having
	// to know whether YAML gave it an integer or a float.
	// the container's own value wins, otherwise the pod's applies, otherwise
	// nothing is asserted.
	//
	// Without this the same three situations - not declared anywhere, inherited
	// from the pod, and set explicitly on the container - all reported as "not
	// set". Findings reading "runAsNonRoot not set" appeared on pods that had
	// explicitly set it to FALSE, which is a materially worse condition
	// described in words that make it sound like an oversight.
	add("securityvalue_dyn_dyn_string", func(args ...ref.Val) ref.Val {
		if len(args) != 3 {
			return types.String("")
		}
		v, _ := effectiveSecurity(native(args[0]), native(args[1]), scalarString(native(args[2])))
		return types.String(scalarString(v))
	})

	// securitySource() says WHERE the effective value came from, so a finding
	// can name the line to edit rather than sending a reader to look in two
	// places.
	add("securitysource_dyn_dyn_string", func(args ...ref.Val) ref.Val {
		if len(args) != 3 {
			return types.String("not declared")
		}
		_, src := effectiveSecurity(native(args[0]), native(args[1]), scalarString(native(args[2])))
		return types.String(src)
	})

	// runsAsRoot() answers, for a whole workload, whether anything in it will
	// run as the root user - either because a UID of 0 is set, or because
	// running as non-root is explicitly switched off.
	//
	// "Nothing is declared" is deliberately NOT root here. On a cluster
	// enforcing a restricted policy an undeclared user is assigned an arbitrary
	// non-root one, so calling it root would be a statement the manifest does
	// not support. The undeclared case is its own, lesser finding.
	add("runsasroot_dyn", func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.Bool(false)
		}
		obj, ok := native(args[0]).(map[string]any)
		if !ok {
			return types.Bool(false)
		}
		return types.Bool(workloadRunsAsRoot(obj))
	})

	// mountersOf() is every workload in the release that mounts a given
	// PersistentVolumeClaim.
	//
	// It is what turns a claim-side finding into a sentence about software: not
	// "this claim is ReadWriteMany" - which is a fact, not a defect - but "these
	// two workloads write to it, and this one writes as root".
	add("mountersof_dyn", func(args ...ref.Val) ref.Val {
		out := []any{}
		claim := resolve(idx, args)
		if claim == nil {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		name := claim.Name()
		for _, w := range idx.OfKind(compliance.WorkloadKinds...) {
			if !sameNamespace(w, claim) {
				continue
			}
			if mountsClaim(w, name) {
				out = append(out, w.Object)
			}
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	})

	// configRefs() is every ConfigMap and Secret a workload asks the kubelet to
	// give it, from all six places a pod spec can name one.
	//
	// Two checks need it and both are about the same class of defect: a
	// reference to an object that is not there. A missing ConfigMap is not a
	// warning at install time - the pod stays in CreateContainerConfigError
	// forever, and nothing in the chart hints at which of the six places the
	// name came from.
	add("configrefs_dyn", func(args ...ref.Val) ref.Val {
		out := []any{}
		if len(args) != 1 {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		spec, ok := podSpecOf(native(args[0]))
		if !ok {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		return types.DefaultTypeAdapter.NativeToValue(configRefs(spec))
	})

	// sorted() puts a list of strings in a fixed order.
	//
	// # Why a check cannot do without it
	//
	// A finding that lists the offending keys renders them from a map
	// comprehension, and map iteration order is randomised. The check is
	// correct, the finding is correct, and the WORDS come out in a different
	// order on every run - so the same release checked twice produces different
	// text, and a release-over-release comparison reports the finding as fixed
	// and reintroduced with nothing having changed. "The same release checked
	// twice is identical" is a merge gate in this package, not a preference.
	add("sorted_list", func(args ...ref.Val) ref.Val {
		out := []any{}
		if len(args) != 1 {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		list, ok := native(args[0]).([]any)
		if !ok {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		strs := make([]string, 0, len(list))
		for _, v := range list {
			strs = append(strs, scalarString(v))
		}
		sort.Strings(strs)
		for _, v := range strs {
			out = append(out, v)
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	})

	// disruptionsAllowed() is how many copies a maintenance rule lets the
	// platform move at once. -1 means the rule states neither bound, and -2
	// means it selects nothing so there is no replica count to compute against.
	add("disruptionsallowed_dyn", func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.Int(-1)
		}
		pdb := resolve(idx, args)
		if pdb == nil {
			return types.Int(-1)
		}
		spec, ok := mapField(pdb.Object, "spec")
		if !ok {
			return types.Int(-1)
		}
		worst := -2
		for _, w := range idx.OfKind(compliance.WorkloadKinds...) {
			if !sameNamespace(w, pdb) {
				continue
			}
			sel, _ := mapField(pdb.Object, "spec", "selector")
			if !SelectorMatches(sel, w.PodLabels()) {
				continue
			}
			n := disruptionsAllowed(spec, replicaCount(w.Object))
			if n == -1 {
				return types.Int(-1)
			}
			// The tightest workload decides: a rule covering two services
			// deadlocks maintenance as soon as it deadlocks one of them.
			if worst == -2 || n < worst {
				worst = n
			}
		}
		return types.Int(worst)
	})

	// shippedObjectName() reports whether a literal is the NAME of something
	// this release ships.
	//
	// An environment variable set to "session-store-v1" in a field called
	// APP_SESSION_SECRET is an opaque string in a field named for a credential,
	// which is the shape the credential detector corroborates on. It is also
	// exactly what a reference to a Secret object looks like - and a reference
	// to a credential is the correct pattern, not a leak.
	add("shippedobjectname_string", func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.Bool(false)
		}
		want := strings.TrimSpace(scalarString(native(args[0])))
		if want == "" {
			return types.Bool(false)
		}
		for _, r := range idx.OfKind("Secret", "ConfigMap") {
			if r.Name() == want {
				return types.Bool(true)
			}
		}
		return types.Bool(false)
	})

	unary := func(id string, fn func(string) bool) {
		add(id, func(args ...ref.Val) ref.Val {
			if len(args) != 1 {
				return types.Bool(false)
			}
			return types.Bool(fn(scalarString(native(args[0]))))
		})
	}
	unary("runtimelabelkey_string", IsRuntimeSuppliedLabelKey)
	unary("builtinapigroup_string", IsBuiltinAPIGroup)

	add("decodebase64_string", func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.String("")
		}
		return types.String(DecodeBase64(scalarString(native(args[0]))))
	})

	add("credentialclass_string_string", func(args ...ref.Val) ref.Val {
		if len(args) != 2 {
			return types.String("")
		}
		return types.String(CredentialClass(scalarString(native(args[0])), scalarString(native(args[1]))))
	})

	return out
}

// probeHandler renders a probe's handler in a canonical form.
//
// Timing fields are dropped on purpose: two probes that call the same endpoint
// at different intervals are still the same test, and that is the condition the
// check is about. Defaults are filled in - an httpGet with no scheme is HTTP -
// so two probes that differ only in writing the default out do not read as
// different.
func probeHandler(probe map[string]any) string {
	if h, ok := probe["httpGet"].(map[string]any); ok {
		scheme := scalarString(h["scheme"])
		if scheme == "" {
			scheme = "HTTP"
		}
		path := scalarString(h["path"])
		if path == "" {
			path = "/"
		}
		host := scalarString(h["host"])
		return "http " + strings.ToLower(scheme) + "://" + host + ":" + scalarString(h["port"]) + path
	}
	if h, ok := probe["tcpSocket"].(map[string]any); ok {
		return "tcp " + scalarString(h["host"]) + ":" + scalarString(h["port"])
	}
	if h, ok := probe["grpc"].(map[string]any); ok {
		return "grpc :" + scalarString(h["port"]) + "/" + scalarString(h["service"])
	}
	if h, ok := probe["exec"].(map[string]any); ok {
		cmd, _ := h["command"].([]any)
		parts := make([]string, 0, len(cmd))
		for _, c := range cmd {
			parts = append(parts, scalarString(c))
		}
		return "exec " + strings.Join(parts, " ")
	}
	return ""
}

// podSpecOf finds a workload's pod spec wherever its kind keeps it.
func podSpecOf(obj any) (map[string]any, bool) {
	m, ok := obj.(map[string]any)
	if !ok {
		return nil, false
	}
	r := compliance.Resource{Object: m}
	return r.PodSpec()
}

// effectiveSecurity resolves one securityContext field for a container, and
// says where the value came from.
//
// The three sources are distinct findings with distinct fixes, which is why the
// provenance is returned rather than only the value.
func effectiveSecurity(container, owner any, field string) (any, string) {
	if c, ok := container.(map[string]any); ok {
		if v, found := compliance.Lookup(c, "securityContext."+field); found && v != nil {
			return v, "set on the container"
		}
	}
	if spec, ok := podSpecOf(owner); ok {
		if v, found := compliance.Lookup(spec, "securityContext."+field); found && v != nil {
			return v, "set on the pod, and inherited by every container in it"
		}
	}
	return nil, "not declared"
}

// workloadRunsAsRoot reports whether a workload will run anything as root.
func workloadRunsAsRoot(obj map[string]any) bool {
	spec, ok := podSpecOf(obj)
	if !ok {
		// A bare Pod hands us its own spec.
		if s, found := compliance.Lookup(obj, "spec"); found {
			spec, ok = s.(map[string]any)
		}
		if !ok {
			return false
		}
	}
	asRoot := func(sc any) bool {
		m, ok := sc.(map[string]any)
		if !ok {
			return false
		}
		if u, found := m["runAsUser"]; found && scalarString(u) == "0" {
			return true
		}
		if n, found := m["runAsNonRoot"]; found {
			if b, ok := n.(bool); ok && !b {
				return true
			}
		}
		return false
	}
	if asRoot(spec["securityContext"]) {
		return true
	}
	for _, field := range []string{"initContainers", "containers"} {
		list, _ := spec[field].([]any)
		for _, c := range list {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if asRoot(cm["securityContext"]) {
				return true
			}
		}
	}
	return false
}

// mountsClaim reports whether a workload mounts a claim by name, through a
// volume or through a StatefulSet's own claim templates.
func mountsClaim(w *compliance.Resource, claim string) bool {
	if claim == "" {
		return false
	}
	spec, ok := w.PodSpec()
	if !ok {
		return false
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		pvc, ok := vm["persistentVolumeClaim"].(map[string]any)
		if !ok {
			continue
		}
		if scalarString(pvc["claimName"]) == claim {
			return true
		}
	}
	return false
}

// replicaCount reads a workload's copy count with the Kubernetes default of 1.
func replicaCount(obj map[string]any) int {
	v, found := compliance.Lookup(obj, "spec.replicas")
	if !found || v == nil {
		return 1
	}
	n, ok := intOrPercent(v, 0, math.Floor)
	if !ok {
		return 1
	}
	return n
}

// sameNamespace reports whether two rendered objects will land in the same
// namespace.
//
// # Why an absent namespace matches anything
//
// `helm template` emits `metadata.namespace` only where a chart hard-codes it.
// Where the chart says nothing, the object lands in the namespace the release is
// installed into - which is also where the hard-coded ones land, whenever the
// chart's author wrote the namespace they were installing to. Real charts mix
// both conventions freely, often within one release and sometimes within one
// pair of objects.
//
// Comparing the rendered strings therefore compares a namespace against an
// empty string, and every cross-object join fails. In a validation run that
// produced three "this workload has no disruption policy" findings and four
// "this policy protects no workload" findings, about the SAME three pairs -
// each check asserting the exact opposite of the other, and all seven wrong.
//
// The rule here is the one the manifest supports: an object that names no
// namespace could be in any of them, so it is not evidence of a mismatch. The
// cost is a false negative where a chart genuinely splits two related objects
// across namespaces, which is rare, deliberate, and visible in the chart. The
// cost of the other choice is a contradiction printed twice at blocking
// severity.
func sameNamespace(a, b *compliance.Resource) bool {
	if a == nil || b == nil {
		return false
	}
	an, bn := a.Namespace(), b.Namespace()
	return an == "" || bn == "" || an == bn
}

// disruptionsAllowed is how many copies a PodDisruptionBudget lets the platform
// move at once, or -1 when the policy states neither bound.
//
// # Why this is arithmetic and not a string comparison
//
// A check that pattern-matches on the literal `minAvailable: 1` reports the
// configuration this organization's own standard RECOMMENDS for a two-copy
// service - "for replicas=2, a common safe setting is maxUnavailable: 1 (or
// minAvailable: 1)" - as a blocking deadlock. A chart author who acts on that
// finding makes their release worse, which is the most damaging thing a
// compliance report can do.
//
// It also misses the real deadlock, because the dangerous forms are the
// percentages: `maxUnavailable: 10%` over a single copy is floor(0.1) = 0, and
// reads as permissive while being absolute.
//
// Kubernetes rounds minAvailable percentages UP and maxUnavailable percentages
// DOWN, and both roundings are where quorum-sized workloads deadlock.
func disruptionsAllowed(spec map[string]any, replicas int) int {
	if v, ok := spec["minAvailable"]; ok && v != nil {
		need, ok := intOrPercent(v, replicas, math.Ceil)
		if !ok {
			return -1
		}
		return replicas - need
	}
	if v, ok := spec["maxUnavailable"]; ok && v != nil {
		allowed, ok := intOrPercent(v, replicas, math.Floor)
		if !ok {
			return -1
		}
		return allowed
	}
	return -1
}

// intOrPercent reads the IntOrString both disruption fields use.
//
// The percentage form is a STRING, so a numeric comparison skips it silently -
// which is how an implementation catches one of the four spellings of a
// deadlock and reports the other three as compliant.
func intOrPercent(v any, of int, round func(float64) float64) (int, bool) {
	s := strings.TrimSpace(scalarString(v))
	if s == "" {
		return 0, false
	}
	if pct, found := strings.CutSuffix(s, "%"); found {
		f, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			return 0, false
		}
		return int(round(float64(of) * f / 100)), true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// configRefs collects every ConfigMap and Secret a pod spec names.
//
// The result is ordered, because a list whose order changed between runs would
// make two runs of one release produce different output - which is a merge gate
// in this package, not a preference.
func configRefs(spec map[string]any) []any {
	type refKey struct{ kind, name string }
	seen := map[refKey]bool{}
	optional := map[refKey]bool{}

	note := func(kind, name string, opt any) {
		if name == "" {
			return
		}
		k := refKey{kind, name}
		if !seen[k] {
			seen[k] = true
			optional[k] = true
		}
		// A reference is only optional if EVERY use of it says so. One
		// mandatory use is enough to fail the install.
		if b, ok := opt.(bool); !ok || !b {
			optional[k] = false
		}
	}

	fromSource := func(src map[string]any) {
		if cm, ok := src["configMap"].(map[string]any); ok {
			note("ConfigMap", scalarString(cm["name"]), cm["optional"])
		}
		if sec, ok := src["secret"].(map[string]any); ok {
			name := scalarString(sec["secretName"])
			if name == "" {
				name = scalarString(sec["name"])
			}
			note("Secret", name, sec["optional"])
		}
	}

	for _, v := range asList(spec["volumes"]) {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		fromSource(vm)
		if proj, ok := vm["projected"].(map[string]any); ok {
			for _, src := range asList(proj["sources"]) {
				if sm, ok := src.(map[string]any); ok {
					fromSource(sm)
				}
			}
		}
	}
	for _, ps := range asList(spec["imagePullSecrets"]) {
		if pm, ok := ps.(map[string]any); ok {
			note("Secret", scalarString(pm["name"]), false)
		}
	}
	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		for _, c := range asList(spec[field]) {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			for _, e := range asList(cm["envFrom"]) {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if r, ok := em["configMapRef"].(map[string]any); ok {
					note("ConfigMap", scalarString(r["name"]), r["optional"])
				}
				if r, ok := em["secretRef"].(map[string]any); ok {
					note("Secret", scalarString(r["name"]), r["optional"])
				}
			}
			for _, e := range asList(cm["env"]) {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				vf, ok := em["valueFrom"].(map[string]any)
				if !ok {
					continue
				}
				if r, ok := vf["configMapKeyRef"].(map[string]any); ok {
					note("ConfigMap", scalarString(r["name"]), r["optional"])
				}
				if r, ok := vf["secretKeyRef"].(map[string]any); ok {
					note("Secret", scalarString(r["name"]), r["optional"])
				}
			}
		}
	}

	keys := make([]refKey, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].name < keys[j].name
	})
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"kind": k.kind, "name": k.name, "optional": optional[k],
		})
	}
	return out
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
