package product

import "fmt"

// Warning is a configuration that is VALID and probably not what was meant.
//
// It exists because the most expensive mistakes in this schema are not the
// ones that fail. A mirror whose tag glob excludes every signature is a
// perfectly well-formed document: it applies cleanly, syncs successfully, and
// then breaks destination verification for every package with no visible
// cause. Rejecting it outright would be wrong — a product that does not verify
// at the destination has no reason to mirror signatures — so the only honest
// treatment is to say so and let the reader decide.
//
// Warnings never fail validation and never block a load. `transferctl config
// validate` prints them, which puts them in front of a human before merge,
// which is the only place they are cheap.
type Warning struct {
	Field   string
	Message string
	Hint    string
}

func (w Warning) String() string {
	if w.Hint != "" {
		return fmt.Sprintf("%s: %s — %s", w.Field, w.Message, w.Hint)
	}
	return fmt.Sprintf("%s: %s", w.Field, w.Message)
}

// computeWarnings runs every advisory check over a document that has already
// validated.
func (p *Product) computeWarnings() []Warning {
	var out []Warning
	out = append(out, p.warnMirrorExcludesSignatures()...)
	out = append(out, p.warnRedundantVendorEgress()...)
	return out
}

// warnMirrorExcludesSignatures is defence 1 of 3 against the trap in
// docs/design/18 section 9.
//
// Cosign publishes a signature as the TAG `sha256-<hex>.sig`. A mirror rule of
// `v3.*` — the obvious rule, the one anybody would write — matches every
// release and not one signature. Quay reports success. The images arrive.
// Destination verification then fails for every package, and the cause is a
// field three screens away that looks correct.
//
// Only warned when the product actually verifies at the destination and
// transfers signatures, because otherwise there is nothing wrong with it.
func (p *Product) warnMirrorExcludesSignatures() []Warning {
	var out []Warning
	for i, t := range p.Spec.Targets {
		m, ok := t.MirrorSettings()
		if !ok || len(m.Tags) == 0 || SelectsSignatures(m.Tags) {
			continue
		}
		v := p.VerificationFor(t.Verification)
		if !v.Enabled || !v.AtDestination || !v.ShouldTransferSignatures() {
			continue
		}
		out = append(out, Warning{
			Field: fmt.Sprintf("spec.targets[%d].replication.mirror.tags", i),
			Message: fmt.Sprintf(
				"no glob can match a cosign signature tag, but %q verifies at the destination",
				t.Name),
			Hint: "signatures are published as tags of the form `sha256-<hex>.sig`, so this mirror will " +
				"copy the images and leave every signature behind — the sync will report success and " +
				"verification will then fail for every package. Add 'sha256-*.sig' to the list",
		})
	}
	return out
}

// warnRedundantVendorEgress flags two copy targets pulling the same content
// from the origin when one of them could mirror from the other.
//
// Not an error: a second independent copy is occasionally exactly the point,
// which is what a disaster-recovery registry is for. But on a metered or
// contended vendor link it is usually an accident, and it is invisible —
// nothing in the configuration says "this costs twice".
func (p *Product) warnRedundantVendorEgress() []Warning {
	var quay []string
	copies := 0
	for _, t := range p.Spec.Targets {
		if !t.IsEnabled() || t.PromotionOnly {
			continue
		}
		if t.ReplicationMode() != ReplicationCopy {
			continue
		}
		copies++
		if t.Type == RegistryQuay {
			quay = append(quay, t.Name)
		}
	}
	if copies < 2 || len(quay) == 0 {
		return nil
	}
	return []Warning{{
		Field: "spec.targets",
		Message: fmt.Sprintf(
			"%d targets copy from the origin independently, and %v is a Quay registry that could mirror from one of them instead",
			copies, quay),
		Hint: "each copy target pulls the full package from the vendor, so this multiplies vendor egress; " +
			"set replication.mode: mirror with mirror.from on the Quay target to pull once and have Quay fetch the rest",
	}}
}
