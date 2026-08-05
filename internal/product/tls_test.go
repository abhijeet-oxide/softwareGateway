package product

import "testing"

// TestSkipsTLSVerificationInheritance covers the reason InsecureSkipVerify is a
// pointer. Without it the last two cases are indistinguishable from silence,
// and a source could never keep verification on under a product that turned it
// off.
func TestSkipsTLSVerificationInheritance(t *testing.T) {
	cases := []struct {
		name     string
		product  Network
		override *Network
		want     bool
	}{
		{
			name: "absent everywhere means verified",
			want: false,
		},
		{
			name:    "product-level on, no override",
			product: Network{TLS: &TLS{InsecureSkipVerify: boolPtr(true)}},
			want:    true,
		},
		{
			name:     "product-level on, override says nothing: inherited",
			product:  Network{TLS: &TLS{InsecureSkipVerify: boolPtr(true)}},
			override: &Network{},
			want:     true,
		},
		{
			name:     "product-level on, override has a tls block but no choice",
			product:  Network{TLS: &TLS{InsecureSkipVerify: boolPtr(true)}},
			override: &Network{TLS: &TLS{}},
			want:     true,
		},
		{
			name:     "override turns it back OFF",
			product:  Network{TLS: &TLS{InsecureSkipVerify: boolPtr(true)}},
			override: &Network{TLS: &TLS{InsecureSkipVerify: boolPtr(false)}},
			want:     false,
		},
		{
			name:     "only the override turns it on",
			override: &Network{TLS: &TLS{InsecureSkipVerify: boolPtr(true)}},
			want:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SkipsTLSVerification(tc.product, tc.override); got != tc.want {
				t.Errorf("SkipsTLSVerification = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNilTLSHelpersAreSafe(t *testing.T) {
	var n *TLS
	if n.SetsSkipVerify() || n.SkipsVerify() {
		t.Error("a nil TLS block must report neither a choice nor a skip")
	}
}

func TestRejectsCABundleAlongsideSkipVerify(t *testing.T) {
	p := valid()
	p.Spec.Network = Network{
		CABundleRef: &SecretRef{SecretName: "vendor-ca"},
		TLS:         &TLS{InsecureSkipVerify: boolPtr(true)},
	}

	err := p.Validate(nil)
	e := requireField(t, err, "spec.network.tls.insecureSkipVerify")
	if e.Hint == "" {
		t.Error("the error should say what to do about it")
	}
}

// A caBundleRef at product level with insecureSkipVerify on ONE source is the
// shape the override exists for, and must stay legal.
func TestAllowsCABundleAtProductAndSkipVerifyAtSource(t *testing.T) {
	p := valid()
	p.Spec.Network = Network{CABundleRef: &SecretRef{SecretName: "vendor-ca"}}
	p.Spec.Sources[0].Network = &Network{TLS: &TLS{InsecureSkipVerify: boolPtr(true)}}

	if err := p.Validate(nil); err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}
