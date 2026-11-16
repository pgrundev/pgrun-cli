package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTerminal(t *testing.T) {
	terminal := map[string]bool{
		StatusReady: true, StatusFailed: true, StatusDeleted: true,
		StatusStopped: true, StatusUnhealthy: true,
		StatusDeleting: false, "creating": false, "snapshotting": false,
		"provisioning": false, "starting": false, "": false,
	}
	for status, want := range terminal {
		if got := Terminal(status); got != want {
			t.Errorf("Terminal(%q) = %v, want %v", status, got, want)
		}
	}
}

func TestWaitFailureReason(t *testing.T) {
	if got := WaitFailureReason("b", StatusReady); got != "" {
		t.Errorf("ready should have no failure reason, got %q", got)
	}
	for _, status := range []string{StatusFailed, StatusDeleted, StatusStopped, StatusUnhealthy, "bogus"} {
		reason := WaitFailureReason("mybranch", status)
		if reason == "" {
			t.Errorf("status %q got an empty reason", status)
		}
	}
	if got := WaitFailureReason("mybranch", StatusDeleted); got != "branch mybranch was deleted while waiting" {
		t.Errorf("StatusDeleted reason = %q", got)
	}
}

// TestWaitForTerminal_DeletedStopsImmediately is F1's scripted-poll case: a
// branch that shows up as "deleted" mid-wait must stop the poll loop the
// instant that status is observed — not run out the clock on the caller's
// timeout. PollInterval is shrunk and the context timeout is left generous
// (2s) specifically so a bug that fell through to the timeout path would
// make this test visibly slow instead of silently passing.
func TestWaitForTerminal_DeletedStopsImmediately(t *testing.T) {
	old := PollInterval
	PollInterval = time.Millisecond
	t.Cleanup(func() { PollInterval = old })

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusOK)
		if n < 2 {
			w.Write([]byte(`{"id":"b1","name":"feature-x","status":"creating"}`))
			return
		}
		w.Write([]byte(`{"id":"b1","name":"feature-x","status":"deleted"}`))
	}))
	t.Cleanup(srv.Close)
	client := New(srv.URL, "test-token")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, branch, err := client.WaitForTerminal(ctx, "proj1", nil, Branch{Name: "feature-x", Status: "creating"})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("WaitForTerminal returned an error (should stop cleanly on a terminal status): %v", err)
	}
	if branch.Status != StatusDeleted {
		t.Fatalf("branch.Status = %q, want %q", branch.Status, StatusDeleted)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("took %s — looks like it ran out the 2s timeout instead of stopping on \"deleted\"", elapsed)
	}
}

// A create response that already says "ready" carries no connection URL (only a GET of the single
// branch does): WaitForTerminal fetches it once rather than returning a ready branch without one.
func TestWaitForTerminal_ReadyOnCreateFetchesURLOnce(t *testing.T) {
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		w.Write([]byte(`{"id":"b1","name":"x","status":"ready","connection_url":"postgres://u:p@h/x"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	_, b, err := c.WaitForTerminal(context.Background(), "p", nil, Branch{Name: "x", Status: StatusReady})
	if err != nil || b.ConnectionURL != "postgres://u:p@h/x" || atomic.LoadInt32(&gets) != 1 {
		t.Fatalf("branch %+v err %v gets %d, want the URL from exactly one GET", b, err, gets)
	}
	// Ready and not yet credentialed on the GET as well: returned as is, no loop.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&gets, 1)
		w.Write([]byte(`{"id":"b1","name":"x","status":"ready"}`))
	}))
	defer srv2.Close()
	atomic.StoreInt32(&gets, 0)
	_, b, err = New(srv2.URL, "t").WaitForTerminal(context.Background(), "p", nil, Branch{Name: "x", Status: StatusReady})
	if err != nil || b.ConnectionURL != "" || atomic.LoadInt32(&gets) != 1 {
		t.Fatalf("branch %+v err %v gets %d", b, err, gets)
	}
}
