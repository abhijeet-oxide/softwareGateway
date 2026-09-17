package log

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestShippedLinesAreTheLinesOnStdout is the property the io.Writer wrapping
// buys, and the reason it is not a second slog.Handler.
//
// A handler would serialise the record twice, and the two could disagree - one
// carrying a field the other dropped, or a different timestamp - which is the
// worst possible failure for a log store, because the discrepancy is invisible
// until somebody compares them during an incident.
func TestShippedLinesAreTheLinesOnStdout(t *testing.T) {
	got := newCollector(t)

	shipper := startShipper(t, got.server.URL)
	var stdout strings.Builder
	logger := New(Config{Format: "json"}, Writer(&stdout, shipper), "coordinator")

	logger.Info("http request", slog.String("path", "/api/v1/transfers"),
		slog.Int64("queries", 3), slog.Float64("dbSeconds", 0.0125))

	if err := shipper.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	shipped := got.lines()
	if len(shipped) != 1 {
		t.Fatalf("shipped %d lines, want 1: %q", len(shipped), shipped)
	}
	if shipped[0] != strings.TrimSpace(stdout.String()) {
		t.Errorf("the shipped line differs from the one on stdout.\n  stdout:  %s\n  shipped: %s",
			strings.TrimSpace(stdout.String()), shipped[0])
	}

	// And it is the shape VictoriaLogs indexes: slog's own keys, carrying the
	// correlation fields the dashboards and traces join on.
	var rec map[string]any
	if err := json.Unmarshal([]byte(shipped[0]), &rec); err != nil {
		t.Fatalf("the shipped line is not JSON: %v", err)
	}
	for _, key := range []string{"time", "msg", KeyComponent, "queries", "dbSeconds"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("the shipped line has no %q field: %v", key, rec)
		}
	}
}

// TestTheIngestUrlNamesSlogsFields keeps the query parameters and the log
// format together.
//
// VictoriaLogs has to be told which field is the timestamp and which is the
// message. Its defaults are `_time` and `_msg`; slog writes `time` and `msg`.
// Get this wrong and ingestion still succeeds - every line stored with the
// server's arrival time and an empty message, which looks like working until
// somebody searches for a message.
func TestTheIngestUrlNamesSlogsFields(t *testing.T) {
	got := newCollector(t)
	shipper := startShipper(t, got.server.URL)

	logger := New(Config{Format: "json"}, Writer(io.Discard, shipper), "worker")
	logger.Info("hello")
	if err := shipper.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(got.path())
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/insert/jsonline" {
		t.Errorf("posted to %q, want /insert/jsonline", u.Path)
	}
	for key, want := range map[string]string{
		"_time_field":    "time",
		"_msg_field":     "msg",
		"_stream_fields": KeyComponent,
	} {
		if got := u.Query().Get(key); got != want {
			t.Errorf("%s=%q, want %q - slog's field names and this URL have to "+
				"agree or every line stores with an empty message", key, got, want)
		}
	}
	if ct := got.contentType(); ct != "application/stream+json" {
		t.Errorf("Content-Type %q, want application/stream+json", ct)
	}
}

// TestLoggingDoesNotBlockOnAnUnreachableStore is the one that matters most.
//
// This sits on the path of every log line, including the ones written while
// serving a request. A log store that is down, slow, or a black hole must cost
// the service nothing - otherwise the observability stack becomes the outage,
// which is the most embarrassing way to have one.
func TestLoggingDoesNotBlockOnAnUnreachableStore(t *testing.T) {
	// A server that accepts the connection and then never answers.
	//
	// CLEANUP ORDER MATTERS HERE, and getting it wrong deadlocks the test
	// rather than failing it: httptest.Server.Close waits for outstanding
	// requests, and t.Cleanup runs last-registered-first. Close is registered
	// FIRST so it runs LAST - after the unblocking below has let the handler
	// return.
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-blocked
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(blocked) })

	shipper, err := NewShipper(ShipConfig{
		Endpoint: srv.URL, Component: "coordinator",
		Interval: 20 * time.Millisecond, Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shipper.Close(context.Background()) })
	logger := New(Config{Format: "json"}, Writer(io.Discard, shipper), "coordinator")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 50_000 {
			logger.Info("http request", slog.Int("i", i))
		}
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("writing logs blocked on a log store that never answers.\n" +
			"The buffer has to drop rather than wait: an unreachable log store " +
			"must not become a slow service.")
	}

	_, dropped, _ := shipper.Stats()
	if dropped == 0 {
		t.Error("50,000 lines against a store that never answers dropped none of " +
			"them, so the buffer is unbounded - which is how a log shipper runs " +
			"the host out of memory during an incident")
	}
}

// TestAFullBufferDropsRatherThanGrows pins the bound itself.
func TestAFullBufferDropsRatherThanGrows(t *testing.T) {
	// No server at all, and an interval long enough that nothing drains.
	s, err := NewShipper(ShipConfig{
		Endpoint: "http://127.0.0.1:1", Component: "coordinator",
		Interval: time.Hour, MaxLines: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	for range 100 {
		if _, err := s.Write([]byte(`{"msg":"x"}` + "\n")); err != nil {
			t.Fatalf("Write returned an error, which slog would discard anyway: %v", err)
		}
	}

	s.mu.Lock()
	held := len(s.buf)
	s.mu.Unlock()
	if held > 10 {
		t.Errorf("the buffer holds %d lines with a bound of 10", held)
	}
	if _, dropped, _ := s.Stats(); dropped != 90 {
		t.Errorf("dropped %d of 100 lines with a bound of 10, want 90", dropped)
	}
}

// TestShippingDisabledIsJustStdout covers the default configuration.
func TestShippingDisabledIsJustStdout(t *testing.T) {
	s, err := NewShipper(ShipConfig{Component: "coordinator"})
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Fatal("an empty endpoint should disable shipping")
	}

	var stdout strings.Builder
	logger := New(Config{Format: "json"}, Writer(&stdout, s), "coordinator")
	logger.Info("hello")
	if !strings.Contains(stdout.String(), `"msg":"hello"`) {
		t.Errorf("stdout lost the line when shipping was off: %q", stdout.String())
	}
	// A nil shipper must still be safe to close and read.
	if err := s.Close(t.Context()); err != nil {
		t.Errorf("closing a disabled shipper: %v", err)
	}
	if sent, dropped, failed := s.Stats(); sent|dropped|failed != 0 {
		t.Errorf("a disabled shipper reported %d/%d/%d", sent, dropped, failed)
	}
}

// TestCloseFlushesWhatIsBuffered stops a clean shutdown losing the last
// second of logs - which is the second that says why it shut down.
func TestCloseFlushesWhatIsBuffered(t *testing.T) {
	got := newCollector(t)
	s, err := NewShipper(ShipConfig{
		Endpoint: got.server.URL, Component: "coordinator", Interval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	logger := New(Config{Format: "json"}, Writer(io.Discard, s), "coordinator")
	logger.Error("shutting down", slog.String("reason", "signal"))

	if err := s.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if lines := got.lines(); len(lines) != 1 {
		t.Errorf("close flushed %d lines, want 1 - the last second of logs is "+
			"the one that says why the process is stopping", len(lines))
	}
}

func startShipper(t *testing.T, endpoint string) *Shipper {
	t.Helper()
	s, err := NewShipper(ShipConfig{
		Endpoint: endpoint, Component: "coordinator", Interval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// collector is a VictoriaLogs stand-in that records what it was sent.
type collector struct {
	server *httptest.Server

	mu   sync.Mutex
	recv []string
	url  string
	ct   string
}

func newCollector(t *testing.T) *collector {
	t.Helper()
	c := &collector{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.url = r.URL.String()
		c.ct = r.Header.Get("Content-Type")
		scanner := bufio.NewScanner(r.Body)
		for scanner.Scan() {
			if line := strings.TrimSpace(scanner.Text()); line != "" {
				c.recv = append(c.recv, line)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *collector) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.recv...)
}

func (c *collector) path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url
}

func (c *collector) contentType() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ct
}
