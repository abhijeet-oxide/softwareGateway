package log

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Shipping logs to a log store, alongside stdout rather than instead of it.
//
// # Why the application ships its own logs
//
// The usual answer is a collector reading the container runtime's log files,
// and in a cluster that remains the record - see the comment on worker_logs in
// the schema. It is a poor fit for the OTHER two runtimes this repository has
// to work in. `task run` has no container runtime at all, and compose would
// need a host path that differs between Docker and Podman, mounted into a
// sidecar, to read files this process already has in memory.
//
// Writing them from here is the same in all three, which is the property the
// rest of the repository is organised around.
//
// # Why it is an io.Writer and not a slog.Handler
//
// Because slog's JSONHandler already emits exactly what VictoriaLogs' JSON
// stream API ingests: one JSON object per line. Wrapping the WRITER rather
// than the handler means the shipped line and the line on stdout are the same
// bytes by construction, with no second serialisation to drift - and no
// mapping to maintain, since `_time_field=time&_msg_field=msg` names slog's
// own keys.
//
// # What it must never do
//
// This sits on the path of every log line in the process, including the ones
// written while serving a request. So it never blocks the caller, never
// allocates without bound, and never fails a write: a full buffer DROPS, and
// says how many it dropped. A log store being down is not a reason for the
// service to slow down or stop, and stdout has every line either way.
type Shipper struct {
	url    string
	client *http.Client

	mu     sync.Mutex
	buf    [][]byte
	bufLen int

	maxLines int
	maxBytes int

	dropped atomic.Int64
	failed  atomic.Int64
	sent    atomic.Int64

	flush  chan struct{}
	done   chan struct{}
	closed chan struct{}
}

// ShipConfig selects where logs go and how much may be held.
type ShipConfig struct {
	// Endpoint is the base URL of a VictoriaLogs instance, e.g.
	// http://victorialogs:9428. Empty disables shipping entirely.
	Endpoint string
	// Component labels the log stream. One stream per component keeps the
	// per-stream cardinality at three rather than one per pod.
	Component string
	// Interval is how often a partial batch is sent. Zero means the default.
	Interval time.Duration
	// MaxLines and MaxBytes bound what may be held while the store is
	// unreachable. Zero means the defaults.
	MaxLines int
	MaxBytes int
	// Timeout bounds one delivery. Zero means the default.
	//
	// Deliberately short: a log store that has stopped answering should be
	// given up on quickly, because every second spent waiting is a second the
	// buffer fills without draining, and the buffer filling is what turns a
	// slow log store into dropped lines.
	Timeout time.Duration
}

// Defaults chosen for a service logging a few hundred lines a second.
//
// One second of latency is nothing for a log a person greps later, and the
// bound is what one second of a very loud incident looks like - past that,
// dropping is the correct behaviour and the counter says so.
const (
	defaultShipInterval = time.Second
	defaultShipMaxLines = 10_000
	defaultShipMaxBytes = 8 << 20
	defaultShipTimeout  = 5 * time.Second
)

// NewShipper starts a shipper. A nil return means shipping is disabled, which
// is a valid io.Writer target through Writer below.
func NewShipper(cfg ShipConfig) (*Shipper, error) {
	if cfg.Endpoint == "" {
		return nil, nil
	}
	base, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse log endpoint %q: %w", cfg.Endpoint, err)
	}
	base.Path = "/insert/jsonline"
	// slog's own field names. Declared here rather than by reformatting the
	// log line, so what is stored is what stdout shows.
	q := base.Query()
	q.Set("_time_field", "time")
	q.Set("_msg_field", "msg")
	q.Set("_stream_fields", KeyComponent)
	base.RawQuery = q.Encode()

	s := &Shipper{
		url:      base.String(),
		client:   &http.Client{Timeout: orDefaultDuration(cfg.Timeout, defaultShipTimeout)},
		maxLines: orDefaultInt(cfg.MaxLines, defaultShipMaxLines),
		maxBytes: orDefaultInt(cfg.MaxBytes, defaultShipMaxBytes),
		flush:    make(chan struct{}, 1),
		done:     make(chan struct{}),
		closed:   make(chan struct{}),
	}
	go s.run(orDefaultDuration(cfg.Interval, defaultShipInterval))
	return s, nil
}

// Write queues one log line. It never fails and never blocks.
//
// The returned count is always len(p): a caller writing to a log is not in a
// position to do anything useful about a log store, and an error here would
// propagate into slog, which discards it anyway.
func (s *Shipper) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	// COPIED. slog reuses its buffer between records, so holding p would
	// queue a line that has become a later one by the time it is sent.
	line := bytes.Clone(p)

	s.mu.Lock()
	if len(s.buf) >= s.maxLines || s.bufLen+len(line) > s.maxBytes {
		s.mu.Unlock()
		s.dropped.Add(1)
		return len(p), nil
	}
	s.buf = append(s.buf, line)
	s.bufLen += len(line)
	full := len(s.buf) >= s.maxLines/2
	s.mu.Unlock()

	if full {
		select {
		case s.flush <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

// Stats reports what has happened, for the metric that makes silent loss
// visible. A shipper that is dropping is worse than one that is off, because
// the logs look complete.
func (s *Shipper) Stats() (sent, dropped, failed int64) {
	if s == nil {
		return 0, 0, 0
	}
	return s.sent.Load(), s.dropped.Load(), s.failed.Load()
}

// Close flushes what is buffered and stops the shipper.
func (s *Shipper) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
		return nil // already closed
	default:
	}
	close(s.done)
	select {
	case <-s.closed:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (s *Shipper) run(interval time.Duration) {
	defer close(s.closed)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			s.send(s.take())
			return
		case <-t.C:
			s.send(s.take())
		case <-s.flush:
			s.send(s.take())
		}
	}
}

// take removes everything buffered, so the lock is held for a slice swap
// rather than for the duration of an HTTP request.
func (s *Shipper) take() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil
	}
	out := s.buf
	s.buf, s.bufLen = nil, 0
	return out
}

func (s *Shipper) send(lines [][]byte) {
	if len(lines) == 0 {
		return
	}
	var body bytes.Buffer
	for _, l := range lines {
		body.Write(l)
		if len(l) == 0 || l[len(l)-1] != '\n' {
			body.WriteByte('\n')
		}
	}

	req, err := http.NewRequest(http.MethodPost, s.url, &body)
	if err != nil {
		s.failed.Add(int64(len(lines)))
		return
	}
	req.Header.Set("Content-Type", "application/stream+json")

	resp, err := s.client.Do(req)
	if err != nil {
		// DROPPED, not retried. A retry queue that outlives the incident it
		// was built for is how a log shipper becomes the thing that runs the
		// host out of memory. stdout still has every line.
		s.failed.Add(int64(len(lines)))
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 300 {
		s.failed.Add(int64(len(lines)))
		return
	}
	s.sent.Add(int64(len(lines)))
}

// Writer returns the destination for the logger: stdout, plus the shipper when
// one is configured.
//
// STDOUT ALWAYS. Shipping is an addition, never a replacement - a container
// whose logs only exist in a log store is a container nobody can debug when
// the log store is the thing that is broken.
func Writer(stdout io.Writer, s *Shipper) io.Writer {
	if s == nil {
		return stdout
	}
	return io.MultiWriter(stdout, s)
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orDefaultDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}
