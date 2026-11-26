package worker

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// A manually added Twitch job names, on its row, the broadcast its capture is
// starting on (owner ruling RV-6), and a row that later takes another
// broadcast (a re-initialised one) names the later one: the operator's Delete
// holds back whichever the row holds. A monitor-created job writes nothing,
// its job ID being the monitor's key already.
//
// Mutants: the twitch_stream_id write dropped from processTwitchLive (the
// manual row holds none); written only while the row holds none (the second
// start still reads 555); the ManuallyAdded guard dropped (the monitor's row
// holds one); the history row written here, at the start, instead of by the
// Delete (an entry is listed while the job exists).
func TestTwitchLiveStartNamesAManualJobsBroadcast(t *testing.T) {
	start := func(sp *StreamProcessor, job *database.Job, streamID, startedAt string) {
		t.Helper()
		sp.hlsMasterFn = func(context.Context, string) ([]twitch.TwitchHLSVariant, bool, error) {
			return sixtyBesideThirty(), false, nil
		}
		sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{IsLive: true, ChannelLogin: "streamer",
			StreamID: streamID, StartedAt: startedAt})
		res, err := sp.processTwitchLive(context.Background(), job, "streamer")
		if err != nil || res == nil || !res.ShouldDownload {
			t.Fatalf("processTwitchLive = %+v, %v; want a download", res, err)
		}
	}
	held := func(db *database.Database, id string) string {
		t.Helper()
		row, err := db.GetJob(id)
		if err != nil || row == nil {
			t.Fatalf("GetJob: %v", err)
		}
		return row.TwitchStreamID
	}

	t.Run("manual", func(t *testing.T) {
		sp, db, job := twitchCancelFixture(t, database.StatusUpcoming)
		start(sp, job, "555", "2026-11-26T10:00:00Z")
		if got := held(db, job.ID); got != "555" {
			t.Fatalf("twitch_stream_id after the first start = %q, want 555", got)
		}
		// Nothing goes in the history until the job is deleted: an entry
		// there would be listed as orphaned beside its living job's row.
		if orphaned, err := db.ListOrphanedHistory(); err != nil || len(orphaned) != 0 {
			t.Errorf("orphaned history while the job records = %v (err %v), want none", orphaned, err)
		}
		// Re-initialised, the row waits again and takes the next broadcast.
		job, _ = db.GetJob(job.ID)
		start(sp, job, "777", "2026-11-27T10:00:00Z")
		if got := held(db, job.ID); got != "777" {
			t.Errorf("twitch_stream_id after a later broadcast's start = %q, want 777", got)
		}
	})

	t.Run("monitor-created", func(t *testing.T) {
		sp, db, job := twitchCancelFixture(t, database.StatusUpcoming)
		db.UpdateJobFields(job.ID, map[string]any{"manually_added": false})
		job, _ = db.GetJob(job.ID)
		start(sp, job, "555", "2026-11-26T10:00:00Z")
		if got := held(db, job.ID); got != "" {
			t.Errorf("a monitor-created job's twitch_stream_id = %q, want none", got)
		}
	})
}
