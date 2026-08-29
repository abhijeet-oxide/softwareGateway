package regclient

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/abhijeet-oxide/softwareGateway/internal/product"
	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// The bug these exist for.
//
// The worker read its products once, at startup, and never again. A product
// added or corrected while it was running did not exist as far as it was
// concerned - the Coordinator reloaded, planned the work and handed it out, and
// every job came back "product … is not configured on this worker" until
// somebody restarted the process.
//
// Two halves have to work for a restart to stop being the fix: the registry has
// to see the new configuration, and the client CACHE in front of it has to stop
// answering from the old one.

func productNamed(name, registry string) *product.Product {
	p := &product.Product{Spec: product.Spec{
		Sources: []product.Source{
			// Anonymous, so these cases turn on the reload rather than on the
			// secret volume a unit test has no business mounting.
			{Name: "vendor", Registry: registry, Repository: "vendor/suite", Anonymous: true},
		},
	}}
	p.Metadata.Name = name
	return p
}

func endpoint(productName, registry string) v1.JobEndpoint {
	return v1.JobEndpoint{
		Product: productName, Name: "vendor", Role: "source",
		Registry: registry, Repository: "vendor/suite", Type: "generic",
	}
}

func newTestClients(reg *product.Registry) *Clients {
	return NewClients(reg, product.NewSecretResolver(""), "/etc/softwaregateway/products",
		slog.New(slog.DiscardHandler))
}

// A product that arrives after the process started is usable without a restart.
func TestAProductAddedAfterStartupIsResolvable(t *testing.T) {
	reg := product.NewRegistry()
	clients := newTestClients(reg)

	if _, err := clients.For(endpoint("late", "registry.example.com")); err == nil {
		t.Fatal("resolved a product nothing had loaded")
	}

	reg.Swap(product.LoadResult{Valid: []*product.Product{
		productNamed("late", "registry.example.com"),
	}})

	if _, err := clients.For(endpoint("late", "registry.example.com")); err != nil {
		t.Fatalf("still unresolvable after a reload that added it: %v", err)
	}
}

// THE HALF THAT IS EASY TO MISS. The cache is keyed by endpoint, so a product
// resolved before a reload keeps answering from the client built out of the OLD
// configuration - rotate a credential or correct a proxy and the running worker
// never notices.
func TestAReloadDiscardsClientsBuiltFromTheOldConfiguration(t *testing.T) {
	reg := product.NewRegistry()
	reg.Swap(product.LoadResult{Valid: []*product.Product{
		productNamed("p", "registry.example.com"),
	}})
	clients := newTestClients(reg)

	first, err := clients.For(endpoint("p", "registry.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	// Cached: the same handle, which is the optimisation this must not break.
	again, err := clients.For(endpoint("p", "registry.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("a second resolution built a second client, so the cache is not working " +
			"and every blob will pay for its own token exchange")
	}

	reg.Swap(product.LoadResult{Valid: []*product.Product{
		productNamed("p", "registry.example.com"),
	}})

	rebuilt, err := clients.For(endpoint("p", "registry.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt == first {
		t.Error("the client survived a configuration reload, so a rotated credential, " +
			"a corrected proxy or a raised connection ceiling would need a restart")
	}
}

// A reload that changes nothing must not throw the cache away: the point of the
// cache is that one handle serves every job against a repository.
func TestResolvingRepeatedlyWithoutAReloadKeepsTheCache(t *testing.T) {
	reg := product.NewRegistry()
	reg.Swap(product.LoadResult{Valid: []*product.Product{
		productNamed("p", "registry.example.com"),
	}})
	clients := newTestClients(reg)

	first, err := clients.For(endpoint("p", "registry.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		got, err := clients.For(endpoint("p", "registry.example.com"))
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatal("the cache was discarded without a reload")
		}
	}
}

// The message a worker gives when it genuinely cannot see a product has to name
// where it looked, because the reader's instinct is to go and check the
// Coordinator - which planned the job correctly.
func TestAnUnknownProductSaysWhereThisWorkerLooked(t *testing.T) {
	reg := product.NewRegistry()
	reg.Swap(product.LoadResult{Valid: []*product.Product{
		productNamed("known", "registry.example.com"),
	}})
	clients := newTestClients(reg)

	_, err := clients.For(endpoint("missing", "registry.example.com"))
	if err == nil {
		t.Fatal("resolved a product that is not loaded")
	}
	for _, want := range []string{"missing", "/etc/softwaregateway/products", "known"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q:\n%s", want, err)
		}
	}
}
