package cel

import (
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The second half of the engine function library: the lookups and the
// heuristics. Split from funcs.go only for length; the contract is identical.
func bindings2(idx *compliance.Index) map[string]impl {
	out := map[string]impl{}
	add := func(names []string, fn impl) {
		// Registered under the overload id; the function name is what an author
		// writes and the id is what the checked expression refers to.
		out[names[len(names)-1]] = fn
	}

	// covers() is pdbFor() from the other side: given an object with a
	// selector, the workloads it selects.
	add([]string{FnCovers, "covers_dyn"}, func(args ...ref.Val) ref.Val {
		out := []any{}
		obj := resolve(idx, args)
		if obj == nil {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		sel, ok := mapField(obj.Object, "spec", "selector")
		if !ok {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		// A Service's selector is a bare label map; a PDB's, a Deployment's and
		// a NetworkPolicy's is a LabelSelector with matchLabels. Both shapes
		// reach here, and reading only one of them is how a check silently
		// applies to half the objects it names.
		for _, w := range idx.OfKind(compliance.WorkloadKinds...) {
			if w.Namespace() != obj.Namespace() {
				continue
			}
			labels := w.PodLabels()
			matched := false
			if isLabelSelector(sel) {
				matched = SelectorMatches(sel, labels)
			} else {
				matched = matchLabels(toStringMap(sel), labels)
			}
			if matched {
				out = append(out, w.Object)
			}
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	})

	// replicas() applies the Kubernetes default of 1 for an absent field, so a
	// check does not have to decide what "no replicas key" means.
	add([]string{FnReplicas, "replicas_dyn"}, func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.NewErr("replicas() takes one argument")
		}
		obj, ok := native(args[0]).(map[string]any)
		if !ok {
			return types.Int(1)
		}
		v, found := compliance.Lookup(obj, "spec.replicas")
		if !found || v == nil {
			return types.Int(1)
		}
		switch t := v.(type) {
		case int64:
			return types.Int(t)
		case float64:
			return types.Int(int64(t))
		case int:
			return types.Int(int64(t))
		}
		return types.Int(1)
	})

	// declaresPort() accepts a port by number or by name, because a probe may
	// name either and a check that understood only one would fire on half the
	// correct charts.
	add([]string{FnDeclaresPort, "declaresport_dyn_dyn"}, func(args ...ref.Val) ref.Val {
		if len(args) != 2 {
			return types.NewErr("declaresPort() takes a container and a port")
		}
		c, ok := native(args[0]).(map[string]any)
		if !ok {
			return types.Bool(false)
		}
		want := scalarString(native(args[1]))
		if want == "" {
			return types.Bool(false)
		}
		ports, _ := c["ports"].([]any)
		for _, p := range ports {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if scalarString(pm["containerPort"]) == want || scalarString(pm["name"]) == want {
				return types.Bool(true)
			}
		}
		return types.Bool(false)
	})

	// boundToRole() derives RBAC-02's exemption rather than assuming it: a
	// ServiceAccount that is bound to nothing has no use for a mounted token,
	// and one that is bound to a Role does.
	add([]string{FnBoundToRole, "boundtorole_string_string"}, func(args ...ref.Val) ref.Val {
		if len(args) != 2 {
			return types.NewErr("boundToRole() takes a namespace and a service account name")
		}
		ns := scalarString(native(args[0]))
		sa := scalarString(native(args[1]))
		if sa == "" {
			return types.Bool(false)
		}
		for _, b := range idx.OfKind("RoleBinding", "ClusterRoleBinding") {
			subjects, _ := b.Object["subjects"].([]any)
			for _, s := range subjects {
				sm, ok := s.(map[string]any)
				if !ok {
					continue
				}
				if scalarString(sm["kind"]) != "ServiceAccount" || scalarString(sm["name"]) != sa {
					continue
				}
				// A RoleBinding's subject namespace defaults to the binding's
				// own. A ClusterRoleBinding's does not, and comparing without
				// that default would miss every cluster-scoped binding.
				subjectNS := scalarString(sm["namespace"])
				if subjectNS == "" {
					subjectNS = b.Namespace()
				}
				if subjectNS == ns || b.Kind() == "ClusterRoleBinding" {
					return types.Bool(true)
				}
			}
		}
		return types.Bool(false)
	})

	// selectorKeys() is every label key an object selects on, across the
	// several shapes a selector takes.
	add([]string{FnSelectorKeys, "selectorkeys_dyn"}, func(args ...ref.Val) ref.Val {
		out := []any{}
		if len(args) != 1 {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		obj, ok := native(args[0]).(map[string]any)
		if !ok {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		seen := map[string]bool{}
		emit := func(k string) {
			if k != "" && !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		collect := func(sel map[string]any) {
			if isLabelSelector(sel) {
				for k := range toStringMap(mapOf(sel["matchLabels"])) {
					emit(k)
				}
				exprs, _ := sel["matchExpressions"].([]any)
				for _, e := range exprs {
					if em, ok := e.(map[string]any); ok {
						emit(scalarString(em["key"]))
					}
				}
				return
			}
			for k := range toStringMap(sel) {
				emit(k)
			}
		}
		if sel, ok := mapField(obj, "spec", "selector"); ok {
			collect(sel)
		}
		if sel, ok := mapField(obj, "spec", "podSelector"); ok {
			collect(sel)
		}
		return types.DefaultTypeAdapter.NativeToValue(sortedAny(out))
	})

	// pvcMountPaths() is where a pod mounts persistent claims, which STO-07
	// needs in order to tell a chown of a local scratch directory from a chown
	// of a network filesystem that will take hours.
	add([]string{FnMountPaths, "pvcmountpaths_dyn"}, func(args ...ref.Val) ref.Val {
		out := []any{}
		w := resolve(idx, args)
		if w == nil {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		spec, ok := w.PodSpec()
		if !ok {
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
		claims := map[string]bool{}
		vols, _ := spec["volumes"].([]any)
		for _, v := range vols {
			vm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if _, isPVC := vm["persistentVolumeClaim"]; isPVC {
				claims[scalarString(vm["name"])] = true
			}
		}
		// A StatefulSet's volumeClaimTemplates are claims too, and they are the
		// ones a data workload actually uses.
		for _, t := range listField(w.Object, "spec", "volumeClaimTemplates") {
			if tm, ok := t.(map[string]any); ok {
				claims[stringField(tm, "metadata", "name")] = true
			}
		}
		for _, field := range []string{"initContainers", "containers"} {
			list, _ := spec[field].([]any)
			for _, c := range list {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				mounts, _ := cm["volumeMounts"].([]any)
				for _, m := range mounts {
					mm, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if claims[scalarString(mm["name"])] {
						out = append(out, scalarString(mm["mountPath"]))
					}
				}
			}
		}
		return types.DefaultTypeAdapter.NativeToValue(sortedAny(out))
	})

	unary := func(names []string, fn func(string) bool) {
		add(names, func(args ...ref.Val) ref.Val {
			if len(args) != 1 {
				return types.Bool(false)
			}
			return types.Bool(fn(scalarString(native(args[0]))))
		})
	}
	unary([]string{FnUnstableKey, "unstablelabelkey_string"}, IsUnstableLabelKey)
	unary([]string{FnLooksGen, "looksgenerated_string"}, LooksGenerated)
	unary([]string{FnPlaceholder, "placeholdercredential_string"}, IsPlaceholderCredential)
	unary([]string{FnExtendedRes, "extendedresource_string"}, IsExtendedResource)
	unary([]string{FnOperational, "operationalpath_string"}, IsOperationalPath)

	// The object's own metadata merged with its pod template's, for whichever
	// of the two maps is asked for. The pod template's wins where both carry a
	// key, which is the direction that matters: the template is what the pod
	// actually gets.
	mergedMeta := func(field string) func(args ...ref.Val) ref.Val {
		return func(args ...ref.Val) ref.Val {
			out := map[string]any{}
			if len(args) != 1 {
				return types.DefaultTypeAdapter.NativeToValue(out)
			}
			obj, ok := native(args[0]).(map[string]any)
			if !ok {
				return types.DefaultTypeAdapter.NativeToValue(out)
			}
			for k, v := range stringMapField(obj, "metadata", field) {
				out[k] = v
			}
			// Wherever this kind keeps its pod template - so a CronJob's are
			// included too.
			tmp := compliance.Resource{Object: obj}
			if path := compliance.PodSpecPath(tmp.Kind()); len(path) > 1 {
				meta := append(append([]string{}, path[:len(path)-1]...), "metadata", field)
				for k, v := range stringMapField(obj, meta...) {
					out[k] = v
				}
			}
			return types.DefaultTypeAdapter.NativeToValue(out)
		}
	}
	add([]string{FnAllLabels, "alllabels_dyn"}, mergedMeta("labels"))
	add([]string{FnAllAnnots, "allannotations_dyn"}, mergedMeta("annotations"))

	add([]string{FnRuleGrants, "rulegrants_dyn_string_list"}, func(args ...ref.Val) ref.Val {
		if len(args) != 3 {
			return types.NewErr("ruleGrants() takes a rule, a resource and a list of verbs")
		}
		rule, ok := native(args[0]).(map[string]any)
		if !ok {
			return types.Bool(false)
		}
		wantVerbs := stringsOf(native(args[2]))
		return types.Bool(RuleGrants(
			stringsOf(rule["resources"]), stringsOf(rule["verbs"]),
			scalarString(native(args[1])), wantVerbs))
	})

	add([]string{FnCredential, "lookslikecredential_string_string"}, func(args ...ref.Val) ref.Val {
		if len(args) != 2 {
			return types.Bool(false)
		}
		return types.Bool(LooksLikeCredential(scalarString(native(args[0])), scalarString(native(args[1]))))
	})

	return out
}

// isLabelSelector distinguishes a LabelSelector from a bare label map.
//
// The two are structurally different and both appear under `spec.selector`: a
// Service's is a bare map, everything else's is a LabelSelector. A check
// reading only one shape silently applies to half the objects it names, which
// is how the inherited network_policy.rego ends up asserting existence and
// nothing else.
func isLabelSelector(m map[string]any) bool {
	if m == nil {
		return false
	}
	_, hasML := m["matchLabels"]
	_, hasME := m["matchExpressions"]
	return hasML || hasME
}

// stringsOf reads a YAML list of strings, dropping anything that is not one.
func stringsOf(v any) []string {
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s := scalarString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func toStringMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func listField(doc map[string]any, path ...string) []any {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	l, _ := cur.([]any)
	return l
}

// sortedAny keeps returned lists deterministic. A check iterating a list whose
// order changed between runs would produce results in a different order, and
// "the same release checked twice is byte-identical" is a merge gate.
func sortedAny(in []any) []any {
	strs := make([]string, 0, len(in))
	for _, v := range in {
		strs = append(strs, scalarString(v))
	}
	for i := 1; i < len(strs); i++ {
		for j := i; j > 0 && strs[j] < strs[j-1]; j-- {
			strs[j], strs[j-1] = strs[j-1], strs[j]
		}
	}
	out := make([]any, 0, len(strs))
	for _, s := range strs {
		out = append(out, s)
	}
	return out
}
