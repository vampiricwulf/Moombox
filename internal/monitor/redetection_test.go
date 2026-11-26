package monitor

import (
	"context"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// A YouTube video still upcoming whose job was Cancelled (or ended in Error)
// is handled: the feed cycle neither probes it nor offers it as found, where
// it used to spend a player probe and log "Video found" every cycle for a
// job insert that was always refused (owner ruling R4-4). A Finished job is
// still offered, so an incomplete-tail job can resume on re-detection.
//
// Mutant: the walk and archive gates back on HasActiveJob — the Cancelled
// and Error cases probe and emit.
func TestFeed_ASettledJobHoldsItsBroadcastBack(t *testing.T) {
	for _, tc := range []struct {
		status database.JobStatus
		held   bool
	}{
		{database.StatusCancelled, true},
		{database.StatusError, true},
		{database.StatusFinished, false},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			db := newTestDB(t)
			now := fixedNow()
			seedRow(t, db, "UC1", "up000000001", now, "exact", "rss", "upcoming")
			if _, err := db.AddJob(&database.Job{ID: "up000000001", VideoID: "up000000001", URL: "u", Status: tc.status}); err != nil {
				t.Fatal(err)
			}
			probes := 0
			fm := newTestFeedMonitor(t, db, withRSS(rss404()), withMembership(membWith()),
				withProbe(func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
					probes++
					return &VideoProbeResult{StreamStatus: "upcoming", Title: videoID}, nil
				}), withNow(now))
			found := recordVideoFound(fm)
			fm.runCycleForTest(t, "UC1")

			if tc.held {
				if probes != 0 || len(*found) != 0 {
					t.Fatalf("probes = %d, found = %v; a %s job must hold the video back", probes, *found, tc.status)
				}
				return
			}
			if probes != 1 || len(*found) != 1 || (*found)[0].d != DispositionBroadcast {
				t.Fatalf("probes = %d, found = %v; a Finished job must stay eligible for re-detection", probes, *found)
			}
		})
	}
}

// The DECAPI half of the same rule: its newest video, upcoming, with a
// Cancelled job is not probed every 15 s, and with a Finished job it is.
func TestDecapi_ASettledJobHoldsItsBroadcastBack(t *testing.T) {
	for _, tc := range []struct {
		status database.JobStatus
		held   bool
	}{
		{database.StatusCancelled, true},
		{database.StatusError, true},
		{database.StatusFinished, false},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			db := newTestDB(t)
			if _, err := db.AddJob(&database.Job{ID: "vidDecHld01", VideoID: "vidDecHld01", URL: "u", Status: tc.status}); err != nil {
				t.Fatal(err)
			}
			probes := 0
			dm := newTestDecapiMonitor(t, db, func(ctx context.Context, videoID string) (*VideoProbeResult, error) {
				probes++
				return &VideoProbeResult{StreamStatus: "upcoming", Title: "free chat"}, nil
			})
			found := recordDecapiVideoFound(dm)
			ch := &config.ChannelConfig{ID: "UC1", Name: "UC1"}
			if err := dm.processResponse(context.Background(), decapiBody("vidDecHld01", "free chat"), ch); err != nil {
				t.Fatal(err)
			}
			if tc.held {
				if probes != 0 || len(*found) != 0 {
					t.Fatalf("probes = %d, found = %v; a %s job must hold the video back", probes, *found, tc.status)
				}
				return
			}
			if probes != 1 || len(*found) != 1 {
				t.Fatalf("probes = %d, found = %v; a Finished job must stay eligible", probes, *found)
			}
		})
	}
}
