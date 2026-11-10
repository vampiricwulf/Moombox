package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The three HLS end-verdict sites TestHlsEndVerdictSites does not reach —
// the stuck-skip exit, init exhaustion and the stale window — each pinned
// for all four verdicts. Every site keeps its own mapping (the endVerdict
// doc lists them); these rows pin that mapping so routing the sites through
// consultEnd cannot move one.

// endVerdictScript returns a scripted status check that answers `first` on
// call 1 and "ended" on every later call, so a site that continues past its
// first consult still terminates on the next one. first is one of
// "ended", "live", "err".
func endVerdictScript(first string) func(call int) (bool, error) {
	return func(call int) (bool, error) {
		if call > 1 {
			return true, nil
		}
		switch first {
		case "ended":
			return true, nil
		case "live":
			return false, nil
		default:
			return false, errors.New("gql flap")
		}
	}
}

// wireCheck sets fn (counting calls) as the downloader's status check, or
// leaves it unwired for a nil fn.
func wireCheck(fn func(call int) (bool, error), calls *atomic.Int32) func(context.Context) (bool, error) {
	if fn == nil {
		return nil
	}
	return func(context.Context) (bool, error) {
		return fn(int(calls.Add(1)))
	}
}

// TestHlsEndVerdict_StuckSkipExit pins the stuck-skip exit: after three
// consecutive skipped segments the loop asks the status check once per
// further skip. Ended finalizes there (three gaps); live, a failed check and
// no wired check all continue, so the fourth dead segment is skipped too and
// the ENDLIST round ends the run. The file stays empty throughout (StopOnGap
// with nothing written takes the skip branch).
func TestHlsEndVerdict_StuckSkipExit(t *testing.T) {
	tests := []struct {
		name       string
		check      func(call int) (bool, error)
		wantChecks int32
		wantGaps   int
		wantWarn   bool
	}{
		{name: "ended finalizes on the third skip", check: endVerdictScript("ended"), wantChecks: 1, wantGaps: 3},
		{name: "live continues", check: endVerdictScript("live"), wantChecks: 2, wantGaps: 4},
		{name: "failed check continues and warns", check: endVerdictScript("err"), wantChecks: 2, wantGaps: 4, wantWarn: true},
		{name: "no check wired continues", check: nil, wantChecks: 0, wantGaps: 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
				var b strings.Builder
				b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
				for i := range 4 {
					fmt.Fprintf(&b, "#EXTINF:1.0,\ndead%d.ts\n", i)
				}
				b.WriteString("#EXT-X-ENDLIST\n")
				fmt.Fprint(w, b.String())
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			var calls atomic.Int32
			warns := &warnCollector{}
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL:           srv.URL + "/playlist.m3u8",
				OutputFile:        filepath.Join(t.TempDir(), "video.ts"),
				StartSeq:          -1,
				IsHls:             true,
				StopOnGap:         true,
				MaxRetries:        1,
				Logger:            warns,
				CheckStreamStatus: wireCheck(tc.check, &calls),
			})
			d.delays = fastDelays()
			d.delays.hlsStuckRetry = time.Millisecond
			var gaps atomic.Int32
			d.OnGap = func(DownloadGap) { gaps.Add(1) }

			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			if err := d.Start(ctx); err != nil {
				t.Fatalf("Start() = %v, want nil", err)
			}
			if !d.streamEnded.Load() {
				t.Error("streamEnded = false, want true (stuck-skip end or ENDLIST)")
			}
			if got := calls.Load(); got != tc.wantChecks {
				t.Errorf("status checks = %d, want %d", got, tc.wantChecks)
			}
			if got := int(gaps.Load()); got != tc.wantGaps {
				t.Errorf("gaps = %d, want %d", got, tc.wantGaps)
			}
			hasWarn := strings.Contains(warns.joined(), "stream status check failed")
			if hasWarn != tc.wantWarn {
				t.Errorf("status-check warn logged = %v, want %v (warns: %q)", hasWarn, tc.wantWarn, warns.joined())
			}
		})
	}
}

// TestHlsEndVerdict_InitExhaustion pins init exhaustion: an fMP4 playlist
// whose #EXT-X-MAP never fetches exhausts MaxSegmentRetries rounds and asks
// the status check once. Ended finalizes cleanly; live AND a failed check
// both hand back ErrQualityLost (a variant refresh mints a fresh init URI —
// a terminal error would end a live recording over a rotten token); no wired
// check is the one terminal error.
func TestHlsEndVerdict_InitExhaustion(t *testing.T) {
	tests := []struct {
		name            string
		check           func(call int) (bool, error)
		wantEnded       bool
		wantQualityLost bool
		wantErrContains string
		wantChecks      int32
		wantWarn        bool
	}{
		{name: "ended finalizes", check: endVerdictScript("ended"), wantEnded: true, wantChecks: 1},
		{name: "live refreshes the variant", check: endVerdictScript("live"), wantQualityLost: true, wantChecks: 1},
		{name: "failed check refreshes the variant and warns", check: endVerdictScript("err"), wantQualityLost: true, wantChecks: 1, wantWarn: true},
		{name: "no check wired is terminal", check: nil, wantErrContains: "HLS init segment fetch failed after 5 rounds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
				var b strings.Builder
				b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
				b.WriteString(`#EXT-X-MAP:URI="init0.mp4"` + "\n")
				b.WriteString("#EXTINF:1.000,live\nseg0.mp4\n")
				fmt.Fprint(w, b.String())
			})
			mux.HandleFunc("/init0.mp4", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			})
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "[%s]", strings.TrimPrefix(r.URL.Path, "/"))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			var calls atomic.Int32
			warns := &warnCollector{}
			d := NewSegmentDownloader(DownloaderOptions{
				BaseURL:           srv.URL + "/playlist.m3u8",
				OutputFile:        filepath.Join(t.TempDir(), "video_stream"),
				StartSeq:          -1,
				IsHls:             true,
				StopOnGap:         true,
				MaxRetries:        1,
				Logger:            warns,
				CheckStreamStatus: wireCheck(tc.check, &calls),
			})
			d.delays = fastDelays()
			d.delays.hlsStuckRetry = time.Millisecond

			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			err := d.Start(ctx)
			switch {
			case tc.wantEnded:
				if err != nil {
					t.Fatalf("Start() = %v, want nil", err)
				}
			case tc.wantQualityLost:
				if !errors.Is(err, ErrQualityLost) {
					t.Fatalf("Start() = %v, want ErrQualityLost", err)
				}
			default:
				if err == nil || errors.Is(err, ErrQualityLost) || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("Start() = %v, want a terminal error containing %q", err, tc.wantErrContains)
				}
			}
			if got := d.streamEnded.Load(); got != tc.wantEnded {
				t.Errorf("streamEnded = %v, want %v", got, tc.wantEnded)
			}
			if got := calls.Load(); got != tc.wantChecks {
				t.Errorf("status checks = %d, want %d", got, tc.wantChecks)
			}
			hasWarn := strings.Contains(warns.joined(), "stream status check failed")
			if hasWarn != tc.wantWarn {
				t.Errorf("status-check warn logged = %v, want %v (warns: %q)", hasWarn, tc.wantWarn, warns.joined())
			}
		})
	}
}

// TestHlsEndVerdict_StaleWindow pins the stale window: after five refreshes
// with no new segment the loop asks CheckStaleEnd (or CheckStreamStatus when
// none is wired). Ended finalizes; live and a failed check both fall through
// to the seq-regression check and wait another full window (the second ask
// answers ended here); with nothing wired the window only resets, so the run
// goes on until cancelled.
func TestHlsEndVerdict_StaleWindow(t *testing.T) {
	tests := []struct {
		name string
		// stale selects CheckStaleEnd as the wired func instead of
		// CheckStreamStatus.
		stale      bool
		check      func(call int) (bool, error)
		wantEnded  bool
		wantChecks int32
		wantWarn   bool
	}{
		{name: "ended finalizes", check: endVerdictScript("ended"), wantEnded: true, wantChecks: 1},
		{name: "live waits another window", check: endVerdictScript("live"), wantEnded: true, wantChecks: 2},
		{name: "failed check waits another window and warns", check: endVerdictScript("err"), wantEnded: true, wantChecks: 2, wantWarn: true},
		{name: "CheckStaleEnd ended finalizes", stale: true, check: endVerdictScript("ended"), wantEnded: true, wantChecks: 1},
		{name: "CheckStaleEnd failed check waits and warns", stale: true, check: endVerdictScript("err"), wantEnded: true, wantChecks: 2, wantWarn: true},
		{name: "no check wired never ends", check: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			var fetches atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/playlist.m3u8", func(w http.ResponseWriter, _ *http.Request) {
				// Six full stale windows without an end means none is coming.
				if fetches.Add(1) > 31 {
					cancel()
				}
				fmt.Fprint(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:1.0,\nseg0.ts\n")
			})
			mux.HandleFunc("/", echoSegment)
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			var calls atomic.Int32
			warns := &warnCollector{}
			opts := DownloaderOptions{
				BaseURL:    srv.URL + "/playlist.m3u8",
				OutputFile: filepath.Join(t.TempDir(), "video.ts"),
				StartSeq:   -1,
				IsHls:      true,
				Logger:     warns,
			}
			if tc.stale {
				opts.CheckStaleEnd = wireCheck(tc.check, &calls)
			} else {
				opts.CheckStreamStatus = wireCheck(tc.check, &calls)
			}
			d := NewSegmentDownloader(opts)
			d.delays = fastDelays()
			d.delays.hlsReloadUnit = time.Millisecond

			err := d.Start(ctx)
			if tc.wantEnded {
				if err != nil {
					t.Fatalf("Start() = %v, want nil", err)
				}
			} else if err == nil {
				t.Fatalf("Start() = nil, want the cancel error (nothing can end a stale window)")
			}
			if got := d.streamEnded.Load(); got != tc.wantEnded {
				t.Errorf("streamEnded = %v, want %v", got, tc.wantEnded)
			}
			if got := calls.Load(); got != tc.wantChecks {
				t.Errorf("status checks = %d, want %d", got, tc.wantChecks)
			}
			hasWarn := strings.Contains(warns.joined(), "stream status check failed")
			if hasWarn != tc.wantWarn {
				t.Errorf("status-check warn logged = %v, want %v (warns: %q)", hasWarn, tc.wantWarn, warns.joined())
			}
		})
	}
}
