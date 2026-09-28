package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hlsStallServer serves a fixed single-segment live window (no EXT-X-ENDLIST)
// so that after the first segment is written the playlist never yields another
// — the exact "stream went quiet at the live edge but YouTube still says live"
// shape the MaxTimeout backstop exists for.
func hlsStallServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
			"#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:1.0,\nseg100.ts\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
	})
	return httptest.NewServer(mux)
}

// TestHlsLoop_EnforceMaxTimeoutForcesFinalize pins the YouTube HLS backstop:
// with EnforceMaxTimeout set, a stream that stops delivering segments is
// force-finalized once the no-segment gap exceeds MaxTimeout even though
// CheckStreamStatus keeps reporting the stream live — and it does so WITHOUT
// consulting CheckStreamStatus (the backstop is the authority here), so the
// call count stays zero.
func TestHlsLoop_EnforceMaxTimeoutForcesFinalize(t *testing.T) {
	srv := hlsStallServer()
	defer srv.Close()

	var statusChecks atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
		StartSeq:   -1,
		IsHls:      true,
		// 60ms, not fast(300ms): a pure fastScale division lands at 15ms,
		// inside timer jitter. 60 ms is about one reload cycle at fast scale
		// (50 ms flowing, 25 ms stalled), so the backstop fires on the
		// second or third reload — the same shape 300 ms had against the 1 s
		// production cycle. Not a pure ÷20 (15 ms) because that is inside
		// timer jitter.
		MaxTimeout:        60 * time.Millisecond,
		EnforceMaxTimeout: true,
		CheckStreamStatus: func(ctx context.Context) (bool, error) {
			statusChecks.Add(1)
			return false, nil // perpetually "live"
		},
	})
	d.delays = fastDelays()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (backstop finalizes cleanly)", err)
	}
	if !d.streamEnded.Load() {
		t.Error("streamEnded not set — backstop should mark a clean end")
	}
	if got := statusChecks.Load(); got != 0 {
		t.Errorf("CheckStreamStatus called %d times, want 0 (backstop finalizes without a status check)", got)
	}
}

// TestHlsLoop_NoEnforceRespectsStatusCheck is the Twitch-parity guard: with
// EnforceMaxTimeout false the backstop must never fire (even though the shared
// constructor defaulted MaxTimeout to a non-zero value), so the same stalled
// stream instead terminates through the normal stale-window status check —
// proven by CheckStreamStatus actually being consulted.
func TestHlsLoop_NoEnforceRespectsStatusCheck(t *testing.T) {
	srv := hlsStallServer()
	defer srv.Close()

	var statusChecks atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    srv.URL + "/playlist.m3u8",
		OutputFile: filepath.Join(t.TempDir(), "video_stream"),
		StartSeq:   -1,
		IsHls:      true,
		// A short MaxTimeout that WOULD fire almost immediately if the loop
		// honored it — but EnforceMaxTimeout is false, so it must be ignored.
		// 60ms rather than fast(300ms), for the jitter reason above.
		MaxTimeout:        60 * time.Millisecond,
		EnforceMaxTimeout: false,
		CheckStreamStatus: func(ctx context.Context) (bool, error) {
			statusChecks.Add(1)
			return true, nil // report ended so the stale path can exit
		},
	})
	d.delays = fastDelays()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (stale-path status check ends it)", err)
	}
	if got := statusChecks.Load(); got < 1 {
		t.Errorf("CheckStreamStatus called %d times, want >=1 (backstop must NOT shortcut Twitch HLS)", got)
	}
}

// TestHlsLoop_EnforceMaxTimeoutPausesForOfflineOutage is the regression guard
// for the delta-introduced bug: an offline outage longer than MaxTimeout must
// NOT count toward the backstop, so reconnecting doesn't force-finalize a
// still-live recording. The loop enters an offline-recovery branch (404 while
// the device is offline), waits out the outage, and on reconnect must keep
// recording — proven by the post-reconnect playlist actually being fetched
// (the backstop would otherwise return before any further fetch).
//
// Connectivity here is EXPLICIT STATE, never a probe counter: the outage is
// opened by the handler that serves the 404 and closed a fixed moment later,
// so WHICH of the loop's IsOnline() sites asks, and in what order, cannot
// change the answer. A counter ("offline for the first two calls") made this
// test race the loop instead: a stall longer than MaxTimeout right after the
// segment landed — which a loaded runner produces — let the backstop at the
// top of the loop ask first. Its own check took the first "offline" answer
// and its waitOnline took the second plus the first "online" one, so the 404
// branch, the round this test is about, read "online" on the fourth call. It
// then went to consultStreamEnd, which answers "still live" here, and Start()
// returned ErrQualityLost ("stream quality became unavailable") — not the
// backstop this test guards.
//
// The timing has one relationship and it is the whole point: the outage lasts
// one connectivityPoll (waitForConnectivity's entry check reads offline, the
// flip lands before its first tick), and MaxTimeout is half a poll — so the
// outage is twice the budget it must not spend, while still being an outage
// LONGER than MaxTimeout, which is the condition under test. The 500 ms
// budget also leaves ~450 ms of slack over the ~50 ms reload cadence before
// the 404 round, where the old 200 ms left ~150 ms.
//
// MUTANT: drop the d.lastSegTime.StoreNow() from waitOnline — the one-second
// outage ages the clock past the 500 ms budget, the backstop fires at the top
// of the next iteration and the post-reconnect playlist is never fetched:
// "recording force-finalized on reconnect".
func TestHlsLoop_EnforceMaxTimeoutPausesForOfflineOutage(t *testing.T) {
	const (
		// outagePoll is this test's connectivityPoll, 4x fastDelays' 250 ms.
		// It is what makes the outage's LENGTH deterministic: whenever the
		// flip lands inside the first poll, waitForConnectivity returns on
		// that first tick and the stretch is exactly one poll.
		outagePoll = time.Second
		// flipOnlineAfter is when the device comes back, measured from the
		// 404. Three quarters of a poll: late enough that the entry check
		// and the whole 404 branch around it still read offline, early
		// enough to be seen by the first tick. Landing LATE is harmless
		// (the next tick returns and the outage is simply longer); landing
		// before the entry check is what would shorten it, which is why it
		// is not a few milliseconds.
		flipOnlineAfter = 750 * time.Millisecond
		// maxTimeout is half the outage, so the reset under test is what
		// separates a pass from a fail, with no arithmetic near the edge.
		maxTimeout = 500 * time.Millisecond
	)

	var playlistFetches atomic.Int32
	var postReconnectServed atomic.Bool
	var offline atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		switch playlistFetches.Add(1) {
		case 1:
			// Live window with one segment (no ENDLIST) — seeds lastSegTime.
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
				"#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:1.0,\nseg100.ts\n")
		case 2:
			// 404 while the device is offline → offline-recovery branch, the
			// exact path whose missing lastSegTime reset caused the bug. The
			// outage opens HERE, before the 404 reaches the loop, so every
			// IsOnline() the round makes reads offline.
			offline.Store(true)
			time.AfterFunc(flipOnlineAfter, func() { offline.Store(false) })
			w.WriteHeader(http.StatusNotFound)
		default:
			// Post-reconnect fetch: reached only if the recording survived the
			// outage. End cleanly so the test terminates.
			postReconnectServed.Store(true)
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
				"#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:1.0,\nseg100.ts\n#EXT-X-ENDLIST\n")
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "[%s]", strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".ts"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The device is offline for exactly the stretch the 404 handler opened —
	// no call count anywhere. waitForConnectivity's entry check reads it
	// offline and its first tick, one outagePoll later, reads it online, so
	// lastSegTime ages a full second: twice the MaxTimeout budget, and only
	// the waitOnline reset keeps the backstop from finalizing on reconnect.
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:           srv.URL + "/playlist.m3u8",
		OutputFile:        filepath.Join(t.TempDir(), "video_stream"),
		StartSeq:          -1,
		IsHls:             true,
		MaxTimeout:        maxTimeout,
		EnforceMaxTimeout: true,
		IsOnline:          func() bool { return !offline.Load() },
		CheckStreamStatus: func(ctx context.Context) (bool, error) { return false, nil },
	})
	// fastDelays keeps the ~50 ms reload cadence; only the connectivity poll
	// is stretched, because it is the one that sets the outage's length.
	dl := fastDelays()
	dl.connectivityPoll = outagePoll
	d.delays = dl

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if !postReconnectServed.Load() {
		t.Error("recording force-finalized on reconnect — offline outage counted toward MaxTimeout (lastSegTime not paused)")
	}
	if !d.streamEnded.Load() {
		t.Error("streamEnded not set — expected a clean EXT-X-ENDLIST finish after reconnect")
	}
}
