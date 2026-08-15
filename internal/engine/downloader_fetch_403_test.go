package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestForbiddenBehindHeadRefreshesAndRetries is the regression test for the
// 2026-08-15 live stall: segments that exist (currentSeq well below the
// harvested head) started 403ing, and the downloader declared them
// permanently gone with zero retries, so the recording stopped forever while
// a manual restart with fresh credentials resumed it instantly.
func TestForbiddenBehindHeadRefreshesAndRetries(t *testing.T) {
	var refreshed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", "5000")
		// The token only becomes acceptable after a refresh.
		if r.URL.Query().Get("pot") == "fresh" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("segment-bytes"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		PoToken:    "stale",
		OnCredentialRefresh: func() (string, string) {
			refreshed.Store(true)
			return "", "fresh"
		},
	})
	// Behind head: head 5000, current 10.
	d.noteHeadSeq(5000)
	d.currentSeq.Store(10)
	// behindHeadTailPending also requires the MaxTimeout budget to still be
	// open (internal/engine/downloader_dash.go:449). That clock is normally
	// seeded by runDashLoop before any segment fetch runs; this test drives
	// fetchSegmentWithRetry directly, so it must seed it too — the same
	// idiom every other direct test of this gate uses (see
	// downloader_dash_headseq_test.go's d.lastSegTime.StoreNow() calls).
	d.lastSegTime.StoreNow()

	body, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(10), nil)
	if err != nil {
		t.Fatalf("fetch failed after refresh: %v", err)
	}
	if string(body) != "segment-bytes" {
		t.Errorf("body = %q, want the segment", string(body))
	}
	if !refreshed.Load() {
		t.Error("OnCredentialRefresh was never called")
	}
}

// TestForbiddenAtHeadStaysPermanent guards end-of-stream detection: a 403 at
// or past the head is how a finished stream terminates, and VOD/post-live
// finalization depends on it. Recovery must not touch that case.
func TestForbiddenAtHeadStaysPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Head-Seqnum", "100")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	var refreshCalls atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		PoToken:    "stale",
		OnCredentialRefresh: func() (string, string) {
			refreshCalls.Add(1)
			return "", "fresh"
		},
	})
	d.noteHeadSeq(100)
	d.currentSeq.Store(101) // past head — this is the end of the stream

	_, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(101), nil)
	if err != ErrSegmentPermanent {
		t.Fatalf("err = %v, want ErrSegmentPermanent at head", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("HTTP attempts = %d, want exactly 1 (no retry past head)", got)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 past head", got)
	}
}

// TestGoneAlwaysPermanent: 410 means the segment is really evicted. It must
// never trigger a refresh or a retry regardless of head position.
func TestGoneAlwaysPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Head-Seqnum", "5000")
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	var refreshCalls atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		OnCredentialRefresh: func() (string, string) {
			refreshCalls.Add(1)
			return "", "fresh"
		},
	})
	d.noteHeadSeq(5000)
	d.currentSeq.Store(10)

	if _, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(10), nil); err != ErrSegmentPermanent {
		t.Fatalf("err = %v, want ErrSegmentPermanent for 410", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("HTTP attempts = %d, want exactly 1 for 410", got)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("refresh calls = %d, want 0 for 410", got)
	}
}

// TestForbiddenBehindHeadURLRefreshRebuildsSegURL is the regression test for
// the URL half of credential recovery: segURL is captured ONCE before the
// retry loop starts, so a refresh that installs a fresh BASE URL (sig/n-param
// rotation — the case SetBaseURL and OnCredentialRefresh exist for) is
// invisible to the retry unless the caller supplies a rebuild closure that
// re-derives segURL from the current base. Without the rebuild, every retry
// re-fetches the SAME stale URL and 403s identically, exhausting to
// ErrSegmentPermanent even though fresh credentials were available.
//
// The server enforces this directly: only the "/v2" path (the refreshed
// base) succeeds; the original "/v" path always 403s regardless of PO
// token, so a passing test proves the rebuilt URL was actually used.
func TestForbiddenBehindHeadURLRefreshRebuildsSegURL(t *testing.T) {
	var refreshed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", "5000")
		if r.URL.Path == "/v2" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("segment-bytes"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		PoToken:    "stale",
		OnCredentialRefresh: func() (string, string) {
			refreshed.Store(true)
			// URL-only refresh: empty token, so SetPoToken is a no-op
			// (SetPoToken ignores "") and only the base URL half is
			// exercised — isolating this from the token-only path
			// TestForbiddenBehindHeadRefreshesAndRetries already covers.
			return srv.URL + "/v2?x=1", ""
		},
	})
	d.noteHeadSeq(5000)
	d.currentSeq.Store(10)
	d.lastSegTime.StoreNow() // see comment in the token-refresh test above

	seq := 10
	segURL := d.buildSegmentURL(seq)
	body, err := d.fetchSegmentWithRetry(context.Background(), segURL, func() string {
		return d.buildSegmentURL(seq)
	})
	if err != nil {
		t.Fatalf("fetch failed after URL refresh: %v", err)
	}
	if string(body) != "segment-bytes" {
		t.Errorf("body = %q, want the segment", string(body))
	}
	if !refreshed.Load() {
		t.Error("OnCredentialRefresh was never called")
	}
}

// TestForbiddenBehindHeadExhaustsToPermanent pins the bound on the recovery
// loop: when the server never accepts any credential (refresh keeps failing
// to help), fetchSegmentWithRetry must still terminate at exactly
// forbiddenRefreshAttempts HTTP attempts and report ErrSegmentPermanent —
// the existing "3xx/410-caller can't hang" contract must survive credential
// recovery being added on top of it.
func TestForbiddenBehindHeadExhaustsToPermanent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Head-Seqnum", "5000")
		w.WriteHeader(http.StatusForbidden) // never accepts anything
	}))
	defer srv.Close()

	var refreshCalls atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/v?x=1",
		OutputFile: "x",
		PoToken:    "stale",
		OnCredentialRefresh: func() (string, string) {
			refreshCalls.Add(1)
			return "", "fresh" // installs fine, just never satisfies the server
		},
	})
	d.noteHeadSeq(5000)
	d.currentSeq.Store(10)
	d.lastSegTime.StoreNow()

	_, err := d.fetchSegmentWithRetry(context.Background(), d.buildSegmentURL(10), nil)
	if err != ErrSegmentPermanent {
		t.Fatalf("err = %v, want ErrSegmentPermanent once refresh attempts are exhausted", err)
	}
	if got := calls.Load(); got != forbiddenRefreshAttempts {
		t.Errorf("HTTP attempts = %d, want exactly forbiddenRefreshAttempts (%d)", got, forbiddenRefreshAttempts)
	}
	// The cooldown allows exactly one real OnCredentialRefresh invocation
	// (credentialRefreshCooldown is 5s, far longer than this test runs);
	// the remaining refreshCredentials() calls are no-ops that don't reach
	// the callback. Assert at least one fired rather than an exact count so
	// this test doesn't also pin the cooldown's timing.
	if got := refreshCalls.Load(); got < 1 {
		t.Errorf("refresh calls = %d, want at least 1 before exhaustion", got)
	}
}
