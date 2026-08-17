package product

import (
	"strings"
	"testing"
)

// withQuayChain returns the topology this feature exists for: a JFrog target
// our workers push to, and a Quay target that mirrors from it.
func withQuayChain() *Product {
	p := valid()
	p.Spec.Targets = []Target{
		{
			Name: "jfrog-store", Registry: "artifactory.example.com",
			Repository: "docker-local/vendor-a/platform",
			Type:       RegistryArtifactory, Anonymous: true, Default: true,
		},
		{
			Name: "ocp-prod", Registry: "quay.apps.ocp.example.com",
			Repository: "platform-prod/vendor-a-platform",
			Type:       RegistryQuay, Anonymous: true,
			Quay: &QuaySettings{APITokenRef: &SecretRef{SecretName: "quay-api", Key: "token"}},
			Replication: &Replication{
				Mode: ReplicationMirror,
				Mirror: &MirrorConfig{
					From:  "jfrog-store",
					Tags:  []string{"v3.*", "sha256-*.sig"},
					Robot: "platform-prod+swgw-mirror",
				},
			},
		},
	}
	p.Spec.AutoDownload.Rules[0].Targets = []string{"jfrog-store", "ocp-prod"}
	return p
}

func TestMirrorChainValidates(t *testing.T) {
	if err := withQuayChain().Validate(nil); err != nil {
		t.Fatalf("expected the reference topology to validate, got: %v", err)
	}
}

func TestAbsentReplicationIsCopy(t *testing.T) {
	tgt := valid().Spec.Targets[0]
	if got := tgt.ReplicationMode(); got != ReplicationCopy {
		t.Fatalf("a target with no replication block must mean copy, got %q", got)
	}
	if tgt.Delegated() {
		t.Fatal("a copy target is not delegated")
	}
}

// ---------------------------------------------------------------------------
// The headline error class for this block: a regex where a glob belongs.
//
// This one is worth more than the others put together. `^v\d+\.\d+\.\d+$` is
// correct everywhere else in the schema; as a Quay tag glob it matches nothing,
// the sync succeeds, and the destination stays empty while reporting success.
// ---------------------------------------------------------------------------

func TestRejectsRE2InTagGlob(t *testing.T) {
	for _, glob := range []string{`^v3\.2`, `v3.*$`, `v\d+.*`, `v3.*|v4.*`, `(?i)v3.*`, `v3\.2.*`} {
		p := withQuayChain()
		p.Spec.Targets[1].Replication.Mirror.Tags = []string{glob}

		e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.tags[0]")
		if !strings.Contains(e.Message, "shell wildcards") {
			t.Fatalf("glob %q: message should name the dialect, got %q", glob, e.Message)
		}
		if !strings.Contains(e.Hint, "RE2") {
			t.Fatalf("glob %q: hint should explain why this field differs, got %q", glob, e.Hint)
		}
	}
}

// Quay's field is CSV, so a comma inside one entry silently becomes two rules.
func TestRejectsCommaInsideOneGlob(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Tags = []string{"v3.*,sha256-*.sig"}

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.tags[0]")
	if !strings.Contains(e.Hint, "splits") {
		t.Fatalf("hint should explain what Quay does with the comma, got %q", e.Hint)
	}
}

func TestAcceptsGenuineGlobs(t *testing.T) {
	for _, glob := range []string{"v3.*", "*", "sha256-*.sig", "release-2?", "v[0-9]*"} {
		p := withQuayChain()
		p.Spec.Targets[1].Replication.Mirror.Tags = []string{glob}
		if err := p.Validate(nil); err != nil {
			t.Fatalf("glob %q should be accepted, got: %v", glob, err)
		}
	}
}

func TestSignatureTagDetection(t *testing.T) {
	cases := map[string]bool{
		"v3.*":              false,
		"v3.*,sha256-*.sig": false, // one string, not two globs
		"*":                 true,
		"sha256-*.sig":      true,
		"sha256-*":          true,
	}
	for glob, want := range cases {
		if got := SelectsSignatures([]string{glob}); got != want {
			t.Fatalf("SelectsSignatures(%q) = %v, want %v", glob, got, want)
		}
	}
	if !SelectsSignatures([]string{"v3.*", "sha256-*.sig"}) {
		t.Fatal("a glob set containing a signature glob selects signatures")
	}
}

// ---------------------------------------------------------------------------
// Mode and type
// ---------------------------------------------------------------------------

func TestRejectsUnknownMode(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mode = "sync"

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mode")
	if !strings.Contains(e.Message, "copy, mirror, proxy") {
		t.Fatalf("message should list the closed set, got %q", e.Message)
	}
}

func TestRejectsDelegationOnNonQuayTarget(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Type = RegistryArtifactory

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mode")
	if !strings.Contains(e.Message, "requires type: quay") {
		t.Fatalf("got %q", e.Message)
	}
}

func TestRejectsMirrorBlockOnCopyMode(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mode = ReplicationCopy

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror")
}

func TestRejectsMirrorModeWithoutMirrorBlock(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror = nil

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror")
}

// ---------------------------------------------------------------------------
// The control-API credential, which is the one people miss
// ---------------------------------------------------------------------------

func TestRequiresQuayAPITokenForDelegatedMode(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Quay = nil

	e := requireField(t, p.Validate(nil), "spec.targets[1].quay.apiTokenRef")
	if !strings.Contains(e.Hint, "/v2") || !strings.Contains(e.Hint, "OAuth") {
		t.Fatalf("hint must explain that a robot is not an API token, got %q", e.Hint)
	}
}

func TestCopyModeQuayTargetNeedsNoAPIToken(t *testing.T) {
	p := valid()
	p.Spec.Targets[0].Type = RegistryQuay
	if err := p.Validate(nil); err != nil {
		t.Fatalf("a copy-mode Quay target must stay exactly as simple as today, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Upstream
// ---------------------------------------------------------------------------

func TestRejectsBothFromAndExternalReference(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.ExternalReference = "artifactory.example.com/docker-local"

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror")
}

func TestRejectsNeitherFromNorExternalReference(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.From = ""

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror")
}

func TestRejectsUnknownMirrorSource(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.From = "nowhere"

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.from")
	if !strings.Contains(e.Hint, "externalReference") {
		t.Fatalf("hint should offer the escape hatch, got %q", e.Hint)
	}
}

func TestRejectsDisabledMirrorSource(t *testing.T) {
	p := withQuayChain()
	off := false
	p.Spec.Targets[0].Enabled = &off
	p.Spec.Targets[0].Default = false

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.from")
}

func TestRejectsMirrorFromProxyCache(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[0] = Target{
		Name: "jfrog-store", Registry: "quay.apps.ocp.example.com",
		Repository: "dev-cache/vendor-a", Type: RegistryQuay, Anonymous: true,
		Quay: &QuaySettings{APITokenRef: &SecretRef{SecretName: "quay-api", Key: "token"}},
		Replication: &Replication{Mode: ReplicationProxy, Proxy: &ProxyCacheConfig{
			Organization: "dev-cache", UpstreamRegistry: "artifactory.example.com",
		}},
	}

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.from")
	if !strings.Contains(e.Hint, "enumerated") {
		t.Fatalf("hint should say why a cache cannot be an upstream, got %q", e.Hint)
	}
}

func TestRejectsSelfMirror(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.From = "ocp-prod"

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.from")
}

func TestRejectsMirrorCycle(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[0] = Target{
		Name: "jfrog-store", Registry: "quay.apps.ocp.example.com",
		Repository: "platform-prod/store", Type: RegistryQuay, Anonymous: true, Default: true,
		Quay: &QuaySettings{APITokenRef: &SecretRef{SecretName: "quay-api", Key: "token"}},
		Replication: &Replication{Mode: ReplicationMirror, Mirror: &MirrorConfig{
			From: "ocp-prod", Tags: []string{"v3.*"}, Robot: "platform-prod+swgw",
		}},
	}

	e := requireField(t, p.Validate(nil), "spec.targets")
	if !strings.Contains(e.Message, "cycle") {
		t.Fatalf("got %q", e.Message)
	}
	if !strings.Contains(e.Message, "->") {
		t.Fatalf("message should render the chain so the reader can see it, got %q", e.Message)
	}
}

func TestRejectsExternalReferenceWithTagOrScheme(t *testing.T) {
	for _, ref := range []string{
		"https://artifactory.example.com/docker-local",
		"artifactory.example.com/docker-local:v1",
		"artifactory.example.com/docker-local@sha256:abc",
		"artifactory.example.com",
	} {
		p := withQuayChain()
		p.Spec.Targets[1].Replication.Mirror.From = ""
		p.Spec.Targets[1].Replication.Mirror.ExternalReference = ref

		requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.externalReference")
	}
}

// ---------------------------------------------------------------------------
// Robot
// ---------------------------------------------------------------------------

func TestRequiresRobot(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Robot = ""

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.robot")
	if !strings.Contains(e.Hint, "robot") {
		t.Fatalf("hint should say why Quay insists, got %q", e.Hint)
	}
}

func TestRejectsRobotInWrongOrganization(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Robot = "other-org+swgw-mirror"

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.robot")
	if !strings.Contains(e.Message, "platform-prod") {
		t.Fatalf("message should name both organizations, got %q", e.Message)
	}
}

func TestRejectsMalformedRobot(t *testing.T) {
	for _, robot := range []string{"swgw-mirror", "+swgw", "platform-prod+"} {
		p := withQuayChain()
		p.Spec.Targets[1].Replication.Mirror.Robot = robot
		requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.robot")
	}
}

// ---------------------------------------------------------------------------
// Bounds Quay imposes
// ---------------------------------------------------------------------------

func TestRejectsSkopeoTimeoutAboveQuayMaximum(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.SkopeoTimeout = Duration(MaxSkopeoTimeout + 1)

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.skopeoTimeout")
}

func TestRejectsSubMinuteInterval(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.Interval = Duration(30)

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.interval")
}

func TestRejectsNonRFC3339StartAt(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Replication.Mirror.StartAt = "tomorrow"

	requireField(t, p.Validate(nil), "spec.targets[1].replication.mirror.startAt")
}

func TestMirrorDefaults(t *testing.T) {
	m := &MirrorConfig{}
	if got := m.EffectiveInterval(); got != DefaultMirrorInterval {
		t.Fatalf("interval default = %v, want %v", got, DefaultMirrorInterval)
	}
	if got := m.EffectiveSkopeoTimeout(); got != DefaultSkopeoTimeout {
		t.Fatalf("skopeo default = %v, want %v", got, DefaultSkopeoTimeout)
	}
	if !m.IsEnabled() || !m.SyncsOnRequest() || !m.VerifiesTLS() {
		t.Fatal("enabled, syncOnRequest and verifyTLS all default to true")
	}
	if m.ManageMode() != ManageApply {
		t.Fatalf("manage default = %q, want apply", m.ManageMode())
	}
	m.SkopeoTimeout = Duration(MaxSkopeoTimeout * 2)
	if got := m.EffectiveSkopeoTimeout(); got != MaxSkopeoTimeout {
		t.Fatalf("skopeo timeout must clamp to %v, got %v", MaxSkopeoTimeout, got)
	}
}

// ---------------------------------------------------------------------------
// Proxy cache
// ---------------------------------------------------------------------------

func withProxyCache() *Product {
	p := valid()
	p.Spec.Targets = append(p.Spec.Targets, Target{
		Name: "dev-cache", Registry: "quay.apps.ocp.example.com",
		Repository: "dev-cache/vendor-a/platform",
		Type:       RegistryQuay, Anonymous: true,
		Quay: &QuaySettings{APITokenRef: &SecretRef{SecretName: "quay-api", Key: "token"}},
		Replication: &Replication{Mode: ReplicationProxy, Proxy: &ProxyCacheConfig{
			Organization: "dev-cache", UpstreamRegistry: "artifactory.example.com/docker-local",
		}},
	})
	return p
}

func TestProxyCacheValidates(t *testing.T) {
	if err := withProxyCache().Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestRejectsProxyOrganizationDisagreeingWithRepository(t *testing.T) {
	p := withProxyCache()
	p.Spec.Targets[1].Replication.Proxy.Organization = "other"

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.proxy.organization")
	if !strings.Contains(e.Hint, "proxy layout") {
		t.Fatalf("hint should explain the path is the layout, got %q", e.Hint)
	}
}

// A proxy target as a download destination must fail at LOAD. Left to run
// time it would fail on the day a rule first matched, which may be months
// after the edit that caused it.
func TestRejectsProxyTargetAsRuleDestination(t *testing.T) {
	p := withProxyCache()
	p.Spec.AutoDownload.Rules[0].Targets = []string{"lab", "dev-cache"}

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].targets[1]")
	if !strings.Contains(e.Hint, "warm") {
		t.Fatalf("hint must name the command that does work here, got %q", e.Hint)
	}
}

func TestRejectsProxyTargetAsDefault(t *testing.T) {
	p := withProxyCache()
	p.Spec.Targets[0].Default = false
	p.Spec.Targets[1].Default = true
	p.Spec.AutoDownload.Rules[0].Targets = []string{"lab"}

	requireField(t, p.Validate(nil), "spec.targets[1].default")
}

func TestRejectsProxyTargetAsPromotionOnly(t *testing.T) {
	p := withProxyCache()
	p.Spec.Targets[1].PromotionOnly = true

	requireField(t, p.Validate(nil), "spec.targets[1].promotionOnly")
}

// ---------------------------------------------------------------------------
// Rules and promotion against delegated targets
// ---------------------------------------------------------------------------

func TestRejectsRuleTargetingMirrorThatWillNotSyncOnRequest(t *testing.T) {
	p := withQuayChain()
	off := false
	p.Spec.Targets[1].Replication.Mirror.SyncOnRequest = &off

	e := requireField(t, p.Validate(nil), "spec.autoDownload.rules[0].targets[1]")
	if !strings.Contains(e.Hint, "syncOnRequest") {
		t.Fatalf("hint should name the field that fixes it, got %q", e.Hint)
	}
}

func TestRejectsDelegatedPromotionDestination(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Environment = "production"

	e := requireField(t, p.Validate(nil), "spec.targets[1].replication.mode")
	if !strings.Contains(e.Hint, "read-only") {
		t.Fatalf("hint should explain Quay's robot-only push, got %q", e.Hint)
	}
}

// A mirror target may be promoted FROM: it is an ordinary readable repository.
func TestAllowsDelegatedPromotionSource(t *testing.T) {
	p := withQuayChain()
	p.Spec.Targets[1].Environment = "lab"

	if err := p.Validate(nil); err != nil {
		t.Fatalf("a mirror is a legitimate promotion source, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Accessors
// ---------------------------------------------------------------------------

func TestTargetOrganization(t *testing.T) {
	cases := map[string]string{
		"platform-prod/vendor-a-platform": "platform-prod",
		"dev-cache":                       "dev-cache",
		"a/b/c":                           "a",
		"":                                "",
	}
	for repo, want := range cases {
		if got := (Target{Repository: repo}).Organization(); got != want {
			t.Fatalf("Organization(%q) = %q, want %q", repo, got, want)
		}
	}
}

func TestMirrorAndProxySettingsAreModeGated(t *testing.T) {
	p := withQuayChain()
	if _, ok := p.Spec.Targets[1].MirrorSettings(); !ok {
		t.Fatal("mirror target must expose its mirror settings")
	}
	if _, ok := p.Spec.Targets[1].ProxySettings(); ok {
		t.Fatal("a mirror target must not expose proxy settings")
	}
	if _, ok := p.Spec.Targets[0].MirrorSettings(); ok {
		t.Fatal("a copy target has no mirror settings")
	}
}
