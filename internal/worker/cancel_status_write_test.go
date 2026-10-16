package worker

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/twitch"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// The status a run writes once it knows what the broadcast is doing — Live
// on the go-live paths, Upcoming as a Twitch wait begins — was decided on a
// read of the row made earlier: processJob's, or the waiting loop's last
// check. An operator's Cancel writes the row (Cancelled) and only then
// cancels the run's ctx, so a Cancel that landed in between was written
// over: the row read Live, or Upcoming, until handleCancellation wrote
// Cancelled again, and the dashboard showed a live job the operator had
// just cancelled. Each such write now applies only to a row with no outcome
// (UpdateJobFieldsUnlessTerminal), as waitForLive's first write already did;
// a go-live write that is refused stops the run as a cancelled one
// (cancelledResult), and a wait's first write leaves it to the loop's first
// check.

// liveFetch is a full fetch of a broadcast that is live now.
func liveFetch() *youtube.VideoInfo {
	return &youtube.VideoInfo{Title: "Fetched Title", ChannelName: "A Channel",
		ScheduledStartTime: "2026-10-16T18:00:00Z", IsLive: true,
		StreamStatus: youtube.StreamLive, PlayabilityError: youtube.PlayabilityOK}
}

// wantCancelledRun fails t unless res is cancelledResult and the row reads
// Cancelled.
func wantCancelledRun(t *testing.T, db *database.Database, jobID string, res *StreamProcessResult) {
	t.Helper()
	if res == nil || res.ShouldDownload || !errors.Is(res.ErrSentinel, ErrCancelled) {
		t.Errorf("the cancelled run returned %+v, want cancelledResult", res)
	}
	row, err := db.GetJob(jobID)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row.Status != database.StatusCancelled {
		t.Errorf("row status = %q after the Cancel, want %q", row.Status, database.StatusCancelled)
	}
}

// A Cancel that lands after Process read the row leaves it Cancelled, and the
// run that found the stream live stops. In the first run the Cancel — the
// real CancelJob — lands after the take, inside the owed "Stream Found" the
// run sends; in the second the job is owed nothing and the Cancel lands on
// the run's own metadata write, after the read Process makes for a run that
// took no flag. Both runs went on to write Live over the Cancel and return a
// download.
//
// Mutants: the live arm's status write unconditional again (both rows read
// Live, both runs return a download); its refusal ignored (both runs return
// a download).
func TestCancelBeforeTheLiveWriteLeavesTheRowCancelled(t *testing.T) {
	t.Run("after the take", func(t *testing.T) {
		q := queuedFoundJob(t, true)
		q.sp.notifier = cancelOnFound{Sender: q.rec, cancel: func() { q.w.CancelJob(q.job.ID) }}
		q.sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) { return liveFetch(), nil }
		res, err := q.sp.Process(q.ctx, q.job)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		wantCancelledRun(t, q.db, q.job.ID, res)
	})

	t.Run("on the metadata write, owed nothing", func(t *testing.T) {
		q := queuedFoundJob(t, false)
		// Not a sync.Once: CancelJob's own write reaches this subscriber
		// from inside the call, and a nested Do would never return.
		var cancelled atomic.Bool
		unsub := q.db.OnJobUpdate(func(j *database.Job) {
			if j.ID == q.job.ID && j.Title == "Fetched Title" && cancelled.CompareAndSwap(false, true) {
				q.w.CancelJob(q.job.ID)
			}
		})
		defer unsub()
		q.sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) { return liveFetch(), nil }
		res, err := q.sp.Process(q.ctx, q.job)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		wantCancelledRun(t, q.db, q.job.ID, res)
	})
}

// completeStreamTransition's write — Live with is_vod for a broadcast that
// has begun, is_vod alone for one that is over — applies only to a row with
// no outcome. The waiting loop's last check read the row before the full
// fetch that confirmed the stream, and a Cancel that landed after it (the
// row Cancelled, the run's ctx not cancelled yet) was written over with
// Live. A run whose write is refused, live or ended, stops instead of going
// on to a download.
//
// Mutants: the write unconditional again (the live row reads Live, and both
// runs return a download); the refusal ignored (both runs return a
// download).
func TestCompleteStreamTransitionStopsOnACancelledRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		info *youtube.VideoInfo
	}{
		{"live", liveFetch()},
		{"ended", &youtube.VideoInfo{Title: "Fetched Title", StreamStatus: youtube.StreamPostLive,
			ScheduledStartTime: "2026-10-16T18:00:00Z", PlayabilityError: youtube.PlayabilityOK}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, _, job := foundTestJob(t)
			sp.cfg = &config.MoomboxConfig{}
			if !sp.db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled}) {
				t.Fatal("could not cancel the row")
			}
			wantCancelledRun(t, sp.db, job.ID, sp.completeStreamTransition(job, tc.info, nil))
		})
	}
}

// twitchCancelFixture is a Twitch job on channel "streamer" in a database of
// its own, at status, and a processor over it with chat capture off.
func twitchCancelFixture(t *testing.T, status database.JobStatus) (*StreamProcessor, *database.Database, *database.Job) {
	t.Helper()
	sp, db, job, _ := twitchCancelFixtureAt(t, status)
	return sp, db, job
}

// twitchCancelFixtureAt is twitchCancelFixture, with the database file's path.
func twitchCancelFixtureAt(t *testing.T, status database.JobStatus) (*StreamProcessor, *database.Database, *database.Job, string) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Paths.StagingDirectory = t.TempDir()
	cfg.Paths.OutputDirectory = t.TempDir()
	cfg.Downloader.DownloadChat = false
	path := filepath.Join(t.TempDir(), "twitch-cancel.db")
	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	job := &database.Job{ID: "tw_cancel", VideoID: "tw_cancel", URL: "https://twitch.tv/streamer",
		Platform: "twitch", Status: status, ManuallyAdded: true, QualityPreference: "best"}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob(job.ID)
	if err != nil || stored == nil {
		t.Fatalf("GetJob: %v", err)
	}
	sp := &StreamProcessor{db: db, cfg: cfg, twitchHints: newTwitchHintCache(), logger: discardLogger{}}
	return sp, db, stored, path
}

// A Twitch wait that begins on a row a Cancel has already written leaves it
// Cancelled and stops at its first check. Its first write — Upcoming,
// "Waiting for stream..." — turned the Cancelled row back into a waiting
// one, and the loop's check then read Upcoming and went on polling until
// the Cancel's ctx cancel arrived.
//
// Mutant: waitForTwitchLive's first write unconditional again (the row reads
// Upcoming, and the wait runs on to the ctx deadline).
func TestTwitchWaitLeavesACancelledRowCancelled(t *testing.T) {
	sp, db, job := twitchCancelFixture(t, database.StatusUpcoming)
	// The operator's Cancel lands after processJob read the row; its ctx
	// cancel has yet to arrive.
	if !db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled}) {
		t.Fatal("could not cancel the row")
	}
	const deadline = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	began := time.Now()
	info, err := sp.waitForTwitchLive(ctx, job, "streamer")
	if info != nil || err != nil {
		t.Errorf("waitForTwitchLive = %+v, %v; want the cancelled wait's nil, nil", info, err)
	}
	if took := time.Since(began); took >= deadline {
		t.Errorf("the wait ran %v, to its ctx deadline, rather than stopping at its first check", took)
	}
	row, err := db.GetJob(job.ID)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row.Status != database.StatusCancelled || row.Progress != "" {
		t.Errorf("row status %q, progress %q; want the Cancelled row with no waiting line", row.Status, row.Progress)
	}
}

// The Twitch live start's write — the variant it records, and Live — applies
// only to a row with no outcome. processJob read the row before the stream
// lookup and the playlist fetch, and a Cancel that landed during them (the
// row Cancelled, the run's ctx not cancelled yet) was written over with
// Live, and the run returned a download. It stops now, writing nothing.
//
// Mutants: startTwitchVariant's status write unconditional (the row reads
// Live, the run returns a download); processTwitchLive ignoring the refusal
// (the run returns a download).
func TestTwitchLiveStartStopsOnACancelledRow(t *testing.T) {
	sp, db, job := twitchCancelFixture(t, database.StatusLive)
	sp.StashTwitchStreamInfo(&twitch.TwitchStreamInfo{IsLive: true, ChannelLogin: "streamer",
		StreamID: "1", StartedAt: "2026-10-16T18:00:00Z"})
	sp.hlsMasterFn = func(context.Context, string) ([]twitch.TwitchHLSVariant, bool, error) {
		// The operator clicks Cancel while the playlist loads.
		if !db.UpdateJobFieldsUnlessTerminal(job.ID, map[string]any{"status": database.StatusCancelled}) {
			t.Error("could not cancel the row")
		}
		return sixtyBesideThirty(), false, nil
	}
	res, err := sp.processTwitchLive(context.Background(), job, "streamer")
	if err != nil {
		t.Fatalf("processTwitchLive: %v", err)
	}
	wantCancelledRun(t, db, job.ID, res)
	if row, _ := db.GetJob(job.ID); row != nil && row.TwitchQuality != "" {
		t.Errorf("twitch_quality = %q, want nothing written to the Cancelled row", row.TwitchQuality)
	}
}
