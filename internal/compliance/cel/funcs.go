package cel

import (
	"math"
	"strconv"
	"strings"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/interpreter/functions"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// bindings supplies the implementations behind the signatures declared in
// env.go, closed over one run's resource index.
//
// # Why they are Go and not expressions
//
// Every function here is a Kubernetes semantic that is easy to get subtly
// wrong, and each one was got wrong at least once in the sixteen policies this
// platform inherited. Written here, they are wrong at most once and unit-tested
// against the behaviour they model; written per check, they are wrong in a
// different way in each of the eighty-eight.
//
// The one in this file that most deserves the treatment is selects(). The
// shipped pdb.rego compares matchLabels only, ignores matchExpressions
// entirely, and ignores namespaces - so it silently passes every workload
// covered by a selector written the modern way, and silently matches a PDB in
// one namespace against a workload in another.
func bindings(idx *compliance.Index) []*functions.Overload {
	var list []*functions.Overload
	add := func(names []string, impl func(args ...ref.Val) ref.Val) {
		for _, n := range names {
			list = append(list, &functions.Overload{Operator: n, Function: impl})
		}
	}

	add([]string{FnPresent, "present_dyn_string"}, func(args ...ref.Val) ref.Val {
		doc, path, ok := docAndPath(args)
		if !ok {
			return types.Bool(false)
		}
		return types.Bool(compliance.Present(doc, path))
	})

	add([]string{FnValue, "value_dyn_string"}, func(args ...ref.Val) ref.Val {
		doc, path, ok := docAndPath(args)
		if !ok {
			return types.NullValue
		}
		v, found := compliance.Lookup(doc, path)
		if !found || v == nil {
			return types.NullValue
		}
		return types.DefaultTypeAdapter.NativeToValue(v)
	})

	add([]string{FnText, "text_dyn_string"}, func(args ...ref.Val) ref.Val {
		doc, path, ok := docAndPath(args)
		if !ok {
			return types.String("")
		}
		v, found := compliance.Lookup(doc, path)
		if !found {
			return types.String("")
		}
		return types.String(scalarString(v))
	})

	add([]string{FnQuantity, "quantity_dyn"}, func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.NewErr("quantity() takes one argument")
		}
		n, err := ParseQuantity(scalarString(native(args[0])))
		if err != nil {
			return types.NewErr("%s", err.Error())
		}
		return types.Double(n)
	})

	add([]string{FnImageRef, "imageref_string"}, func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.NewErr("imageRef() takes one argument")
		}
		return types.DefaultTypeAdapter.NativeToValue(ParseImageRef(scalarString(native(args[0]))).Map())
	})

	add([]string{FnSelects, "selects_dyn_dyn"}, func(args ...ref.Val) ref.Val {
		if len(args) != 2 {
			return types.NewErr("selects() takes a selector and an object")
		}
		sel, _ := native(args[0]).(map[string]any)
		obj := native(args[1])
		return types.Bool(SelectorMatches(sel, labelsOf(obj)))
	})

	add([]string{FnPDBFor, "pdbfor_dyn"}, func(args ...ref.Val) ref.Val {
		r := resolve(idx, args)
		if r == nil {
			return types.DefaultTypeAdapter.NativeToValue(map[string]any{})
		}
		for _, pdb := range idx.OfKind("PodDisruptionBudget") {
			if pdb.Namespace() != r.Namespace() {
				continue
			}
			sel, _ := mapField(pdb.Object, "spec", "selector")
			if SelectorMatches(sel, r.PodLabels()) {
				return types.DefaultTypeAdapter.NativeToValue(pdb.Object)
			}
		}
		return types.DefaultTypeAdapter.NativeToValue(map[string]any{})
	})

	add([]string{FnServicesFor, "servicesfor_dyn"}, func(args ...ref.Val) ref.Val {
		r := resolve(idx, args)
		out := []any{}
		if r != nil {
			labels := r.PodLabels()
			for _, svc := range idx.OfKind("Service") {
				if svc.Namespace() != r.Namespace() {
					continue
				}
				sel := stringMapField(svc.Object, "spec", "selector")
				if len(sel) == 0 {
					// A Service with no selector is routed by hand-written
					// Endpoints. It does not select this workload and must not
					// be reported as covering it - an empty selector matching
					// everything is the classic label-selector inversion.
					continue
				}
				if matchLabels(sel, labels) {
					out = append(out, svc.Object)
				}
			}
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	})

	add([]string{FnSelected, "selectedby_dyn"}, func(args ...ref.Val) ref.Val {
		svc := resolve(idx, args)
		out := []any{}
		if svc != nil {
			sel := stringMapField(svc.Object, "spec", "selector")
			if len(sel) > 0 {
				for _, w := range idx.OfKind(compliance.WorkloadKinds...) {
					if w.Namespace() != svc.Namespace() {
						continue
					}
					if matchLabels(sel, w.PodLabels()) {
						out = append(out, w.Object)
					}
				}
			}
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	})

	add([]string{FnCRDFor, "crdfor_dyn"}, func(args ...ref.Val) ref.Val {
		cr := resolve(idx, args)
		if cr == nil {
			return types.DefaultTypeAdapter.NativeToValue(map[string]any{})
		}
		group, _, _ := strings.Cut(cr.APIVersion(), "/")
		kind := cr.Kind()
		for _, crd := range idx.OfKind("CustomResourceDefinition") {
			if stringField(crd.Object, "spec", "group") != group {
				continue
			}
			if stringField(crd.Object, "spec", "names", "kind") != kind {
				continue
			}
			return types.DefaultTypeAdapter.NativeToValue(crd.Object)
		}
		return types.DefaultTypeAdapter.NativeToValue(map[string]any{})
	})

	add([]string{FnResourcesIn, "resourcesin_list"}, func(args ...ref.Val) ref.Val {
		if len(args) != 1 {
			return types.NewErr("resourcesIn() takes a list of kinds")
		}
		var kinds []string
		if list, ok := native(args[0]).([]any); ok {
			for _, k := range list {
				kinds = append(kinds, scalarString(k))
			}
		}
		out := []any{}
		for _, r := range idx.OfKind(kinds...) {
			out = append(out, r.Object)
		}
		return types.DefaultTypeAdapter.NativeToValue(out)
	})

	add([]string{FnSemverCmp, "semvercompare_string_string"}, func(args ...ref.Val) ref.Val {
		if len(args) != 2 {
			return types.NewErr("semverCompare() takes two versions")
		}
		return types.Int(CompareSemver(scalarString(native(args[0])), scalarString(native(args[1]))))
	})

	return list
}

// programOptions are the run-time options every compiled expression is planned
// with.
func programOptions(idx *compliance.Index) []celgo.ProgramOption {
	return []celgo.ProgramOption{
		celgo.Functions(bindings(idx)...),
		// The bound that makes termination a guarantee rather than a hope. An
		// expression exceeding it is an error, so the check reports `error` and
		// the run is inconclusive - never a pass.
		celgo.CostLimit(costLimit),
		celgo.EvalOptions(celgo.OptOptimize),
	}
}

// resolve finds the indexed resource an expression handed us.
//
// Expressions pass plain maps - `self`, or an element of a list a previous
// function returned - so the identity has to be recovered from the document.
// Matching on apiVersion/kind/namespace/name is exact and cannot alias: two
// objects with all four equal are the same object to Kubernetes too.
func resolve(idx *compliance.Index, args []ref.Val) *compliance.Resource {
	if len(args) != 1 {
		return nil
	}
	obj, ok := native(args[0]).(map[string]any)
	if !ok {
		return nil
	}
	tmp := compliance.Resource{Object: obj}
	return idx.Get(tmp.APIVersion(), tmp.Kind(), tmp.Namespace(), tmp.Name())
}

func docAndPath(args []ref.Val) (any, string, bool) {
	if len(args) != 2 {
		return nil, "", false
	}
	return native(args[0]), scalarString(native(args[1])), true
}

// native converts a CEL value back to the Go shapes the rest of this package
// reads: map[string]any, []any and scalars.
//
// # Why not ConvertToNative(any)
//
// It returns map[interface{}]interface{} for a map, which every type assertion
// in compliance.Lookup and in the helpers below fails on - silently, producing
// "field absent" for a document that has the field. That is a check quietly
// passing or failing everything, which is the one class of bug this package
// exists to make impossible, so the conversion is explicit and total.
//
// Values adapted from Go natives come back from Value() already in the right
// shape; values CEL itself constructed - a list literal, a map comprehension -
// do not, and are walked.
func native(v ref.Val) any {
	if v == nil {
		return nil
	}
	switch t := v.(type) {
	case traits.Mapper:
		out := make(map[string]any, int(t.Size().(types.Int)))
		it := t.Iterator()
		for it.HasNext() == types.True {
			k := it.Next()
			key, ok := native(k).(string)
			if !ok {
				// A non-string key cannot have come from a Kubernetes
				// document, and coercing it would invent a field name.
				continue
			}
			out[key] = native(t.Get(k))
		}
		return out
	case traits.Lister:
		n := int(t.Size().(types.Int))
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, native(t.Get(types.Int(i))))
		}
		return out
	case types.Null:
		return nil
	}
	return v.Value()
}

// scalarString renders a scalar the way a manifest wrote it.
//
// Numbers are formatted without a trailing ".0", because `replicas: 3` in a
// report must read as 3. A check comparing against "3.0" would be comparing
// against something no manifest contains.
func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case types.String:
		return string(t)
	default:
		return ""
	}
}

func labelsOf(obj any) map[string]string {
	m, ok := obj.(map[string]any)
	if !ok {
		return nil
	}
	return stringMapField(m, "metadata", "labels")
}

func mapField(doc map[string]any, path ...string) (map[string]any, bool) {
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
	m, ok := cur.(map[string]any)
	return m, ok
}

func stringField(doc map[string]any, path ...string) string {
	var cur any = doc
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

func stringMapField(doc map[string]any, path ...string) map[string]string {
	m, ok := mapField(doc, path...)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func matchLabels(want, have map[string]string) bool {
	if len(want) == 0 {
		return false
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
