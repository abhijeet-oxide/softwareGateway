package tlscompat

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"io"
	stdlog "log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMergeGODEBUG(t *testing.T) {
	cases := []struct {
		name    string
		current string
		want    string
	}{
		{"empty", "", "x509negativeserial=1"},
		{"preserves others", "http2debug=1", "http2debug=1,x509negativeserial=1"},
		{"replaces same key", "x509negativeserial=0", "x509negativeserial=1"},
		{"replaces among others", "http2debug=1,x509negativeserial=0,panicnil=1",
			"http2debug=1,x509negativeserial=1,panicnil=1"},
		{"drops empty entries", "http2debug=1,,", "http2debug=1,x509negativeserial=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeGODEBUG(tc.current, "x509negativeserial", "1"); got != tc.want {
				t.Errorf("mergeGODEBUG(%q) = %q, want %q", tc.current, got, tc.want)
			}
		})
	}
}

// TestApplyPreservesExistingGODEBUG is the property that makes this safe to
// call in a container whose image already sets GODEBUG for something else.
func TestApplyPreservesExistingGODEBUG(t *testing.T) {
	t.Setenv("GODEBUG", "http2debug=2")

	Apply(Options{AllowNegativeSerialNumbers: true}, discardLogger())

	got := os.Getenv("GODEBUG")
	if !strings.Contains(got, "http2debug=2") {
		t.Errorf("GODEBUG = %q, lost the pre-existing http2debug entry", got)
	}
	if !strings.Contains(got, "x509negativeserial=1") {
		t.Errorf("GODEBUG = %q, missing x509negativeserial=1", got)
	}
}

func TestApplyNoOptionsChangesNothing(t *testing.T) {
	t.Setenv("GODEBUG", "http2debug=2")

	if applied := Apply(Options{}, discardLogger()); applied != nil {
		t.Errorf("Apply with no options returned %v, want nil", applied)
	}
	if got := os.Getenv("GODEBUG"); got != "http2debug=2" {
		t.Errorf("GODEBUG = %q, want it untouched", got)
	}
}

// TestNegativeSerialHandshake is the test this whole package exists for.
//
// It measures three things against a real TLS server whose certificate has a
// negative serial number:
//
//  1. the default client fails, which is the error the operator reported;
//  2. InsecureSkipVerify ALSO fails, with the same message - the correction,
//     since skipping verification is the obvious guess and it does not work;
//  3. Apply fixes it.
//
// Point 2 is the one worth having a test for. It is the difference between
// shipping a fix and shipping something that looks like one.
func TestNegativeSerialHandshake(t *testing.T) {
	// Restored after the test, so the rest of the suite runs with stock rules.
	t.Setenv("GODEBUG", "")

	url, pool := negativeSerialServer(t)

	verifying := func() *http.Client {
		return &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		}}
	}
	skipping := func() *http.Client {
		return &http.Client{Transport: &http.Transport{
			// InsecureSkipVerify is the whole point of this case: it must NOT help.
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true},
		}}
	}

	// 1. Stock rules: the reported failure.
	err := get(verifying(), url)
	if err == nil {
		t.Fatal("expected the handshake to fail with stock x509 rules; the test " +
			"certificate does not have a negative serial number")
	}
	if !strings.Contains(err.Error(), "negative serial number") {
		t.Fatalf("expected a negative-serial parse failure, got: %v", err)
	}

	// 2. InsecureSkipVerify does NOT help: the parse fails before verification.
	if err := get(skipping(), url); err == nil {
		t.Error("InsecureSkipVerify succeeded - if this ever passes, the doc " +
			"comments in product.TLS and transport.Config are wrong and must be corrected")
	} else if !strings.Contains(err.Error(), "negative serial number") {
		t.Errorf("expected the same negative-serial failure with InsecureSkipVerify, got: %v", err)
	}

	// 3. Apply fixes it, with verification still fully on.
	if applied := Apply(Options{AllowNegativeSerialNumbers: true}, discardLogger()); len(applied) != 1 {
		t.Fatalf("Apply returned %d settings, want 1", len(applied))
	}
	if err := get(verifying(), url); err != nil {
		t.Fatalf("after Apply the handshake should succeed with verification ON: %v", err)
	}
}

// negativeSerialServer starts a TLS server whose leaf certificate carries a
// negative serial number, and returns the pool that trusts it.
//
// The certificate has to be assembled by hand. x509.CreateCertificate enforces
// the positive-serial rule on the WRITE path too, and unlike the read path that
// check is unconditional - no GODEBUG relaxes it. So the certificate is minted
// with a positive serial, the serial's high bit is flipped inside the raw
// TBSCertificate, and the result is re-signed. Re-signing is what keeps the
// certificate genuinely valid, so the third assertion can verify the chain
// properly rather than passing because verification was off.
func negativeSerialServer(t *testing.T) (url string, pool *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Eight bytes with the top bit of the first byte clear, so the DER INTEGER
	// is exactly `02 08 <bytes>` with no leading pad - which makes it findable,
	// and makes flipping that bit a same-length edit.
	serialBytes := []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}

	template := &x509.Certificate{
		SerialNumber:          new(big.Int).SetBytes(serialBytes),
		Subject:               pkix.Name{CommonName: "negative-serial.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	// Certificate ::= SEQUENCE { tbsCertificate, signatureAlgorithm, signature }
	var cert struct {
		TBS  asn1.RawValue
		Algo pkix.AlgorithmIdentifier
		Sig  asn1.BitString
	}
	if _, err := asn1.Unmarshal(der, &cert); err != nil {
		t.Fatalf("unmarshal certificate: %v", err)
	}

	tbs := append([]byte(nil), cert.TBS.FullBytes...)
	pattern := append([]byte{0x02, byte(len(serialBytes))}, serialBytes...)
	i := bytes.Index(tbs, pattern)
	if i < 0 {
		t.Fatal("serial number INTEGER not found in the TBSCertificate")
	}
	tbs[i+2] |= 0x80 // two's complement: the high bit makes it negative

	// Re-sign the modified TBS. ECDSA P-256 keys get ECDSA-SHA256 from
	// CreateCertificate, so SHA-256 matches the AlgorithmIdentifier we carry
	// over unchanged.
	digest := sha256.Sum256(tbs)
	sig, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	cert.TBS = asn1.RawValue{FullBytes: tbs}
	cert.Sig = asn1.BitString{Bytes: sig, BitLength: len(sig) * 8}

	der, err = asn1.Marshal(cert)
	if err != nil {
		t.Fatalf("marshal certificate: %v", err)
	}

	// Confirm the fixture is what it claims to be, with the relaxation on. If
	// this fails the rest of the test proves nothing.
	restore := os.Getenv("GODEBUG")
	if err := os.Setenv("GODEBUG", mergeGODEBUG(restore, "x509negativeserial", "1")); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse patched certificate: %v", err)
	}
	if err := os.Setenv("GODEBUG", restore); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	if leaf.SerialNumber.Sign() >= 0 {
		t.Fatalf("patched serial is %v, want negative", leaf.SerialNumber)
	}
	if err := leaf.CheckSignatureFrom(leaf); err != nil {
		t.Fatalf("patched certificate is not self-consistent: %v", err)
	}

	pool = x509.NewCertPool()
	pool.AddCert(leaf)

	// Not httptest.NewTLSServer: StartTLS re-parses the certificate under stock
	// rules and panics on exactly the certificate this test needs. A plain
	// listener hands the DER straight to the handshake, with Leaf pre-parsed so
	// crypto/tls never has to parse it either.
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}},
	})

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
		// The first two clients are EXPECTED to abort the handshake, so the
		// server's complaints about it are noise that reads like failure.
		ErrorLog: stdlog.New(io.Discard, "", 0),
	}
	go func() { _ = srv.Serve(tlsLn) }()
	t.Cleanup(func() { _ = srv.Close() })

	return "https://" + ln.Addr().String(), pool
}

func get(c *http.Client, url string) error {
	resp, err := c.Get(url) //nolint:noctx // a loopback test server
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
