package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hlsFailMode is how the playlist endpoint fails, which selects WHICH of
// runHlsLoop's three end-verdict sites the loop reaches.
type hlsFailMode int

const (
	// fail404 reaches site A (the 404/410 branch), consulted on the FIRST
	// failure.
	fail404 hlsFailMode = iota
	// fail500 skips the 404 branch entirely, so the 6th consecutive FETCH
	// failure escalates to site B.
	fail500
	// failGarbage answers 200 OK with a body ParseHls rejects (its first
	// line is not #EXTM3U) — the CDN-error-page shape the parse branch was
	// written for — so the 6th consecutive PARSE failure escalates to site C.
	failGarbage
)

// hlsFailServer serves the chosen failure for every playlist request and
// counts the requests. No segment route is needed: none of the three sites
// under test is reachable after a segment has been fetched.
func hlsFailServer(t *testing.T, mode hlsFailMode) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		switch mode {
		case fail404:
			w.WriteHeader(http.StatusNotFound)
		case fail500:
			w.WriteHeader(http.StatusInternalServerError)
		case failGarbage:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><body>origin error</body></html>"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &fetches
}

// newEndVerdictDownloader wires a failing playlist to a scripted status
// check. check is called with the 1-based call number so a row can script
// "error first, answer second".
func newEndVerdictDownloader(t *testing.T, url string, warns *warnCollector, check func(call int) (bool, error)) (*SegmentDownloader, *atomic.Int32) {
	t.Helper()
	var checks atomic.Int32
	d := NewSegmentDownloader(DownloaderOptions{
		BaseURL:    url,
		OutputFile: filepath.Join(t.TempDir(), "video.ts"),
		StartSeq:   -1,
		IsHls:      true,
		Logger:     warns,
		CheckStreamStatus: func(context.Context) (bool, error) {
			return check(int(checks.Add(1)))
		},
	})
	d.delays = fastDelays()
	// Arc 2 seam: 5 s ÷ fastScale is still 250 ms per retry round, and the
	// escalation rows burn six of them.
	d.delays.hlsPlaylistRetry = 10 * time.Millisecond
	return d, &checks
}

// TestHlsEndVerdictSites is the O4 symmetry table: runHlsLoop asks
// CheckStreamStatus at three sites — the playlist 404/410 branch (A), the
// consecutive-FETCH-failure escalation (B) and the consecutive-PARSE-failure
// escalation (C) — and the SAME answer must mean the SAME thing at all three.
// A confirmed "ended" finalizes cleanly (nil, streamEnded true, so
// runHlsLoop's defer clears the resume sidecar); a confirmed "still live"
// hands the orchestrator ErrQualityLost for its variant refresh; a failed
// check is not a verdict at all.
//
// It absorbs the three site-A tests that used to live in this file
// (TestHlsEndVerdict_CheckErrorDoesNotFinalize,
// _StillLiveAfterFailedCheckRefreshes, _ConfirmedEndStillFinalizes) — every
// assertion they carried survives as a row field below.
func TestHlsEndVerdictSites(t *testing.T) {
	// alwaysEnded / alwaysLive / alwaysErr are the three scripted verdicts.
	alwaysEnded := func(int) (bool, error) { return true, nil }
	alwaysLive := func(int) (bool, error) { return false, nil }
	alwaysErr := func(int) (bool, error) { return false, errors.New("gql flap") }
	// errThenLive pins the recovery the T1-2 fix exists for: the first check
	// fails, the loop retries, the second answers "still live".
	errThenLive := func(call int) (bool, error) {
		if call == 1 {
			return false, errors.New("gql flap")
		}
		return false, nil
	}

	tests := []struct {
		name  string
		mode  hlsFailMode
		check func(call int) (bool, error)
		// wantErr: Start() must return non-nil. wantErrContains is an
		// optional substring of that error ("" skips the check).
		wantErr         bool
		wantErrContains string
		wantQualityLost bool
		wantEnded       bool
		// wantChecks/wantFetches are exact when > 0; minChecks/minFetches are
		// floors used where the retry budget makes the exact count brittle.
		wantChecks, wantFetches int
		minChecks, minFetches   int
		wantWarnContains        string
		// mutant names the change this row alone catches.
		mutant string
	}{
		{
			name: "A/404 confirmed ended finalizes on the first check",
			mode: fail404, check: alwaysEnded,
			wantEnded: true, wantChecks: 1, wantFetches: 1,
			mutant: "routing every 404 through the retry budget regardless of the verdict",
		},
		{
			name: "A/404 still live refreshes the variant",
			mode: fail404, check: errThenLive,
			wantErr: true, wantQualityLost: true,
			minChecks: 2,
			mutant:    "a fix that defers the verdict but never re-consults (Start never reaches ErrQualityLost)",
		},
		{
			name: "A/404 failed check is not a verdict",
			mode: fail404, check: alwaysErr,
			wantErr: true, wantErrContains: "consecutive errors",
			minChecks: 2, minFetches: 2,
			wantWarnContains: "stream status check failed; deferring end verdict",
			mutant:           "restoring `d.streamEnded.Store(true); return nil` under a non-nil checkErr (T1-2)",
		},
		{
			name: "B/fetch-escalation confirmed ended finalizes cleanly",
			mode: fail500, check: alwaysEnded,
			wantEnded: true, wantChecks: 1, wantFetches: 6,
			mutant: "deleting `case verdictEnded:` at site B — Start returns the consecutive-error failure with streamEnded false, so the deferred ClearResume never runs and the sidecar outlives the recording (O4)",
		},
		{
			name: "B/fetch-escalation still live refreshes the variant",
			mode: fail500, check: alwaysLive,
			wantErr: true, wantQualityLost: true,
			wantChecks: 1, wantFetches: 6,
			mutant: "a `default:` latch at site B that finalizes on ANY answered check, including a confirmed-live one",
		},
		{
			name: "B/fetch-escalation failed check keeps the fetch failure",
			mode: fail500, check: alwaysErr,
			wantErr: true, wantErrContains: "HLS playlist fetch failed after 6 consecutive errors",
			wantChecks: 1, wantFetches: 6,
			wantWarnContains: "stream status check failed; deferring end verdict",
			mutant:           "treating verdictUnknown as ended at site B — Start returns nil and clears the sidecar a later Resume needs",
		},
		{
			name: "C/parse-escalation confirmed ended finalizes cleanly",
			mode: failGarbage, check: alwaysEnded,
			wantEnded: true, wantChecks: 1, wantFetches: 6,
			mutant: "deleting `case verdictEnded:` at site C — same sidecar/error asymmetry as site B (O4)",
		},
		{
			name: "C/parse-escalation still live refreshes the variant",
			mode: failGarbage, check: alwaysLive,
			wantErr: true, wantQualityLost: true,
			wantChecks: 1, wantFetches: 6,
			mutant: "a `default:` latch at site C that finalizes on a confirmed-live check",
		},
		{
			name: "C/parse-escalation failed check keeps the parse failure",
			mode: failGarbage, check: alwaysErr,
			wantErr: true, wantErrContains: "failed to parse HLS playlist after 6 consecutive errors",
			wantChecks: 1, wantFetches: 6,
			wantWarnContains: "stream status check failed; deferring end verdict",
			mutant:           "treating verdictUnknown as ended at site C",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, fetches := hlsFailServer(t, tc.mode)
			warns := &warnCollector{}
			d, checks := newEndVerdictDownloader(t, srv.URL+"/playlist.m3u8", warns, tc.check)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			err := d.Start(ctx)

			if tc.wantErr && err == nil {
				t.Fatalf("Start() = nil, want an error — mutant: %s", tc.mutant)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Start() = %v, want nil (a confirmed end finalizes) — mutant: %s", err, tc.mutant)
			}
			if tc.wantQualityLost && !errors.Is(err, ErrQualityLost) {
				t.Fatalf("Start() = %v, want ErrQualityLost — mutant: %s", err, tc.mutant)
			}
			// An UNKNOWN verdict must never claim the stream is still live:
			// ErrQualityLost would send the orchestrator into a variant
			// refresh on no evidence at all.
			if !tc.wantQualityLost && errors.Is(err, ErrQualityLost) {
				t.Fatalf("Start() = ErrQualityLost on a non-live verdict; got %v", err)
			}
			if tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains) {
				t.Errorf("Start() = %v, want an error containing %q — mutant: %s", err, tc.wantErrContains, tc.mutant)
			}
			if got := d.streamEnded.Load(); got != tc.wantEnded {
				t.Errorf("streamEnded = %v, want %v — when true the deferred ClearResume removes the sidecar; when false it must survive for a later Resume. Mutant: %s",
					got, tc.wantEnded, tc.mutant)
			}
			if tc.wantChecks > 0 {
				if got := int(checks.Load()); got != tc.wantChecks {
					t.Errorf("CheckStreamStatus called %d times, want exactly %d — a verdict must not cost an extra consult round", got, tc.wantChecks)
				}
			}
			if tc.minChecks > 0 {
				if got := int(checks.Load()); got < tc.minChecks {
					t.Errorf("CheckStreamStatus called %d times, want >= %d — the loop must RE-ASK after a failed check, not latch the first answer", got, tc.minChecks)
				}
			}
			if tc.wantFetches > 0 {
				if got := int(fetches.Load()); got != tc.wantFetches {
					t.Errorf("playlist fetched %d times, want exactly %d", got, tc.wantFetches)
				}
			}
			if tc.minFetches > 0 {
				if got := int(fetches.Load()); got < tc.minFetches {
					t.Errorf("playlist fetched %d times, want >= %d — the failure must fall into the consecutive-error retry budget", got, tc.minFetches)
				}
			}
			j := warns.joined()
			if tc.wantWarnContains != "" && !strings.Contains(j, tc.wantWarnContains) {
				t.Errorf("warn wording = %q, want the DASH loop's %q", j, tc.wantWarnContains)
			}
			// "assuming ended" described a finalize these paths never
			// performed; Arc 2 removed it and it must not come back.
			if strings.Contains(j, "assuming ended") {
				t.Errorf("warn wording = %q, want no \"assuming ended\"", j)
			}
		})
	}
}
