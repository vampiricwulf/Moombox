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

// TestTwitch_ManualJobNamingABroadcastHoldsIt is owner ruling RV-8. A
// manually added job that names a broadcast (twitch_stream_id) holds it back
// from the live monitor while the row is active or Cancelled. The Cancelled
// case is the Delete's window: both delete paths cancel the job, wait for the
// worker to let go, and only then delete the row and leave the history entry
// (the test above starts there). A poll in between used to find the
// broadcast and record it again under a job of the monitor's own. It is also
// a plain Cancel, which the monitor used to answer within one poll. A
// Finished or Errored row holds nothing, and neither does a row that names
// another broadcast.
//
// Mutants: the ManualTwitchJobHoldsStream gate dropped from
// processStreamInfo (the three held cases find 900); Cancelled added to the
// statuses its query excludes (the Cancelled case finds 900); Error dropped
// from them (the Errored case finds nothing); the stream ID dropped from its
// WHERE (the "another broadcast" case finds nothing); manually_added dropped
// from it (the "not manually added" case finds nothing).
func TestTwitch_ManualJobNamingABroadcastHoldsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		jobID    string
		status   database.JobStatus
		streamID string
		held     bool
		monitors bool // the row is not a manually added one
	}{
		{"cancelled, not yet deleted", "tw_manual_ch_1", database.StatusCancelled, "900", true, false},
		// Rows manualJobClaims never reads: a `moombox add` row (the bare
		// login) and a broadcast add re-initialised onto a later broadcast.
		{"a moombox add row recording it", "ch", database.StatusDownloading, "900", true, false},
		{"a re-initialised broadcast add muxing it", "tw_800", database.StatusMuxing, "900", true, false},
		{"errored", "tw_manual_ch_1", database.StatusError, "900", false, false},
		{"finished", "tw_manual_ch_1", database.StatusFinished, "900", false, false},
		{"cancelled, naming another broadcast", "tw_manual_ch_1", database.StatusCancelled, "899", false, false},
		// The rule is about the operator's own jobs: the worker writes the
		// column for those only, and a monitor's row is held by its history.
		{"cancelled, not manually added", "x1", database.StatusCancelled, "900", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tm := newTestTwitchMonitor(t, func(ctx context.Context, logins []string) ([]*twitch.TwitchStreamInfo, []error, error) {
				infos := make([]*twitch.TwitchStreamInfo, len(logins))
				for i, login := range logins {
					infos[i] = &twitch.TwitchStreamInfo{StreamID: "900", ChannelLogin: login, IsLive: true,
						Title: "live", StartedAt: "2026-11-26T10:00:00Z"}
				}
				return infos, make([]error, len(logins)), nil
			}, config.ChannelConfig{ID: "ch", Name: "ch", Platform: "twitch"})
			if _, err := tm.db.AddJob(&database.Job{ID: tc.jobID, VideoID: tc.jobID, URL: "https://twitch.tv/ch",
				Platform: "twitch", Status: tc.status, ManuallyAdded: !tc.monitors}); err != nil {
				t.Fatal(err)
			}
			tm.db.UpdateJobFields(tc.jobID, map[string]any{"twitch_stream_id": tc.streamID})

			var found []string
			tm.OnStreamFound = func(info *twitch.TwitchStreamInfo, _ *config.ChannelConfig) {
				found = append(found, info.StreamID)
			}
			tm.doCheck(context.Background())

			if tc.held && len(found) != 0 {
				t.Errorf("streams found = %v, want none: the %s row names broadcast 900 and holds it", found, tc.status)
			}
			if !tc.held && !slices.Equal(found, []string{"900"}) {
				t.Errorf("streams found = %v, want [900]: a %s row naming %s holds nothing", found, tc.status, tc.streamID)
			}
		})
	}
}
