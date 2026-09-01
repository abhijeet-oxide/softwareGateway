package cel

import "testing"

func TestParseQuantity(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		bad  bool
	}{
		{in: "1", want: 1},
		{in: "0.5", want: 0.5},
		{in: "250m", want: 0.25},
		{in: "1500m", want: 1.5},
		// The pair a string comparison gets wrong by 7%, which on a memory
		// limit is the difference between a pod that fits and one that does not.
		{in: "1G", want: 1e9},
		{in: "1Gi", want: 1 << 30},
		{in: "512Mi", want: 512 << 20},
		{in: "2Ki", want: 2048},
		{in: "1.5e3", want: 1500},
		{in: "100k", want: 100000},
		// "K" is not a Kubernetes suffix; accepting it would make this tool
		// agree with a manifest the API server rejects.
		{in: "1K", bad: true},
		{in: "", bad: true},
		{in: "abc", bad: true},
		{in: "1Gib", bad: true},
	}
	for _, c := range cases {
		got, err := ParseQuantity(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseQuantity(%q) = %v, want an error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseQuantity(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseQuantity(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseImageRef(t *testing.T) {
	cases := []struct {
		in                            string
		registry, repository, tag, dg string
	}{
		{in: "nginx", repository: "nginx"},
		{in: "nginx:1.25", repository: "nginx", tag: "1.25"},
		{in: "library/nginx:1.25", repository: "library/nginx", tag: "1.25"},
		{in: "reg.example.com/team/app:v1", registry: "reg.example.com", repository: "team/app", tag: "v1"},
		// The case the naive split gets wrong, and the one every disconnected
		// environment is full of.
		{in: "registry.example.com:5000/team/app:v1", registry: "registry.example.com:5000", repository: "team/app", tag: "v1"},
		{in: "localhost:5000/app", registry: "localhost:5000", repository: "app"},
		{in: "localhost/app", registry: "localhost", repository: "app"},
		{
			in: "reg.example.com/app@sha256:abc", registry: "reg.example.com",
			repository: "app", dg: "sha256:abc",
		},
		{
			in: "reg.example.com:5000/a/b:v2@sha256:def", registry: "reg.example.com:5000",
			repository: "a/b", tag: "v2", dg: "sha256:def",
		},
	}
	for _, c := range cases {
		got := ParseImageRef(c.in)
		if got.Registry != c.registry || got.Repository != c.repository || got.Tag != c.tag || got.Digest != c.dg {
			t.Errorf("ParseImageRef(%q) = {reg:%q repo:%q tag:%q digest:%q}, want {%q %q %q %q}",
				c.in, got.Registry, got.Repository, got.Tag, got.Digest, c.registry, c.repository, c.tag, c.dg)
		}
		if got.HasDigest() != (c.dg != "") {
			t.Errorf("ParseImageRef(%q).HasDigest() = %v", c.in, got.HasDigest())
		}
	}
}

func TestSelectorMatches(t *testing.T) {
	labels := map[string]string{"app": "etcd", "tier": "data"}

	expr := func(key, op string, values ...string) map[string]any {
		vs := make([]any, 0, len(values))
		for _, v := range values {
			vs = append(vs, v)
		}
		m := map[string]any{"key": key, "operator": op}
		if len(vs) > 0 {
			m["values"] = vs
		}
		return m
	}

	cases := []struct {
		name string
		sel  map[string]any
		want bool
	}{
		{"nil selector selects nothing", nil, false},
		{"empty selector selects everything in scope", map[string]any{}, true},
		{"matchLabels hit", map[string]any{"matchLabels": map[string]any{"app": "etcd"}}, true},
		{"matchLabels miss", map[string]any{"matchLabels": map[string]any{"app": "redis"}}, false},
		// The spelling the inherited pdb.rego ignores completely.
		{"matchExpressions In", map[string]any{"matchExpressions": []any{expr("app", "In", "etcd", "redis")}}, true},
		{"matchExpressions In miss", map[string]any{"matchExpressions": []any{expr("app", "In", "redis")}}, false},
		{"NotIn on a present label", map[string]any{"matchExpressions": []any{expr("app", "NotIn", "etcd")}}, false},
		{"NotIn on an absent label matches", map[string]any{"matchExpressions": []any{expr("zone", "NotIn", "a")}}, true},
		{"Exists", map[string]any{"matchExpressions": []any{expr("tier", "Exists")}}, true},
		{"Exists on an absent label", map[string]any{"matchExpressions": []any{expr("zone", "Exists")}}, false},
		{"DoesNotExist", map[string]any{"matchExpressions": []any{expr("zone", "DoesNotExist")}}, true},
		{"both forms must hold", map[string]any{
			"matchLabels":      map[string]any{"app": "etcd"},
			"matchExpressions": []any{expr("tier", "In", "control")},
		}, false},
		// An operator Kubernetes does not define makes the selector invalid.
		// Treating it as matching would silently widen the caller.
		{"unknown operator", map[string]any{"matchExpressions": []any{expr("app", "Contains", "etc")}}, false},
	}
	for _, c := range cases {
		if got := SelectorMatches(c.sel, labels); got != c.want {
			t.Errorf("%s: SelectorMatches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.0.1", -1},
		// The comparison string ordering gets wrong, and gets wrong exactly
		// when a component has been maintained long enough to matter.
		{"1.10.0", "1.9.0", 1},
		{"v1.10.0", "1.9.0", 1},
		{"2.0", "2.0.0", 0},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.1", 1},
		{"1.0.0+build9", "1.0.0+build1", 0},
	}
	for _, c := range cases {
		if got := CompareSemver(c.a, c.b); got != c.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
