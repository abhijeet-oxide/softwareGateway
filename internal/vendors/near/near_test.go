package near

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/vendors"
)

// Digests and sizes below are the real ones, taken from manifests pulled from a
// live NEAR registry. Fixtures invented from the specification would have
// missed the two things that actually bite: the wrapper's payload child carries
// NO mediaType, and the signature layer is PKCS#7 rather than anything Sigstore
// understands.
const (
	payloadDigest   = "sha256:3fc4cbf79451e24ac789b166a278f6c3e61c004a04d85115c37906812a56c3b6"
	signatureDigest = "sha256:999283c6c6567c04889b58f54027a05b7adccde10cf199b392c3e107b9333e28"
	wrapperDigest   = "sha256:aaaa0000bbbb1111cccc2222dddd3333eeee4444ffff5555aaaa6666bbbb7777"
)

// wrapperTag builds `signed_orb_23.8.1076` as the registry actually returns it.
func wrapperTag() vendors.ScannedTag {
	return vendors.ScannedTag{
		Tag: "signed_orb_23.8.1076",
		Descriptor: registry.Descriptor{
			MediaType: registry.MediaTypeOCIIndex,
			Digest:    registry.Digest(wrapperDigest),
			Size:      875,
		},
		Annotations: map[string]string{
			"org.opencontainers.image.vendor":  "Nokia",
			"org.opencontainers.image.created": "2024-06-12T18:05:59Z",
			"com.nokia.ncd.orb.rb.name":        "CFX-5000-k8s",
			"com.nokia.ncd.orb.rb.version":     "signed_orb_23.8.1076",
		},
		Children: []registry.Descriptor{
			{
				MediaType: registry.MediaTypeOCIManifest,
				Digest:    registry.Digest(signatureDigest),
				Size:      814,
				Annotations: map[string]string{
					annRefName: "orbs/CFX-5000-k8s:signature_orb_23.8.1076",
					annOrbType: orbTypeSignature,
				},
			},
			{
				// NO MediaType - the live registry omits it on this child while
				// setting it on the sibling. Technically a spec violation, and
				// it must not stop the package being grouped.
				Digest: registry.Digest(payloadDigest),
				Size:   108979,
				Annotations: map[string]string{
					annRefName: "orbs/CFX-5000-k8s:orb_23.8.1076",
				},
			},
		},
	}
}

func payloadTagFixture() vendors.ScannedTag {
	return vendors.ScannedTag{
		Tag: "orb_23.8.1076",
		Descriptor: registry.Descriptor{
			MediaType: registry.MediaTypeOCIIndex,
			Digest:    registry.Digest(payloadDigest),
			Size:      108979,
		},
	}
}

func signatureTagFixture() vendors.ScannedTag {
	return vendors.ScannedTag{
		Tag: "signature_orb_23.8.1076",
		Descriptor: registry.Descriptor{
			MediaType: registry.MediaTypeOCIManifest,
			Digest:    registry.Digest(signatureDigest),
			Size:      814,
		},
	}
}

func group(t *testing.T, tags ...vendors.ScannedTag) []vendors.Package {
	t.Helper()
	pkgs, err := Layout{}.Group(t.Context(), nil, tags)
	if err != nil {
		t.Fatalf("group: %v", err)
	}
	return pkgs
}

// The headline behaviour: one release is ONE package, not three rows.
func TestThreeTagsCollapseToOnePackage(t *testing.T) {
	pkgs := group(t, payloadTagFixture(), signatureTagFixture(), wrapperTag())

	if len(pkgs) != 1 {
		names := make([]string, len(pkgs))
		for i, p := range pkgs {
			names[i] = p.Tag
		}
		t.Fatalf("got %d packages %v, want exactly 1", len(pkgs), names)
	}

	p := pkgs[0]
	// Identity is what a person says, not the vendor's plumbing.
	if p.Tag != "orb_23.8.1076" {
		t.Errorf("package tag = %q, want the payload tag orb_23.8.1076", p.Tag)
	}
}

// The load-bearing one: transferring the payload alone would leave the
// signature behind and make destination-side verification impossible forever.
func TestTransferRootIsTheWrapper(t *testing.T) {
	p := group(t, payloadTagFixture(), signatureTagFixture(), wrapperTag())[0]

	if got := p.EffectiveRoot().Digest.String(); got != wrapperDigest {
		t.Errorf("transfer root = %s, want the wrapper %s", got, wrapperDigest)
	}
	if p.RootTag != "signed_orb_23.8.1076" {
		t.Errorf("root tag = %q, want signed_orb_23.8.1076 so the destination can be tagged identically", p.RootTag)
	}
}

func TestSignatureAndWrapperAreRecordedAsRelated(t *testing.T) {
	p := group(t, payloadTagFixture(), signatureTagFixture(), wrapperTag())[0]

	roles := map[vendors.Role]vendors.Related{}
	for _, r := range p.Related {
		roles[r.Role] = r
	}

	sig, ok := roles[vendors.RoleSignature]
	if !ok {
		t.Fatalf("no signature recorded; related = %+v", p.Related)
	}
	if sig.Tag != "signature_orb_23.8.1076" {
		t.Errorf("signature tag = %q", sig.Tag)
	}
	if sig.Descriptor.Digest.String() != signatureDigest {
		t.Errorf("signature digest = %s, want %s", sig.Descriptor.Digest, signatureDigest)
	}

	// The wrapper is kept so the destination reproduces the vendor's layout;
	// a consumer expecting signed_orb_* must still find it after replication.
	if _, ok := roles[vendors.RoleWrapper]; !ok {
		t.Errorf("wrapper not recorded; related = %+v", p.Related)
	}
}

func TestSignedStatus(t *testing.T) {
	signed := group(t, payloadTagFixture(), signatureTagFixture(), wrapperTag())[0]
	if got := signed.Status(true); got != vendors.SignatureSigned {
		t.Errorf("status = %q, want signed", got)
	}

	// A release with no wrapper and no signature. The vendor does not sign
	// everything - older releases and hotfixes routinely go unsigned - so this
	// is real data, not a hypothetical.
	bare := group(t, vendors.ScannedTag{
		Tag:        "orb_22.1.0001",
		Descriptor: registry.Descriptor{Digest: registry.Digest(payloadDigest)},
	})[0]
	if got := bare.Status(true); got != vendors.SignatureUnsigned {
		t.Errorf("status = %q, want unsigned - we looked and found none", got)
	}
	// And the distinction the whole tri-state exists for.
	if got := bare.Status(false); got != vendors.SignatureUnknown {
		t.Errorf("status = %q, want unknown - nobody looked", got)
	}
}

// Discovery has no ordering guarantee, so the wrapper may be scanned before or
// after the tags it references. Grouping must not depend on which.
func TestGroupingIsOrderIndependent(t *testing.T) {
	orders := [][]vendors.ScannedTag{
		{payloadTagFixture(), signatureTagFixture(), wrapperTag()},
		{wrapperTag(), payloadTagFixture(), signatureTagFixture()},
		{signatureTagFixture(), wrapperTag(), payloadTagFixture()},
	}
	for i, order := range orders {
		pkgs := group(t, order...)
		if len(pkgs) != 1 || pkgs[0].Tag != "orb_23.8.1076" {
			t.Errorf("order %d produced %d package(s), first tag %q", i, len(pkgs), pkgs[0].Tag)
		}
	}
}

// The prefix only narrows candidates; the annotations decide. A tag that merely
// begins with signed_ but names no payload must stay an ordinary package rather
// than vanish - silently dropping content because it failed a vendor heuristic
// is the worse failure.
func TestUnrecognisedSignedPrefixStaysAPackage(t *testing.T) {
	impostor := vendors.ScannedTag{
		Tag:        "signed_by_someone_else",
		Descriptor: registry.Descriptor{Digest: registry.Digest(wrapperDigest)},
		Children: []registry.Descriptor{
			{Digest: registry.Digest(payloadDigest)}, // no ref.name annotation
		},
	}

	pkgs := group(t, impostor)
	if len(pkgs) != 1 || pkgs[0].Tag != "signed_by_someone_else" {
		t.Fatalf("got %+v, want the tag preserved as its own package", pkgs)
	}
	if pkgs[0].Root.Digest != "" {
		t.Error("an unrecognised tag must not acquire a transfer root")
	}
}

// A repository holding several releases must group each independently.
func TestSeveralReleasesInOneRepository(t *testing.T) {
	second := wrapperTag()
	second.Tag = "signed_orb_23.9.0001"
	second.Descriptor.Digest = registry.Digest(
		"sha256:1111000022223333444455556666777788889999aaaabbbbccccddddeeeeffff")
	second.Children[0].Annotations[annRefName] = "orbs/CFX-5000-k8s:signature_orb_23.9.0001"
	second.Children[1].Annotations[annRefName] = "orbs/CFX-5000-k8s:orb_23.9.0001"

	pkgs := group(t,
		payloadTagFixture(), signatureTagFixture(), wrapperTag(),
		vendors.ScannedTag{Tag: "orb_23.9.0001"},
		vendors.ScannedTag{Tag: "signature_orb_23.9.0001"},
		second,
	)

	if len(pkgs) != 2 {
		names := make([]string, len(pkgs))
		for i, p := range pkgs {
			names[i] = p.Tag
		}
		t.Fatalf("got %d packages %v, want 2", len(pkgs), names)
	}
}

func TestTagFromRefName(t *testing.T) {
	cases := map[string]string{
		"orbs/CFX-5000-k8s:orb_23.8.1076": "orb_23.8.1076",
		"orb_23.8.1076":                   "orb_23.8.1076",
		"a/b/c:tag":                       "tag",
		"":                                "",
		"trailing:":                       "",
	}
	for in, want := range cases {
		if got := tagFromRefName(in); got != want {
			t.Errorf("tagFromRefName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The listing shows the short form; both spellings must resolve as input, or
// the abbreviation is a trap - someone copies what they see, gets "not found",
// and reasonably concludes the package is gone.
func TestDisplayTagStripsOnlyTheVendorNoise(t *testing.T) {
	cases := map[string]string{
		"orb_23.8.1076":           "23.8.1076",
		"signed_orb_23.8.1076":    "signed_23.8.1076",
		"signature_orb_23.8.1076": "signature_23.8.1076",

		// Nothing to remove: empty, which the core reads as "no shortening"
		// rather than as an empty name.
		"v1.2.3":   "",
		"orb_":     "",
		"orbital":  "",
		"signed_x": "",
	}
	for in, want := range cases {
		if got := displayTag(in); got != want {
			t.Errorf("displayTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupedPackageCarriesItsDisplayTag(t *testing.T) {
	p := group(t, payloadTagFixture(), signatureTagFixture(), wrapperTag())[0]
	if p.DisplayTag != "23.8.1076" {
		t.Errorf("display tag = %q, want 23.8.1076", p.DisplayTag)
	}
	// The real tag is untouched: it is the identity, and what gets stored,
	// transferred and returned by -o json.
	if p.Tag != "orb_23.8.1076" {
		t.Errorf("tag = %q, want the real orb_23.8.1076", p.Tag)
	}
}

// The repository half of the same idea, and the reason it moved here.
//
// `orbs/` used to be trimmed in the CLI by dropping whichever prefix a page of
// results happened to share. That needed no vendor knowledge - which was the
// appeal - and was wrong twice over: it shortened paths on registries with no
// such convention, and it made a row say different things depending on what
// else was on the page. The rule now comes from a source declaring
// `vendor: near`, and it lives in the one file that is allowed to know it.
func TestDisplayRepositoryStripsOnlyTheVendorNamespace(t *testing.T) {
	cases := map[string]string{
		"orbs/cfx-5000-k8s": "cfx-5000-k8s",
		"orbs/cfx-5000-db":  "cfx-5000-db",
		// A deeper path keeps everything below the namespace: only the leading
		// segment is NEAR's, and the rest distinguishes one repository from
		// another.
		"orbs/team/cfx-5000-k8s": "team/cfx-5000-k8s",
		// Leading and trailing slashes are noise from a hand-typed path.
		"/orbs/cfx-5000-k8s": "cfx-5000-k8s",

		// Nothing to remove: empty, which the core reads as "no shortening".
		"cfx-5000-k8s":     "",
		"orbs":             "",
		"orbs/":            "",
		"orbital/cfx-5000": "",
		"":                 "",
	}
	for in, want := range cases {
		if got := (Layout{}).DisplayRepository(in); got != want {
			t.Errorf("DisplayRepository(%q) = %q, want %q", in, got, want)
		}
	}
}

// The bug this fixes, stated as a test.
//
// An orb's index lists its parts as plain image manifests with no artifactType,
// and discovery does not fetch each one - so the config media type that would
// separate a chart from an image is absent. Classified on the OCI fields alone
// every part reads as an image, and Helm charts become invisible.
func TestClassifyArtifactReadsTheOrbType(t *testing.T) {
	var l Layout

	for _, tc := range []struct {
		name    string
		orbType string
		want    string
	}{
		{"helm chart", "helmchart", "chart"},
		{"container image", "cnfimage", "image"},
		{"signature", "generic_signature", "signature"},
		{"custom data", "generic_custo", "file"},
		{"another generic", "generic_config", "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := l.ClassifyArtifact(map[string]string{
				"com.nokia.ncd.orb.type": tc.orbType,
			})
			if got != tc.want {
				t.Errorf("orb type %q classified as %q, want %q", tc.orbType, got, tc.want)
			}
		})
	}
}

// An unfamiliar value defers rather than guessing: a vendor adding a type we
// have never seen should fall through to the OCI rules, not be forced into
// whichever bucket happened to be last.
func TestClassifyArtifactDefersOnTheUnknown(t *testing.T) {
	var l Layout

	for _, annotations := range []map[string]string{
		nil,
		{},
		{"com.nokia.ncd.orb.type": "something-new"},
		{"org.opencontainers.image.ref.name": "orbs/x:orb_1"},
	} {
		if got := l.ClassifyArtifact(annotations); got != "" {
			t.Errorf("ClassifyArtifact(%v) = %q, want \"\" so the OCI rules decide", annotations, got)
		}
	}
}
