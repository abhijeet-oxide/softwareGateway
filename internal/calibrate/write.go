package calibrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry/transport"
)

// The target half.
//
// # Why this can push real bytes and still leave nothing behind
//
// Distribution v2 splits a blob upload into a session and a commit. `POST
// /v2/<name>/blobs/uploads/` opens a session, `PATCH` streams bytes into it,
// and the blob only becomes part of the repository when a `PUT` names its
// digest. This probe never sends that PUT. It PATCHes, measures, and then
// `DELETE`s the session, which the spec defines as cancelling the upload.
//
// So the bytes traverse exactly the path a transfer's bytes traverse - same
// proxy, same TLS, same registry front end, same storage backend accepting a
// chunk - and the repository ends the run byte-for-byte as it started. Nothing
// is committed, so nothing can be pulled, listed, or garbage-collected later.
//
// The alternative was to measure the write path by pushing a real blob and
// deleting it afterwards, which is worse in every way: it needs delete
// permission this tool otherwise never uses, it is visible in the registry
// between push and delete, and a probe that crashed mid-run would leave a
// committed artefact behind.

// Write-probe sizing.
const (
	// randomBufferBytes is the pool of incompressible bytes every stream sends
	// slices of. Random rather than zeroes: a proxy or registry that
	// transparently compresses would otherwise report a throughput no real
	// layer - already gzip or zstd - could reach.
	//
	// One buffer, shared read-only by every stream, so sixteen streams cost
	// eight megabytes rather than a hundred and twenty-eight.
	randomBufferBytes = 8 << 20

	// initialChunk starts small so that a slow link completes at least one
	// acknowledged chunk inside the budget. Bytes are counted only when the
	// registry has accepted them, so a chunk still in flight at the deadline
	// counts for nothing - on a 1 MB/s link a fixed eight-megabyte chunk would
	// measure zero.
	initialChunk = 1 << 20
	// maxChunk bounds the growth. Each chunk costs one round trip, so on a fast
	// high-latency path small chunks measure the latency instead of the
	// bandwidth; doubling up to eight megabytes amortises that away.
	maxChunk = randomBufferBytes
)

// randomBytes is built once per process. Generating eight megabytes of
// cryptographic randomness per level would put the CPU in the measurement.
var randomBytes = sync.OnceValue(func() []byte {
	buf := make([]byte, randomBufferBytes)
	if _, err := rand.Read(buf); err != nil {
		// Cannot happen on any supported platform, and a zero buffer is still
		// a usable - if compressible - probe. Not worth failing the run for.
		return buf
	}
	return buf
})

// checkUploadSupported opens a session and immediately cancels it.
//
// Run before the sweep so a target that refuses uploads is reported as one
// clear reason rather than as a sweep of zeroes. It is also the only write
// permission check this system has: preflight deliberately refuses to verify
// writability because it will not leave artefacts behind, and this proves it
// without leaving one either.
func checkUploadSupported(ctx context.Context, cfg registry.ClientConfig) error {
	client, err := transport.New(transportConfig(withConnections(cfg, 1)))
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	session, err := startUpload(probeCtx, client, cfg)
	if err != nil {
		return err
	}
	cancelUpload(ctx, client, session)
	return nil
}

// probeWrite measures aggregate write throughput at one concurrency.
func probeWrite(
	ctx context.Context, cfg registry.ClientConfig, streams int, budget time.Duration,
) LevelResult {
	res := LevelResult{Concurrency: streams}

	client, err := transport.New(transportConfig(cfg))
	if err != nil {
		res.Errors++
		res.FirstError = err.Error()
		return res
	}

	var (
		sent      atomic.Int64
		requests  atomic.Int64
		failures  atomic.Int64
		throttled atomic.Int64

		mu    sync.Mutex
		ttfbs []time.Duration
		first string
	)
	note := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if first == "" {
			first = err.Error()
		}
	}

	// Sessions are opened BEFORE the clock starts. Opening one is a proxy
	// CONNECT, a TLS handshake, a token exchange and a POST - four round trips
	// that are setup rather than throughput, and on a path with a one-second
	// round trip they consume the whole budget on their own and leave a level
	// reporting nothing.
	sessions := openSessions(ctx, client, cfg, streams, &failures, &throttled, note)
	defer func() {
		for _, s := range sessions {
			cancelUpload(ctx, client, s)
		}
	}()

	runCtx, cancel2 := context.WithTimeout(ctx, budget)
	defer cancel2()

	deadline, _ := runCtx.Deadline()
	started := time.Now()
	var wg sync.WaitGroup
	for _, session := range sessions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			chunk := initialChunk
			lastChunkTook := time.Duration(0)
			for runCtx.Err() == nil {
				// Do not start a chunk the deadline will cut off. Only
				// acknowledged bytes count, so an abandoned chunk is budget
				// spent for nothing - and at eight megabytes that is a
				// measurable bite out of the sample.
				if lastChunkTook > 0 && time.Until(deadline) < lastChunkTook {
					return
				}

				requestStarted := time.Now()
				err := session.patch(runCtx, client, randomBytes()[:chunk])
				if err != nil {
					if runCtx.Err() != nil {
						return
					}
					countFailure(err, &failures, &throttled)
					note(err)
					return
				}

				elapsed := time.Since(requestStarted)
				lastChunkTook = elapsed
				sent.Add(int64(chunk))
				requests.Add(1)

				mu.Lock()
				ttfbs = append(ttfbs, elapsed)
				mu.Unlock()

				// Grow while a chunk is cheap relative to the budget, so a fast
				// path stops paying a round trip per megabyte and a slow one
				// keeps its granularity. A sixteenth, so that even after
				// doubling a chunk stays a small enough slice of the run that
				// losing the last one to the deadline does not matter.
				if chunk < maxChunk && elapsed < budget/16 {
					chunk *= 2
				}
			}
		}()
	}
	wg.Wait()

	res.Duration = workingTime(started, budget)
	res.Bytes = sent.Load()
	res.Requests = int(requests.Load())
	res.Errors = int(failures.Load())
	res.Throttled = int(throttled.Load())
	res.TTFB = median(ttfbs)
	res.FirstError = describeEmptyLevel(first, res.Requests, res.Errors, budget)
	res.Rate = ratePerSecond(res.Bytes, res.Duration)
	if streams > 0 {
		res.PerStream = res.Rate / float64(streams)
	}
	return res
}

// openSessions opens one upload session per stream, outside the measured
// window, and returns those that opened.
//
// Every session is cancelled by the caller whether or not the measurement ran -
// an abandoned session is the one piece of litter this probe could leave.
func openSessions(
	ctx context.Context, client *http.Client, cfg registry.ClientConfig, streams int,
	failures, throttled *atomic.Int64, note func(error),
) []*upload {
	setupCtx, cancel := context.WithTimeout(ctx, warmupBudget)
	defer cancel()

	var (
		mu  sync.Mutex
		out []*upload
		wg  sync.WaitGroup
	)
	for range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session, err := startUpload(setupCtx, client, cfg)
			if err != nil {
				countFailure(err, failures, throttled)
				note(err)
				return
			}
			mu.Lock()
			out = append(out, session)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// upload is one open session.
type upload struct {
	// location is where the next chunk goes. Registries are permitted to move
	// it on EVERY response, and some do.
	location string
	offset   int64
	// base is the scheme and host, kept so a relative Location can be resolved
	// against it. The spec permits a relative one and real registries send
	// both; resolving only the first and then trusting the rest is a bug that
	// survives every test against a registry that happens to send absolute
	// URLs, and then loses the session - and the cleanup that cancels it -
	// against one that does not.
	base string
}

// at resolves a Location header, which may be relative.
func (u *upload) at(location string) string {
	if strings.HasPrefix(location, "http://") || strings.HasPrefix(location, "https://") {
		return location
	}
	if !strings.HasPrefix(location, "/") {
		location = "/" + location
	}
	return u.base + location
}

// startUpload opens a session with POST /v2/<name>/blobs/uploads/.
func startUpload(
	ctx context.Context, client *http.Client, cfg registry.ClientConfig,
) (*upload, error) {
	endpoint := registryURL(cfg, "/blobs/uploads/")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Length", "0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open upload session: %w", err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("open upload session: %s: %w",
			resp.Status, registry.ClassifyStatus(resp.StatusCode))
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return nil, fmt.Errorf("open upload session: the registry returned %s with no Location",
			resp.Status)
	}
	u := &upload{base: registryBase(cfg)}
	u.location = u.at(location)
	return u, nil
}

// patch streams one chunk into the session.
func (u *upload) patch(ctx context.Context, client *http.Client, chunk []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u.location,
		bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(chunk))
	// Inclusive, per the distribution spec. Some registries ignore it and
	// append; the ones that validate it reject a chunked upload without it.
	req.Header.Set("Content-Range",
		fmt.Sprintf("%d-%d", u.offset, u.offset+int64(len(chunk))-1))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload chunk at %d: %w", u.offset, err)
	}
	defer drain(resp)

	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("upload chunk at %d: %s: %w", u.offset, resp.Status,
			registry.ClassifyStatus(resp.StatusCode))
	}

	u.offset += int64(len(chunk))
	if next := resp.Header.Get("Location"); next != "" {
		u.location = u.at(next)
	}
	return nil
}

// cancelUpload discards the session, so nothing is ever committed.
//
// Deliberately takes a context WITHOUT the run's deadline: the whole reason
// the session must go is that the run has ended, and inheriting the expired
// deadline would make cleanup fail exactly when it is needed. A registry that
// does not implement DELETE simply keeps an incomplete session, which every
// implementation garbage-collects - but leaving that to chance when one request
// prevents it would be sloppy.
func cancelUpload(ctx context.Context, client *http.Client, u *upload) {
	if u == nil || u.location == "" {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cleanupCtx, http.MethodDelete, u.location, nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	drain(resp)
}

// registryBase is the scheme and host, without a path.
func registryBase(cfg registry.ClientConfig) string {
	if cfg.PlainHTTP {
		return "http://" + cfg.Registry
	}
	return "https://" + cfg.Registry
}

// registryURL builds a /v2/<name><suffix> URL, escaping the path per segment
// as the read client does - a repository path legitimately contains slashes,
// which must survive, and may contain characters that must not.
func registryURL(cfg registry.ClientConfig, suffix string) string {
	segments := strings.Split(strings.Trim(cfg.Repository, "/"), "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return registryBase(cfg) + "/v2/" + strings.Join(segments, "/") + suffix
}

// drain reads and closes a response body so the connection returns to the pool.
//
// It matters more here than usual: this probe is measuring connection reuse
// among other things, and a leaked body would make every request open a new TCP
// connection and quietly measure handshakes.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}
