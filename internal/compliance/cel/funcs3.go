package cel

import (
	"sort"
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
			if w.Namespace() != claim.Namespace() {
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
