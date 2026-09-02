// Command fakeregistry serves the development OCI registries.
//
// The interface is only honest against real registry responses: discovery walks
// tag lists, a comparison resolves two manifests and diffs their trees, and a
// transfer streams blobs. Pointing the development Coordinator at hostnames
// that do not resolve gives every one of those a Bad Gateway, so the seeded
// database can show a package list and nothing that reads from a registry.
//
// This wraps test/fakeregistry - the same in-process OCI Distribution server
// the suite runs against - in a TLS listener per vendor hostname, seeded with
// the release trees dev/seed/seed.py records in the database. One binary, no
// Docker, and the products in dev/products keep their real-looking names.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/test/fakeregistry"
)

// release is one tagged bundle: an index over per-component image manifests.
type release struct {
	tag        string
	ageDays    int
	components []component
}

// published is when the vendor shipped it. Derived from ageDays so the whole
// catalogue slides forward with the clock rather than aging into 2024.
//
// TRUNCATED TO THE DAY, and that is not cosmetic. This timestamp goes into
// `org.opencontainers.image.created` on every manifest, so it is part of every
// manifest's digest - and at second resolution the whole catalogue was
// re-digested on every restart of this binary. A registry restart therefore
// invalidated the seeded database: the Coordinator held digests nothing served
// any more, and every walk, compliance run and comparison answered 404 until
// discovery ran again. Which is thirty minutes away by default, so in practice
// the estate was simply broken until somebody re-seeded it.
//
// A day is stable enough that restarting the registry - which dev/seed/up.sh
// itself does, to switch the declared sizes - leaves the database valid, and
// coarse enough that nothing on screen reads differently.
func (r release) published() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -r.ageDays)
}

type component struct {
	name   string
	title  string
	layers int
}

// catalogue mirrors dev/seed/seed.py. The two are seeded together, so a release
// the database lists is a release this serves.
var catalogue = map[string][]release{
	"mavenir/converged-core": {
		{"25.11.0", 6, mavenir(12)},
		{"25.10.2", 34, mavenir(12)},
		{"25.10.1", 61, mavenir(10)},
		{"25.09.0", 96, mavenir(9)},
		{"26.01.0-rc3", 1, mavenir(8)},
	},
	"ericsson/cloud-ran": {
		{"24.Q4.2", 11, ericsson(6)},
		{"24.Q4.1", 45, ericsson(6)},
		{"24.Q3.4", 88, ericsson(4)},
		{"25.Q1.0-beta1", 2, ericsson(3)},
	},
	"nokia/cmm": {
		{"23.5.1", 19, nokia(5)},
		{"23.5.0", 52, nokia(5)},
		{"23.4.3", 120, nokia(3)},
	},
	// A NEAR orb, and the reason it is here.
	//
	// Every other repository above publishes conformantly: a chart declares
	// itself with the Helm config media type, and the OCI rules are enough. A
	// NEAR orb's charts are plain image manifests carrying an ORDINARY IMAGE
	// CONFIG - media type, artifact type and config media type are identical
	// for its charts and its images - and the only evidence anywhere is the
	// annotation the vendor wrote on the index's children.
	//
	// Without one of these in the estate, every reader that classifies
	// artifacts is exercised only on the easy path. A compliance run selecting
	// charts by config media type passed every test here and answered "this
	// release ships no Helm charts" for a real orb of ninety-five.
	"orbs/cfx-5000-k8s": {
		// Ninety-five charts and a hundred and fifty images, which is what a
		// real orb ships and the only fixture here at that scale.
		{"orb_24.7.3099", 7, orb(95, 150)},
		{"orb_24.6.2871", 40, orb(58, 92)},
	},
}

func mavenir(n int) []component {
	all := []component{
		{"cfx-amf", "Access & Mobility Function", 5},
		{"cfx-smf", "Session Management Function", 5},
		{"cfx-upf", "User Plane Function", 6},
		{"cfx-ausf", "Authentication Server", 4},
		{"cfx-udm", "Unified Data Management", 4},
		{"cfx-nrf", "Network Repository Function", 3},
		{"cfx-pcf", "Policy Control Function", 4},
		{"cfx-nssf", "Network Slice Selection", 3},
		{"cfx-operator", "Kubernetes operator", 3},
		{"cfx-observability", "Metrics and tracing sidecar", 4},
		{"cfx-core", "Umbrella Helm chart", 1},
		{"cfx-crds", "Custom resource definitions", 1},
	}
	return all[:min(n, len(all))]
}

func ericsson(n int) []component {
	all := []component{
		{"cu-cp", "Centralised Unit - control plane", 5},
		{"cu-up", "Centralised Unit - user plane", 6},
		{"du-manager", "Distributed Unit manager", 4},
		{"ric-platform", "Near-RT RIC platform", 5},
		{"ran-analytics", "RAN analytics pipeline", 4},
		{"cloud-ran", "Umbrella Helm chart", 1},
	}
	return all[:min(n, len(all))]
}

// orb is a NEAR-shaped release: the same mix of charts and images as any other,
// published so that only the vendor annotation tells them apart.
// orb builds a NEAR orb: charts and images, in the proportion a real one ships.
//
// # Why this one is generated and the others are not
//
// A real orb is ninety-five charts beside a hundred and fifty images, and that
// scale is not decoration - it is the thing several parts of this system exist
// to survive. A compliance run of nine charts is over before the progress panel
// has drawn itself; a run of ninety-five is minutes, and it is the only fixture
// in this estate that exercises the concurrent fetch and render, the
// per-release budgets, the stage timings and the progress log at the size a
// vendor actually delivers.
//
// A component is a chart here exactly when it has ONE layer, because that is
// what orbAnnotations turns into `com.nokia.ncd.orb.type: helmchart` - the one
// piece of evidence distinguishing the two in a NEAR index.
//
// The named services come first so a release still reads as a packet core
// rather than as a list of numbers, and the rest are filled in behind them.
func orb(charts, images int) []component {
	// Network functions a 5G core actually carries, so the coverage table reads
	// as a product and not as cfx-13 through cfx-95.
	roles := []string{
		"amf", "smf", "upf", "nrf", "ausf", "udm", "pcf", "nssf", "nef", "nwdaf",
		"bsf", "chf", "scp", "sepp", "lmf", "gmlc", "eir", "n3iwf", "tngf", "af",
		"dra", "hss", "cgf", "smsf", "udf", "mfaf", "dccf", "adrf", "nssaaf", "ucmf",
	}
	name := func(prefix string, i int) string {
		role := roles[i%len(roles)]
		if g := i / len(roles); g > 0 {
			return fmt.Sprintf("cfx-%s-%s%d", role, prefix, g+1)
		}
		return "cfx-" + role + "-" + prefix
	}

	out := make([]component, 0, charts+images)
	// The umbrella and the CRDs are the two every orb has, and the two a
	// reader looks for first.
	fixed := []component{
		{"cfx-core", "Umbrella Helm chart", 1},
		{"cfx-crds", "Custom resource definitions", 1},
		{"cfx-network", "Secondary network attachments", 1},
		{"cfx-observability", "Metrics and tracing", 1},
	}
	for i := 0; i < charts; i++ {
		if i < len(fixed) {
			out = append(out, fixed[i])
			continue
		}
		out = append(out, component{
			name:  name("chart", i-len(fixed)),
			title: strings.ToUpper(roles[(i-len(fixed))%len(roles)]) + " Helm chart",
			// One layer, which is what makes it a chart.
			layers: 1,
		})
	}
	for i := 0; i < images; i++ {
		out = append(out, component{
			name:  name("img", i),
			title: strings.ToUpper(roles[i%len(roles)]) + " container image",
			// Two or more, which is what makes it an image.
			layers: 2 + i%5,
		})
	}
	return out
}

func nokia(n int) []component {
	all := []component{
		{"cmm-mme", "Mobility Management Entity", 5},
		{"cmm-sgw", "Serving Gateway", 4},
		{"cmm-pgw", "Packet Gateway", 5},
		{"cmm-hss-proxy", "HSS proxy", 3},
		{"cmm", "Umbrella Helm chart", 1},
	}
	return all[:min(n, len(all))]
}

// hosts maps a listen address to the vendor hostname it answers for, and to
// the repositories it serves. The destination registry starts empty: a transfer
// is what puts anything there, which is the whole point of watching one run.
var hosts = []struct {
	addr  string
	names []string
	repos []string
}{
	{":9443", []string{
		"registry.mavenir.example.com",
		"registry.ericsson.example.com",
		"registry.nokia.example.com",
		"registry.near.example.com",
	}, []string{"mavenir/converged-core", "ericsson/cloud-ran", "nokia/cmm", "orbs/cfx-5000-k8s"}},
	{":9444", []string{"artifactory.internal.example.com"}, nil},
}

// inflate multiplies the layer SIZES a manifest declares, without changing the
// bytes stored behind them.
//
// It exists because two things a demo needs are in direct conflict. Releases
// have to weigh what real ones weigh - a 23 GB packet core is the whole point
// of this system - and a fake registry that really served 23 GB would need 23 GB
// of memory. Declaring the size without serving it breaks the push, because the
// Worker sets Content-Length from the descriptor.
//
// So: seed and transfer with honest kilobyte sizes, then restart with -inflate
// once the bytes have moved. Every page then agrees on what a release weighs,
// including the release comparison, which reads the registry live rather than
// the database. A transfer started after that WILL fail - restart without the
// flag first. dev/seed/up.sh does both halves in order.
var inflate = flag.Bool("inflate", false,
	"declare realistic layer sizes without storing them (run only after transfers have settled)")

func main() {
	flag.Parse()

	for _, h := range hosts {
		reg := fakeregistry.New()
		seed(reg, h.repos)

		backend, err := url.Parse(reg.URL())
		if err != nil {
			log.Fatalf("parse backend url: %v", err)
		}
		proxy := httputil.NewSingleHostReverseProxy(backend)

		cert, err := selfSigned(h.names)
		if err != nil {
			log.Fatalf("certificate for %v: %v", h.names, err)
		}
		srv := &http.Server{
			Addr:              h.addr,
			Handler:           proxy,
			TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
			ReadHeaderTimeout: 10 * time.Second,
		}
		ln, err := net.Listen("tcp", h.addr)
		if err != nil {
			log.Fatalf("listen %s: %v", h.addr, err)
		}
		fmt.Printf("serving %s on %s (%d repositories)\n",
			strings.Join(h.names, ", "), h.addr, len(h.repos))
		go func() {
			if err := srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("serve: %v", err)
			}
		}()
	}

	select {}
}

// seed builds every release of every repository.
//
// A component image is stored in the release's OWN repository and left
// untagged, which is how a vendor bundle actually ships: the index carries the
// release tag, its children are addressed by digest, and the tag list therefore
// names releases rather than every image inside them. Seeding the children into
// per-component repositories instead is what made the transfer planner 404 -
// the index referenced digests the repository did not hold.
//
// The index is assembled here rather than through AddAnnotatedIndex because the
// per-child `org.opencontainers.image.ref.name` annotation is the thing the
// interface aligns two releases on, and that helper only annotates the index.
func seed(reg *fakeregistry.Registry, repos []string) {
	for _, repo := range repos {
		for _, rel := range catalogue[repo] {
			children := make([]map[string]any, 0, len(rel.components))
			for _, comp := range rel.components {
				layers := make([]fakeregistry.Layer, 0, comp.layers)
				weights := layerWeights(comp)
				for i := 0; i < comp.layers; i++ {
					l := fakeregistry.NewLayer(fmt.Sprintf(
						"%s|%s|%s|layer-%d|%s",
						repo, comp.name, rel.tag, i, filler(comp.name, rel.tag, i)))
					l.Annotations = map[string]string{
						"org.opencontainers.image.title": layerTitle(i),
					}
					if *inflate {
						l.Size = weights[i]
					}
					layers = append(layers, l)
				}
				// A chart component gets a REAL chart, so `helm template`
				// can render it and the compliance run has something to
				// judge. Everything else is filler, because nothing else
				// reads the bytes.
				//
				// A NEAR repository publishes its charts as ORDINARY IMAGE
				// MANIFESTS - AddImage, not AddChart - so the config media
				// type says "image config" and the annotation below is the
				// only thing that says otherwise. That is the whole point of
				// having one of these in the estate.
				var digest string
				switch {
				case comp.layers != 1:
					digest = reg.AddImage(repo, "", layers...)
				case isNEAR(repo):
					chart := fakeregistry.NewLayer(string(chartTarball(comp.name, rel.tag)))
					digest = reg.AddImage(repo, "", chart)
				default:
					digest = reg.AddChart(repo, "", chartTarball(comp.name, rel.tag))
				}
				raw, ok := reg.ManifestBytes(repo, digest)
				if !ok {
					log.Fatalf("seed: %s/%s has no manifest", repo, comp.name)
				}

				children = append(children, map[string]any{
					"mediaType":   "application/vnd.oci.image.manifest.v1+json",
					"digest":      digest,
					"size":        len(raw),
					"platform":    map[string]any{"os": "linux", "architecture": "amd64"},
					"annotations": orbAnnotations(repo, comp, rel),
				})
			}

			index := map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.oci.image.index.v1+json",
				"manifests":     children,
				"annotations": map[string]string{
					"org.opencontainers.image.version": rel.tag,
					"org.opencontainers.image.title":   repo,
					"org.opencontainers.image.created": rel.published().Format(time.RFC3339),
				},
			}
			raw, err := json.Marshal(index)
			if err != nil {
				log.Fatalf("seed: marshal index %s:%s: %v", repo, rel.tag, err)
			}
			reg.AddManifest(repo, rel.tag, raw, "application/vnd.oci.image.index.v1+json")
		}
	}
}

// layerWeights splits a component's declared size across its layers, base layer
// first and largest, the way an image built on a distribution base is.
//
// The total is derived from the component NAME rather than randomly, so the same
// component weighs the same in every release: a User Plane Function that is
// 3.4 GB in one release and 0.9 GB in the next would make every comparison read
// as a rebuild. dev/seed/dress.py derives the database's sizes the same way, so
// the two agree without either reading the other.
func layerWeights(comp component) []int64 {
	const gb = 1 << 30
	sum := sha256.Sum256([]byte(comp.name))
	seed := int64(binary.BigEndian.Uint32(sum[:4]))

	if comp.layers == 1 { // a Helm chart, which is megabytes and stays so
		return []int64{1<<20 + seed%(3<<20)}
	}

	total := int64(float64(gb) * (0.6 + float64(seed%3400)/1000.0))
	out := make([]int64, comp.layers)
	remaining := total
	for i := range out {
		var share int64
		switch {
		case i == 0:
			share = total * 45 / 100
		case i == comp.layers-1:
			share = remaining
		default:
			share = remaining / int64(comp.layers-i)
		}
		if share < 64<<10 {
			share = 64 << 10
		}
		out[i] = share
		remaining -= share
		if remaining < 0 {
			remaining = 0
		}
	}
	return out
}

func layerTitle(i int) string {
	titles := []string{
		"base rootfs (ubuntu 22.04)", "runtime dependencies", "application binaries",
		"configuration and licences", "python site-packages", "JVM and shared libraries",
	}
	return titles[i%len(titles)]
}

// filler pads a layer so sizes differ between components and releases the way
// real ones do; a tree whose every layer is the same size hides every bug in
// the size arithmetic.
func filler(repo, tag string, i int) string {
	n := (len(repo)*7 + len(tag)*13 + i*29) % 800
	return strings.Repeat("x", 64+n)
}

// selfSigned issues one certificate covering every hostname a listener answers
// for. The products set `insecureSkipVerify: true`, so this only has to exist -
// but issuing it properly costs nothing and keeps the handshake ordinary.
func selfSigned(names []string) (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"softwareGateway development"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              append([]string{"localhost"}, names...),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

var _ = os.Exit

// isNEAR reports whether a repository publishes the NEAR way.
func isNEAR(repo string) bool { return strings.HasPrefix(repo, "orbs/") }

// orbAnnotations are the per-child annotations on an index.
//
// For a NEAR repository this includes `com.nokia.ncd.orb.type`, which is the
// ONLY evidence distinguishing a chart from an image there - discovery records
// what the index listed without fetching each child, so the config media type
// is not available at that point either.
func orbAnnotations(repo string, comp component, rel release) map[string]string {
	out := map[string]string{
		"org.opencontainers.image.ref.name": repo + "/" + comp.name + ":" + rel.tag,
		"org.opencontainers.image.title":    comp.title,
		"org.opencontainers.image.version":  rel.tag,
	}
	if isNEAR(repo) {
		if comp.layers == 1 {
			out["com.nokia.ncd.orb.type"] = "helmchart"
		} else {
			out["com.nokia.ncd.orb.type"] = "cnfimage"
		}
	}
	return out
}
