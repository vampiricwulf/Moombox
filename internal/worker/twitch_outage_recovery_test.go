package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// liveTwitchWindow serves a live media playlist that never ends (a sliding
// window of one-second segments) and a real one-second MPEG-TS for every
// segment, and signals the first segment request.
func liveTwitchWindow(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	ffmpegPath, _ := requireFFmpegTools(t)
	segPath := filepath.Join(t.TempDir(), "seg.ts")
	if out, err := exec.Command(ffmpegPath, "-nostdin", "-y", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration=1",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-f", "mpegts", segPath).CombinedOutput(); err != nil {
		t.Fatalf("make segment: %v\n%s", err, out)
	}
	ts, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	var reqs atomic.Int32
	first := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			head := int(time.Since(start).Seconds())
			lo := max(0, head-5)
			var b strings.Builder
			fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", lo)
			for s := lo; s <= head; s++ {
				fmt.Fprintf(&b, "#EXTINF:1.000,\n/seg/%d.ts\n", s)
			}
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(b.String()))
			return
		}
		if reqs.Add(1) == 1 {
			first <- struct{}{}
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(ts)
	}))
	t.Cleanup(srv.Close)
	return srv, first
}

// outageThenRecover runs ExecuteTwitch on a broadcast that stays live, cuts
// connectivity once segments are landing, restores it, and returns how the
// run ended.
func outageThenRecover(t *testing.T, h *endVerdictHarness, srv *httptest.Server, first <-chan struct{}) (*database.Job, error) {
	t.Helper()
	old := postOutageRetryDelay
	postOutageRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() { postOutageRetryDelay = old })

	h.variant.URL = srv.URL + "/live.m3u8"
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
	h.jobCtx.OutputDir = t.TempDir()
	h.jobCtx.Filename = "outage"

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil) }()
	select {
	case <-first:
	case <-time.After(20 * time.Second):
		t.Fatal("no segment fetched")
	}
	time.Sleep(1500 * time.Millisecond)
	h.conn.set(false)
	time.Sleep(300 * time.Millisecond)
	h.conn.set(true)
	select {
	case err := <-done:
		fresh, _ := h.db.GetJob(h.job.ID)
		return fresh, err
	case <-time.After(45 * time.Second):
		t.Fatal("ExecuteTwitch did not return")
		return nil, nil
	}
}

// Back online after an outage, with the SAME broadcast confirmed live, a
// failed master-playlist refresh used to finalize the job as Finished on the
// spot — mid-broadcast — and the monitor, seeing a finished job for that
// stream, never re-archived the rest. It is retried, then re-verified like the
// in-loop refresh: a broadcast still live leaves the job in Error with its
// staging, never Finished.
//
// Mutant: the recovery finalizing on the first failed refresh again, or not
// retrying it.
func TestPostOutageRefreshFailureDoesNotFinishALiveBroadcast(t *testing.T) {
	srv, first := liveTwitchWindow(t)
	h := newEndVerdictHarness(t, "tw_outage_refresh")
	var refreshes atomic.Int32
	h.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		refreshes.Add(1)
		return nil, errors.New("usher 503")
	}
	fresh, err := outageThenRecover(t, h, srv, first)
	if err == nil || fresh.Status == database.StatusFinished {
		t.Errorf("a still-live broadcast ended as err=%v status=%s, want an error and no Finished", err, fresh.Status)
	}
	if n := refreshes.Load(); n < postOutageRefreshAttempts {
		t.Errorf("the post-outage refresh was tried %d times, want %d", n, postOutageRefreshAttempts)
	}
}

// A post-outage recheck that never gets an answer is an UNKNOWN verdict; it
// used to read as "not live" and finalize the job Finished.
//
// Mutant: recheckTwitchBroadcast returning no error when every attempt failed,
// or the caller ignoring it.
func TestPostOutageRecheckFailureDoesNotFinishTheJob(t *testing.T) {
	srv, first := liveTwitchWindow(t)
	h := newEndVerdictHarness(t, "tw_outage_recheck")
	h.variant.RecheckStreamFn = func(context.Context) (*twitch.TwitchStreamInfo, error) {
		return nil, errors.New("gql unreachable")
	}
	fresh, err := outageThenRecover(t, h, srv, first)
	if err == nil || fresh.Status == database.StatusFinished {
		t.Errorf("an unanswered recheck ended as err=%v status=%s, want an error and no Finished", err, fresh.Status)
	}
}
