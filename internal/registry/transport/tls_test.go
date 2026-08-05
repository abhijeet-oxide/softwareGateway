package transport

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestInsecureSkipVerifyReachesTheHandshake proves the flag does what the
// configuration says it does, against a real server with an untrusted
// certificate — rather than asserting that a struct field was copied.
func TestInsecureSkipVerifyReachesTheHandshake(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0)
	defer srv.Close()

	t.Run("verifying client is refused", func(t *testing.T) {
		c := mustClient(t, Config{Registry: "127.0.0.1"})
		err := probe(c, srv.URL)
		if err == nil {
			t.Fatal("expected the untrusted certificate to be rejected")
		}
		var unknown x509.UnknownAuthorityError
		var hostErr x509.HostnameError
		if !errors.As(err, &unknown) && !errors.As(err, &hostErr) {
			t.Fatalf("expected a certificate verification failure, got: %v", err)
		}
	})

	t.Run("skipping client connects", func(t *testing.T) {
		c := mustClient(t, Config{Registry: "127.0.0.1", InsecureSkipVerify: true})
		if err := probe(c, srv.URL); err != nil {
			t.Fatalf("InsecureSkipVerify did not take effect: %v", err)
		}
	})
}

// TestCABundleStillAppliesWithoutSkipping guards the ordering in tlsConfigFor:
// the CA pool must not be dropped when InsecureSkipVerify is absent, which is
// the regression an early-return in the wrong place would cause.
func TestCABundleStillAppliesWithoutSkipping(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0)
	defer srv.Close()

	pem := certPEM(t, srv.Certificate().Raw)
	c := mustClient(t, Config{Registry: "127.0.0.1", CABundle: pem})

	if err := probe(c, srv.URL); err != nil {
		t.Fatalf("a trusted CA bundle should have been enough: %v", err)
	}
	cfg := tlsConfigOf(t, c)
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is on when configuration did not ask for it")
	}
	if cfg.RootCAs == nil {
		t.Error("the CA bundle was not installed")
	}
}

func TestMinimumTLSVersionIsAlwaysSet(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		c := mustClient(t, Config{Registry: "r.example.com", InsecureSkipVerify: insecure})
		if got := tlsConfigOf(t, c).MinVersion; got != tls.VersionTLS12 {
			t.Errorf("InsecureSkipVerify=%v: MinVersion = %x, want TLS 1.2", insecure, got)
		}
	}
}

// ---- helpers ----

func mustClient(t *testing.T, cfg Config) *http.Client {
	t.Helper()
	cfg.ForceHTTP1 = true
	// One attempt: these tests assert on the FIRST failure, and a retry policy
	// would only make a deterministic rejection slower.
	cfg.MaxRetryAttempts = 1
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("build transport: %v", err)
	}
	return c
}

func probe(c *http.Client, url string) error {
	resp, err := c.Get(url) //nolint:noctx // a loopback test server
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}

// tlsConfigOf digs the tls.Config out of the layered round trippers, which is
// the only way to assert on settings that never appear on the wire.
func tlsConfigOf(t *testing.T, c *http.Client) *tls.Config {
	t.Helper()
	rt := c.Transport
	for range 8 {
		switch v := rt.(type) {
		case *http.Transport:
			return v.TLSClientConfig
		case *traceTransport:
			rt = v.next
		case *userAgentTransport:
			rt = v.next
		case *rateLimitTransport:
			rt = v.next
		case *retryTransport:
			rt = v.next
		case *authTransport:
			rt = v.next
		default:
			t.Fatalf("unexpected round tripper %T in the chain", rt)
		}
	}
	t.Fatal("did not reach the base transport")
	return nil
}

func certPEM(t *testing.T, der []byte) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
