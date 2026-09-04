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
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/ext"

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

	FnCovers       = "covers"
	FnReplicas     = "replicas"
	FnDeclaresPort = "declaresPort"
	FnBoundToRole  = "boundToRole"
	FnSelectorKeys = "selectorKeys"
	FnUnstableKey  = "unstableLabelKey"
	FnLooksGen     = "looksGenerated"
	FnCredential   = "looksLikeCredential"
	FnPlaceholder  = "placeholderCredential"
	FnExtendedRes  = "extendedResource"
	FnMountPaths   = "pvcMountPaths"
	FnOperational  = "operationalPath"
	FnRuleGrants   = "ruleGrants"
	FnAllLabels    = "allLabels"
	FnAllAnnots    = "allAnnotations"

	FnProbeHandler = "probeHandler"
	FnPodField     = "podField"
	FnSecValue     = "securityValue"
	FnSecSource    = "securitySource"
	FnSecLocus     = "securityLocus"
	FnRunsAsRoot   = "runsAsRoot"
	FnMountersOf   = "mountersOf"
	FnRuntimeKey   = "runtimeLabelKey"
	FnBuiltinGroup = "builtinApiGroup"
	FnDecode64     = "decodeBase64"
	FnCredClass    = "credentialClass"
	FnConfigRefs   = "configRefs"
	FnSorted       = "sorted"
	FnDisruptions  = "disruptionsAllowed"
	FnShippedName  = "shippedObjectName"
	FnSecretClass  = "secretMaterialClass"
	FnCredShape    = "credentialShape"
	FnVolumeState  = "volumeStateLabel"
	FnStateVolume  = "stateVolume"
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
func NewEnv() (*celgo.Env, error) { return newEnv(nil) }

// newRuntimeEnv builds the environment programs are PLANNED against: the same
// declarations, with the implementations bound to one run's resource index.
//
// Two environments rather than one because the two jobs happen at different
// times. Compilation - where every error an author can fix lives - happens when
// the pack loads, against declarations alone. Planning happens per run, because
// "which PodDisruptionBudget selects this workload" has no answer outside a
// release. Building it per run costs one environment construction and buys
// load-time diagnostics.
func newRuntimeEnv(idx *compliance.Index) (*celgo.Env, error) { return newEnv(idx) }

// impl is one engine function's implementation.
type impl = func(args ...ref.Val) ref.Val

// newEnv builds the environment. With a nil index the functions are declared
// and not implemented, which is all a type check needs.
func newEnv(idx *compliance.Index) (*celgo.Env, error) {
	dyn := celgo.DynType
	str := celgo.StringType
	b := celgo.BoolType
	dbl := celgo.DoubleType
	i := celgo.IntType
	mapSS := celgo.MapType(str, dyn)
	listDyn := celgo.ListType(dyn)
	listStr := celgo.ListType(str)

	impls := map[string]impl{}
	if idx != nil {
		for id, fn := range bindings(idx) {
			impls[id] = fn
		}
		for id, fn := range bindings2(idx) {
			impls[id] = fn
		}
		for id, fn := range bindings3(idx) {
			impls[id] = fn
		}
	}
	// overload declares a function overload, with its implementation when one
	// is available. Wrapping it here is what keeps the signature and the
	// implementation from drifting: there is one list, not two.
	overload := func(id string, args []*celgo.Type, result *celgo.Type) celgo.FunctionOpt {
		opts := []celgo.OverloadOpt{}
		if fn, ok := impls[id]; ok {
			opts = append(opts, celgo.FunctionBinding(fn))
		}
		return celgo.Overload(id, args, result, opts...)
	}

	return celgo.NewEnv(
		// String helpers - join, split, trim, indexOf - from cel-go itself, no
		// new dependency. They are what an `observed:` expression needs to
		// report a list of offending values rather than the word "invalid".
		ext.Strings(),

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
			overload("present_dyn_string", []*celgo.Type{dyn, str}, b)),
		celgo.Function(FnValue,
			overload("value_dyn_string", []*celgo.Type{dyn, str}, dyn)),
		celgo.Function(FnText,
			overload("text_dyn_string", []*celgo.Type{dyn, str}, str)),

		// Kubernetes semantics, implemented once.
		celgo.Function(FnQuantity,
			overload("quantity_dyn", []*celgo.Type{dyn}, dbl)),
		celgo.Function(FnImageRef,
			overload("imageref_string", []*celgo.Type{str}, mapSS)),
		celgo.Function(FnSelects,
			overload("selects_dyn_dyn", []*celgo.Type{dyn, dyn}, b)),

		// Cross-resource lookups. These are the reason an expression language
		// with no set logic is enough: the joins live in Go.
		celgo.Function(FnPDBFor,
			overload("pdbfor_dyn", []*celgo.Type{dyn}, dyn)),
		celgo.Function(FnServicesFor,
			overload("servicesfor_dyn", []*celgo.Type{dyn}, listDyn)),
		celgo.Function(FnSelected,
			overload("selectedby_dyn", []*celgo.Type{dyn}, listDyn)),
		celgo.Function(FnCRDFor,
			overload("crdfor_dyn", []*celgo.Type{dyn}, dyn)),
		celgo.Function(FnResourcesIn,
			overload("resourcesin_list", []*celgo.Type{listStr}, listDyn)),

		// covers() is the reverse of pdbFor: the workloads an object's own
		// selector matches. A PodDisruptionBudget covering a single-replica
		// workload blocks drains forever, and that is only visible from the
		// PDB's side.
		celgo.Function(FnCovers,
			overload("covers_dyn", []*celgo.Type{dyn}, listDyn)),
		// replicas() reads a workload's replica count with the Kubernetes
		// default of 1 for an absent field, so a check does not have to decide
		// what "no replicas key" means and get it wrong differently each time.
		celgo.Function(FnReplicas,
			overload("replicas_dyn", []*celgo.Type{dyn}, i)),
		// declaresPort() answers whether a container declares a port, by number
		// or by name. A probe pointing at a sidecar's port passes a health
		// check the application never answers.
		celgo.Function(FnDeclaresPort,
			overload("declaresport_dyn_dyn", []*celgo.Type{dyn, dyn}, b)),
		// boundToRole() reports whether the release binds a ServiceAccount to
		// any Role or ClusterRole. RBAC-02's exemption is DERIVED from this
		// rather than assumed, which is what makes it precise instead of
		// annoying.
		celgo.Function(FnBoundToRole,
			overload("boundtorole_string_string", []*celgo.Type{str, str}, b)),
		// selectorKeys() is every label key an object selects on, whichever of
		// the several selector shapes it uses.
		celgo.Function(FnSelectorKeys,
			overload("selectorkeys_dyn", []*celgo.Type{dyn}, listStr)),
		// pvcMountPaths() is where a pod mounts persistent volume claims.
		celgo.Function(FnMountPaths,
			overload("pvcmountpaths_dyn", []*celgo.Type{dyn}, listStr)),

		// The heuristics, each with a stated false-positive budget in
		// heuristics.go. Shared so there is one answer to "what does this tool
		// consider a credential?", which is the first question a vendor asks
		// when it is wrong about one.
		celgo.Function(FnUnstableKey,
			overload("unstablelabelkey_string", []*celgo.Type{str}, b)),
		celgo.Function(FnLooksGen,
			overload("looksgenerated_string", []*celgo.Type{str}, b)),
		celgo.Function(FnCredential,
			overload("lookslikecredential_string_string", []*celgo.Type{str, str}, b)),
		celgo.Function(FnPlaceholder,
			overload("placeholdercredential_string", []*celgo.Type{str}, b)),
		celgo.Function(FnExtendedRes,
			overload("extendedresource_string", []*celgo.Type{str}, b)),
		celgo.Function(FnOperational,
			overload("operationalpath_string", []*celgo.Type{str}, b)),
		// ruleGrants() honours the RBAC wildcards. A rule with resources: ["*"]
		// grants secrets, and a check comparing against the literal string
		// reports that it does not.
		celgo.Function(FnRuleGrants,
			overload("rulegrants_dyn_string_list", []*celgo.Type{dyn, str, listStr}, b)),
		// allLabels() is every label the object carries, its pod template's
		// included. An oversized value on a pod template is rejected at install
		// exactly as one on the controller is, and a check reading only the
		// controller's own metadata misses the half that charts actually
		// generate.
		celgo.Function(FnAllLabels,
			overload("alllabels_dyn", []*celgo.Type{dyn}, celgo.MapType(str, str))),
		// allAnnotations() is the same reading for annotations, and it exists
		// because that is where a chart DECLARES things: a rationale for a
		// toleration the standard forbids by default is written as an
		// annotation, and a chart may reasonably write it on the controller or
		// on the pod template. A check that read only one of the two would
		// reject a declaration that is plainly there.
		celgo.Function(FnAllAnnots,
			overload("allannotations_dyn", []*celgo.Type{dyn}, celgo.MapType(str, str))),

		// Probe comparison. A probe reduced to what it actually calls, with
		// the timing fields dropped and the defaults filled in, so two
		// probes are compared on the question the check is asking.
		celgo.Function(FnProbeHandler,
			overload("probehandler_dyn", []*celgo.Type{dyn}, str)),
		// A value from a workload's pod spec, wherever that kind keeps it -
		// two levels deeper on a CronJob than on everything else.
		celgo.Function(FnPodField,
			overload("podfield_dyn_string", []*celgo.Type{dyn, str}, dyn)),
		// The kubelet's own resolution order for a securityContext field:
		// container, then pod, then nothing. securitySource() names which of
		// the three it was, because those are three different fixes.
		celgo.Function(FnSecValue,
			overload("securityvalue_dyn_dyn_string", []*celgo.Type{dyn, dyn, str}, str)),
		celgo.Function(FnSecSource,
			overload("securitysource_dyn_dyn_string", []*celgo.Type{dyn, dyn, str}, str)),
		// The path the effective value actually came from. Pod-level answers
		// are absolute, so a finding does not send a reader to a container
		// field that is not in the manifest.
		celgo.Function(FnSecLocus,
			overload("securitylocus_dyn_dyn_string", []*celgo.Type{dyn, dyn, str}, str)),
		// Whether a whole workload runs anything as root. Undeclared is not
		// root: on a restricted cluster it is an assigned non-root UID.
		celgo.Function(FnRunsAsRoot,
			overload("runsasroot_dyn", []*celgo.Type{dyn}, b)),
		// The workloads that mount a claim, so a storage finding can name the
		// software rather than the volume.
		celgo.Function(FnMountersOf,
			overload("mountersof_dyn", []*celgo.Type{dyn}, listDyn)),
		// Labels Kubernetes adds itself at pod creation. A selector on one of
		// them cannot be matched against a rendered chart, and treating that as
		// "selects nothing" is a false finding on every per-replica Service.
		celgo.Function(FnRuntimeKey,
			overload("runtimelabelkey_string", []*celgo.Type{str}, b)),
		// Whether an apiVersion is served by the cluster itself, so the
		// custom-resource check stops asking for a definition of Deployment.
		celgo.Function(FnBuiltinGroup,
			overload("builtinapigroup_string", []*celgo.Type{str}, b)),
		// Secret values are base64. That is an encoding, not protection, and a
		// detector reading the encoded form finds nothing at all.
		celgo.Function(FnDecode64,
			overload("decodebase64_string", []*celgo.Type{str}, str)),
		// What class of credential a value looks like, for a finding that names
		// the material without reprinting it.
		celgo.Function(FnCredClass,
			overload("credentialclass_string_string", []*celgo.Type{str, str}, str)),
		// Every ConfigMap and Secret a pod asks for, from all six places a pod
		// spec can name one.
		celgo.Function(FnConfigRefs,
			overload("configrefs_dyn", []*celgo.Type{dyn}, listDyn)),
		// A fixed order for a list built from a map. Map iteration is
		// randomised, so a finding that lists offending keys comes out in a
		// different order on every run without it.
		celgo.Function(FnSorted,
			overload("sorted_list", []*celgo.Type{listDyn}, listStr)),
		// How many copies a maintenance rule lets the platform move at once,
		// computed rather than pattern-matched, and including the percentage
		// forms where the rounding is what produces the deadlock. -1 when the
		// rule states neither bound, -2 when it selects nothing.
		celgo.Function(FnDisruptions,
			overload("disruptionsallowed_dyn", []*celgo.Type{dyn}, i)),
		// Whether a literal is the name of a Secret or ConfigMap this release
		// ships - which makes it a reference rather than a credential.
		celgo.Function(FnShippedName,
			overload("shippedobjectname_string", []*celgo.Type{str}, b)),
		// What is inside one Secret value, including the credential fields of a
		// configuration file shipped as one value - which is where the
		// deliberately EMPTY password lives.
		celgo.Function(FnSecretClass,
			overload("secretmaterialclass_string_string", []*celgo.Type{str, str}, str)),
		// The conclusive half of the credential detector: what a value IS,
		// with no reference to what the field is called. Lets a check separate
		// what it reads from what it infers, and rate them differently.
		celgo.Function(FnCredShape,
			overload("credentialshape_string", []*celgo.Type{str}, str)),
		// What a volume means for the workload's data, and whether it means
		// anything at all: a hugepage allocation and a /sys mount are neither
		// scratch space nor a folder anybody stores data in.
		celgo.Function(FnVolumeState,
			overload("volumestatelabel_dyn", []*celgo.Type{dyn}, str)),
		celgo.Function(FnStateVolume,
			overload("statevolume_dyn", []*celgo.Type{dyn}, b)),

		// Version comparison, returning -1, 0 or 1. Semver rather than string
		// order, because "1.10.0" sorts before "1.9.0" as a string and a check
		// comparing operator versions that way is wrong exactly when it starts
		// to matter.
		celgo.Function(FnSemverCmp,
			overload("semvercompare_string_string", []*celgo.Type{str, str}, i)),
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

// programOptions are what every compiled expression is planned with.
func programOptions() []celgo.ProgramOption {
	return []celgo.ProgramOption{
		// The bound that makes termination a guarantee rather than a hope. An
		// expression exceeding it is an error, so the check reports `error` and
		// the run is inconclusive - never a pass.
		celgo.CostLimit(costLimit),
		celgo.EvalOptions(celgo.OptOptimize),
	}
}
