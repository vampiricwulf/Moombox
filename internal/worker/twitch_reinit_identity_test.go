package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// A re-initialised manually added Twitch row that records a later broadcast
// takes that broadcast's identity: its start as well as its stream ID
// (r3-worker-twitch-history-1). It kept the start of the broadcast it had
// recorded before, so a restart mid-recording read its own broadcast as a
// new one and ended the job in Error ("a new broadcast is live"), and the
// outage path finalized a capture whose broadcast was still running. A row
// that holds a capture keeps its start: the minute of tolerance does not
// move it. A monitor-created row's start is not replaced.
//
// Mutants: the start not replaced (the restart is refused as a new
// broadcast); replaced for a row that holds a capture (the restart's
// jittered start is written); replaced for a monitor-created row.
func TestReinitialisedManualTwitchRowTakesTheLaterBroadcastsStart(t *testing.T) {
	const (
		oldStart = "2026-11-20T10:00:00Z"
		newStart = "2026-11-26T10:00:00Z"
		jittered = "2026-11-26T10:00:30Z"
	)
	run := func(t *testing.T, sp *StreamProcessor, job *database.Job, startedAt string) *StreamProcessResult {
		t.Helper()
		sp.hlsMasterFn = func(context.Context, string) ([]twitch.TwitchHLSVariant, bool, error) {
			return sixtyBesideThirty(), false, nil
		}
		sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{IsLive: true, ChannelLogin: "streamer",
			StreamID: "222", StartedAt: startedAt})
		res, err := sp.processTwitchLive(context.Background(), job, "streamer")
		if err != nil || res == nil {
			t.Fatalf("processTwitchLive = %+v, %v", res, err)
		}
		return res
	}
	startOf := func(t *testing.T, db *database.Database, id string) string {
		t.Helper()
		row, err := db.GetJob(id)
		if err != nil || row == nil {
			t.Fatalf("GetJob: %v", err)
		}
		return row.StreamStartTime
	}

	t.Run("manual", func(t *testing.T) {
		sp, db, job := twitchCancelFixture(t, database.StatusUpcoming)
		// What a Reinitialize leaves: the earlier broadcast's start and ID.
		db.UpdateJobFields(job.ID, map[string]any{"stream_start_time": oldStart, "twitch_stream_id": "111"})
		job, _ = db.GetJob(job.ID)
		if res := run(t, sp, job, newStart); !res.ShouldDownload {
			t.Fatalf("the later broadcast's start = %+v, want a download", res)
		}
		if got := startOf(t, db, job.ID); got != newStart {
			t.Fatalf("stream_start_time after the later broadcast's start = %q, want %q", got, newStart)
		}

		// Moombox restarts mid-recording; the same broadcast is live.
		db.UpdateJobFields(job.ID, map[string]any{"status": database.StatusDownloading})
		job, _ = db.GetJob(job.ID)
		if res := run(t, sp, job, jittered); !res.ShouldDownload {
			t.Fatalf("the restart on the row's own broadcast = %+v, want it attached", res)
		}
		if got := startOf(t, db, job.ID); got != newStart {
			t.Errorf("a row holding a capture had its start moved to %q, want %q kept", got, newStart)
		}
	})

	t.Run("monitor-created", func(t *testing.T) {
		sp, db, job := twitchCancelFixture(t, database.StatusUpcoming)
		db.UpdateJobFields(job.ID, map[string]any{"manually_added": false, "stream_start_time": oldStart})
		job, _ = db.GetJob(job.ID)
		run(t, sp, job, newStart)
		if got := startOf(t, db, job.ID); got != oldStart {
			t.Errorf("a monitor-created row's start = %q, want %q kept", got, oldStart)
		}
	})
}

// The end-unconfirmed mux of a re-initialised broadcast add: the row tw_800
// recorded broadcast 900 (twitch_stream_id), and 900 is still live. The
// confirmation compared 900 with the job ID's 800, read the broadcast over
// and muxed a capture whose broadcast was still running.
//
// Mutant: TwitchBroadcastOver not reading twitch_stream_id first (the row is
// claimed for the mux).
func TestAutoMuxLeavesAReinitialisedRowWhoseBroadcastIsLive(t *testing.T) {
	w, db := testWorkerSetup(t)
	latchedJob(t, w, db, "tw_800", 0, true)
	db.UpdateJobFields("tw_800", map[string]any{"manually_added": true, "twitch_stream_id": "900"})
	w.twitchLiveness = func(context.Context, string) (*twitch.TwitchStreamInfo, error) {
		return &twitch.TwitchStreamInfo{StreamID: "900", IsLive: true}, nil
	}

	w.AutoMuxEndedBroadcast("tw_800")
	w.wg.Wait()

	j, _ := db.GetJob("tw_800")
	if j.Status != database.StatusError || j.ParkReason != database.ParkReasonTwitchEndUnconfirmed ||
		j.Error != "HLS playlist fetch failed" {
		t.Errorf("row = %s / %q / %q, want it untouched (its broadcast is live)", j.Status, j.ParkReason, j.Error)
	}
}
