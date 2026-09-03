// Package security owns the platform's repository-independent view of
// vulnerability findings.
//
// See docs/design/21-security-posture.md.
//
// Nothing in this package knows what a scanner is called or what its API looks
// like. That is deliberate and it is the whole point: JFrog Xray is reached
// from inside the JFrog repository plugin (internal/registry/artifactory), and
// what crosses back into the core is the model defined here. A core that spoke
// Xray's JSON would have to be rewritten to add a second scanner, and the
// second scanner is the one nobody has budget for.
//
// The boundary is Provider (provider.go). Everything else here - severities,
// findings, postures, comparisons - is vocabulary the core reasons in.
package security

import "strings"

// Severity is the normalized severity ladder.
//
// Five values rather than a number, because a number is not what anybody asks
// about: releases are discussed in terms of "two criticals", never in terms of
// CVSS 9.8. Scanners that report a score as well keep it on the finding
// (Finding.CVSSScore); this is what the platform sorts, counts and compares on.
//
// Unknown is a real value and not a missing one. A scanner that has looked at a
// component and cannot grade the issue has told us something different from a
// scanner that has not looked, and flattening the two would let an unscanned
// release read as a clean one - the single worst failure this package can have.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityUnknown  Severity = "unknown"
)

// Severities is the ladder, worst first. Iteration order for every count,
// table and export in the system, so that "critical, high, medium, low" is
// never assembled by hand in two places and never assembled differently.
var Severities = []Severity{
	SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityUnknown,
}

// Rank orders severities, worst highest. Unknown ranks below low: it is the
// absence of a grade, and letting it outrank a graded low would push
// ungraded noise to the top of every list.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 5
	case SeverityHigh:
		return 4
	case SeverityMedium:
		return 3
	case SeverityLow:
		return 2
	case SeverityUnknown:
		return 1
	default:
		return 0
	}
}

// Weight is what a severity contributes to a comparison verdict.
//
// The gaps are wide on purpose: one critical must outweigh any number of lows,
// because a release that trades a critical for a hundred lows is better and a
// linear scale would say otherwise. Unknown weighs nothing - it cannot be
// allowed to decide "better or worse" on its own, and a comparison whose answer
// rests on ungraded findings is reported as inconclusive instead (see Compare).
func (s Severity) Weight() int {
	switch s {
	case SeverityCritical:
		return 1000
	case SeverityHigh:
		return 100
	case SeverityMedium:
		return 10
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// Label is the severity in the words the interface shows.
func (s Severity) Label() string {
	switch s {
	case SeverityCritical:
		return "Critical"
	case SeverityHigh:
		return "High"
	case SeverityMedium:
		return "Medium"
	case SeverityLow:
		return "Low"
	default:
		return "Unknown"
	}
}

// Valid reports whether s is one of the five.
func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityUnknown:
		return true
	}
	return false
}

// ParseSeverity maps a scanner's spelling onto the ladder.
//
// Anything unrecognised becomes Unknown rather than an error. A scanner that
// invents a sixth word - and they do, "Information", "Negligible", "None" -
// must not fail a release's whole security read; it must land somewhere honest.
func ParseSeverity(s string) Severity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "critical":
		return SeverityCritical
	case "high", "important":
		return SeverityHigh
	case "medium", "moderate":
		return SeverityMedium
	case "low", "negligible", "informational", "information", "info":
		return SeverityLow
	default:
		return SeverityUnknown
	}
}

// SeverityCounts is a count per severity.
//
// A struct rather than a map, because every consumer wants all five keys
// present: a map with no "critical" key renders as a blank cell in a table
// where zero is the answer, and a JSON body whose shape depends on the data is
// harder for a client to hold than one whose shape does not.
type SeverityCounts struct {
	Critical int `json:"critical"`
	High     int `json:"high"`
	Medium   int `json:"medium"`
	Low      int `json:"low"`
	Unknown  int `json:"unknown"`
}

// Add records one finding at the given severity.
func (c *SeverityCounts) Add(s Severity, n int) {
	switch s {
	case SeverityCritical:
		c.Critical += n
	case SeverityHigh:
		c.High += n
	case SeverityMedium:
		c.Medium += n
	case SeverityLow:
		c.Low += n
	default:
		c.Unknown += n
	}
}

// Get returns the count for one severity.
func (c SeverityCounts) Get(s Severity) int {
	switch s {
	case SeverityCritical:
		return c.Critical
	case SeverityHigh:
		return c.High
	case SeverityMedium:
		return c.Medium
	case SeverityLow:
		return c.Low
	default:
		return c.Unknown
	}
}

// Total is every severity summed.
func (c SeverityCounts) Total() int {
	return c.Critical + c.High + c.Medium + c.Low + c.Unknown
}

// Empty reports whether nothing was counted.
func (c SeverityCounts) Empty() bool { return c.Total() == 0 }

// Weight is the comparison weight of everything counted.
func (c SeverityCounts) Weight() int {
	total := 0
	for _, s := range Severities {
		total += c.Get(s) * s.Weight()
	}
	return total
}

// Plus returns the sum of two count sets.
func (c SeverityCounts) Plus(o SeverityCounts) SeverityCounts {
	for _, s := range Severities {
		c.Add(s, o.Get(s))
	}
	return c
}

// Counts is a full account of a set of findings: how many, how severe, how
// many of them anybody can actually do something about, and how many are
// already being exploited.
//
// Fixable is carried separately rather than derived on read because it is the
// number that decides what a release manager does this afternoon. A release
// with 900 non-fixable findings and 4 fixable ones has four pieces of work in
// it, and a summary that reports only 904 hides all four.
//
// KEV is carried for the same reason and it is the stronger of the two. A
// vulnerability on CISA's Known Exploited list is not a risk estimate; it is a
// report that somebody has already used it. Nine hundred criticals graded from
// a CVSS vector and one KEV are not the same backlog, and a summary that can
// only say "901 criticals" cannot tell a release manager which one to open.
type Counts struct {
	Total      int `json:"total"`
	Fixable    int `json:"fixable"`
	NonFixable int `json:"nonFixable"`
	// KEV is the findings a scanner marked known-exploited, and KEVFixable the
	// subset with a version to upgrade to - which is the whole of this
	// afternoon's work, in one number.
	KEV        int `json:"kev"`
	KEVFixable int `json:"kevFixable"`

	BySeverity        SeverityCounts `json:"bySeverity"`
	FixableBySeverity SeverityCounts `json:"fixableBySeverity"`
	// KEVBySeverity grades the exploited ones, because "4 KEVs" and "4 critical
	// KEVs" are read differently and the ladder is what the interface sorts on.
	KEVBySeverity SeverityCounts `json:"kevBySeverity"`
}

// Add records one finding.
func (c *Counts) Add(sev Severity, fixable bool) { c.AddGrade(Grade{Severity: sev, Fixable: fixable}) }

// Grade is what counting one finding needs to know about it.
//
// A struct rather than three positional booleans, because the next scanner will
// bring a fourth fact worth counting - reachability, an exploit maturity - and
// a signature that grows by one parameter per scanner is one every call site
// has to be edited for.
type Grade struct {
	Severity Severity
	Fixable  bool
	// KEV says the vulnerability is on a known-exploited list.
	KEV bool
}

// GradeOf is one finding's countable facts.
func GradeOf(f Finding) Grade {
	return Grade{Severity: f.Severity, Fixable: f.Fixable, KEV: f.KEV}
}

// AddGrade records one finding.
func (c *Counts) AddGrade(g Grade) {
	c.Total++
	c.BySeverity.Add(g.Severity, 1)
	if g.KEV {
		c.KEV++
		c.KEVBySeverity.Add(g.Severity, 1)
		if g.Fixable {
			c.KEVFixable++
		}
	}
	if g.Fixable {
		c.Fixable++
		c.FixableBySeverity.Add(g.Severity, 1)
		return
	}
	c.NonFixable++
}

// Plus returns the sum of two accounts.
func (c Counts) Plus(o Counts) Counts {
	return Counts{
		Total:             c.Total + o.Total,
		Fixable:           c.Fixable + o.Fixable,
		NonFixable:        c.NonFixable + o.NonFixable,
		KEV:               c.KEV + o.KEV,
		KEVFixable:        c.KEVFixable + o.KEVFixable,
		BySeverity:        c.BySeverity.Plus(o.BySeverity),
		FixableBySeverity: c.FixableBySeverity.Plus(o.FixableBySeverity),
		KEVBySeverity:     c.KEVBySeverity.Plus(o.KEVBySeverity),
	}
}

// Empty reports whether nothing was counted.
func (c Counts) Empty() bool { return c.Total == 0 }
