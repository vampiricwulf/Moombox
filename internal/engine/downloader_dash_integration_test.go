package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGVS simulates a googlevideo segment endpoint for the DASH loop:
// query-style &sq=N addressing, X-Head-Seqnum on every response (success
// and error alike, like real GVS), per-seq attempt counting so scenarios
// can model transient failures. These are the only end-to-end tests of
// runDashLoop — everything else pokes the error handlers directly.
type fakeGVS struct {
	t        *testing.T
	head     int
	mu       sync.Mutex
	attempts map[int]int
	// decide returns the HTTP status for a request for seq on its nth
	// attempt (1-based). 200 serves a "seg-N;" body.
	decide func(seq, attempt int) int
}

func newFakeGVS(t *testing.T, head int, decide func(seq, attempt int) int) (*fakeGVS, *httptest.Server) {
	t.Helper()
	g := &fakeGVS{t: t, head: head, attempts: map[int]int{}, decide: decide}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sqStr := r.URL.Query().Get("sq")
		w.Header().Set("X-Head-Seqnum", strconv.Itoa(g.head))
		if sqStr == "" {
			// Bare base-URL fetch (not used by the DASH loop, but harmless).
			w.WriteHeader(http.StatusOK)
			return
		}
		seq, err := strconv.Atoi(sqStr)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.attempts[seq]++
		attempt := g.attempts[seq]
		g.mu.Unlock()
		status := g.decide(seq, attempt)
		if status == http.StatusOK {
			fmt.Fprintf(w, "seg-%05d;", seq)
			return
		}
		w.WriteHeader(status)
		w.Write([]byte("gone"))
	}))
	t.Cleanup(srv.Close)
	return g, srv
}

// warnCollector implements DownloaderLogger, collecting Warn messages.
type warnCollector struct {
	mu    sync.Mutex
	warns []string
}

func (w *warnCollector) Debug(string, ...any) {}
func (w *warnCollector) Info(string, ...any)  {}
func (w *warnCollector) Error(string, ...any) {}
func (w *warnCollector) Warn(msg string, _ ...any) {
	w.mu.Lock()
	w.warns = append(w.warns, msg)
	w.mu.Unlock()
}
func (w *warnCollector) joined() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.warns, "\n")
}

func wantSegments(t *testing.T, path string, from, to int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	var want strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&want, "seg-%05d;", i)
	}
	if got := string(data); got != want.String() {
		t.Errorf("output = %q, want segments %d..%d", got, from, to)
	}
}

// TestDashLoopCleanPostLiveEnd drives the real runDashLoop end-to-end
// against a fake GVS: an ended stream with segments 0..head available.
// The loop must fetch all of them (harvesting head from segment response
// headers), hit the past-head gone burst, verify the end, finalize
// cleanly, and clear the resume sidecar.
func TestDashLoopCleanPostLiveEnd(t *testing.T) {
	t.Parallel()
	const head = 5
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= head {
			return http.StatusOK
		}
		return http.StatusForbidden
	})

	out := filepath.Join(t.TempDir(), "v")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:           srv.URL + "/videoplayback?id=itest.1&itag=140",
		OutputFile:        out,
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil }, // post-live: ended
	})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil (clean finalize)", err)
	}
	wantSegments(t, out, 0, head)
	if !d.streamEnded.Load() {
		t.Error("streamEnded not set on clean end")
	}
	if got := d.headSeq.Load(); got != head {
		t.Errorf("headSeq = %d, want %d (harvested from segment responses)", got, head)
	}
	if _, err := os.Stat(out + ".resume.json"); !os.IsNotExist(err) {
		t.Errorf("resume sidecar not cleared on clean end (stat err = %v)", err)
	}
}

// TestDashLoopTransientBurstRecovers models a mid-download 403 blip (POT
// hiccup / CDN blip) on one segment: the loop must ride it out and produce
// a COMPLETE recording — the scenario that silently truncated before the
// behind-head guard.
func TestDashLoopTransientBurstRecovers(t *testing.T) {
	t.Parallel()
	const head = 8
	const flakySeq = 4
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		switch {
		case seq == flakySeq && attempt <= 3:
			return http.StatusForbidden // transient burst
		case seq <= head:
			return http.StatusOK
		default:
			return http.StatusForbidden // past end
		}
	})

	out := filepath.Join(t.TempDir(), "v")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:           srv.URL + "/videoplayback?id=itest.2&itag=140",
		OutputFile:        out,
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	wantSegments(t, out, 0, head) // no gap at flakySeq
}

// TestDashLoopBehindHeadBudgetExhaustionWarns models the unavoidable
// truncation: segments beyond an early point 403 forever while head says
// more exist. The loop must defer within the MaxTimeout budget, then
// finalize with the loud unfetched-tail warning instead of a silent
// clean-looking success.
func TestDashLoopBehindHeadBudgetExhaustionWarns(t *testing.T) {
	t.Parallel()
	const head = 10
	const lastAvailable = 4
	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		if seq <= lastAvailable {
			return http.StatusOK
		}
		return http.StatusForbidden // tail permanently unavailable
	})

	out := filepath.Join(t.TempDir(), "v")
	warns := &warnCollector{}
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.3&itag=140",
		OutputFile: out,
		// Budget: long enough that the escalation (10 gones x 500ms) fires
		// with budget remaining — exercising the defer loop — short enough
		// the test finishes promptly.
		MaxTimeout:        7 * time.Second,
		Logger:            warns,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})

	start := time.Now()
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil (bounded finalize)", err)
	}
	wantSegments(t, out, 0, lastAvailable)
	if elapsed := time.Since(start); elapsed < 6*time.Second {
		t.Errorf("finalized after %v — did not defer within the MaxTimeout budget", elapsed)
	}
	if !strings.Contains(warns.joined(), "unfetched tail") {
		t.Errorf("no unfetched-tail warning on truncated finalize; warns:\n%s", warns.joined())
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded latched on a known-incomplete finalize")
	}
	// The resume sidecar must survive an incomplete finalize: it is the ONLY
	// resume mechanism for post-live jobs, and a later retry uses it to
	// append the missing tail instead of truncating and starting over.
	if _, err := os.Stat(out + ".resume.json"); err != nil {
		t.Errorf("resume sidecar missing after incomplete finalize (stat err = %v) — retry would restart from scratch", err)
	}
}

// TestDashLoopRecoversFromCredentialExpiry reproduces the 2026-08-15
// mid-stream-join stall end to end: segments below the head start 403ing
// after N successes -- the credentials went stale mid-download, the stream
// itself did not end -- and the loop must refresh credentials via
// OnCredentialRefresh and carry on rather than escalating to ErrQualityLost
// (which would tear the downloader down for a full restart). Before the
// fix, handleGoneError never called OnCredentialRefresh, so the burst rides
// past goneRetryDuringDownload and CheckStreamStatus's "still live" verdict
// turns it into ErrQualityLost -- see the report for how that was confirmed.
//
// head is kept below stayBehindSegments+CatchupThreshold so segsBehind
// never crosses the parallel-catch-up gate: every fetch, including the
// recovery, flows through the single-segment path (handleGoneError) rather
// than the catch-up worker pool's own fetchSegmentWithRetry recovery, which
// TestForbiddenBehindHeadRefreshesAndRetries already covers directly. This
// test is the missing end-to-end proof that runDashLoop's sequential loop
// wires the same recovery through handleGoneError.
func TestDashLoopRecoversFromCredentialExpiry(t *testing.T) {
	t.Parallel()
	const expireAfter = 20 // seq 0..19 succeed; 20+ 403 until refreshed
	const head = 35        // < stayBehindSegments(30)+CatchupThreshold(10): no catch-up
	const endSeq = 34
	var refreshed atomic.Bool

	_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
		// Credentials go stale after `expireAfter` segments and only a
		// refresh revives them -- exactly what production showed (~60-100
		// segments per credential, then 403 until a fresh token arrived).
		if seq >= expireAfter && !refreshed.Load() {
			return http.StatusForbidden
		}
		return http.StatusOK
	})

	out := filepath.Join(t.TempDir(), "v")
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/videoplayback?id=itest.4&itag=140",
		OutputFile: out,
		EndSeq:     endSeq,
		MaxTimeout: 30 * time.Second,
		// Still live, not post-live: this models a mid-stream join, the
		// scenario that actually failed in production.
		CheckStreamStatus: func(context.Context) (bool, error) { return false, nil },
		OnCredentialRefresh: func() (string, string) {
			refreshed.Store(true)
			return "", "fresh-token"
		},
	})

	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil (credential refresh should have recovered the download)", err)
	}
	wantSegments(t, out, 0, endSeq)
	if !refreshed.Load() {
		t.Error("OnCredentialRefresh never fired")
	}
}
