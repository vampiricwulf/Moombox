package worker

import (
	"errors"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/jobstate"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// TestJobFailedSaysWhatHappensNext: the Job Failed embed carries a "Next"
// field when the row the failure wrote is waiting on an automatic mux or
// retry, in the sentence both UIs show for it, and has none otherwise. The
// embed is the surface an operator away from the dashboard acts on; a capture
// the monitor muxes by itself read there as a lost recording.
//
// The answer is jobstate.PendingAutoAction's, asked of the row as written, so
// the test compares against that predicate and never restates its rule.
//
// Mutants: the predicate asked of the row as it was before the write (still
// Live, so never pending: no case gets a field); the field added
// unconditionally (an empty value is a Discord 400 that drops the embed).
func TestJobFailedSaysWhatHappensNext(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		name string
		job  *database.Job
		err  func(t *testing.T, job *database.Job) error
		want jobstate.AutoAction
	}{
		{"a live Twitch capture whose end is unconfirmed",
			&database.Job{ID: "tw_7001", VideoID: "7001", URL: "https://twitch.tv/streamer", Platform: "twitch", Status: database.StatusDownloading},
			func(*testing.T, *database.Job) error { return markEndUnconfirmed(errors.New("playlist fetch failed")) },
			jobstate.AutoActionMux},
		{"the first offline flap of a monitored broadcast",
			&database.Job{ID: "tw_7002", VideoID: "7002", URL: "https://twitch.tv/streamer", Platform: "twitch", Status: database.StatusLive},
			func(t *testing.T, job *database.Job) error { return offlineRefusal(t, job, base) },
			jobstate.AutoActionRetry},
		{"the offline flap of a manually added row, which the recovery never reaches",
			&database.Job{ID: "tw_7003", VideoID: "7003", URL: "https://twitch.tv/streamer", Platform: "twitch", Status: database.StatusLive, ManuallyAdded: true},
			// The refusal is built from a monitored copy: a manual row's stream
			// processing waits for the channel instead of refusing, and the text
			// and reason reach a manual row from before that wait existed.
			func(t *testing.T, job *database.Job) error {
				monitored := *job
				monitored.ManuallyAdded = false
				return offlineRefusal(t, &monitored, base)
			},
			jobstate.AutoActionNone},
		{"a plain YouTube failure",
			&database.Job{ID: "yt_7004", VideoID: "7004", Platform: "youtube", Status: database.StatusDownloading},
			func(*testing.T, *database.Job) error { return errors.New("boom") },
			jobstate.AutoActionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			t.Cleanup(w.Stop)
			rec := notificationtest.New()
			w.notifier = rec
			if _, err := db.AddJob(tc.job); err != nil {
				t.Fatal(err)
			}
			w.setJobError(tc.job, tc.err(t, tc.job))

			row, err := db.GetJob(tc.job.ID)
			if err != nil || row == nil {
				t.Fatalf("GetJob: %v", err)
			}
			if got := jobstate.PendingAutoAction(row); got != tc.want {
				t.Fatalf("the written row's pending action = %q, want %q", got, tc.want)
			}
			calls := rec.ByEvent("error")
			if len(calls) != 1 {
				t.Fatalf("recorded %d Job Failed sends, want 1", len(calls))
			}
			got, has := calls[0].Field("Next")
			if tc.want == jobstate.AutoActionNone {
				if has {
					t.Errorf("Next = %q for a row nothing automatic will touch", got)
				}
				return
			}
			if !has || got != tc.want.Text() {
				t.Errorf("Next = %q (present %v), want %q", got, has, tc.want.Text())
			}
		})
	}
}
