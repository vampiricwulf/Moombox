package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestEndUnconfirmedMarkKeepsTheMessage: the marker rides beside the latch's
// cause without changing a word of what the row, the embed and the log say,
// and setJobError's classification turns it — and only it — into the Error
// row's park_reason.
//
// Mutants: endUnconfirmedError.Error() adding to the cause's text;
// parkReasonForError ignoring ErrTwitchEndUnconfirmed (the latched row carries
// no marker); parkReasonForError marking every Error (a plain failure is
// handed to the automatic mux).
func TestEndUnconfirmedMarkKeepsTheMessage(t *testing.T) {
	cause := fmt.Errorf("HLS playlist fetch failed after 6 consecutive errors: %w", engine.ErrQualityLost)
	marked := markEndUnconfirmed(cause)
	if marked.Error() != cause.Error() {
		t.Errorf("marked error reads %q, want the cause's %q", marked.Error(), cause.Error())
	}
	if !errors.Is(marked, engine.ErrQualityLost) || !errors.Is(marked, ErrTwitchEndUnconfirmed) {
		t.Errorf("marked error must still be its cause AND carry the marker: %v", marked)
	}
	if got := parkReasonForError(marked); got != database.ParkReasonTwitchEndUnconfirmed {
		t.Errorf("parkReasonForError(marked) = %q, want %q", got, database.ParkReasonTwitchEndUnconfirmed)
	}
	if got := parkReasonForError(cause); got != database.ParkReasonNone {
		t.Errorf("parkReasonForError(unmarked) = %q, want none", got)
	}
}

// TestOnlyTheLiveLatchIsMarked drives the real ExecuteTwitch onto its three
// Error exits. The LIVE unconfirmed-end latch marks its error; a VOD's latch
// (no live end to confirm) and a failed part advance (a local I/O failure,
// which says nothing about the broadcast) do not — the automatic mux is for a
// broadcast whose end was merely unknown.
//
// Mutants: latchIfUnconfirmed's live arm leaving the cause unmarked (the live
// row is never auto-muxed); its VOD arm marking (a truncated VOD would be
// muxed as soon as its channel went offline); latchPartFailure marking.
func TestOnlyTheLiveLatchIsMarked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	live := newEndVerdictHarness(t, "tw_mark_live")
	live.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
	if err := live.o.ExecuteTwitch(ctx, live.jobCtx, live.variant, false, nil); !errors.Is(err, ErrTwitchEndUnconfirmed) {
		t.Errorf("live latch returned %v, want it marked ErrTwitchEndUnconfirmed", err)
	}

	vod := newEndVerdictHarness(t, "tw_mark_vod")
	if err := vod.o.ExecuteTwitch(ctx, vod.jobCtx, vod.variant, true, nil); err == nil || errors.Is(err, ErrTwitchEndUnconfirmed) {
		t.Errorf("VOD latch returned %v, want an unmarked error", err)
	}

	part := newEndVerdictHarness(t, "tw_mark_part")
	part.variant.CheckStreamFn = func(context.Context) (bool, error) { return true, nil }
	part.variant.FetchVariantsFn = func(context.Context) ([]twitch.TwitchHLSVariant, error) {
		return []twitch.TwitchHLSVariant{{URL: "http://127.0.0.1:1/x.m3u8", Name: "480p", Width: 854, Height: 480, FPS: 30}}, nil
	}
	if err := os.WriteFile(filepath.Join(part.jobCtx.StagingDir, "seg_0"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := part.o.ExecuteTwitch(ctx, part.jobCtx, part.variant, false, nil)
	if err == nil || !strings.Contains(err.Error(), "advance to new part") || errors.Is(err, ErrTwitchEndUnconfirmed) {
		t.Errorf("part-advance failure returned %v, want an unmarked advance error", err)
	}
}

// TestTwitchBroadcastOver: over means offline, or live as ANOTHER broadcast —
// by stream ID when the job's ID carries one, else by stream_start_time; a job
// that knows neither is never judged over against a live channel.
//
// Mutants: the stream-ID comparison inverted; the start-time fallback dropped
// (the manual job whose broadcast restarted is never over); an unknown
// identity read as over (the last case auto-muxes a live capture).
func TestTwitchBroadcastOver(t *testing.T) {
	live := func(id, started string) *twitch.TwitchStreamInfo {
		return &twitch.TwitchStreamInfo{StreamID: id, StartedAt: started, IsLive: true}
	}
	for _, tc := range []struct {
		name string
		job  *database.Job
		info *twitch.TwitchStreamInfo
		want bool
	}{
		{"offline", &database.Job{ID: "tw_1"}, nil, true},
		{"not live", &database.Job{ID: "tw_1"}, &twitch.TwitchStreamInfo{StreamID: "1"}, true},
		{"same stream", &database.Job{ID: "tw_1"}, live("1", ""), false},
		{"another stream", &database.Job{ID: "tw_1"}, live("2", ""), true},
		{"manual, same start", &database.Job{ID: "tw_manual_x_1", StreamStartTime: "2026-10-08T12:00:00Z"}, live("9", "2026-10-08T12:00:30Z"), false},
		{"manual, another start", &database.Job{ID: "tw_manual_x_1", StreamStartTime: "2026-10-08T12:00:00Z"}, live("9", "2026-10-08T15:00:00Z"), true},
		{"manual, start unknown", &database.Job{ID: "tw_manual_x_1"}, live("9", "2026-10-08T15:00:00Z"), false},
	} {
		if got := TwitchBroadcastOver(tc.job, tc.info); got != tc.want {
			t.Errorf("%s: TwitchBroadcastOver = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// latchedJob inserts a Twitch row the way setJobError leaves a live latch's —
// Error, the download failure's text, the marker when marked — over a staged
// recording `seconds` long (0 stages nothing).
func latchedJob(t *testing.T, w *DownloadWorker, db *database.Database, jobID string, seconds int, marked bool) {
	t.Helper()
	if _, err := db.AddJob(&database.Job{ID: jobID, VideoID: strings.TrimPrefix(jobID, "tw_"),
		URL: "https://twitch.tv/streamer", Title: jobID, Platform: "twitch", Status: database.StatusError,
		Error: "HLS playlist fetch failed", OutputDirectory: t.TempDir()}); err != nil {
		t.Fatalf("AddJob %s: %v", jobID, err)
	}
	if marked {
		db.UpdateJobFields(jobID, map[string]any{"park_reason": database.ParkReasonTwitchEndUnconfirmed})
	}
	if seconds > 0 {
		var stagingBase string
		w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })
		staging := filepath.Join(stagingBase, jobID)
		if err := os.MkdirAll(staging, 0o755); err != nil {
			t.Fatal(err)
		}
		ffmpegPath, _ := requireFFmpegTools(t)
		writeMuxFixture(t, ffmpegPath, filepath.Join(staging, "video.mp4"), seconds)
	}
}

// TestAutoMuxArchivesAConfirmedEndOnce is D-T4 end to end on the worker: a
// latched row whose broadcast the confirmation finds over is muxed exactly as
// the Mux action muxes it — Finished — and the marker is gone, so nothing
// offers it again.
//
// Mutants: autoMuxNow not clearing the marker (the Finished row still carries
// it); AutoMuxEndedBroadcast skipping the confirmation (the liveness seam is
// never asked).
func TestAutoMuxArchivesAConfirmedEndOnce(t *testing.T) {
	w, db := testWorkerSetup(t)
	latchedJob(t, w, db, "tw_11", 10, true)
	var asked atomic.Int32
	w.twitchLiveness = func(context.Context, string) (*twitch.TwitchStreamInfo, error) {
		asked.Add(1)
		return nil, nil // offline
	}

	w.AutoMuxEndedBroadcast("tw_11")
	w.wg.Wait()

	j, _ := db.GetJob("tw_11")
	if j == nil || j.Status != database.StatusFinished {
		t.Fatalf("auto-muxed row = %v (error %q), want Finished", statusOf(j), errorOf(j))
	}
	if j.ParkReason != database.ParkReasonNone {
		t.Errorf("the Finished row still carries park_reason %q", j.ParkReason)
	}
	if asked.Load() != 1 {
		t.Errorf("the broadcast's end was confirmed %d times, want once", asked.Load())
	}
}

// TestAutoMuxWaitsForAConfirmedEnd: the monitor's one sample is not a
// verdict. When the two-sample confirmation finds the same broadcast live, or
// cannot answer, the row is left exactly as it was — still marked, for the
// next poll.
//
// Mutants: AutoMuxEndedBroadcast acting whatever the confirmation's verdict
// (the still-live row is claimed); twitchBroadcastConfirmedOver reading a
// failed check as over (the failing check's row is claimed).
func TestAutoMuxWaitsForAConfirmedEnd(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *twitch.TwitchStreamInfo
		err  error
	}{
		{"same broadcast still live", &twitch.TwitchStreamInfo{StreamID: "12", IsLive: true}, nil},
		{"check failed", nil, errors.New("gql 503")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			latchedJob(t, w, db, "tw_12", 0, true)
			w.twitchLiveness = func(context.Context, string) (*twitch.TwitchStreamInfo, error) { return tc.info, tc.err }

			w.AutoMuxEndedBroadcast("tw_12")
			w.wg.Wait()

			j, _ := db.GetJob("tw_12")
			if j.Status != database.StatusError || j.ParkReason != database.ParkReasonTwitchEndUnconfirmed ||
				j.Error != "HLS playlist fetch failed" {
				t.Errorf("row = %s / %q / %q, want it untouched (Error, still marked)", j.Status, j.ParkReason, j.Error)
			}
		})
	}
}

// TestAutoMuxFailureIsFinalAndSaysSo: a mux that cannot run leaves the row in
// Error with an error saying the automatic mux failed, and WITHOUT the marker
// — so a later poll does not try again, in a loop or at all; the operator's Mux
// is the retry. An unmarked Error row is never touched.
//
// Mutants: autoMuxNow clearing the marker only after a successful mux (the
// failed row is confirmed again on the next poll); the failure written
// without autoMuxFailure's wording; endUnconfirmedRow ignoring the marker
// (the unmarked row is confirmed and muxed).
func TestAutoMuxFailureIsFinalAndSaysSo(t *testing.T) {
	w, db := testWorkerSetup(t)
	latchedJob(t, w, db, "tw_13", 0, true) // nothing staged: the mux cannot start
	latchedJob(t, w, db, "tw_14", 0, false)
	var asked atomic.Int32
	w.twitchLiveness = func(context.Context, string) (*twitch.TwitchStreamInfo, error) {
		asked.Add(1)
		return nil, nil
	}

	w.AutoMuxEndedBroadcast("tw_13")
	w.AutoMuxEndedBroadcast("tw_14")
	w.wg.Wait()
	// The next poll: the failed row is offered again and must not be a
	// candidate any more.
	w.AutoMuxEndedBroadcast("tw_13")
	w.wg.Wait()

	j, _ := db.GetJob("tw_13")
	if j.Status != database.StatusError || !strings.HasPrefix(j.Error, "automatic mux after the broadcast ended failed") {
		t.Errorf("failed auto-mux left %s / %q, want Error saying the automatic mux failed", j.Status, j.Error)
	}
	if j.ParkReason != database.ParkReasonNone {
		t.Errorf("failed auto-mux kept park_reason %q — every later poll would try again", j.ParkReason)
	}
	if u, _ := db.GetJob("tw_14"); u.Error != "HLS playlist fetch failed" {
		t.Errorf("an unmarked Error row was touched: %q", u.Error)
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("confirmations = %d, want 1 — neither the unmarked row nor the already-failed one is a candidate", got)
	}
}

// TestOperatorMuxClearsTheMarker: the operator's Mux of a latched row ends
// what the marker was waiting for, so the row no longer carries it — a manual
// mux that failed must not be followed by an automatic one the next time the
// channel reads offline.
//
// Mutant: muxJob's Muxing write leaving park_reason alone.
func TestOperatorMuxClearsTheMarker(t *testing.T) {
	w, db := testWorkerSetup(t)
	latchedJob(t, w, db, "tw_16", 10, true)
	if err := w.MuxJob("tw_16"); err != nil {
		t.Fatalf("MuxJob: %v", err)
	}
	w.wg.Wait()
	j, _ := db.GetJob("tw_16")
	if j.ParkReason != database.ParkReasonNone {
		t.Errorf("after the operator's Mux the row (%s) still carries park_reason %q", j.Status, j.ParkReason)
	}
}
