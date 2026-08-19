package compare

import "testing"

// The complaint: "I analysed the package, and the comparison is no faster."
//
// It was not, and analysis was not the reason. Analysis caches manifests, and
// manifests stopped being the expensive part the moment file comparison was
// switched on — every layer of every changed component was downloaded, on both
// sides, to learn what the manifests had already said.
//
// A titled layer names its file and states its content digest for free. This
// pins down that only the layers which could still say something are opened.
func TestOnlyLayersThatCanStillSaySomethingAreOpened(t *testing.T) {
	a := []Layer{
		{Digest: "sha256:aa", Title: "CONFIG/nodes.json"},
		{Digest: "sha256:bb", Title: "CONFIG/routes.json"},
		{Digest: "sha256:cc", Title: "notes/RELEASE.md"},
		{Digest: "sha256:dd"},
	}
	b := []Layer{
		{Digest: "sha256:aa", Title: "CONFIG/nodes.json"},
		{Digest: "sha256:b2", Title: "CONFIG/routes.json"},
		{Digest: "sha256:ee", Title: "notes/UPGRADE.md"},
		{Digest: "sha256:ff"},
	}

	open := worthOpening(a, b)

	cases := []struct {
		layer Layer
		want  bool
		why   string
	}{
		{a[0], false, "identical on both sides — byte-identical content cannot differ"},
		{a[1], true, "the same path with different content: only the archives say which lines"},
		{b[1], true, "the same, from the other side — the set must be symmetric"},
		{a[2], false, "removed outright; opening it cannot change that it is gone"},
		{b[2], false, "added outright; opening it cannot change that it is new"},
		{a[3], true, "untitled: opening it is the only way to learn one path inside it"},
	}
	for _, c := range cases {
		name := c.layer.Title
		if name == "" {
			name = c.layer.Digest
		}
		if got := open(c.layer); got != c.want {
			t.Errorf("open(%s) = %v, want %v — %s", name, got, c.want, c.why)
		}
	}
}

// Opening an archive on one side and not the other would compare its inner
// paths against the other side's archive NAME, and report every file in it as
// both added and removed. The set is shared, so that cannot happen.
func TestTheOpenDecisionIsTheSameOnBothSides(t *testing.T) {
	a := []Layer{{Digest: "sha256:11", Title: "bundle.tar.gz"}}
	b := []Layer{{Digest: "sha256:22", Title: "bundle.tar.gz"}}

	open := worthOpening(a, b)
	if !open(a[0]) || !open(b[0]) {
		t.Fatal("one side would be opened and the other not, which fabricates a " +
			"finding for every path inside the archive")
	}
}
