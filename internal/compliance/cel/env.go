// Package cel compiles declared checks into evaluable programs.
//
// # Why CEL and not Rego
//
// Three reasons, argued in full in docs/compliance/02-authoring-checks.md §7
// and recorded as decision 5 in docs/design/23-compliance.md.
//
// Measured dependency cost, against this repository: embedding OPA's rego
// package adds 18 modules to the compiled binary and 59 to the module graph an
// audit has to enumerate - among them a WebAssembly runtime, an embedded
// key-value store, secp256k1 and two separate Levenshtein implementations.
// cel-go adds 4 and 3. A tool whose purpose is telling people what is inside
// their software does not quietly add a wasm runtime to itself.
//
// Robustness: CEL is not Turing-complete. It has no recursion and no unbounded
// loop, and a program's cost is estimated before it runs. Bounded evaluation is
// therefore a property of the language rather than a timeout the engine hopes
// fires in time - and the four capability flags a Rego evaluator has to set
// correctly (no network, no filesystem, no clock, no runaway comprehension)
// reduce to three that do not exist and one that is refused at compile time.
//
// Direction: CEL is what Kubernetes chose for CRD validation rules and
// ValidatingAdmissionPolicy. An engineer who learns it here uses it again.
//
// # The division of labour
//
// Expressions do not do cross-resource work by hand. Label-selector semantics,
// quantity parsing and image-reference parsing are engine functions - Go, unit
// tested once, correct for every caller - because §3.3 of the policy review is
// what happens when each policy re-implements selector matching: the shipped
// pdb.rego ignores matchExpressions, ignores namespaces, and catches one of the
// four spellings of a disruption deadlock.
package cel

import (
	"fmt"

	celgo "cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// Variables bound for every expression.
const (
	// VarSelf is the subject: one resource, or one container when the check
	// scopes to containers. Never the release - a check cannot accidentally
	// judge something outside its own denominator because it is never handed
	// one.
	VarSelf = "self"
	// VarOwner is the workload owning self, when self is a container.
	VarOwner = "owner"
	// VarAddress is where the subject is - chart, version, digest, file, line.
	VarAddress = "address"
	// VarChart is the chart self came from, with its own default values.
	VarChart = "chart"
	// VarRelease is product, tag and package digest.
	VarRelease = "release"
	// VarConfig is site configuration: approved registries, probe bounds. Data
	// the check consults, never policy - the rule changes on a standards review
	// and the registry list changes when a datacentre opens.
	VarConfig = "config"
)

// Function names, as an author writes them.
const (
	FnPresent     = "present"
	FnValue       = "value"
	FnText        = "text"
	FnQuantity    = "quantity"
	FnImageRef    = "imageRef"
	FnSelects     = "selects"
	FnPDBFor      = "pdbFor"
	FnServicesFor = "servicesFor"
	FnSelected    = "selectedBy"
	FnCRDFor      = "crdFor"
	FnResourcesIn = "resourcesIn"
	FnSemverCmp   = "semverCompare"
)

// costLimit bounds any single evaluation.
//
// # Why a number and not a timeout
//
// A timeout measures wall clock, so the same expression passes on an idle
// Coordinator and fails on a busy one, and a run's results stop being
// reproducible - which Rule 5 forbids. Cost counts operations, so the limit
// means the same thing on every machine and on every run.
//
// The value is generous: a check reaching this has written a cross product over
// a large release, which is the shape the limit exists to refuse.
const costLimit = 1_000_000

// NewEnv builds the compilation environment: the variables and the function
// SIGNATURES.
//
// The implementations are not here. They close over one run's resource index
// and are supplied when the program is planned (see funcs.go), which is what
// lets a check be type-checked at load, in front of the person editing it,
// while still being able to ask which PodDisruptionBudget selects a workload at
// run time.
func NewEnv() (*celgo.Env, error) {
	dyn := celgo.DynType
	str := celgo.StringType
	b := celgo.BoolType
	dbl := celgo.DoubleType
	i := celgo.IntType
	mapSS := celgo.MapType(str, dyn)
	listDyn := celgo.ListType(dyn)
	listStr := celgo.ListType(str)

	return celgo.NewEnv(
		celgo.Variable(VarSelf, dyn),
		celgo.Variable(VarOwner, dyn),
		celgo.Variable(VarAddress, mapSS),
		celgo.Variable(VarChart, mapSS),
		celgo.Variable(VarRelease, mapSS),
		celgo.Variable(VarConfig, mapSS),

		// Path access that is absent-safe. A raw `self.spec.foo` on a document
		// without spec.foo is an evaluation error in CEL, and an author writing
		// eighty checks would get that wrong eighty times - each one turning a
		// compliant chart into an undecidable result. These take the path as a
		// string, so a missing field is a value, not a fault.
		celgo.Function(FnPresent,
			celgo.Overload("present_dyn_string", []*celgo.Type{dyn, str}, b)),
		celgo.Function(FnValue,
			celgo.Overload("value_dyn_string", []*celgo.Type{dyn, str}, dyn)),
		celgo.Function(FnText,
			celgo.Overload("text_dyn_string", []*celgo.Type{dyn, str}, str)),

		// Kubernetes semantics, implemented once.
		celgo.Function(FnQuantity,
			celgo.Overload("quantity_dyn", []*celgo.Type{dyn}, dbl)),
		celgo.Function(FnImageRef,
			celgo.Overload("imageref_string", []*celgo.Type{str}, mapSS)),
		celgo.Function(FnSelects,
			celgo.Overload("selects_dyn_dyn", []*celgo.Type{dyn, dyn}, b)),

		// Cross-resource lookups. These are the reason an expression language
		// with no set logic is enough: the joins live in Go.
		celgo.Function(FnPDBFor,
			celgo.Overload("pdbfor_dyn", []*celgo.Type{dyn}, dyn)),
		celgo.Function(FnServicesFor,
			celgo.Overload("servicesfor_dyn", []*celgo.Type{dyn}, listDyn)),
		celgo.Function(FnSelected,
			celgo.Overload("selectedby_dyn", []*celgo.Type{dyn}, listDyn)),
		celgo.Function(FnCRDFor,
			celgo.Overload("crdfor_dyn", []*celgo.Type{dyn}, dyn)),
		celgo.Function(FnResourcesIn,
			celgo.Overload("resourcesin_list", []*celgo.Type{listStr}, listDyn)),

		// Version comparison, returning -1, 0 or 1. Semver rather than string
		// order, because "1.10.0" sorts before "1.9.0" as a string and a check
		// comparing operator versions that way is wrong exactly when it starts
		// to matter.
		celgo.Function(FnSemverCmp,
			celgo.Overload("semvercompare_string_string", []*celgo.Type{str, str}, i)),
	)
}

// Bindings is the activation for one subject.
func activation(subj compliance.Subject, rel *compliance.Release) map[string]any {
	self := any(subj.Value())
	owner := any(map[string]any{})
	if subj.Resource != nil {
		owner = subj.Resource.Object
	}
	chart := map[string]any{"name": "", "version": "", "appVersion": "", "values": map[string]any{}}
	if subj.Resource != nil && subj.Resource.Chart != nil {
		c := subj.Resource.Chart
		chart = map[string]any{
			"name": c.Name, "version": c.Version, "appVersion": c.AppVersion,
			"digest": c.Digest, "ref": c.Ref, "subchartPath": c.SubchartPath,
			"values": orEmpty(c.Values),
		}
	}
	a := subj.Address
	return map[string]any{
		VarSelf:  self,
		VarOwner: owner,
		VarChart: chart,
		VarAddress: map[string]any{
			"product": a.Product, "release": a.Release, "packageDigest": a.PackageDigest,
			"chart": a.Chart, "chartVersion": a.ChartVersion, "subchartPath": a.SubchartPath,
			"artifactDigest": a.ArtifactDigest, "artifactRef": a.ArtifactRef,
			"sourceFile": a.SourceFile, "apiVersion": a.APIVersion, "kind": a.Kind,
			"namespace": a.Namespace, "name": a.Name,
			"container": a.Container, "containerType": a.ContainerType,
		},
		VarRelease: map[string]any{
			"product": rel.Product, "tag": rel.Tag, "packageDigest": rel.PackageDigest,
		},
		VarConfig: orEmpty(rel.Config),
	}
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// asBool reads a CEL result as a boolean, refusing anything else.
//
// An expression that returned a string is a check whose author wrote a value
// where a condition belongs. Treating a non-empty string as true would make
// that check pass everything, silently, which is the failure this package is
// built to prevent - so it is an error and the result is undecidable.
func asBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case types.Bool:
		return bool(t), nil
	}
	return false, fmt.Errorf("expression returned %T, not a boolean: an assertion must be a condition", v)
}
