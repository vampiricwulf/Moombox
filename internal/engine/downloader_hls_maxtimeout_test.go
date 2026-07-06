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
		BaseURL:           srv.URL + "/playlist.m3u8",
		OutputFile:        filepath.Join(t.TempDir(), "video_stream"),
		StartSeq:          -1,
		IsHls:             true,
		MaxTimeout:        300 * time.Millisecond,
		EnforceMaxTimeout: true,
		CheckStreamStatus: func(ctx context.Context) (bool, error) {
			statusChecks.Add(1)
			return false, nil // perpetually "live"
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
		MaxTimeout:        300 * time.Millisecond,
		EnforceMaxTimeout: false,
		CheckStreamStatus: func(ctx context.Context) (bool, error) {
			statusChecks.Add(1)
			return true, nil // report ended so the stale path can exit
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
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
func TestHlsLoop_EnforceMaxTimeoutPausesForOfflineOutage(t *testing.T) {
	var playlistFetches atomic.Int32
	var postReconnectServed atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, r *http.Request) {
		switch playlistFetches.Add(1) {
		case 1:
			// Live window with one segment (no ENDLIST) — seeds lastSegTime.
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n"+
				"#EXT-X-MEDIA-SEQUENCE:100\n#EXTINF:1.0,\nseg100.ts\n")
		case 2:
			// 404 while the device is offline → offline-recovery branch, the
			// exact path whose missing lastSegTime reset caused the bug.
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

	// Offline for the first two probes (the 404 branch's direct check +
	// waitForConnectivity's first poll), online thereafter. The intervening 5s
	// connectivity poll ages lastSegTime well past the 3s MaxTimeout, so only
	// the waitOnline reset keeps the backstop from finalizing on reconnect.
	var probes atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:           srv.URL + "/playlist.m3u8",
		OutputFile:        filepath.Join(t.TempDir(), "video_stream"),
		StartSeq:          -1,
		IsHls:             true,
		MaxTimeout:        3 * time.Second,
		EnforceMaxTimeout: true,
		IsOnline:          func() bool { return probes.Add(1) > 2 },
		CheckStreamStatus: func(ctx context.Context) (bool, error) { return false, nil },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
