package baseline_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/compliance"
)

// The output contract.
//
// # Why these are assertions and not review notes
//
// Both of the failures below happened, shipped, and were found by a person
// reading a report rather than by CI:
//
//  1. A pack stopped compiling and its checks silently vanished from the run.
//     Nothing failed - the good fixture was still clean, because a check that
//     does not exist cannot fire - and the report simply had a category missing
//     from it. Absence and compliance are indistinguishable on the screen a
//     release manager reads, which is the one thing this package exists to
//     prevent.
//
//  2. Two checks emitted findings with an empty observed value, one of them
//     with an unsubstituted fragment where the value should have been. Both
//     were blocking, so they were among the first rows a reviewer opened, and
//     neither told the reader what was actually wrong.
//
// Each is one assertion, and each would have caught its own defect before
// release.

// A pack that fails to load takes its checks with it. That must fail the build,
// not the report.
func TestEveryShippedPackLoads(t *testing.T) {
	cat := loadShipped(t)
	total := 0
	for _, p := range cat.Packs() {
		if !p.OK() {
			t.Errorf("pack %s did not load cleanly, so the checks it owns are missing from every run:", p.Name)
			for _, e := range p.Errors {
				t.Errorf("    %s", e)
			}
		}
		if p.Checks == 0 {
			t.Errorf("pack %s registered no checks", p.Name)
		}
		total += p.Checks
	}
	if total != cat.Len() {
		t.Errorf("packs report %d checks but the catalogue holds %d", total, cat.Len())
	}
	t.Logf("%d packs, %d checks", len(cat.Packs()), cat.Len())
}

// Every finding has to say what was actually seen.
//
// A row naming a field and leaving the value blank makes the reader open the
// chart to learn what the tool already knew, and a row containing a template
// fragment tells them the tool is broken. Neither is worth emitting.
func TestFindingsCarryTheValueTheyJudged(t *testing.T) {
	fixtures := []string{
		"bad-config.yaml", "bad-cronjob.yaml", "bad-metadata.yaml", "bad-network.yaml",
		"bad-observability.yaml", "bad-pdb.yaml", "bad-probes.yaml", "bad-rbac.yaml",
		"bad-resources.yaml", "bad-scheduling.yaml", "bad-security.yaml",
		"bad-storage.yaml", "bad-supply.yaml", "bad-upgrade.yaml",
	}
	// Fragments that mean a value never got substituted, or that a list was
	// rendered through a conversion that produces nothing.
	broken := []string{"{{", "}}", "<no value>", "%!", "(MISSING)"}

	var problems []string
	for _, f := range fixtures {
		for _, r := range runFixture(t, f) {
			if r.Outcome != compliance.OutcomeFail {
				continue
			}
			where := r.CheckID + " @ " + f + " " + r.Address.Kind + "/" + r.Address.Name
			if r.Address.Locus != "" && strings.TrimSpace(r.Observed) == "" {
				problems = append(problems, where+": names "+r.Address.Locus+" and reports no value for it")
			}
			for _, frag := range broken {
				if strings.Contains(r.Observed, frag) {
					problems = append(problems, where+": observed value contains "+frag+" - "+r.Observed)
				}
				if strings.Contains(r.Message, frag) {
					problems = append(problems, where+": message contains "+frag+" - "+r.Message)
				}
			}
			if strings.TrimSpace(r.Message) == "" {
				problems = append(problems, where+": no message")
			}
			if strings.TrimSpace(r.Remediation) == "" {
				problems = append(problems, where+": no remediation, so the finding states a defect with no fix")
			}
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// An object that names no namespace is not an object in a different namespace.
//
// # The defect this exists for
//
// A validation run produced three findings saying "this workload has no
// disruption policy" and four saying "this disruption policy protects no
// workload" - about the same three object pairs. Both cannot be true. Each was
// the mirror image of the other, and all seven followed from ONE broken join:
// `helm template` emits `metadata.namespace` only where a chart hard-codes it,
// real releases mix both conventions freely, and comparing the rendered strings
// made every cross-object lookup fail.
//
// The reviewer's own diagnostic is worth stating here even though this test
// cannot check it in general: two checks asserting contradictory things about
// one pair of objects almost always means one broken join rather than two
// broken rules. The fixture below is that pair, built deliberately - a policy
// with no namespace protecting a workload that declares one - and the two
// mirror checks must both stay silent on it.
//
// The assertion is scoped to the join-dependent checks on purpose. The fixture
// workload fails plenty of unrelated checks, and demanding otherwise would make
// this a second copy of the good fixture.
func TestAnAbsentNamespaceDoesNotBreakAJoin(t *testing.T) {
	joinDependent := map[string]bool{
		"PDB-01": true, // "no policy protects this workload"
		"PDB-09": true, // "this policy protects no workload"
		"NET-07": true, // "this Service routes nowhere"
		"OBS-01": true, // reached through servicesFor
	}
	for _, r := range runFixture(t, "bad-pdb.yaml") {
		if r.Outcome != compliance.OutcomeFail || !joinDependent[r.CheckID] {
			continue
		}
		if r.Address.Name == "implicit-namespace-pdb" || r.Address.Name == "namespaced" {
			t.Errorf("%s fired on %s/%s. The rule declares no namespace and the workload "+
				"it protects declares one, which is the ordinary shape of a real chart - "+
				"and %s asserts the opposite of its mirror check about the same pair",
				r.CheckID, r.Address.Kind, r.Address.Name, r.CheckID)
		}
	}
}

// A check that exists to REPORT something says it on a pass.
//
// The alternative is what the audit found: an inventory check has to fail on
// every subject in order to say anything, so three checks produced roughly a
// third of a report's rows and close to none of its defects. `observeOnPass`
// is the mechanism that separates the two, and this asserts it works - because
// a silent pass row is indistinguishable from the check not existing.
func TestInventoryChecksSayWhatTheySawOnAPass(t *testing.T) {
	want := map[string]string{
		// The tasks Helm runs outside the ordinary install. Nothing else in a
		// compliance report shows them: hooks do not appear among the deployed
		// objects.
		"UPG-08": "Helm runs this Job at: pre-install, pre-upgrade",
	}
	seen := map[string]bool{}
	for _, r := range runFixture(t, "good-app.yaml") {
		if r.Outcome != compliance.OutcomePass {
			continue
		}
		expect, ok := want[r.CheckID]
		if !ok {
			continue
		}
		seen[r.CheckID] = true
		if r.Observed != expect {
			t.Errorf("%s passed and recorded %q, want %q", r.CheckID, r.Observed, expect)
		}
	}
	for id := range want {
		if !seen[id] {
			t.Errorf("%s produced no passing result on the good fixture", id)
		}
	}
}

// An engineer searching for the mechanism finds the checks about it.
//
// # Why this is a test and not a convention
//
// The findings are written so that somebody who is not a Kubernetes engineer
// can act on them, which means the title of the PodDisruptionBudget check says
// "a service with more than one copy survives planned maintenance" and contains
// the word "PodDisruptionBudget" nowhere at all. That is right for the person
// deciding whether to ship and useless for the person fixing it, who types
// `toleration` or `RWX` or `seccomp` into the search box.
//
// The technical vocabulary is therefore carried deliberately rather than left
// to whatever words the sentences happen to use - and the moment it is
// deliberate, it can be forgotten. Every term below is one somebody would
// plausibly search for, mapped to the check that must come back. A rewritten
// description cannot silently take any of them away.
func TestTechnicalTermsFindTheirChecks(t *testing.T) {
	want := map[string][]string{
		"pdb":                          {"PDB-01", "PDB-02", "PDB-03", "PDB-09"},
		"poddisruptionbudget":          {"PDB-01", "PDB-09"},
		"toleration":                   {"SCH-08", "SCH-09"},
		"taint":                        {"SCH-08", "SCH-09"},
		"tolerationseconds":            {"SCH-09"},
		"topologyspreadconstraints":    {"SCH-01", "SCH-02", "SCH-04"},
		"nodeaffinity":                 {"SCH-03", "SCH-05"},
		"maxunavailable":               {"PDB-01", "PDB-02", "PDB-05"},
		"terminationgraceperiod":       {"PDB-08", "PDB-10", "PRB-07"},
		"prestop":                      {"PRB-07"},
		"readinessprobe":               {"PRB-01", "PRB-03", "PRB-04", "PRB-05", "PRB-06"},
		"startupprobe":                 {"PRB-02", "PRB-11"},
		"seccomp":                      {"SEC-06"},
		"runasnonroot":                 {"SEC-01", "SEC-12"},
		"allowprivilegeescalation":     {"SEC-02"},
		"capabilities":                 {"SEC-04", "SEC-13"},
		"hostpath":                     {"SEC-08", "STO-10"},
		"hostnetwork":                  {"SEC-07"},
		"scc":                          {"SEC-10"},
		"emptydir":                     {"SEC-11", "STO-10"},
		"automountserviceaccounttoken": {"RBAC-02"},
		"clusterrolebinding":           {"RBAC-04", "RBAC-10"},
		"impersonate":                  {"RBAC-07"},
		"resourcenames":                {"RBAC-11"},
		"hpa":                          {"RES-04"},
		"oomkilled":                    {"RES-02"},
		"throttling":                   {"RES-03"},
		"rwx":                          {"STO-02", "STO-13"},
		"readwritemany":                {"STO-02", "STO-13"},
		"fsgroup":                      {"STO-07", "STO-08"},
		"storageclassname":             {"STO-01"},
		"volumeclaimtemplates":         {"STO-05", "STO-10"},
		"networkpolicy":                {"NET-01", "NET-02", "NET-03", "NET-04", "NET-13"},
		"namespaceselector":            {"NET-13"},
		"targetport":                   {"NET-11"},
		"sr-iov":                       {"NET-09", "NET-10"},
		"hugepages":                    {"NET-10"},
		"multus":                       {"NET-09"},
		"ingress":                      {"NET-01", "NET-02", "NET-05", "NET-06"},
		"checksum":                     {"CFG-04"},
		"secretkeyref":                 {"CFG-13", "CFG-07", "CFG-11"},
		"stringdata":                   {"CFG-14"},
		"envfrom":                      {"CFG-07"},
		"digest":                       {"SUP-01"},
		"registry":                     {"SUP-02"},
		"helm.sh/hook":                 {"MTA-08", "UPG-08", "UPG-09"},
		"rollback":                     {"UPG-09"},
		"flux":                         {"UPG-09"},
		"gitops":                       {"UPG-09", "CFG-14"},
		"crd":                          {"UPG-07", "UPG-11"},
		"helm.sh/resource-policy":      {"UPG-11"},
		"servicemonitor":               {"OBS-01"},
		"runbook_url":                  {"OBS-09"},
		"stdout":                       {"OBS-05"},
	}

	// The index a search runs over: everything on the check that a LIKE would
	// see. Deliberately NOT the remediation, which is the same advice on many
	// checks and would make every term match everything.
	index := map[string]string{}
	for _, c := range loadShipped(t).Checks() {
		index[c.ID] = strings.ToLower(strings.Join(append([]string{
			c.ID, c.Title, c.Category, c.Subcategory,
		}, c.Keywords...), " "))
	}

	for term, ids := range want {
		for _, id := range ids {
			text, ok := index[id]
			if !ok {
				t.Errorf("%s does not exist, so %q cannot find it", id, term)
				continue
			}
			if !strings.Contains(text, term) {
				t.Errorf("searching %q does not find %s, which is about exactly that", term, id)
			}
		}
	}
}

// The subcategory vocabulary is closed.
//
// # Why the list is written out here
//
// A subcategory is what an engineer filters by, so its value is entirely in
// being shared. Free text drifts within a month - "Helm hooks" and "Helm hook",
// "Probe timing" and "Probe timings" - and a filter offering both is worse than
// no filter, because each of them hides half the findings and neither says so.
//
// So the list is declared, and adding to it is an edit somebody makes on
// purpose. It is not sorted by category: a mechanism can be reached from more
// than one section of the standard - Helm hooks are metadata to the labels
// section and lifecycle to the upgrade one - and collecting a mechanism ACROSS
// sections is most of the reason this field exists.
var subcategoryVocabulary = map[string]bool{
	// Scheduling & placement
	"Topology spread constraints": true,
	"Affinity & node selection":   true,
	"Taints & tolerations":        true,
	// Disruption & availability
	"PodDisruptionBudget": true,
	"Rollout strategy":    true,
	"Graceful shutdown":   true,
	// Health probes & lifecycle
	"Readiness probe":     true,
	"Startup probe":       true,
	"Probe handlers":      true,
	"Probe timing":        true,
	"Probe target port":   true,
	"Probe applicability": true,
	"Lifecycle hooks":     true,
	// Container security posture
	"Run-as user":               true,
	"Privilege escalation":      true,
	"Privileged containers":     true,
	"Linux capabilities":        true,
	"Read-only root filesystem": true,
	"Seccomp":                   true,
	"Host namespaces":           true,
	"Host path volumes":         true,
	"Security policy grants":    true,
	"Ephemeral storage":         true,
	// Identity & access
	"Service accounts":              true,
	"Service account tokens":        true,
	"Role rules":                    true,
	"Cluster-scoped RBAC":           true,
	"Secret access":                 true,
	"Privilege escalation via RBAC": true,
	"Impersonation & exec":          true,
	"Role bindings":                 true,
	// Resources & scaling
	"Resource requests": true,
	"Resource limits":   true,
	"Memory limits":     true,
	"CPU limits":        true,
	"Autoscaling":       true,
	// Storage & data
	"Storage class":      true,
	"Shared storage":     true,
	"Volume sizing":      true,
	"Volume permissions": true,
	"Stateful workloads": true,
	// Networking
	"Network policy":       true,
	"Network policy rules": true,
	"External exposure":    true,
	"Service selectors":    true,
	"Service ports":        true,
	"Secondary networks":   true,
	"Extended resources":   true,
	// Metadata
	"Standard labels": true,
	"Label syntax":    true,
	"Selector labels": true,
	"Annotations":     true,
	"Custom labels":   true,
	"Helm hooks":      true,
	// Configuration & secrets
	"Credentials in configuration":    true,
	"Credentials in the environment":  true,
	"Credentials in the chart":        true,
	"Credentials on the command line": true,
	"ConfigMap immutability":          true,
	"Configuration rollout":           true,
	"Configuration references":        true,
	"TLS material":                    true,
	"Secret scoping":                  true,
	// Supply chain
	"Image tags":       true,
	"Image digests":    true,
	"Image registries": true,
	// Upgrade & maintenance readiness
	"Custom resource definitions": true,
	"Forced operations":           true,
	// Observability
	"Metrics":  true,
	"Logging":  true,
	"Alerting": true,
}

func TestSubcategoriesAreAVocabulary(t *testing.T) {
	used := map[string]int{}
	for _, c := range loadShipped(t).Checks() {
		if c.Deprecated || c.Subcategory == "" {
			continue
		}
		used[c.Subcategory]++
		if !subcategoryVocabulary[c.Subcategory] {
			t.Errorf("%s uses subcategory %q, which is not in the declared vocabulary. "+
				"Add it here on purpose, or use the existing name - a filter offering "+
				"two spellings of one mechanism hides half the findings under each",
				c.ID, c.Subcategory)
		}
		if len(c.Subcategory) > 34 {
			t.Errorf("%s: subcategory %q is too long to read in a filter", c.ID, c.Subcategory)
		}
	}
	for name := range subcategoryVocabulary {
		if used[name] == 0 {
			t.Errorf("subcategory %q is declared and no check uses it", name)
		}
	}
	t.Logf("%d subcategories across %d checks", len(used), loadShipped(t).Len())
}

// Every check has to carry the fields a non-specialist reader needs, and the
// severity rubric has to hold. Both are cheap to state and impossible to keep
// by convention across a hundred checks in thirteen files.
func TestEveryCheckIsTriageable(t *testing.T) {
	for _, c := range loadShipped(t).Checks() {
		if c.Deprecated {
			continue
		}
		miss := func(field string) { t.Errorf("%s (%s): no %s", c.ID, c.Title, field) }
		if c.Confidence == "" {
			miss("confidence")
		}
		if c.WhenItBites == "" {
			miss("whenItBites - a reader cannot prioritise a finding with no timing")
		}
		if c.FixOwner == "" {
			miss("fixOwner - the finding cannot be routed to anybody")
		}
		if c.FixEffort == "" {
			miss("fixEffort")
		}
		if strings.TrimSpace(c.Remediation) == "" {
			miss("remediation")
		}
		if strings.TrimSpace(c.Rationale) == "" {
			miss("rationale - the finding says what is wrong and not why it matters")
		}
		if strings.TrimSpace(c.Reference) == "" {
			miss("reference to the clause of the standard it enforces")
		}
		// The technical index over a report written in plain language. The
		// title of this check deliberately does not contain the name of the
		// mechanism it is about - that is what makes it readable by somebody
		// who is not a Kubernetes engineer - so if these two are empty, nothing
		// anywhere in the finding carries the words an engineer would search
		// for, and the check is invisible to half its audience.
		if strings.TrimSpace(c.Subcategory) == "" {
			miss("subcategory - the mechanism, in the words an engineer uses for it")
		}
		if len(c.Keywords) < 3 {
			miss("keywords - at least three technical terms it should be findable by")
		}
		if c.Severity == compliance.SeverityCritical && c.Confidence == compliance.ConfidenceNeedsReview {
			t.Errorf("%s: blocks a release on a finding it says needs a human to judge", c.ID)
		}
	}
}

// A privileged container gets one finding, and the ones it cancels say so.
//
// # The defect this exists for
//
// The audit found three blocking findings on one container: privileged, the
// missing capability drop, and the permitted privilege escalation. The kernel
// grants a privileged container the full capability set whatever
// `capabilities.drop` says, and permits escalation whatever
// `allowPrivilegeEscalation` says - so two of the three cannot be acted on. A
// reader fixes them, the container's actual powers are unchanged, and the next
// report says exactly what this one said.
//
// The two dependent checks stand down (see compliance.Supersession) and are
// recorded as SKIPS naming SEC-03, not dropped: a missing row and a passing row
// look the same, and neither is true here.
func TestAPrivilegedContainerGetsOneFinding(t *testing.T) {
	dependents := map[string]bool{"SEC-02": true, "SEC-04": true}
	stoodDown := map[string]bool{}
	sawRootCause := false

	for _, r := range runFixture(t, "bad-security.yaml") {
		if r.Address.Name != "loose" || r.Address.Container != "loose" {
			continue
		}
		switch {
		case r.CheckID == "SEC-03":
			if r.Outcome != compliance.OutcomeFail {
				t.Errorf("SEC-03 is the root cause here and reported %s", r.Outcome)
			}
			sawRootCause = true
			// The root cause has to name what it cancels, or a reader who
			// notices the two missing rows concludes the tool missed them.
			for _, want := range []string{"capabilities.drop", "allowPrivilegeEscalation"} {
				if !strings.Contains(r.Message, want) {
					t.Errorf("SEC-03's message does not say it nullifies %s: %s", want, r.Message)
				}
			}
		case dependents[r.CheckID]:
			if r.Outcome != compliance.OutcomeSkip {
				t.Errorf("%s reported %s on a privileged container; fixing it would change "+
					"nothing until SEC-03 is fixed", r.CheckID, r.Outcome)
			}
			if r.SupersededBy != "SEC-03" {
				t.Errorf("%s stood down and named %q rather than SEC-03", r.CheckID, r.SupersededBy)
			}
			if strings.TrimSpace(r.Message) == "" {
				t.Errorf("%s stood down silently, which reads as the check not having run", r.CheckID)
			}
			stoodDown[r.CheckID] = true
		}
	}
	if !sawRootCause {
		t.Error("SEC-03 produced no result on the privileged container")
	}
	for id := range dependents {
		if !stoodDown[id] {
			t.Errorf("%s produced no result at all on the privileged container", id)
		}
	}

	// And the standalone case is untouched: an ordinary container missing these
	// settings is still a finding, which is the great majority of both checks.
	standalone := map[string]bool{}
	for _, r := range runFixture(t, "bad-security.yaml") {
		if r.Address.Name == "unbounded-scratch" && dependents[r.CheckID] &&
			r.Outcome == compliance.OutcomeFail {
			standalone[r.CheckID] = true
		}
	}
	for id := range dependents {
		if !standalone[id] {
			t.Errorf("%s stopped firing on a container that is not privileged", id)
		}
	}
}
