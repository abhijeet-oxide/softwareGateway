package replication

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry/quay"
)

// FieldDiff is one field on which Git and the registry disagree.
//
// Rendered rather than raw, because the two sides speak different units — six
// hours here, 21600 there — and a diff that made the reader convert would be a
// diff nobody reads.
type FieldDiff struct {
	Field string
	// Want is what the configuration asks for; Got is what the registry says.
	Want string
	Got  string
}

func (d FieldDiff) String() string {
	return fmt.Sprintf("%s: want %s, got %s", d.Field, quoteEmpty(d.Want), quoteEmpty(d.Got))
}

func quoteEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return s
}

// Drift is the whole answer to "does the registry still say what Git says".
type Drift struct {
	// Absent means the registry has no configuration at all: not drift from a
	// previous apply but a target that has never been applied. Distinguished
	// because the fix is different and because reporting "12 fields differ"
	// for a repository with no mirror at all is noise.
	Absent bool

	// StateWrong means the Quay repository is not in the state mirroring
	// needs. Called out separately because it is the destructive part of an
	// apply, and because a mirror configuration on a NORMAL repository is
	// inert — it looks configured and syncs nothing.
	StateWrong  bool
	ActualState string

	Fields []FieldDiff

	// CredentialsRotated is derived from the stored secret hash rather than
	// from any response, since Quay never returns a stored password.
	CredentialsRotated bool
}

// Drifted reports whether anything differs at all.
func (d Drift) Drifted() bool {
	return d.Absent || d.StateWrong || len(d.Fields) > 0 || d.CredentialsRotated
}

// Summary is a one-line description for a table cell or a metric log line.
func (d Drift) Summary() string {
	switch {
	case d.Absent:
		return "not configured on the registry"
	case !d.Drifted():
		return "in sync"
	}
	var parts []string
	if d.StateWrong {
		parts = append(parts, fmt.Sprintf("repository state is %s", d.ActualState))
	}
	if n := len(d.Fields); n == 1 {
		parts = append(parts, d.Fields[0].Field+" differs")
	} else if n > 1 {
		names := make([]string, 0, n)
		for _, f := range d.Fields {
			names = append(names, f.Field)
		}
		parts = append(parts, fmt.Sprintf("%d fields differ (%s)", n, strings.Join(names, ", ")))
	}
	if d.CredentialsRotated {
		parts = append(parts, "credentials rotated")
	}
	return strings.Join(parts, "; ")
}

// DiffMirror compares the desired mirror configuration with what Quay reports.
//
// observed nil means Quay has no mirror configured. appliedSecretHash is what
// we stored at the last successful apply; an empty value means we have never
// applied, in which case a credential comparison would be meaningless rather
// than false.
func DiffMirror(want *quay.MirrorSpec, observed *quay.MirrorState, state string, appliedSecretHash, desiredSecretHash string) Drift {
	if observed == nil {
		return Drift{Absent: true, ActualState: state}
	}

	d := Drift{ActualState: state}
	// An empty state means we did not read the repository, not that it is
	// wrong — a missing observation must never be reported as a fault.
	if state != "" && !strings.EqualFold(state, string(quay.StateMirror)) {
		d.StateWrong = true
	}

	add := func(field, want, got string) {
		if want != got {
			d.Fields = append(d.Fields, FieldDiff{Field: field, Want: want, Got: got})
		}
	}

	add("enabled", boolStr(want.IsEnabled), boolStr(observed.IsEnabled))
	add("externalReference", want.ExtRef, observed.ExtRef)
	add("interval", secondsStr(want.SyncInterval), secondsStr(observed.SyncInterval))
	add("robot", want.RobotUsername, observed.RobotUsername)
	add("sourceCredentials.username", want.ExternalRegistryUsername, observed.ExternalRegistryUsername)

	// Tag globs compared as a SET. Quay does not promise to return them in the
	// order they were sent, and reporting drift because a list came back
	// reordered would be a false alarm that teaches people to ignore real ones.
	if !sameSet(want.RootRule.RuleValue, observed.RootRule.RuleValue) {
		d.Fields = append(d.Fields, FieldDiff{
			Field: "tags",
			Want:  renderList(want.RootRule.RuleValue),
			Got:   renderList(observed.RootRule.RuleValue),
		})
	}

	// skopeoTimeout is only compared when Quay reports one. Older Quay
	// versions omit the field, and a zero it never sent is not a difference.
	if observed.SkopeoTimeoutInterval != 0 {
		add("skopeoTimeout", secondsStr(want.SkopeoTimeoutInterval), secondsStr(observed.SkopeoTimeoutInterval))
	}

	wc, oc := want.ExternalRegistryConfig, observed.ExternalRegistryConfig
	if wc != nil && oc != nil {
		add("network.verifyTLS", boolPtrStr(wc.VerifyTLS), boolPtrStr(oc.VerifyTLS))
		add("acceptUnsignedImages", boolPtrStr(wc.UnsignedImages), boolPtrStr(oc.UnsignedImages))
		wp, op := wc.Proxy, oc.Proxy
		if wp == nil {
			wp = &quay.ProxyConfig{}
		}
		if op == nil {
			op = &quay.ProxyConfig{}
		}
		add("network.proxy.httpProxy", wp.HTTPProxy, op.HTTPProxy)
		add("network.proxy.httpsProxy", wp.HTTPSProxy, op.HTTPSProxy)
		add("network.proxy.noProxy", wp.NoProxy, op.NoProxy)
	}

	// sync_start_date is NOT compared. It defaults to the apply time, so
	// comparing it would report drift on every reload of a configuration
	// nobody had touched — see the same exclusion in mirrorFingerprint.

	d.CredentialsRotated = appliedSecretHash != "" && appliedSecretHash != desiredSecretHash
	return d
}

// DiffProxy compares the desired proxy cache with what Quay reports.
func DiffProxy(want, observed *quay.ProxyCache, appliedSecretHash, desiredSecretHash string) Drift {
	if observed == nil {
		return Drift{Absent: true}
	}
	d := Drift{}
	add := func(field, w, g string) {
		if w != g {
			d.Fields = append(d.Fields, FieldDiff{Field: field, Want: w, Got: g})
		}
	}
	add("upstreamRegistry", want.UpstreamRegistry, observed.UpstreamRegistry)
	add("expiration", secondsStr(want.ExpirationS), secondsStr(observed.ExpirationS))
	add("insecure", boolStr(want.Insecure), boolStr(observed.Insecure))
	add("upstreamCredentials.username", want.UpstreamRegistryUsername, observed.UpstreamRegistryUsername)

	d.CredentialsRotated = appliedSecretHash != "" && appliedSecretHash != desiredSecretHash
	return d
}

// Pending reports whether the configuration in Git has changed since the last
// apply, which is a different question from whether the REGISTRY has drifted.
//
// Both are worth reporting and they are not the same thing: "you have not
// applied your change yet" is a normal state of affairs after a merge, while
// "somebody edited it in the Quay UI" is not.
func Pending(appliedConfigHash, desiredConfigHash string) bool {
	return appliedConfigHash != desiredConfigHash
}

// Applied is the record of one successful write, stored so drift can be
// computed against what we SENT rather than against what we can read back.
type Applied struct {
	ConfigHash string
	SecretHash string
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func boolPtrStr(b *bool) string {
	if b == nil {
		return "unset"
	}
	return boolStr(*b)
}

func secondsStr(s int64) string {
	if s == 0 {
		return "unset"
	}
	return (time.Duration(s) * time.Second).String()
}

func renderList(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return "[" + strings.Join(v, ", ") + "]"
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
