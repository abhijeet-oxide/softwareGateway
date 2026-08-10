package product

import "testing"

// `vendor` is the source-level switch for every vendor-specific behaviour there
// is: how tags are grouped into packages, where signatures are looked for, and
// which parts of a repository path and a tag are structural noise a listing can
// drop.
//
// It exists because those behaviours used to be reached another way — or, for
// the shortening, not gated at all — and a NEAR-shaped assumption was being
// applied to every registry.

func TestVendorSupersedesTheLayoutSpelling(t *testing.T) {
	cases := []struct {
		name   string
		source Source
		want   string
	}{
		{
			name:   "neither set",
			source: Source{},
			want:   "",
		},
		{
			name:   "vendor only",
			source: Source{Vendor: "near"},
			want:   "near",
		},
		{
			// The older spelling, still honoured: a document written before the
			// field moved must keep working unchanged.
			name:   "signatures.layout only",
			source: Source{Signatures: Signatures{Layout: "near"}},
			want:   "near",
		},
		{
			name:   "both, agreeing",
			source: Source{Vendor: "near", Signatures: Signatures{Layout: "near"}},
			want:   "near",
		},
		{
			// Whitespace is not a second vendor.
			name:   "padded",
			source: Source{Vendor: "  near  "},
			want:   "near",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.source.VendorLayout(); got != tc.want {
				t.Errorf("VendorLayout() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Both spellings present and DISAGREEING has no correct reading, and silently
// picking one would leave the operator's other spelling doing nothing while
// looking as though it does something.
func TestRejectsAVendorThatDisagreesWithTheLayout(t *testing.T) {
	p := valid()
	p.Spec.Sources[0].Vendor = "near"
	p.Spec.Sources[0].Signatures.Layout = "standard"

	err := p.Validate(nil)
	if err == nil {
		t.Fatal("expected a validation error for two disagreeing vendor names")
	}
	e := requireField(t, err, "spec.sources[0].vendor")
	// The message has to carry the fix. Naming the conflict without saying
	// which to keep leaves the reader exactly where they started.
	if e.Hint == "" {
		t.Errorf("error on %s has no hint: %+v", e.Field, e)
	}
}

func TestAgreeingSpellingsAreAccepted(t *testing.T) {
	p := valid()
	p.Spec.Sources[0].Vendor = "near"
	p.Spec.Sources[0].Signatures.Layout = "near"

	if err := p.Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

// The NAME is deliberately not checked here. Which vendors exist depends on
// which plugins the binary registers, and this package must not know that — the
// composition root resolves it and fails loudly on an unknown value.
func TestAnUnknownVendorIsNotAValidationError(t *testing.T) {
	p := valid()
	p.Spec.Sources[0].Vendor = "some-vendor-this-build-does-not-have"

	if err := p.Validate(nil); err != nil {
		t.Fatalf("validation should not adjudicate vendor names, got: %v", err)
	}
}
