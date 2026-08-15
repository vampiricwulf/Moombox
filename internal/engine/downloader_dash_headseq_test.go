package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchSegmentHarvestsHeadSeq pins the manifest-free head discovery
// (yt-dlp 8c1f07d81 port): every GVS segment response carries X-Head-Seqnum,
// and fetchSegment must harvest it into headSeq — making ordinary segment
// traffic keep the live edge fresh without dedicated probe round-trips.
func TestFetchSegmentHarvestsHeadSeq(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Head-Seqnum", "1234")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("segdata"))
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
	if got := d.headSeq.Load(); got != -1 {
		t.Fatalf("headSeq before fetch = %d, want -1", got)
	}

	data, status, err := d.fetchSegment(context.Background(), srv.URL+"/seg?sq=10")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fetchSegment: status=%d err=%v", status, err)
	}
	if string(data) != "segdata" {
		t.Fatalf("fetchSegment body = %q", data)
	}
	if got := d.headSeq.Load(); got != 1234 {
		t.Errorf("headSeq after fetch = %d, want 1234 (harvested from X-Head-Seqnum)", got)
	}
	if d.lastHeadProbeTime.Since() > time.Second {
		t.Errorf("lastHeadProbeTime not refreshed by harvest — dedicated probe would still fire")
	}

	// Monotonic-max: a lower value from an out-of-order concurrent response
	// (parallel catch-up workers share fetchSegment) must not regress head.
	resp := &http.Response{Header: http.Header{"X-Head-Seqnum": []string{"900"}}}
	d.noteHeadSeqFromResponse(resp)
	if got := d.headSeq.Load(); got != 1234 {
		t.Errorf("headSeq regressed to %d after lower harvest, want 1234 (monotonic max)", got)
	}
}

// TestFetchSegmentIgnoresMissingOrInvalidHeadSeq pins that responses without
// a usable X-Head-Seqnum (Twitch segments, error pages, garbage values)
// leave headSeq untouched.
func TestFetchSegmentIgnoresMissingOrInvalidHeadSeq(t *testing.T) {
	header := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if header != "" {
			w.Header().Set("X-Head-Seqnum", header)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("x"))
	}))
	defer srv.Close()

	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
	for _, h := range []string{"", "notanumber", "-5"} {
		header = h
		if _, _, err := d.fetchSegment(context.Background(), srv.URL); err != nil {
			t.Fatalf("fetchSegment(header=%q): %v", h, err)
		}
		if got := d.headSeq.Load(); got != -1 {
			t.Errorf("headSeq after header %q = %d, want -1 (unchanged)", h, got)
		}
	}
}

// TestHandleGoneErrorBehindHeadDefersEnd pins the silent-truncation fix: a
// gone burst while currentSeq is strictly below the known head must NOT
// finalize even though the status check reports the stream ended — the
// unfetched tail demonstrably exists (post-live segments stay available for
// hours). The handler retries instead, and the latched verdict means the
// status check is not re-spent on every retry iteration.
func TestHandleGoneErrorBehindHeadDefersEnd(t *testing.T) {
	checks := 0
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile: filepath.Join(t.TempDir(), "v"),
		MaxTimeout: time.Hour,
		CheckStreamStatus: func(context.Context) (bool, error) {
			checks++
			return true, nil // post-live: always reads as ended
		},
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.StoreNow() // budget clock fresh — well inside MaxTimeout

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // retry sleep returns immediately

	n := goneRetryDuringDownload + 1
	for i := range 3 {
		n++
		if err := d.handleGoneError(ctx, &n, true); err != nil {
			t.Fatalf("iteration %d: handleGoneError = %v, want nil (defer while tail exists)", i, err)
		}
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded latched despite unfetched tail")
	}
	if checks != 1 {
		t.Errorf("CheckStreamStatus called %d times across retries, want 1 (latched verdict)", checks)
	}
}

// TestHandleGoneErrorBehindHeadRefreshesCredentials is the regression test
// for the sequential-loop gap in the 2026-08-15 live-403 recovery work:
// runDashLoop calls fetchSegment directly (not fetchSegmentWithRetry), so
// without this wiring it got NO credential-refresh recovery on a behind-head
// 403/410 burst — only the parallel catch-up worker did, meaning a stream
// stuck in the sequential loop (not far enough behind to trigger catch-up)
// would stall exactly like the original bug. Once the burst persists past
// postBytes403CipherThreshold — the same threshold that already gates the
// cipher-solver invalidation in handleDashError, so "persisted" means the
// same thing in both places — while behindHeadTailPending() is true,
// handleGoneError must ask for fresh credentials before its retry/sleep.
func TestHandleGoneErrorBehindHeadRefreshesCredentials(t *testing.T) {
	var refreshCalls atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile: filepath.Join(t.TempDir(), "v"),
		MaxTimeout: time.Hour,
		OnCredentialRefresh: func() (string, string) {
			refreshCalls.Add(1)
			return "", "fresh"
		},
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.StoreNow()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // retry sleeps return immediately

	n := 0
	// Ramp up to (but not including) the threshold: no refresh yet.
	for n < postBytes403CipherThreshold-1 {
		if err := d.handleGoneError(ctx, &n, true); err != nil {
			t.Fatalf("iteration n=%d: handleGoneError = %v, want nil", n, err)
		}
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("refresh calls = %d before threshold (n=%d), want 0", got, n)
	}

	// One more failure crosses the threshold: refresh must fire.
	if err := d.handleGoneError(ctx, &n, true); err != nil {
		t.Fatalf("iteration n=%d: handleGoneError = %v, want nil", n, err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("refresh calls = %d at threshold (n=%d), want 1", got, n)
	}
}

// TestHandleGoneErrorPastHeadDoesNotRefreshCredentials pins the boundary the
// gap fix must not cross: once currentSeq is at/past head,
// behindHeadTailPending() is false (the segment is legitimately gone, not
// stale-credentialed), so even a long-persisted burst must never call
// OnCredentialRefresh — that would fire a refresh on every ordinary
// end-of-stream 403/410, which is exactly the double-duty hazard this whole
// task exists to avoid.
func TestHandleGoneErrorPastHeadDoesNotRefreshCredentials(t *testing.T) {
	var refreshCalls atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Hour,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
		OnCredentialRefresh: func() (string, string) {
			refreshCalls.Add(1)
			return "", "fresh"
		},
	})
	d.currentSeq.Store(101)
	d.headSeq.Store(100) // past head
	d.lastSegTime.StoreNow()

	n := postBytes403CipherThreshold + 5 // already well past the refresh threshold
	if err := d.handleGoneError(context.Background(), &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError = %v, want errStreamDone (past head, ended)", err)
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Errorf("refresh calls = %d past head, want 0", got)
	}
}

// TestHandleGoneErrorPastHeadFinalizes pins the healthy post-live end: once
// currentSeq has passed the head, a gone burst on an ended stream finalizes
// cleanly exactly as before the behind-head guard existed.
func TestHandleGoneErrorPastHeadFinalizes(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Hour,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})
	d.currentSeq.Store(101)
	d.headSeq.Store(100)
	d.lastSegTime.StoreNow()

	n := goneRetryDuringDownload + 1
	if err := d.handleGoneError(context.Background(), &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError = %v, want errStreamDone (past head, ended)", err)
	}
	if !d.streamEnded.Load() {
		t.Error("streamEnded not latched on clean finalize")
	}
}

// TestHandleGoneErrorBehindHeadTimeoutFinalizes pins the bound on the
// behind-head retry loop: when the no-segment gap exhausts MaxTimeout, the
// downloader finalizes (with a loud warning) rather than spinning forever
// on segments that will never come back.
func TestHandleGoneErrorBehindHeadTimeoutFinalizes(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.Store(time.Now().Add(-2 * time.Minute)) // budget exhausted

	n := goneRetryDuringDownload + 1
	if err := d.handleGoneError(context.Background(), &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError = %v, want errStreamDone (MaxTimeout exhausted)", err)
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded latched on a known-incomplete finalize — resume sidecar would be cleared, destroying the retry path")
	}
}

// TestHandleGoneErrorBehindHeadStillRefreshesLive pins that the guard does
// not swallow the live-stream recovery path: a stream the status check says
// is still LIVE keeps returning ErrQualityLost so the orchestrator refreshes
// URLs, behind head or not.
func TestHandleGoneErrorBehindHeadStillRefreshesLive(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Hour,
		CheckStreamStatus: func(context.Context) (bool, error) { return false, nil }, // still live
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.StoreNow()

	n := goneRetryDuringDownload + 1
	if err := d.handleGoneError(context.Background(), &n, true); err != ErrQualityLost {
		t.Fatalf("handleGoneError = %v, want ErrQualityLost (live stream refresh)", err)
	}
}

// TestHandleGoneErrorCheckErrorDefersWithoutLatching pins that a status-check
// ERROR does not latch the "ended" verdict: the downloader defers (retries)
// and re-asks after the throttle window, so a still-live stream whose probe
// blipped keeps its ErrQualityLost refresh path instead of burning the whole
// MaxTimeout budget — or finalizing — on an assumption.
func TestHandleGoneErrorCheckErrorDefersWithoutLatching(t *testing.T) {
	checks := 0
	fail := true
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile: filepath.Join(t.TempDir(), "v"),
		MaxTimeout: time.Hour,
		CheckStreamStatus: func(context.Context) (bool, error) {
			checks++
			if fail {
				return false, errors.New("innertube probe blip")
			}
			return false, nil // still live
		},
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.StoreNow()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // defer sleeps return immediately

	n := goneRetryDuringDownload + 1
	if err := d.handleGoneError(ctx, &n, true); err != nil {
		t.Fatalf("first escalation = %v, want nil (defer on check error)", err)
	}
	if d.streamEndVerified {
		t.Fatal("check error latched streamEndVerified — a transient probe failure must not stick")
	}
	n++
	if err := d.handleGoneError(ctx, &n, true); err != nil {
		t.Fatalf("throttled escalation = %v, want nil", err)
	}
	if checks != 1 {
		t.Fatalf("checks = %d, want 1 (re-checks throttled to streamStatusCheckInterval)", checks)
	}
	// Throttle re-opens, the probe recovers, stream still live -> the
	// refresh path must be reachable again.
	fail = false
	d.lastStreamStatusCheck.Store(time.Now().Add(-(streamStatusCheckInterval + time.Second)))
	n++
	if err := d.handleGoneError(ctx, &n, true); err != ErrQualityLost {
		t.Fatalf("recovered escalation = %v, want ErrQualityLost (still-live refresh reachable)", err)
	}
}

// TestHandleGoneErrorOfflinePausesTimeoutClock pins the offline-clock parity
// with handleHTTPError: an outage longer than MaxTimeout must not consume
// the behind-head retry budget — lastSegTime resets on reconnect so the
// tail-recovery guard starts from a fresh budget.
func TestHandleGoneErrorOfflinePausesTimeoutClock(t *testing.T) {
	calls := 0
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile: filepath.Join(t.TempDir(), "v"),
		MaxTimeout: time.Minute,
		IsOnline:   func() bool { calls++; return calls > 1 }, // offline once, then back
	})
	d.lastSegTime.Store(time.Now().Add(-2 * time.Minute)) // aged past MaxTimeout during the outage

	n := goneRetryDuringDownload + 1
	if err := d.handleGoneError(context.Background(), &n, true); err != nil {
		t.Fatalf("offline escalation = %v, want nil (reconnect continues)", err)
	}
	if d.lastSegTime.Since() > time.Second {
		t.Error("lastSegTime not reset on reconnect — MaxTimeout budget not refreshed")
	}
	if n != 0 {
		t.Errorf("consecutiveGoneErrors = %d, want 0 after reconnect", n)
	}
}

// TestNoteHeadSeqProbeCannotRegress pins that the dedicated-probe update
// path shares the monotonic-max guard: a stale CDN edge answering a probe
// below an already-harvested head must not lower it (which would silently
// disarm behindHeadTailPending at finalize time).
func TestNoteHeadSeqProbeCannotRegress(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
	if !d.noteHeadSeq(5000) {
		t.Fatal("first noteHeadSeq(5000) should advance")
	}
	if d.noteHeadSeq(4990) {
		t.Error("noteHeadSeq(4990) advanced over 5000 — monotonic max violated")
	}
	if got := d.headSeq.Load(); got != 5000 {
		t.Errorf("headSeq = %d, want 5000", got)
	}
}

// TestNoteHeadSeqFromProbeCorrectsPoisonedHead pins the poison escape
// hatch: a dedicated probe may correct the ratchet DOWNWARD when the
// discrepancy exceeds the staleness slack — without it, one bogus-high
// header would permanently inflate head, turning every stall into an
// ErrQualityLost refresh and every finalize into a full MaxTimeout defer.
func TestNoteHeadSeqFromProbeCorrectsPoisonedHead(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{OutputFile: filepath.Join(t.TempDir(), "v")})
	d.noteHeadSeqFromProbe(5000)
	if got := d.headSeq.Load(); got != 5000 {
		t.Fatalf("headSeq = %d, want 5000", got)
	}
	// Small dip: ordinary edge staleness — the ratchet holds.
	d.noteHeadSeqFromProbe(4990)
	if got := d.headSeq.Load(); got != 5000 {
		t.Errorf("headSeq = %d after small-dip probe, want 5000 (within slack)", got)
	}
	// Huge dip: the tracked head was poisoned — the probe corrects it.
	d.noteHeadSeqFromProbe(500)
	if got := d.headSeq.Load(); got != 500 {
		t.Errorf("headSeq = %d after correction probe, want 500", got)
	}
}

// TestHandleHTTPErrorEndedBehindHeadDefers pins the same guard on the
// stall path: a 30s no-segment gap on an ended stream normally finalizes,
// but not while the known head says unfetched segments remain and the
// MaxTimeout budget is still open — the backoff continues instead.
func TestHandleHTTPErrorEndedBehindHeadDefers(t *testing.T) {
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Hour,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil }, // ended
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.Store(time.Now().Add(-(streamStatusCheckInterval + 5*time.Second)))
	d.lastHeadProbeTime.StoreNow() // suppress the network head re-probe

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // trailing backoff sleep returns immediately

	sameSegRetries, lastRetrySeq, sameHeadRetryDelay, lastConfirmedHead := 0, -1, 0, -1
	err := d.handleHTTPError(ctx, true,
		&sameSegRetries, &lastRetrySeq, &sameHeadRetryDelay, &lastConfirmedHead, 60)
	if err != nil {
		t.Fatalf("handleHTTPError = %v, want nil (defer while tail exists)", err)
	}
}

// TestFinalizedBehindHeadAccessor pins the precise incomplete signal: it is
// set exactly when a finalize fires the unfetched-tail warning, and NOT on a
// clean past-head finalize.
func TestFinalizedBehindHeadAccessor(t *testing.T) {
	// Incomplete finalize: behind head, budget exhausted.
	d := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})
	d.currentSeq.Store(50)
	d.headSeq.Store(100)
	d.lastSegTime.Store(time.Now().Add(-2 * time.Minute))
	n := goneRetryDuringDownload + 1
	if err := d.handleGoneError(context.Background(), &n, true); err != errStreamDone {
		t.Fatalf("handleGoneError = %v, want errStreamDone", err)
	}
	if !d.FinalizedBehindHead() {
		t.Error("FinalizedBehindHead() = false after behind-head finalize")
	}
	if got := d.HeadSeq(); got != 100 {
		t.Errorf("HeadSeq() = %d, want 100", got)
	}

	// Clean finalize: past head — flag must stay false.
	d2 := NewSegmentDownloader(DownloaderOptions{
		OutputFile:        filepath.Join(t.TempDir(), "v"),
		MaxTimeout:        time.Minute,
		CheckStreamStatus: func(context.Context) (bool, error) { return true, nil },
	})
	d2.currentSeq.Store(101)
	d2.headSeq.Store(100)
	d2.lastSegTime.StoreNow()
	n2 := goneRetryDuringDownload + 1
	if err := d2.handleGoneError(context.Background(), &n2, true); err != errStreamDone {
		t.Fatalf("clean handleGoneError = %v, want errStreamDone", err)
	}
	if d2.FinalizedBehindHead() {
		t.Error("FinalizedBehindHead() = true after clean finalize")
	}
}
