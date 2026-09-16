package api

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A SLOW READER MUST NOT HOLD UP THE TICK.
//
// The hub publishes under a lock that every subscriber shares, so a send that
// blocks would stop the change feed for everybody else on this Coordinator -
// one browser on a stalled connection freezing the progress of every other
// reader. Events are hints with a listing poll underneath them, so a full
// subscriber is skipped rather than waited for.
func TestAFullSubscriberIsSkippedRatherThanWaitedFor(t *testing.T) {
	h := newEventHub(nil)
	_, cancel := h.subscribe() // never read from
	defer cancel()

	done := make(chan struct{})
	go func() {
		// Far more than the buffer holds. If publish blocked, this never ends.
		for i := range 10_000 {
			h.publish("transfer-" + string(rune('a'+i%26)))
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber that is not reading - one " +
			"stalled browser stops the change feed for every other reader")
	}
}

// Every subscriber hears about a transfer, and unsubscribing is complete.
func TestEverySubscriberHearsAndUnsubscribingStops(t *testing.T) {
	h := newEventHub(nil)
	a, cancelA := h.subscribe()
	b, cancelB := h.subscribe()
	defer cancelB()

	h.publish("t1")
	for name, ch := range map[string]<-chan string{"a": a, "b": b} {
		select {
		case got := <-ch:
			if got != "t1" {
				t.Errorf("subscriber %s got %q, want t1", name, got)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %s heard nothing", name)
		}
	}

	cancelA()
	if n := h.subscribers(); n != 1 {
		t.Errorf("%d subscribers after one unsubscribed, want 1", n)
	}
	// The cancelled channel is closed, so a reader of it ends rather than
	// hanging on a hub that will never send again.
	if _, open := <-a; open {
		t.Error("the cancelled subscriber's channel is still open")
	}
}

// THE STREAM IS AN EVENT STREAM, and it says so before anything has happened.
//
// The browser's fetch resolves on the headers, so a stream that sent nothing
// until the first transfer moved would leave a page unable to tell "connected
// and quiet" from "still connecting". The headers also have to carry
// X-Accel-Buffering, without which nginx holds every event until the response
// ends - and this response never ends.
func TestTheEventStreamAnnouncesItselfImmediately(t *testing.T) {
	h := newAPIHarness(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want \"no\" - nginx buffers a proxied "+
			"response by default and this one never ends", got)
	}

	// The opening comment, without waiting for a transfer to move.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read the first line: %v", err)
	}
	if !strings.HasPrefix(line, ":") {
		t.Errorf("first line is %q, want a comment saying the stream is open", line)
	}
}

// A TRANSFER THAT MOVES REACHES A READER, and a settled one is reported too.
//
// Settling is the most interesting thing a transfer does - the row reaches its
// final state and its rollup becomes readable once and for all - and it is the
// one change that takes a transfer OUT of the set being watched. A feed that
// only reported rows it could still see would go quiet at exactly that moment
// and leave the page to notice on its next poll.
func TestTheFeedReportsBothMovementAndSettling(t *testing.T) {
	h := newAPIHarness(t)
	srv := h.api

	feed, cancel := srv.events.subscribe()
	defer cancel()

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	go srv.watchTransfers(ctx, 10*time.Millisecond)

	id := h.seedTransfer("11111111-aaaa-bbbb-cccc-dddddddddddd")

	if got := waitForEvent(t, feed); got != id {
		t.Fatalf("the feed reported %q for a new live transfer, want %s", got, id)
	}

	// Settling takes it out of the live set, which is the case the feed has to
	// report from its own memory rather than from the query.
	h.exec(`UPDATE transfers SET state = 'succeeded' WHERE id = ?`, id)
	if got := waitForEvent(t, feed); got != id {
		t.Errorf("the feed reported %q when a transfer settled, want %s", got, id)
	}
}

func waitForEvent(t *testing.T, feed <-chan string) string {
	t.Helper()
	// Generous: the tick is 10ms and a loaded machine is not a stopwatch.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case id := <-feed:
			return id
		case <-deadline:
			t.Fatal("the change feed reported nothing")
			return ""
		}
	}
}

// THE STREAM IS NOT COMPRESSED, and that is not an accident to rely on.
//
// middleware.Compress takes an ALLOWLIST of content types. text/event-stream
// is not on it, which is what keeps events arriving one at a time: a gzip
// writer in front of this response would hold each frame in its window until
// it had enough to be worth emitting, and the browser would see nothing for
// as long as the estate was quiet.
//
// Adding `text/plain` or a `text/` prefix to that list would break the feed
// silently and in a way that only shows up as "the page stopped being live".
// This is here so it shows up as a failing test instead.
func TestTheEventStreamIsNotCompressed(t *testing.T) {
	h := newAPIHarness(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Explicitly, because Go's transport sets this itself and then strips the
	// header it decoded - which would hide the very thing under test.
	req.Header.Set("Accept-Encoding", "gzip")

	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Errorf("the event stream came back with Content-Encoding %q - a "+
			"compressor in front of it holds each event until its window "+
			"fills, so the page stops being live", enc)
	}

	// And it still says hello without waiting for anything to happen.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read the first line: %v", err)
	}
	if !strings.HasPrefix(line, ":") {
		t.Errorf("first line is %q, want the opening comment", line)
	}
}
