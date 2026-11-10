package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunExitSidecarOutcome pins, for every way runDashLoop and runHlsLoop
// end, whether the resume sidecar survives the exit. Only a CONFIRMED,
// complete end clears it; every other exit keeps it so a later retry or
// Resume appends instead of starting over. Owner ruling R-G1 (Q10): the
// DASH end that handleHTTPError confirms KEEPS the sidecar, unlike the
// gone-path confirmed end, which clears it — pinned here row by row so a
// refactor of the exits cannot move one.
//
// sidecar: "cleared" (no file), "kept" (the file exists), "none" (nothing
// was written, so there was never a sidecar to keep).
func TestRunExitSidecarOutcome(t *testing.T) {
	const head = 5
	okThrough := func(last, past int) func(seq, attempt int) int {
		return func(seq, attempt int) int {
			if seq <= last {
				return http.StatusOK
			}
			return past
		}
	}
	type row struct {
		name    string
		run     func(t *testing.T, out string) (*SegmentDownloader, error)
		wantErr bool
		sidecar string
		// reason is the finishReason the exit records (finishNone: an error
		// or a cancel records none).
		reason finishReason
	}
	// dash runs a DASH download against a fake GVS whose head is gvsHead.
	// ageBy > 0 makes every failed request look like it came after a silence
	// that long, so handleHTTPError's 30 s status-check window (and the
	// MaxTimeout backstop) is reached without waiting it out. arm adjusts
	// the downloader before Start.
	dash := func(gvsHead int, decide func(seq, attempt int) int, opts DownloaderOptions, ageBy time.Duration, arm func(d *SegmentDownloader)) func(t *testing.T, out string) (*SegmentDownloader, error) {
		return func(t *testing.T, out string) (*SegmentDownloader, error) {
			var dRef atomic.Pointer[SegmentDownloader]
			serve := decide
			if ageBy > 0 {
				serve = func(seq, attempt int) int {
					st := decide(seq, attempt)
					if d := dRef.Load(); st != http.StatusOK && d != nil {
						d.lastSegTime.Store(time.Now().Add(-ageBy))
					}
					return st
				}
			}
			_, srv := newFakeGVS(t, gvsHead, serve)
			opts.BaseURL = srv.URL + "/videoplayback?id=exit.1&itag=140"
			opts.OutputFile = out
			d := NewSegmentDownloader(opts)
			d.delays = fastDelays()
			if arm != nil {
				arm(d)
			}
			dRef.Store(d)
			ctx, cancel := context.WithTimeout(t.Context(), safetyNet)
			defer cancel()
			return d, d.Start(ctx)
		}
	}
	ended := func(context.Context) (bool, error) { return true, nil }
	live := func(context.Context) (bool, error) { return false, nil }

	rows := []row{
		{
			name:    "DASH gone-path confirmed end",
			run:     dash(head, okThrough(head, http.StatusForbidden), DownloaderOptions{MaxTimeout: time.Minute, CheckStreamStatus: ended}, 0, nil),
			sidecar: "cleared",
			reason:  finishConfirmed,
		},
		{
			// The head says 5; only 0..2 are ever served.
			name:    "DASH gone-path behind-head finalize",
			run:     dash(head, okThrough(2, http.StatusForbidden), DownloaderOptions{MaxTimeout: fast(3 * time.Second), CheckStreamStatus: ended}, 0, nil),
			sidecar: "kept",
			reason:  finishBehindHead,
		},
		{
			name: "DASH gone-path Tier-2 interruption finalize",
			run: dash(head, okThrough(head, http.StatusForbidden), DownloaderOptions{
				MaxTimeout:          fast(time.Second),
				InterruptionTimeout: InterruptionNoStall,
				CheckStreamStatus:   func(context.Context) (bool, error) { return false, errors.New("unavailable") },
			}, 0, func(d *SegmentDownloader) { d.MayResume = func() bool { return true } }),
			sidecar: "kept",
			reason:  finishInterrupted,
		},
		{
			// R-G1: confirmed over, reached through handleHTTPError (a 5xx
			// past the head): the sidecar is KEPT.
			name:    "DASH handleHTTPError confirmed end (R-G1)",
			run:     dash(head, okThrough(head, http.StatusServiceUnavailable), DownloaderOptions{MaxTimeout: 2 * time.Hour, CheckStreamStatus: ended}, time.Hour, nil),
			sidecar: "kept",
			reason:  finishConfirmedViaHTTP,
		},
		{
			name:    "DASH handleHTTPError MaxTimeout backstop",
			run:     dash(head, okThrough(head, http.StatusServiceUnavailable), DownloaderOptions{MaxTimeout: time.Minute, CheckStreamStatus: live}, time.Hour, nil),
			sidecar: "kept",
			reason:  finishMaxTimeout,
		},
		{
			name:    "DASH first-segment hunt exhausted",
			run:     dash(0, func(int, int) int { return http.StatusForbidden }, DownloaderOptions{MaxTimeout: time.Minute, CheckStreamStatus: live}, 0, nil),
			sidecar: "none",
			reason:  finishNoFirstSegment,
		},
		{
			name:    "DASH EndSeq reached",
			run:     dash(head, okThrough(head, http.StatusForbidden), DownloaderOptions{MaxTimeout: time.Minute, EndSeq: 2, CheckStreamStatus: ended}, 0, nil),
			sidecar: "kept",
			reason:  finishEndSeq,
		},
		{
			name: "DASH cancelled",
			run: func(t *testing.T, out string) (*SegmentDownloader, error) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var past atomic.Int32
				_, srv := newFakeGVS(t, head, func(seq, attempt int) int {
					if seq <= head {
						return http.StatusOK
					}
					if past.Add(1) >= 3 {
						cancel()
					}
					return http.StatusServiceUnavailable
				})
				d := NewSegmentDownloader(DownloaderOptions{
					BaseURL:           srv.URL + "/videoplayback?id=exit.2&itag=140",
					OutputFile:        out,
					MaxTimeout:        time.Hour,
					CheckStreamStatus: live,
				})
				d.delays = fastDelays()
				return d, d.Start(ctx)
			},
			wantErr: true,
			sidecar: "kept",
			reason:  finishNone,
		},
		{
			name:    "HLS ENDLIST end",
			run:     hlsExitRun("#EXT-X-ENDLIST\n", true, nil),
			sidecar: "cleared",
			reason:  finishConfirmed,
		},
		{
			name:    "HLS VOD playlist consumed",
			run:     hlsExitRun("#EXT-X-ENDLIST\n", false, nil),
			sidecar: "cleared",
			reason:  finishConfirmed,
		},
		{
			name:    "HLS playlist failure exit",
			run:     hlsExitRun("", true, func(n int32) bool { return n > 1 }),
			wantErr: true,
			sidecar: "kept",
			reason:  finishNone,
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := filepath.Join(t.TempDir(), "v")
			d, err := tc.run(t, out)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Start() = %v, wantErr %v", err, tc.wantErr)
			}
			if got := d.finishedWith(); got != tc.reason {
				t.Errorf("finishedWith() = %d, want %d", got, tc.reason)
			}
			_, statErr := os.Stat(out + ".resume.json")
			exists := statErr == nil
			switch tc.sidecar {
			case "cleared":
				if exists {
					t.Error("resume sidecar survived a confirmed, complete end — want it cleared")
				}
				if !d.streamEnded() {
					t.Error("streamEnded = false on a confirmed, complete end")
				}
			case "kept":
				if !exists {
					t.Errorf("resume sidecar missing (stat err = %v) — this exit must keep it", statErr)
				}
				if d.streamEnded() {
					t.Error("streamEnded = true on an exit that keeps the sidecar")
				}
			case "none":
				if exists {
					t.Error("a sidecar was written for a run that wrote nothing")
				}
				if d.streamEnded() {
					t.Error("streamEnded = true on an exit that is not a confirmed end")
				}
			}
		})
	}
}

// TestFinishReasonClearsSidecar pins the one rule the reasons carry: only a
// confirmed, complete end (and an HLS sequence restart) clears the sidecar.
func TestFinishReasonClearsSidecar(t *testing.T) {
	clears := map[finishReason]bool{finishConfirmed: true, finishSeqRestart: true}
	for r := finishNone; r <= finishSeqRestart; r++ {
		if got := r.clearsSidecar(); got != clears[r] {
			t.Errorf("finishReason(%d).clearsSidecar() = %v, want %v", r, got, clears[r])
		}
	}
}

// TestDispatchMatchesWrappedSentinels pins dispatch's table, including a
// wrapped sentinel (which == used to turn into a fatal error).
func TestDispatchMatchesWrappedSentinels(t *testing.T) {
	other := errors.New("boom")
	tests := []struct {
		name      string
		herr      error
		wantCont  bool
		wantRet   error
		wantSkips int
	}{
		{"nil retries", nil, true, nil, 0},
		{"skip continues after afterSkip", errSegmentSkipped, true, nil, 1},
		{"wrapped skip continues after afterSkip", fmt.Errorf("x: %w", errSegmentSkipped), true, nil, 1},
		{"done exits cleanly", errStreamDone, false, nil, 0},
		{"wrapped done exits cleanly", fmt.Errorf("x: %w", errStreamDone), false, nil, 0},
		{"quality lost stops with it", ErrQualityLost, false, ErrQualityLost, 0},
		{"other stops with it", other, false, other, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			skips := 0
			cont, ret := dispatch(tc.herr, func() { skips++ })
			if cont != tc.wantCont || ret != tc.wantRet {
				t.Errorf("dispatch(%v) = (%v, %v), want (%v, %v)", tc.herr, cont, ret, tc.wantCont, tc.wantRet)
			}
			if skips != tc.wantSkips {
				t.Errorf("afterSkip ran %d times, want %d", skips, tc.wantSkips)
			}
		})
	}
}

// hlsExitRun serves a three-segment HLS playlist with tail appended (an
// ENDLIST, or nothing). stopOnGap keeps the run on the sequential live path
// even with an ENDLIST; without it an ENDLIST playlist is the VOD parallel
// path. failAfter, when non-nil, fails every playlist fetch whose 1-based
// number it answers true for, with a 500, and no status check is wired, so
// the run exits with the consecutive-failure error.
func hlsExitRun(tail string, stopOnGap bool, failAfter func(n int32) bool) func(t *testing.T, out string) (*SegmentDownloader, error) {
	return func(t *testing.T, out string) (*SegmentDownloader, error) {
		var fetches atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
			if failAfter != nil && failAfter(fetches.Add(1)) {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			var b strings.Builder
			b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
			for i := range 3 {
				fmt.Fprintf(&b, "#EXTINF:1.0,\nseg%d.ts\n", i)
			}
			b.WriteString(tail)
			fmt.Fprint(w, b.String())
		})
		mux.HandleFunc("/", echoSegment)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		d := NewSegmentDownloader(DownloaderOptions{
			BaseURL:    srv.URL + "/playlist.m3u8",
			OutputFile: out,
			StartSeq:   -1,
			IsHls:      true,
			StopOnGap:  stopOnGap,
		})
		d.delays = fastDelays()
		d.delays.hlsPlaylistRetry = time.Millisecond
		d.delays.hlsResumeSave = 0
		ctx, cancel := context.WithTimeout(t.Context(), safetyNet)
		defer cancel()
		return d, d.Start(ctx)
	}
}
