package compliance

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Resource is one Kubernetes object from the release, with the address the
// engine built for it.
//
// Object is the decoded YAML as nested maps and slices. It is deliberately not
// a typed Kubernetes struct: a release ships custom resources for operators
// this platform has never heard of, and a typed decode would either drop their
// fields or refuse them. Checks read paths, and a path works the same on a
// Deployment and on a vendor's own CR.
type Resource struct {
	Object  map[string]any
	Address Address

	// Chart is the chart this object was rendered from, so a check can read the
	// chart's own values and version without a lookup.
	Chart *Chart
}

// Chart is one rendered chart of the release.
type Chart struct {
	Name         string
	Version      string
	AppVersion   string
	Digest       string
	Ref          string
	SubchartPath string
	// Values is the chart's own default values, as shipped. Not the site's -
	// tier 1 does not have those, which is the whole reason determinacy is
	// measured rather than assumed.
	Values map[string]any
	// RenderStatus and RenderError record whether this chart produced
	// manifests. A chart that would not render is not a chart with no findings.
	RenderStatus string
	RenderError  string
}

// Render statuses.
const (
	RenderOK      = "ok"
	RenderFailed  = "failed"
	RenderSkipped = "skipped"
)

// APIVersion, Kind, Name, Namespace read the object's identity, tolerating the
// fields being absent or the wrong type - which is what a hand-written manifest
// in a release actually looks like.
func (r Resource) APIVersion() string { return stringAt(r.Object, "apiVersion") }
func (r Resource) Kind() string       { return stringAt(r.Object, "kind") }
func (r Resource) Name() string       { return stringAt(r.Object, "metadata", "name") }
func (r Resource) Namespace() string  { return stringAt(r.Object, "metadata", "namespace") }

// Labels and Annotations return the object's metadata maps, never nil, and with
// non-string values dropped rather than coerced: a label whose value YAML
// parsed as a bool was already invalid to Kubernetes, and inventing "true" for
// it would make this tool agree with a manifest the API server rejects.
func (r Resource) Labels() map[string]string { return stringMapAt(r.Object, "metadata", "labels") }
func (r Resource) Annotations() map[string]string {
	return stringMapAt(r.Object, "metadata", "annotations")
}

// PodSpecPath is where a workload keeps its pod template, by kind.
//
// # Why this is a table and not spec.template.spec
//
// A CronJob's pod spec is one level deeper than every other workload's, behind
// spec.jobTemplate. Seven of the sixteen policies this platform inherited list
// CronJob among the kinds they check and then read spec.template.spec: they
// match the object, find nothing there, and report nothing. Every scheduled job
// in the estate passes every one of those checks without being looked at.
//
// That is a silent false negative, which is the failure mode this package is
// built to make impossible, so the traversal is written once, here, and no
// check performs it.
func PodSpecPath(kind string) []string {
	switch kind {
	case "Pod":
		return []string{"spec"}
	case "CronJob":
		return []string{"spec", "jobTemplate", "spec", "template", "spec"}
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "ReplicationController":
		return []string{"spec", "template", "spec"}
	default:
		return nil
	}
}

// WorkloadKinds is every kind that carries a pod template, in the order a
// person lists them. Used by the baseline pack and by the fixture corpus, so
// "every workload kind" means the same set everywhere and adding one is a
// single edit.
var WorkloadKinds = []string{
	"Pod", "Deployment", "StatefulSet", "DaemonSet",
	"ReplicaSet", "ReplicationController", "Job", "CronJob",
}

// PodSpec returns the object's pod spec, and whether it has one.
func (r Resource) PodSpec() (map[string]any, bool) {
	path := PodSpecPath(r.Kind())
	if path == nil {
		return nil, false
	}
	m, ok := mapAt(r.Object, path...)
	return m, ok
}

// PodLabels returns the labels of the pods this workload creates - which are
// the template's, not the workload's.
//
// The distinction matters for every check that joins a workload to a Service or
// a PodDisruptionBudget: those select pods, and a workload whose own labels
// differ from its template's is normal and is exactly the case where a
// selector-matching bug hides.
func (r Resource) PodLabels() map[string]string {
	if r.Kind() == "Pod" {
		return r.Labels()
	}
	path := PodSpecPath(r.Kind())
	if path == nil {
		return nil
	}
	// The template's metadata sits one level above its spec.
	meta := append(append([]string{}, path[:len(path)-1]...), "metadata", "labels")
	return stringMapAt(r.Object, meta...)
}

// Container is one container of a workload, as a subject in its own right.
type Container struct {
	// Object is the container's own map, so a path in a check is relative to
	// the container - "resources.limits.memory", not the full workload path.
	Object map[string]any
	Name   string
	// Type is main, init or ephemeral.
	Type string
	// Index is the position in its own list, needed to build the locus a vendor
	// navigates by.
	Index int
	// Owner is the workload. A container check that needs the workload's kind
	// or replica count reads it here rather than being handed a bare map.
	Owner *Resource
}

// Path is the container's location within its workload, in the form a person
// finds it by: "spec.template.spec.containers[2]".
func (c Container) Path() string {
	base := "containers"
	switch c.Type {
	case ContainerInit:
		base = "initContainers"
	case ContainerEphemeral:
		base = "ephemeralContainers"
	}
	prefix := ""
	if c.Owner != nil {
		if p := PodSpecPath(c.Owner.Kind()); p != nil {
			prefix = strings.Join(p, ".") + "."
		}
	}
	return prefix + base + "[" + strconv.Itoa(c.Index) + "]"
}

// Containers returns the workload's containers in the scope requested.
//
// # Why init containers are in "all"
//
// An init container with no memory limit gets the pod OOM-killed exactly as a
// main one does, and an init container running as root has root on the node's
// mounts. The policy review found this omission in the shipped resource-limits
// policy, and it is the kind of gap that survives because the check looks
// thorough. "all" means all.
func (r *Resource) Containers(scope ContainerScope) []Container {
	spec, ok := r.PodSpec()
	if !ok {
		return nil
	}
	var out []Container
	collect := func(field, typ string) {
		list, _ := spec[field].([]any)
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, Container{
				Object: m,
				Name:   stringAt(m, "name"),
				Type:   typ,
				Index:  i,
				Owner:  r,
			})
		}
	}
	switch scope {
	case ScopeMain:
		collect("containers", ContainerMain)
	case ScopeInit:
		collect("initContainers", ContainerInit)
	case ScopeAll:
		collect("initContainers", ContainerInit)
		collect("containers", ContainerMain)
		collect("ephemeralContainers", ContainerEphemeral)
	}
	return out
}

// Release is everything one run judges: the charts, and every object rendered
// from them.
type Release struct {
	Product       string
	Tag           string
	PackageDigest string

	Charts    []*Chart
	Resources []Resource

	// Config is site configuration a check may consult - approved registries,
	// probe bounds. Data, not policy: the rule lives in the check and the list
	// of registries this organization runs lives in configuration, because one
	// changes on a standards review and the other when a datacentre opens.
	Config map[string]any

	// Rendered records what produced this release, so a result can say what it
	// was derived from. Rule 5: reproducible, or it is an opinion.
	HelmVersion  string
	KubeVersion  string
	BundleDigest string
}

// Index is the cross-resource lookup structure the engine functions read.
//
// # Why it is built once per run
//
// The joins checks need - which PDB selects this workload, which Service
// selects it, which CRD defines this custom resource - are O(n) scans each. A
// 600-resource release with 88 checks would do that scan tens of thousands of
// times. Built once, it is a map lookup, and the cost estimate a check is
// compiled against stays honest.
type Index struct {
	release *Release

	byKind map[string][]*Resource
	byName map[string]*Resource
}

// BuildIndex indexes a release for cross-resource lookups.
func BuildIndex(rel *Release) *Index {
	idx := &Index{
		release: rel,
		byKind:  make(map[string][]*Resource, 32),
		byName:  make(map[string]*Resource, len(rel.Resources)),
	}
	for i := range rel.Resources {
		r := &rel.Resources[i]
		k := r.Kind()
		idx.byKind[k] = append(idx.byKind[k], r)
		idx.byName[objectKey(r.APIVersion(), k, r.Namespace(), r.Name())] = r
	}
	for k := range idx.byKind {
		list := idx.byKind[k]
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Namespace() != list[j].Namespace() {
				return list[i].Namespace() < list[j].Namespace()
			}
			return list[i].Name() < list[j].Name()
		})
	}
	return idx
}

// Release is the release this index was built from.
func (i *Index) Release() *Release { return i.release }

// OfKind returns every object of a kind, in a stable order.
func (i *Index) OfKind(kinds ...string) []*Resource {
	if len(kinds) == 1 {
		return i.byKind[kinds[0]]
	}
	var out []*Resource
	for _, k := range kinds {
		out = append(out, i.byKind[k]...)
	}
	return out
}

// Get returns one object by identity, or nil.
func (i *Index) Get(apiVersion, kind, namespace, name string) *Resource {
	return i.byName[objectKey(apiVersion, kind, namespace, name)]
}

func objectKey(apiVersion, kind, namespace, name string) string {
	return apiVersion + "|" + kind + "|" + namespace + "|" + name
}

// Subject is what one check judges: an object, or one of its containers.
//
// The engine builds subjects from AppliesTo and binds them one at a time. A
// check never sees the release and never loops, which is what makes Rule 1 -
// one result, one resource - structural rather than a convention an author has
// to remember.
type Subject struct {
	Resource  *Resource
	Container *Container
	// PodSpec is set when the check declared subject: podSpec. The engine did
	// the traversal, so a check reads "hostNetwork" and is right about a
	// CronJob too.
	PodSpec map[string]any
	Address Address
}

// Value is what the check's paths are resolved against: the container when the
// check scoped to containers, the pod spec when it scoped to podSpec, the whole
// object otherwise.
func (s Subject) Value() map[string]any {
	if s.Container != nil {
		return s.Container.Object
	}
	if s.PodSpec != nil {
		return s.PodSpec
	}
	if s.Resource != nil {
		return s.Resource.Object
	}
	return nil
}

// Describe names the subject the way a message does.
func (s Subject) Describe() string {
	if s.Container != nil {
		return fmt.Sprintf("%s %s container %q", s.Resource.Kind(), s.Resource.Name(), s.Container.Name)
	}
	if s.Resource != nil {
		return fmt.Sprintf("%s %s", s.Resource.Kind(), s.Resource.Name())
	}
	return ""
}

// PodSpecPrefix is the dotted path from the object to the pod spec, for
// building a locus a vendor can navigate by.
func (s Subject) PodSpecPrefix() string {
	if s.Resource == nil {
		return ""
	}
	p := PodSpecPath(s.Resource.Kind())
	if p == nil {
		return ""
	}
	return strings.Join(p, ".")
}
