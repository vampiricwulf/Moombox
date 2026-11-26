package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/constants"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
)

// gql503 is what gqlRequest returns once a 5xx has outlasted its retries.
var gql503 = errors.New("gql exhausted 3 retries: gql http 503 (ShareClipRenderStatus): 0-byte body")

// TestTwitchBacklogGQLRefusalRequeues is owner ruling R4-5: a Twitch GQL 5xx
// that outlasted gqlRequest's retries, or a 429 whose Retry-After was too
// long to wait, on a swept clip or a Twitch backlog VOD sends the row back to
// Queued through the backlog requeue, as a YouTube backlog VOD's does. A
// manually added clip, which the requeue does not take, still ends in Error
// with the same text it had.
//
// Mutants: the transient-answer check dropped from processTwitchClip (the
// swept clip ends in Error); dropped from processTwitchVod (the backlog VOD
// ends in Error); isTwitchTransientAnswer matching any GQL answer (the 404
// clip is requeued).
func TestTwitchBacklogGQLRefusalRequeues(t *testing.T) {
	ch := "twitch_retry"
	backlogClip := func(t *testing.T, db *database.Database) string {
		t.Helper()
		id := twitch.ClipJobID("FaintLightGullWholeWheat")
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "https://clips.twitch.tv/FaintLightGullWholeWheat",
			Platform: "twitch", Status: database.StatusUpcoming, ChannelID: &ch, QueuePriority: 1}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("swept clip, 5xx", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		w.streamProc.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) { return nil, gql503 }
		id := backlogClip(t, db)
		w.processJob(context.Background(), id)
		if row, _ := db.GetJob(id); row.Status != database.StatusQueued || !w.scheduler.held(id, time.Now()) {
			t.Errorf("status = %s (%q), want Queued and held", row.Status, row.Error)
		}
	})

	t.Run("swept clip, gone", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		w.streamProc.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) {
			return nil, errors.New("gql http 404 (ShareClipRenderStatus): 0-byte body")
		}
		id := backlogClip(t, db)
		w.processJob(context.Background(), id)
		if row, _ := db.GetJob(id); row.Status != database.StatusError {
			t.Errorf("status = %s, want Error: a 404 is Twitch's verdict", row.Status)
		}
	})

	t.Run("manual clip, 5xx", func(t *testing.T) {
		w, db := testWorkerSetup(t)
		w.streamProc.clipFn = func(context.Context, string) (*twitch.TwitchClipInfo, error) { return nil, gql503 }
		job := addClipJob(t, db, t.TempDir())
		w.processJob(context.Background(), job.ID)
		row, _ := db.GetJob(job.ID)
		if row.Status != database.StatusError || row.Error != "twitch clip error: "+gql503.Error() {
			t.Errorf("status = %s (%q), want Error with the GQL answer", row.Status, row.Error)
		}
	})

	t.Run("backlog VOD, long 429", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Retry-After", "120")
			rw.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(rw, "slow down")
		}))
		t.Cleanup(srv.Close)
		oldGQL := constants.TwitchURLs.GQL
		constants.TwitchURLs.GQL = srv.URL + "/gql"
		t.Cleanup(func() { constants.TwitchURLs.GQL = oldGQL })

		w, db := testWorkerSetup(t)
		w.streamProc.tw = twitch.NewService(nil, discardLogger{})
		if _, err := db.AddJob(&database.Job{ID: "tw_v123", VideoID: "tw_v123", URL: "https://www.twitch.tv/videos/123",
			Platform: "twitch", Status: database.StatusUpcoming, ChannelID: &ch, QueuePriority: 1}); err != nil {
			t.Fatal(err)
		}
		w.processJob(context.Background(), "tw_v123")
		row, _ := db.GetJob("tw_v123")
		if row.Status != database.StatusQueued || !w.scheduler.held("tw_v123", time.Now()) {
			t.Errorf("status = %s (%q), want Queued and held", row.Status, row.Error)
		}
	})
}
