package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// ExecuteTwitch wrote Muxing as its session loop ended, ahead of the
// shutdown/user-cancel exit that follows. An operator's Cancel of a live
// capture was written over with Muxing every time — the row read Muxing
// through the part muxes and the chat's shutdown grace until
// handleCancellation wrote Cancelled again — and a shutdown mid-capture left
// the row Muxing, which handleCancellation keeps: at boot muxOnRestart saw
// Muxing with staged media and muxed the partial capture ("Download
// complete") instead of resuming the broadcast. The write is now made only on
// the way to a finalize, and only on a row with no outcome.

// liveCaptureServer is a live Twitch playlist that never ends, two 188-byte
// TS packets ×20 per segment, and the number of segments it has served.
func liveCaptureServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var segs, seq atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			n := seq.Add(1)
			fmt.Fprintf(w, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n"+
				"#EXTINF:1.0,\nseg%d.ts\n#EXTINF:1.0,\nseg%d.ts\n", n, n, n+1)
			return
		}
		segs.Add(1)
		_, _ = w.Write(bytes.Repeat([]byte{0x47}, 188*20))
	}))
	t.Cleanup(srv.Close)
	return srv, &segs
}

// An operator's Cancel of a live Twitch capture leaves the row Cancelled
// from then on, and the run ends as a cancelled one (ErrCancelled, with the
// context's error). A shutdown mid-capture leaves the row Downloading, which
// the restart processes again — resuming the broadcast — rather than muxing
// what is staged.
//
// Mutants: the Muxing write back ahead of the exit, unguarded (the cancel
// row reads Muxing after Cancelled; the shutdown row is Muxing and
// muxOnRestart says mux); the guard dropped, the write conditional (the
// shutdown row is Muxing); the user-cancel exit returning ctx.Err() alone
// (no ErrCancelled).
func TestTwitchLiveCaptureStopNeverWritesMuxing(t *testing.T) {
	for _, mode := range []string{"user cancel", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			srv, segs := liveCaptureServer(t)
			h := newEndVerdictHarness(t, "tw_stop_"+strings.ReplaceAll(mode, " ", "_"))
			h.variant.URL = srv.URL + "/live.m3u8"
			var stagingBase string
			h.w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
			h.jobCtx.StagingDir = filepath.Join(stagingBase, h.job.ID)
			h.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
			statuses := h.watchStatuses()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				defer func() {
					if r := recover(); r != nil {
						done <- fmt.Errorf("panic: %v", r)
					}
				}()
				done <- h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)
			}()
			deadline := time.Now().Add(10 * time.Second)
			for segs.Load() < 2 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if segs.Load() < 2 {
				t.Fatal("the capture never fetched a segment")
			}
			if mode == "user cancel" {
				// CancelJob's row write; the run's listener cancels the session.
				h.db.UpdateJobFieldsUnlessTerminal(h.job.ID, map[string]any{"status": database.StatusCancelled})
			} else {
				cancel() // the worker's ctx: Moombox is stopping
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("ExecuteTwitch did not return")
			}

			seq := statuses()
			row, _ := h.db.GetJob(h.job.ID)
			if row == nil {
				t.Fatal("row vanished")
			}
			if mode == "user cancel" {
				if !errors.Is(err, ErrCancelled) || !errors.Is(err, context.Canceled) {
					t.Errorf("a cancelled capture returned %v, want ErrCancelled with the context's error", err)
				}
				for i, s := range seq {
					if s == database.StatusCancelled {
						if rest := seq[i+1:]; len(rest) != 0 {
							t.Errorf("after the Cancel the row was written %v (statuses %v)", rest, seq)
						}
						break
					}
				}
				if row.Status != database.StatusCancelled {
					t.Errorf("row = %q, want Cancelled", row.Status)
				}
				return
			}
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCancelled) {
				t.Errorf("a shutdown returned %v, want the context's error alone", err)
			}
			if row.Status != database.StatusDownloading || !ShouldProcess(row) || muxOnRestart(row, stagingBase) {
				t.Errorf("after a shutdown mid-capture the row is %q (statuses %v), ShouldProcess %v, muxOnRestart %v; "+
					"want Downloading, processed again to resume the broadcast",
					row.Status, seq, ShouldProcess(row), muxOnRestart(row, stagingBase))
			}
		})
	}
}

// A Cancel whose row write lands just before a clean end, with this run's
// listener held back behind a slower subscriber — so the session is not
// cancelled and the loop ends as a finalize — is not written over with
// Muxing: the write is refused, the run takes the cancel exit with staging
// kept, and nothing is muxed or announced.
//
// Mutants: the write unconditional again behind the guard (UpdateJobFields:
// Muxing lands after Cancelled); the refusal ignored (the run goes on to the
// finalize, which announces the mux).
func TestTwitchCancelBeforeTheMuxingWriteStands(t *testing.T) {
	h := newEndVerdictHarness(t, "tw_cancel_at_end")
	rec := notificationtest.New()
	h.o.notifier = rec

	cancelEntered := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	// Registered before ExecuteTwitch's own listener, so it runs first and
	// holds the Cancel's notification chain.
	unsub := h.db.OnJobUpdate(func(j *database.Job) {
		if j.ID != h.job.ID || j.Status != database.StatusCancelled {
			return
		}
		first := false
		once.Do(func() { first = true; close(cancelEntered) })
		if first {
			select {
			case <-released:
			case <-time.After(10 * time.Second):
			}
		}
	})
	defer unsub()

	cancelDone := make(chan struct{})
	var fired atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The operator clicks Cancel as the broadcast ends: the Cancel's row
		// write lands before the playlist that ends it is served.
		if fired.CompareAndSwap(false, true) {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("panic in the Cancel write: %v", r)
					}
				}()
				defer close(cancelDone)
				h.db.UpdateJobFieldsUnlessTerminal(h.job.ID, map[string]any{"status": database.StatusCancelled})
			}()
			<-cancelEntered
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n" +
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()
	h.variant.URL = srv.URL + "/live.m3u8"
	h.variant.CheckStreamFn = func(context.Context) (bool, error) { return false, nil }
	h.variant.RecheckStreamFn = func(context.Context) (*twitch.TwitchStreamInfo, error) {
		return &twitch.TwitchStreamInfo{IsLive: false}, nil
	}
	statuses := h.watchStatuses()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := h.o.ExecuteTwitch(ctx, h.jobCtx, h.variant, false, nil)
	close(released)
	<-cancelDone

	seq := statuses()
	if !errors.Is(err, ErrCancelled) {
		t.Errorf("returned %v, want ErrCancelled", err)
	}
	for _, s := range seq {
		if s == database.StatusMuxing {
			t.Errorf("the Cancelled row was written Muxing (statuses %v)", seq)
			break
		}
	}
	if row, _ := h.db.GetJob(h.job.ID); row == nil || row.Status != database.StatusCancelled {
		t.Errorf("row = %+v, want Cancelled", row)
	}
	if got := rec.ByEvent("muxing"); len(got) != 0 {
		t.Errorf("%q was sent for a capture the operator had cancelled", got[0].Title)
	}
}
