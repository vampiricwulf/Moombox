package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
