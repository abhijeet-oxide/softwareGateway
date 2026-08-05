package v1

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSlowServerIsATimeoutNotAnOutage is the fix for the message operators kept
// hitting on `products check` and `packages discover`:
//
//	coordinator unreachable: context deadline exceeded (Client.Timeout
//	exceeded while awaiting headers)
//
// The Coordinator was neither unreachable nor broken. It was working.
func TestSlowServerIsATimeoutNotAnOutage(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	c := NewClient(srv.URL, WithTimeout(150*time.Millisecond))
	_, err := c.ListProducts(context.Background())

	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got: %v", err)
	}
	if errors.Is(err, ErrUnreachable) {
		t.Error("a slow server must not be reported as unreachable — that sends " +
			"the operator to investigate a healthy service")
	}
	// The elapsed budget belongs in the message: Go's own wording names neither
	// the deadline nor the flag that changes it.
	if !strings.Contains(err.Error(), "150ms") {
		t.Errorf("the message should name the timeout that was in force, got: %v", err)
	}
}

func TestClosedPortIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	c := NewClient(url, WithTimeout(2*time.Second))
	_, err := c.ListProducts(context.Background())

	if err == nil {
		t.Fatal("expected a connection failure")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("expected ErrUnreachable, got: %v", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a refused connection is not a timeout")
	}
}

// A cancelled caller context is a timeout for classification purposes: no
// answer came back and the request may still be running server-side.
func TestCancelledContextIsNotReportedAsAnOutage(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	c := NewClient(srv.URL, WithTimeout(30*time.Second))
	if _, err := c.ListProducts(ctx); !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout for a deadline-exceeded context, got: %v", err)
	}
}
