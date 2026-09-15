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

// hls404Server serves 404 for every playlist request and counts them. The
// live loop's 404/410 branch is the only exit under test, so no segment
// route is needed.
func hls404Server(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv, &fetches
}

// newEndVerdictDownloader wires a 404 playlist to a scripted status check.
// check is called with the 1-based call number so a test can script "error
// first, answer second".
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
	d.delays.hlsPlaylistRetry = 10 * time.Millisecond
	return d, &checks
}

// TestHlsEndVerdict_CheckErrorDoesNotFinalize is the T1-2 regression guard.
// A playlist 404 whose status check ERRORS must not be read as "the stream
// ended": the loop must keep asking and finally exit with the consecutive-
// error failure, leaving streamEnded false so the resume sidecar survives
// (runHlsLoop's defer clears it only when streamEnded is set).
//
// Mutant this kills: restoring `d.streamEnded.Store(true); return nil` under
// a non-nil checkErr — Start() then returns nil with streamEnded true, and
// both assertions below fail. A weaker test that only checked "err != nil"
// would also pass on a loop that returned ErrQualityLost, which is a
// DIFFERENT (and wrong) verdict for an unknown status, so the ErrQualityLost
// assertion is here too.
func TestHlsEndVerdict_CheckErrorDoesNotFinalize(t *testing.T) {
	srv, fetches := hls404Server(t)
	warns := &warnCollector{}
	d, checks := newEndVerdictDownloader(t, srv.URL+"/playlist.m3u8", warns,
		func(int) (bool, error) { return false, errors.New("gql flap") })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)

	if err == nil {
		t.Fatal("Start() = nil — a failed status check finalized the recording as a clean end (T1-2)")
	}
	if errors.Is(err, ErrQualityLost) {
		t.Fatalf("Start() = ErrQualityLost — an UNKNOWN status must not claim the stream is still live; got %v", err)
	}
	if !strings.Contains(err.Error(), "consecutive errors") {
		t.Errorf("Start() = %v, want the consecutive-error exit", err)
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded set on an unknown verdict — the deferred ClearResume would delete the sidecar the tail needs")
	}
	if got := checks.Load(); got < 2 {
		t.Errorf("CheckStreamStatus called %d times, want >= 2 — the loop must RE-ASK after a failed check, not latch the first answer", got)
	}
	if got := fetches.Load(); got < 2 {
		t.Errorf("playlist fetched %d times, want >= 2 — the 404 must fall into the consecutive-error retry budget", got)
	}
	if j := warns.joined(); !strings.Contains(j, "deferring end verdict") || strings.Contains(j, "assuming ended") {
		t.Errorf("warn wording = %q, want the DASH loop's \"stream status check failed; deferring end verdict\" and no \"assuming ended\"", j)
	}
}

// TestHlsEndVerdict_StillLiveAfterFailedCheckRefreshes pins the recovery the
// fix exists for: the first check fails, the loop retries, the second check
// answers "not ended", and the loop hands the orchestrator ErrQualityLost so
// the variant is refreshed and the SAME job keeps recording
// (orchestrator_twitch.go:511-524, orchestrator_youtube.go:247).
//
// Mutant this kills: a fix that defers the verdict but never re-consults —
// e.g. falling through to `return fmt.Errorf(...)` immediately instead of
// into the retry budget. Start() then never returns ErrQualityLost.
func TestHlsEndVerdict_StillLiveAfterFailedCheckRefreshes(t *testing.T) {
	srv, _ := hls404Server(t)
	warns := &warnCollector{}
	d, _ := newEndVerdictDownloader(t, srv.URL+"/playlist.m3u8", warns, func(call int) (bool, error) {
		if call == 1 {
			return false, errors.New("gql flap")
		}
		return false, nil // still live
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	err := d.Start(ctx)

	if !errors.Is(err, ErrQualityLost) {
		t.Fatalf("Start() = %v, want ErrQualityLost once the status check answers \"still live\"", err)
	}
	if d.streamEnded.Load() {
		t.Error("streamEnded set on a still-live verdict")
	}
}

// TestHlsEndVerdict_ConfirmedEndStillFinalizes pins the untouched half: a
// CONFIRMED "ended" must still finalize immediately and cleanly, on the very
// first check — no extra retry round, no error.
//
// Mutant this kills: a fix that routes every 404 through the retry budget
// regardless of the verdict (checks would be > 1 and Start() would return an
// error instead of nil).
func TestHlsEndVerdict_ConfirmedEndStillFinalizes(t *testing.T) {
	srv, fetches := hls404Server(t)
	warns := &warnCollector{}
	d, checks := newEndVerdictDownloader(t, srv.URL+"/playlist.m3u8", warns,
		func(int) (bool, error) { return true, nil })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := d.Start(ctx); err != nil {
		t.Fatalf("Start() = %v, want nil (a confirmed end finalizes)", err)
	}
	if !d.streamEnded.Load() {
		t.Error("streamEnded not set after a confirmed end — the sidecar would be left behind")
	}
	if got := checks.Load(); got != 1 {
		t.Errorf("CheckStreamStatus called %d times, want exactly 1 — a confirmed end must not pay a retry round", got)
	}
	if got := fetches.Load(); got != 1 {
		t.Errorf("playlist fetched %d times, want exactly 1", got)
	}
}
