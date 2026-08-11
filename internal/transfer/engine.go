package transfer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"

	"github.com/abhijeet-oxide/softwareGateway/internal/platform/backoff"
	"github.com/abhijeet-oxide/softwareGateway/internal/registry"
)

// The engine runs on the WORKER. It is the only place bytes move.
//
// See docs/design/05-transfer-engine.md §4. Everything here is written against
// registry.Repository, never against an OCI library — which is what kept
// ADR-001 a leaf decision and now keeps its closure reversible.
//
// Invariant I5: BYTES NEVER TOUCH WORKER DISK. There is no io.ReadAll, no temp
// file, no decompression and no manifest rewriting anywhere in this file, and
// each of those absences is load-bearing rather than incidental:
//
//   - io.ReadAll on an 8 GB layer would OOM the pod.
//   - A temp file would cap concurrency at the volume's IOPS, add a failure
//     mode (disk full), and require cleanup on crash — the one thing workers
//     are designed not to need. It would also break readOnlyRootFilesystem.
//   - Decompressing to recompress would burn CPU and CHANGE THE DIGEST, which
//     would break every signature over it.
//
// The memory ceiling is therefore independent of blob size: a worker moving
// eight 8 GB layers uses the same memory as one moving eight 8 MB layers.

// Outcome is what happened to a job. It matches the API's completion
// vocabulary and the jobs.state CHECK constraint.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeFailed    Outcome = "failed"
)

// Skip reasons, matching the jobs.skip_reason CHECK constraint. The
// distinction is kept rather than collapsed into "skipped" because it is the
// only way a dedupe regression is visible: a placement-key bug shows up as
// "slightly slower", never as a failure.
const (
	SkipPlacementHit   = "placement_hit"
	SkipExistsAtTarget = "exists_at_target"
	SkipMounted        = "mounted"
)

// Job is one unit of work as the worker sees it.
//
// Self-contained by design: two repository handles, a digest and a size. That
// is what makes workers trivially stateless and what lets any worker pick up
// any job without coordination.
type Job struct {
	ID         int64
	TransferID string
	// Kind is "blob" or "manifest".
	Kind      string
	Digest    string
	SizeBytes int64
	MediaType string
	Attempt   int

	// Source and Target are already-built clients. The worker resolves
	// credentials; the engine never sees one.
	Source registry.Repository
	Target registry.Repository

	// KnownPlacement says the Coordinator believes this digest is already in
	// the target repository — the placement fast path, shipped with the lease
	// batch so resolving sixteen jobs costs zero extra calls.
	KnownPlacement bool

	// Tags are what this manifest must be called at the destination.
	//
	// Plural because a bundle's components each carry their own name, and one
	// artifact may legitimately answer to several. Empty is the common case: a
	// blob has no name, and a platform manifest under an index needs none.
	//
	// Applied only after the manifest is committed, which is what makes
	// invariant I1 hold: a tag never appears at the destination until
	// everything under it is present.
	Tags []string
}

// Result is what the worker reports back.
type Result struct {
	Outcome    Outcome
	SkipReason string
	// BytesMoved is what actually crossed the wire. Zero on every fast path,
	// which is the number that proves deduplication is working.
	BytesMoved int64
	// Placed reports that the destination now holds this content, however it
	// got there.
	Placed     bool
	Duration   time.Duration
	ErrorClass string
	Err        error
}

// ProgressFunc is called as bytes pass. Throttling is the caller's business:
// the engine reports every buffer, the worker decides what to send onward.
type ProgressFunc func(bytesTransferred int64)

// Engine executes leased jobs.
type Engine struct {
	log *slog.Logger
	// copyBuffer is the streaming buffer size. 1 MiB by default: larger stops
	// helping once the bottleneck is the network, smaller increases syscall
	// overhead on fast links.
	copyBuffer int
}

// DefaultCopyBuffer is the streaming buffer size from docs/design/05 §4.5.
const DefaultCopyBuffer = 1 << 20

// NewEngine builds an engine.
func NewEngine(copyBuffer int, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	if copyBuffer <= 0 {
		copyBuffer = DefaultCopyBuffer
	}
	return &Engine{log: log, copyBuffer: copyBuffer}
}

// Run executes one job and reports what happened.
//
// It returns a Result rather than an error for anything the queue should act
// on. An error return is reserved for a job that could not be attempted at all.
func (e *Engine) Run(ctx context.Context, job Job, progress ProgressFunc) Result {
	start := time.Now()

	var res Result
	switch job.Kind {
	case "blob":
		res = e.runBlob(ctx, job, progress)
	case "manifest":
		res = e.runManifest(ctx, job)
	default:
		res = Result{
			Outcome:    OutcomeFailed,
			ErrorClass: "invalid",
			Err:        fmt.Errorf("unknown job kind %q", job.Kind),
		}
	}

	res.Duration = time.Since(start)
	return res
}

// runBlob takes the first of four fast paths that applies, cheapest first.
//
//  1. placement hit (database, shipped with the lease)  0 bytes, 0 RPC
//  2. HEAD blob at destination                          0 bytes, 1 RPC
//  3. cross-repository MOUNT                            0 bytes, 1 RPC
//  4. stream source -> destination                      N bytes
//
// The best transfer is the one that does not happen: deduplication and
// mounting routinely eliminate 30–70% of a package, which is where the largest
// wins live.
func (e *Engine) runBlob(ctx context.Context, job Job, progress ProgressFunc) Result {
	dgst := registry.Digest(job.Digest)

	// 1. The Coordinator already believes this is there. A placement is strong
	// evidence, not proof — a registry's garbage collector can remove content
	// underneath us — but the TTL bounds the staleness and the BLOB_UNKNOWN
	// backstop on manifest push catches the rest.
	if job.KnownPlacement {
		return Result{Outcome: OutcomeSkipped, SkipReason: SkipPlacementHit, Placed: true}
	}

	// 2. Ask the destination. One HEAD, and it settles the question for real.
	if _, err := job.Target.StatBlob(ctx, dgst); err == nil {
		return Result{Outcome: OutcomeSkipped, SkipReason: SkipExistsAtTarget, Placed: true}
	} else if !errors.Is(err, registry.ErrNotFound) {
		// A destination that cannot answer HEAD is a destination that will not
		// accept an upload either. Fail rather than streaming gigabytes into
		// what is probably an auth or connectivity problem.
		return e.failed(err, "stat blob at destination")
	}

	// 3. Same registry? Ask it to relocate the blob server-side. Zero bytes
	// over the wire regardless of size — the dominant optimization for
	// promotion, where a 45 GB move can complete in seconds.
	if mounted := e.tryMount(ctx, job, dgst); mounted != nil {
		return *mounted
	}

	// 4. Stream.
	moved, err := e.stream(ctx, job, dgst, progress)
	if err != nil {
		res := e.failed(err, "stream blob")
		res.BytesMoved = moved
		return res
	}
	return Result{Outcome: OutcomeSucceeded, BytesMoved: moved, Placed: true}
}

// tryMount attempts a cross-repository mount, returning nil to fall through.
//
// Mount only applies within one registry: it is a server-side relocation, not
// a transfer. A 202 Accepted means the registry declined and opened an
// ordinary upload session instead — a NORMAL outcome, not an error, and the
// reason support being uneven in practice costs nothing here.
func (e *Engine) tryMount(ctx context.Context, job Job, dgst registry.Digest) *Result {
	if !sameRegistry(job.Source, job.Target) {
		return nil
	}
	if !job.Target.Capabilities(ctx).SupportsMount {
		return nil
	}

	switch err := job.Target.MountBlob(ctx, dgst, job.Source.Path()); {
	case err == nil:
		return &Result{Outcome: OutcomeSkipped, SkipReason: SkipMounted, Placed: true}
	case errors.Is(err, registry.ErrMountUnsupported),
		errors.Is(err, registry.ErrUnsupported),
		errors.Is(err, registry.ErrNotFound):
		// Declined with a 202 and an ordinary upload session, unsupported, or
		// the source repository does not hold it after all. Streaming is
		// always correct, so fall through rather than fail.
		return nil
	default:
		e.log.DebugContext(ctx, "mount failed; falling back to streaming",
			"job", job.ID, "digest", dgst.Short(), "error", err)
		return nil
	}
}

// stream is the core loop, and it is deliberately one io.CopyBuffer.
//
// BACKPRESSURE IS FREE. Copying from an HTTP response body into an HTTP
// request body means a slow destination naturally slows reads from the source,
// via TCP flow control on both sides. There is no queue between them to grow
// and no buffer to size. That is why this is one Copy rather than a
// producer/consumer pair with a channel — the simpler structure has the better
// behaviour, not merely less code.
func (e *Engine) stream(
	ctx context.Context, job Job, dgst registry.Digest, progress ProgressFunc,
) (int64, error) {
	rc, err := job.Source.FetchBlob(ctx, dgst)
	if err != nil {
		return 0, fmt.Errorf("fetch blob from source: %w", err)
	}
	defer func() { _ = rc.Close() }()

	// Verify what we READ, as we read it. This catches a source registry
	// serving wrong or corrupted bytes; the registry's own check on PUT
	// catches corruption anywhere in our path. Two independent checks that
	// catch different things, together giving end-to-end integrity without
	// trusting either registry or the network.
	verifier, err := newVerifier(dgst, rc)
	if err != nil {
		return 0, err
	}

	// The one buffer in the path, and the one the memory formula in
	// docs/design/05 §4.5 counts: peak is maxConcurrentJobs x (copyBufferSize +
	// transport buffers). Without it the reader is driven by whatever chunk
	// size the destination's writer happens to ask for.
	counter := &countingReader{r: bufio.NewReaderSize(verifier, e.copyBuffer), report: progress}

	if err := job.Target.PushBlob(ctx, dgst, job.SizeBytes, counter); err != nil {
		return counter.n, fmt.Errorf("push blob to destination: %w", err)
	}

	// Checked AFTER the push, because the push is what consumed the reader.
	// A mismatch here means the source lied; the destination will have
	// rejected the bytes on its own account too.
	if !verifier.verified() {
		return counter.n, fmt.Errorf("%w: source served content that is not %s",
			registry.ErrDigestMismatch, dgst.Short())
	}
	return counter.n, nil
}

// runManifest pushes stored manifest bytes, gated by waves so everything it
// references already exists.
//
// # Divergence from docs/design/05 §9 step 2, recorded rather than absorbed
//
// The design says to push "the stored raw bytes (03 §5), verbatim" — the
// Coordinator's copy. This fetches them from the SOURCE by digest instead.
//
// Verbatim is preserved either way: ManifestReader.FetchManifest is
// contractually forbidden from re-serializing, the fetch is BY DIGEST rather
// than by tag so a vendor re-pushing the tag mid-transfer cannot change what
// arrives, and the bytes are digest-addressed so anything else would not match.
//
// What it buys is that the lease response stays small. Shipping manifest
// bodies to the worker would put artifact payloads in every lease response for
// the handful of manifests in a package, when the worker already holds a
// source client and a manifest is kilobytes. What it costs is one GET per
// manifest — a handful per package, against hundreds of blobs.
func (e *Engine) runManifest(ctx context.Context, job Job) Result {
	dgst := registry.Digest(job.Digest)

	// Already there. Re-pushing an identical manifest to the same digest is a
	// no-op at the registry, so this is an optimization rather than a
	// correctness requirement — but it is the one that makes re-transferring
	// an already-present package move ZERO bytes rather than merely few. The
	// tags still have to be applied: the manifest being present says nothing
	// about what any tag points at.
	if _, _, err := job.Target.FetchManifest(ctx, job.Digest); err == nil {
		if err := e.applyTags(ctx, job, dgst); err != nil {
			return e.failed(err, "tag destination")
		}
		return Result{Outcome: OutcomeSkipped, SkipReason: SkipExistsAtTarget}
	}

	raw, res, done := e.manifestBytes(ctx, job, dgst)
	if done {
		return res
	}

	pushed, err := job.Target.PushManifest(ctx, job.Digest, job.MediaType, raw)
	if err != nil {
		// BLOB_UNKNOWN is the self-healing signal, not a plain failure: the
		// destination is telling us a placement record was wrong. The
		// Coordinator invalidates those placements and requeues the blobs
		// (docs/design/11 §2.5); the class is what routes it there.
		if errors.Is(err, registry.ErrNotFound) {
			return Result{
				Outcome:    OutcomeFailed,
				ErrorClass: ClassBlobUnknown,
				Err:        fmt.Errorf("push manifest: %w", err),
			}
		}
		return e.failed(err, "push manifest")
	}

	if pushed.Digest != "" && pushed.Digest != dgst {
		return Result{
			Outcome:    OutcomeFailed,
			ErrorClass: string(registry.ClassIntegrity),
			Err: fmt.Errorf("%w: destination stored the manifest as %s, not %s",
				registry.ErrDigestMismatch, pushed.Digest.Short(), dgst.Short()),
		}
	}

	// TAGGING HAPPENS LAST, and only here. Until this moment the destination
	// holds a set of unreferenced blobs and manifests, which are harmless,
	// invisible to consumers, and useful to the next transfer. This is what
	// makes an interrupted transfer safe: a consumer sees either the old tag
	// or the complete new one, never a half-written one.
	if err := e.applyTags(ctx, job, dgst); err != nil {
		return e.failed(err, "tag destination")
	}

	return Result{Outcome: OutcomeSucceeded, BytesMoved: int64(len(raw)), Placed: false}
}

// applyTags names a committed manifest at the destination.
//
// Every tag, not the first: an artifact the source published under two names
// must answer to both, or a consumer following the vendor's documentation
// finds half of it missing.
//
// A failure here fails the JOB rather than being swallowed, even though the
// content is already safely pushed. An untagged manifest is invisible to
// anyone who does not know its digest, which for a released component is the
// same as not having copied it.
func (e *Engine) applyTags(ctx context.Context, job Job, dgst registry.Digest) error {
	for _, tag := range job.Tags {
		if tag == "" {
			continue
		}
		if err := job.Target.Tag(ctx, dgst, tag); err != nil {
			return fmt.Errorf("apply tag %q: %w", tag, err)
		}
	}
	return nil
}

// manifestBytes fetches the manifest to push and checks it hashes as claimed.
func (e *Engine) manifestBytes(
	ctx context.Context, job Job, dgst registry.Digest,
) (raw []byte, res Result, done bool) {
	_, raw, err := job.Source.FetchManifest(ctx, job.Digest)
	if err != nil {
		return nil, e.failed(err, "fetch manifest from source"), true
	}

	// The bytes must hash to the digest we were asked for. A source that
	// serves something else has to be caught here: a manifest is small enough
	// that nothing downstream would notice, and every signature is over this
	// digest.
	if actual := digest.FromBytes(raw).String(); actual != dgst.String() {
		return nil, Result{
			Outcome:    OutcomeFailed,
			ErrorClass: string(registry.ClassIntegrity),
			Err: fmt.Errorf("%w: source served %s for manifest %s",
				registry.ErrDigestMismatch, actual, dgst.Short()),
		}, true
	}
	return raw, Result{}, false
}

// failed classifies an error into a Result.
func (e *Engine) failed(err error, what string) Result {
	return Result{
		Outcome:    OutcomeFailed,
		ErrorClass: classify(err),
		Err:        fmt.Errorf("%s: %w", what, err),
	}
}

// ClassBlobUnknown is the one class registry.ClassOf cannot express.
//
// At the registry it is a 404, which ClassOf correctly reports as not_found.
// On a MANIFEST PUSH it means something else entirely: the manifest is fine
// and a blob it references is missing, which is the destination telling us a
// placement record was wrong. That is a self-healing signal rather than a
// missing resource, so it needs its own name to be routed differently.
const ClassBlobUnknown = "blob_unknown"

// classify maps an error onto the class the retry policy keys off.
//
// A thin wrapper over registry.ClassOf rather than a second classifier: the
// registry package already owns this vocabulary, and two classifiers would
// eventually disagree about the same error.
func classify(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return string(registry.ClassOf(err))
}

// MaxAttemptsFor is how many times a job with this error class is worth
// retrying, from the caps in docs/design/11 §2.3.
//
// The caps differ by kind of failure for reasons worth restating: a transient
// 5xx deserves the full eight attempts because registries recover; a digest
// mismatch deserves two because retrying rarely helps and may indicate real
// corruption worth surfacing rather than absorbing; an auth failure deserves
// one because credentials do not fix themselves and hammering an auth endpoint
// gets us rate-limited.
func MaxAttemptsFor(class string) int {
	switch registry.Class(class) {
	case registry.ClassAuth, registry.ClassNotFound, registry.ClassUnsupported:
		return int(backoff.AttemptsTerminal)
	case registry.ClassIntegrity:
		return int(backoff.AttemptsIntegrity)
	default:
		return int(backoff.AttemptsTransient)
	}
}

// sameRegistry reports whether a mount could possibly apply.
func sameRegistry(a, b registry.Identity) bool {
	return strings.EqualFold(a.Registry(), b.Registry())
}

// ---------------------------------------------------------------------------
// Streaming helpers
// ---------------------------------------------------------------------------

// verifier hashes bytes as they are read.
//
// One SHA-256 pass over data already in cache — on modern hardware with SHA
// extensions, low single-digit percent of the transfer's CPU, and entirely
// overlapped with network I/O. Free, in the only sense that matters.
type verifier struct {
	want digest.Digest
	v    digest.Verifier
	r    io.Reader
}

func newVerifier(want registry.Digest, r io.Reader) (*verifier, error) {
	d, err := digest.Parse(want.String())
	if err != nil {
		return nil, fmt.Errorf("parse digest %s: %w", want, err)
	}
	v := d.Verifier()
	return &verifier{want: d, v: v, r: io.TeeReader(r, v)}, nil
}

func (v *verifier) Read(p []byte) (int, error) { return v.r.Read(p) }
func (v *verifier) verified() bool             { return v.v.Verified() }

// countingReader reports progress as bytes pass, without a second copy.
type countingReader struct {
	r      io.Reader
	n      int64
	report ProgressFunc
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n += int64(n)
		if c.report != nil {
			c.report(c.n)
		}
	}
	return n, err
}
