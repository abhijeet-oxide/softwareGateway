package discovery

import (
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

func boolPtr(b bool) *bool { return &b }

// TestInsecureSkipVerifyReachesTheClientConfig is the test that would have
// caught shipping the schema field without wiring it: the option existed, the
// documentation described it, and applyNetwork ignored it.
func TestInsecureSkipVerifyReachesTheClientConfig(t *testing.T) {
	cases := []struct {
		name     string
		product  product.Network
		override *product.Network
		want     bool
	}{
		{name: "unset", want: false},
		{
			name:    "product level",
			product: product.Network{TLS: &product.TLS{InsecureSkipVerify: boolPtr(true)}},
			want:    true,
		},
		{
			name:     "source level",
			override: &product.Network{TLS: &product.TLS{InsecureSkipVerify: boolPtr(true)}},
			want:     true,
		},
		{
			name:     "source turns off what the product turned on",
			product:  product.Network{TLS: &product.TLS{InsecureSkipVerify: boolPtr(true)}},
			override: &product.Network{TLS: &product.TLS{InsecureSkipVerify: boolPtr(false)}},
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &product.Product{
				Metadata: product.Metadata{Name: "vendor-a"},
				Spec:     product.Spec{Network: tc.product},
			}

			var cfg registry.ClientConfig
			if err := applyNetwork(&cfg, p, tc.override, product.NewSecretResolver(t.TempDir())); err != nil {
				t.Fatalf("applyNetwork: %v", err)
			}
			if cfg.InsecureSkipVerify != tc.want {
				t.Errorf("ClientConfig.InsecureSkipVerify = %v, want %v",
					cfg.InsecureSkipVerify, tc.want)
			}
		})
	}
}

// TestSourceSpecCarriesInsecureFlag: the controller logs from this field on
// every reconcile, so a source that silently lost it would go quiet.
func TestSourceSpecCarriesInsecureFlag(t *testing.T) {
	p := &product.Product{
		Metadata: product.Metadata{Name: "vendor-a"},
		Spec: product.Spec{
			Sources: []product.Source{{
				Name: "primary", Registry: "registry.example.com",
				Repository: "platform/suite", Anonymous: true,
				Network: &product.Network{TLS: &product.TLS{InsecureSkipVerify: boolPtr(true)}},
			}},
		},
	}

	specs, errs := SourceSpecs(
		[]*product.Product{p},
		map[string]ProductRef{"vendor-a": {ID: 1, Repositories: map[string]int64{"platform/suite": 1}}},
		product.NewSecretResolver(t.TempDir()),
	)
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d specs, want 1", len(specs))
	}
	if !specs[0].InsecureTLS {
		t.Error("SourceSpec.InsecureTLS is false; the warning would never be logged")
	}
}
