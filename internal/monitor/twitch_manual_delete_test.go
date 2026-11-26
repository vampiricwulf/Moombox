package monitor

import (
	"context"
	"slices"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// TestTwitch_DeletedManualAddHoldsItsBroadcastBack is owner ruling RV-6
// through the live monitor. Two channels were added by URL while offline
// ("tw_manual_<login>_<n>", a key the monitor never reads). "recorded" went
// live and its job recorded broadcast 900; "waiting" never went live. The
// operator deletes both jobs, and both channels are then found live. The
// poll must leave broadcast 900 alone (the Delete left its history row,
// "tw_900") and must find the other channel's broadcast, whose job left
// nothing. While the jobs exist, nothing is listed as orphaned.
//
// Mutants: the history insert dropped from deleteJobLeavingHistoryTx, or its
// key built without "tw_" (broadcast 900 is found again);
// DeleteJobLeavingHistory keying a Twitch row by its job ID (the same); the
// row written when the broadcast is learned instead of at the Delete (an
// entry is listed while the job exists).
func TestTwitch_DeletedManualAddHoldsItsBroadcastBack(t *testing.T) {
	chans := []config.ChannelConfig{
		{ID: "recorded", Name: "recorded", Platform: "twitch"},
		{ID: "waiting", Name: "waiting", Platform: "twitch"},
	}
	tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
		infos := make([]*twitch.TwitchStreamInfo, len(logins))
		for i, login := range logins {
			id := "900"
			if login == "waiting" {
				id = "901"
			}
			infos[i] = &twitch.TwitchStreamInfo{StreamID: id, ChannelLogin: login, IsLive: true,
				Title: "live", StartedAt: "2026-11-26T10:00:00Z"}
		}
		return infos, make([]error, len(logins)), nil
	}, chans...)

	jobs := []*database.Job{
		{ID: "tw_manual_recorded_1", VideoID: "tw_manual_recorded_1", URL: "https://twitch.tv/recorded",
			Platform: "twitch", Status: database.StatusFinished, ManuallyAdded: true},
		{ID: "tw_manual_waiting_1", VideoID: "tw_manual_waiting_1", URL: "https://twitch.tv/waiting",
			Platform: "twitch", Status: database.StatusCancelled, ManuallyAdded: true},
	}
	for _, j := range jobs {
		if _, err := tm.db.AddJob(j); err != nil {
			t.Fatal(err)
		}
	}
	// As the worker writes it when the capture starts.
	tm.db.UpdateJobFields("tw_manual_recorded_1", map[string]any{"twitch_stream_id": "900"})

	if orphaned, err := tm.db.ListOrphanedHistory(); err != nil || len(orphaned) != 0 {
		t.Fatalf("orphaned history while both jobs exist = %v (err %v), want none", orphaned, err)
	}

	for _, j := range jobs {
		// The caller's snapshot, read before the worker wrote the column:
		// the Delete goes by the row.
		if err := tm.db.DeleteJobLeavingHistory(j); err != nil {
			t.Fatalf("delete %s: %v", j.ID, err)
		}
	}

	var found []string
	tm.OnStreamFound = func(info *twitch.TwitchStreamInfo, _ *config.ChannelConfig) {
		found = append(found, info.StreamID)
	}
	tm.doCheck(context.Background())

	if !slices.Equal(found, []string{"901"}) {
		t.Errorf("streams found after the deletes = %v, want [901]: 900 was recorded by the deleted job and is held back, 901 was never recorded", found)
	}
	orphaned, err := tm.db.ListOrphanedHistory()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, e := range orphaned {
		keys = append(keys, e.VideoID)
	}
	if !slices.Equal(keys, []string{"tw_900"}) {
		t.Errorf("orphaned history after the deletes = %v, want [tw_900] and nothing for the job that recorded no broadcast", keys)
	}
}
