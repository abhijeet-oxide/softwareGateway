package compliance

import "testing"

func doc() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name": "app",
			"annotations": map[string]any{
				// Annotation keys contain dots routinely, which is why a path
				// segment can be bracketed.
				"acme.example/quorum-size": "5",
			},
		},
		"spec": map[string]any{
			"replicas": int64(0),
			"template": map[string]any{"spec": map[string]any{
				"containers": []any{
					map[string]any{"name": "main", "resources": map[string]any{}},
					map[string]any{"name": "side"},
				},
			}},
		},
	}
}

func TestLookup(t *testing.T) {
	d := doc()
	cases := []struct {
		path  string
		want  any
		found bool
	}{
		{"metadata.name", "app", true},
		{"spec.replicas", int64(0), true},
		{"spec.template.spec.containers[1].name", "side", true},
		{"spec.template.spec.containers.0.name", "main", true},
		{"metadata.annotations[acme.example/quorum-size]", "5", true},
		{"spec.missing", nil, false},
		{"spec.template.spec.containers[9]", nil, false},
		{"metadata.name.deeper", nil, false},
	}
	for _, c := range cases {
		got, found := Lookup(d, c.path)
		if found != c.found {
			t.Errorf("Lookup(%q) found = %v, want %v", c.path, found, c.found)
			continue
		}
		if found && got != c.want {
			t.Errorf("Lookup(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// "Present" is what `required` means to somebody writing a check, and the
// distinction it draws is the one a vendor exploits by declaring a field and
// filling in nothing.
func TestPresent(t *testing.T) {
	d := doc()
	cases := []struct {
		path string
		want bool
	}{
		{"metadata.name", true},
		// A deliberate scale to zero is a set field, not a missing one.
		{"spec.replicas", true},
		// `resources: {}` does not satisfy "resources is required".
		{"spec.template.spec.containers[0].resources", false},
		{"spec.template.spec.containers[1].resources", false},
		{"spec.missing", false},
	}
	for _, c := range cases {
		if got := Present(d, c.path); got != c.want {
			t.Errorf("Present(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// Reaching a CronJob's containers is the engine's job, and getting it wrong is
// a silent false negative on every scheduled job in the estate.
func TestPodSpecPath(t *testing.T) {
	if got := PodSpecPath("CronJob"); len(got) != 5 || got[1] != "jobTemplate" {
		t.Errorf("CronJob pod spec path = %v, want spec.jobTemplate.spec.template.spec", got)
	}
	if got := PodSpecPath("Deployment"); len(got) != 3 {
		t.Errorf("Deployment pod spec path = %v", got)
	}
	if got := PodSpecPath("Pod"); len(got) != 1 {
		t.Errorf("Pod pod spec path = %v", got)
	}
	if PodSpecPath("ConfigMap") != nil {
		t.Error("a ConfigMap has no pod spec")
	}
	// Every kind the baseline calls a workload must actually be reachable.
	for _, k := range WorkloadKinds {
		if PodSpecPath(k) == nil {
			t.Errorf("%s is listed as a workload kind but has no pod spec path", k)
		}
	}
}
