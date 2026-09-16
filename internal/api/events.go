package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	v1 "github.com/abhijeet-oxide/softwareGateway/pkg/apis/softwaregateway/v1"
)

// WHAT CHANGED, pushed, instead of every browser asking whether anything did.
//
// # The problem this replaces
//
// The Downloads page polls its listing every five seconds while anything is
// running, and the listing is the most expensive read in the application. That
// ties the refresh rate to the cost of a refresh: twenty people watching one
// download cost twenty listings every five seconds, and making the page feel
// live means making it more expensive, not less.
//
// A stream inverts it. One query per Coordinator per second finds the
// transfers that moved and tells every browser connected to it; the browser
// re-reads the listing only when there is something to re-read. Idle costs
// nothing, and what it buys is a second of latency where there were five.
//
// # Why the stream carries no data
//
// An event is a transfer ID and nothing else - "this moved, look again". The
// alternative is pushing the rollups themselves, which means two paths that
// can disagree about what a transfer looks like, out-of-order delivery to
// reconcile, and authorization decided at publish time for a reader who may
// have lost the product since. A hint has none of those: the answer still
// comes from the ordinary listing, through the ordinary authorization, and the
// worst a lost or duplicated event can do is cost one extra read or leave the
// existing poll to notice.
//
// That is also why the polls are NOT removed from the web interface. They are
// the floor this rests on: if the stream never connects, or a proxy drops it,
// the page keeps working exactly as it does today.
//
// # Server-sent events, not a WebSocket
//
// Progress is one-directional, and the browser already has a channel for
// commands - the API, with its authorization, its audit trail and its errors.
// A socket would be a second one to re-implement all of that on.
//
// It also has to carry a bearer token: this deployment authenticates with an
// Authorization header, and neither EventSource nor a browser WebSocket can
// set one. `fetch` with a streaming body can, which is what web/src/api/events
// uses - so the stream goes through the same middleware chain as every other
// request and needs no second way in. See docs/design/32-performance.md §5.6.
type eventHub struct {
	mu     sync.Mutex
	next   uint64
	subs   map[uint64]chan string
	logger *slog.Logger
}

func newEventHub(logger *slog.Logger) *eventHub {
	if logger == nil {
		logger = slog.Default()
	}
	return &eventHub{subs: map[uint64]chan string{}, logger: logger}
}

// subscribe returns a channel of transfer IDs and the function that ends it.
func (h *eventHub) subscribe() (<-chan string, func()) {
	// BUFFERED, and dropped rather than blocked on when it fills - see publish.
	ch := make(chan string, 64)

	h.mu.Lock()
	id := h.next
	h.next++
	h.subs[id] = ch
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
}

// publish tells every subscriber that a transfer moved.
//
// A FULL SUBSCRIBER IS SKIPPED, never waited for. These are hints: a reader
// whose connection cannot keep up has a listing poll underneath it that will
// notice anyway, and blocking here would let one stalled browser hold up the
// tick for every other reader on this Coordinator.
func (h *eventHub) publish(transferID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- transferID:
		default:
		}
	}
}

func (h *eventHub) subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// watchTransfers is the change feed: one query per tick, whoever is listening.
//
// It runs only while somebody is subscribed. An estate nobody is looking at
// issues no queries at all, which is the difference between this and the polls
// it replaces - those ran because a tab was open, not because anybody was
// watching.
func (s *Server) watchTransfers(ctx context.Context, every time.Duration) {
	seen := map[string]string{}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if s.events.subscribers() == 0 {
			// Nothing to tell, so nothing is asked. `seen` is deliberately
			// kept: a reader who reconnects gets the transfers that moved
			// while they were away on the next tick, rather than a clean slate
			// that reports everything as new.
			continue
		}

		live, err := s.deps.Packages.LiveTransferVersions(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.deps.Logger.Warn("transfer change feed", "error", err)
			}
			continue
		}

		now := make(map[string]string, len(live))
		for _, t := range live {
			now[t.ID] = t.UpdatedAt
			if seen[t.ID] != t.UpdatedAt {
				s.events.publish(t.ID)
			}
		}
		// A TRANSFER THAT HAS LEFT THE LIVE SET has just settled, and that is
		// the single most interesting thing it will ever do - the row goes to
		// its final state and its rollup becomes readable once and for all.
		// Without this the page would learn about it from the next poll, which
		// is the five seconds this exists to remove.
		for id := range seen {
			if _, still := now[id]; !still {
				s.events.publish(id)
			}
		}
		seen = now
	}
}

// handleTransferEvents streams what changed.
//
// Text/event-stream, flushed per event. The response has no Content-Length and
// never ends on its own, so the two things that break it are a proxy that
// buffers (nginx needs `proxy_buffering off`, which is why the header below is
// sent) and a WriteTimeout on the server, which cmd/coordinator deliberately
// does not set.
func (s *Server) handleTransferEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		Error(w, r, v1.CodeUnavailable,
			"this server cannot stream events; the page will fall back to polling")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// For nginx, which buffers proxied responses by default and would hold
	// every event until the stream ended - which it never does.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	events, cancel := s.events.subscribe()
	defer cancel()

	// An immediate comment, so the browser's fetch resolves and the page knows
	// it is connected rather than waiting for the first transfer to move.
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	// A HEARTBEAT, because an idle stream is indistinguishable from a dead one
	// to everything between here and the browser. Fifteen seconds is inside
	// the idle timeout of every proxy this is likely to sit behind.
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": beat\n\n")
			flusher.Flush()
		case id, open := <-events:
			if !open {
				return
			}
			payload, err := json.Marshal(struct {
				Transfer string `json:"transfer"`
			}{Transfer: id})
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: transfer\ndata: %s\n\n", payload)
			flusher.Flush()
		}
	}
}
