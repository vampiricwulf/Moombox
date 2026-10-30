package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// foundTestJob opens a database holding one freshly discovered Upcoming row —
// the shape createYouTubeJob writes, with no start time yet — and returns a
// processor that records its notifications.
func foundTestJob(t *testing.T) (*StreamProcessor, *notificationtest.Recorder, *database.Job) {
	t.Helper()
	db := openTestDB(t)
	chID := "UC_abc"
	job := &database.Job{
		ID: "found1", VideoID: "found1", Platform: "youtube",
		Title: "Feed Title", ChannelName: "A Channel", Status: database.StatusUpcoming,
		ChannelID: &chID,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob("found1")
	if err != nil || stored == nil {
		t.Fatalf("GetJob: %v", err)
	}
	rec := notificationtest.New()
	return &StreamProcessor{db: db, notifier: rec, logger: discardLogger{}}, rec, stored
}

// A discovery whose first fetch finds a schedule is ONE embed: "Stream Found"
// carrying the start as Starts At, with no "YouTube Start Time Confirmed"
// beside it. It also carries the fetched title, not the feed's.
//
// Mutants: sending the confirmation when announceFound is set too (two
// embeds); sendStreamFound dropping StartsAt (the field lookup fails).
func TestDeferredFoundCarriesTheScheduleInOneEmbed(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	start := "2026-10-12T18:00:00Z"

	sp.applyJobMetadata(job, &youtube.VideoInfo{
		Title:              "Fetched Title",
		ChannelName:        "A Channel",
		ScheduledStartTime: start,
		IsUpcoming:         true,
	}, false, true)

	if got := rec.ByEvent("scheduled"); len(got) != 0 {
		t.Errorf("recorded %d \"scheduled\" embeds beside the discovery, want 0: %+v", len(got), got)
	}
	found := rec.ByEvent("found")
	if len(found) != 1 {
		t.Fatalf("recorded %d \"found\" embeds, want 1: %+v", len(found), rec.Calls())
	}
	ts, _ := time.Parse(time.RFC3339, start)
	want := fmt.Sprintf("<t:%d:f> (<t:%d:R>)", ts.Unix(), ts.Unix())
	if v, ok := found[0].Field("Starts At"); !ok || v != want {
		t.Errorf("Starts At = %q (present %v), want %q", v, ok, want)
	}
	if found[0].Title != "Stream Found" {
		t.Errorf("title = %q, want \"Stream Found\"", found[0].Title)
	}
	if want := "Upcoming: Fetched Title"; found[0].Description != want {
		t.Errorf("description = %q, want %q — the embed should say what the fetch learned", found[0].Description, want)
	}
	if fb := found[0].Opts.Fallback; fb == nil || fb.Event != "scheduled" || fb.Title != "YouTube Start Time Confirmed" {
		t.Errorf("fallback = %+v, want the confirmation for a target that takes `scheduled` but not `found`", fb)
	} else if v, _ := (notificationtest.Call{Fields: fb.Fields}).Field("Starts At"); v != want {
		t.Errorf("fallback Starts At = %q, want %q", v, want)
	}
	if found[0].Opts.JobID != "found1" {
		t.Errorf("JobID = %q, want found1 — an edit-mode target keys the job's message on it", found[0].Opts.JobID)
	}
	if a := found[0].Opts.Author; a == nil || a.Name != "A Channel" || a.URL != "https://www.youtube.com/channel/UC_abc" {
		t.Errorf("author = %+v, want the channel and its page", a)
	}
	if found[0].Opts.URL != "https://www.youtube.com/watch?v=found1" {
		t.Errorf("URL = %q, want the watch-page fallback for a row with no url", found[0].Opts.URL)
	}
}

// No schedule, already live, or a VOD: "Stream Found" still goes out, with no
// Starts At, and nothing else.
func TestDeferredFoundWithoutAScheduleOmitsStartsAt(t *testing.T) {
	for _, tc := range []struct {
		name string
		info youtube.VideoInfo
	}{
		{"upcoming, no schedule set", youtube.VideoInfo{Title: "T", IsUpcoming: true}},
		{"already live", youtube.VideoInfo{Title: "T", ScheduledStartTime: "2026-10-12T18:00:00Z", IsUpcoming: true, IsLive: true}},
		{"a VOD", youtube.VideoInfo{Title: "T", ScheduledStartTime: "2026-10-12T18:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, rec, job := foundTestJob(t)
			sp.applyJobMetadata(job, &tc.info, false, true)
			found := rec.ByEvent("found")
			if len(found) != 1 || len(rec.Calls()) != 1 {
				t.Fatalf("recorded %+v, want exactly one \"found\"", rec.Calls())
			}
			if v, ok := found[0].Field("Starts At"); ok {
				t.Errorf("Starts At = %q on a stream with no start ahead of it", v)
			}
			if fb := found[0].Opts.Fallback; fb != nil {
				t.Errorf("fallback = %+v with no schedule to confirm", fb)
			}
		})
	}
}

// A schedule that appears AFTER the discovery was announced — the stream had
// none when found — still sends "YouTube Start Time Confirmed", and no second
// "Stream Found".
func TestScheduleGainedLaterStillConfirms(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.applyJobMetadata(job, &youtube.VideoInfo{Title: "T", IsUpcoming: true}, false, true)

	sp.updateJobMetadata(job, &youtube.VideoInfo{
		Title: "T", ScheduledStartTime: "2026-10-12T18:00:00Z", IsUpcoming: true,
	}, true)

	if got := len(rec.ByEvent("found")); got != 1 {
		t.Errorf("recorded %d \"found\" embeds, want 1", got)
	}
	if got := len(rec.ByEvent("scheduled")); got != 1 {
		t.Errorf("recorded %d \"scheduled\" embeds for the later schedule, want 1", got)
	}
}

// A first fetch that fails still announces the discovery, from what the row
// held, and only once: the row's flag is taken, so a retry's fetch announces
// nothing.
//
// Mutant: sending only on a successful fetch — the failed discovery is then
// never announced at all.
func TestDeferredFoundSurvivesAFailedFetch(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
		return nil, errors.New("innertube: 503")
	}
	owe(t, sp.db, job)

	if _, err := sp.Process(context.Background(), job); err == nil {
		t.Fatal("Process succeeded on a failing fetch")
	}

	found := rec.ByEvent("found")
	if len(found) != 1 {
		t.Fatalf("recorded %d \"found\" embeds after a failed fetch, want 1: %+v", len(found), rec.Calls())
	}
	if found[0].Description != "Found matching stream: Feed Title" {
		t.Errorf("description = %q, want the row's own title", found[0].Description)
	}
	if sp.db.TakeAnnounceFound(job.ID) {
		t.Error("the flag outlived the fetch that announced it")
	}
}

// A fetch that failed because its run was cancelled — the job deleted or
// cancelled, or the process stopping — announces nothing: the message it
// would open is for a job that is already gone. Nor does it take the flag: a
// cancel or delete has dealt with it already, and a run stopped by shutdown
// leaves it owed to the next run.
//
// Mutants: dropping the ctx.Err() guard from takeAnnounceFound (the cancelled
// run announces and spends the flag); taking the flag before the check (it is
// spent).
func TestCancelledFetchDoesNotAnnounce(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.getVideoInfoFn = func(ctx context.Context, _ string) (*youtube.VideoInfo, error) {
		return nil, ctx.Err()
	}
	owe(t, sp.db, job)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sp.Process(ctx, job); err == nil {
		t.Fatal("Process succeeded on a cancelled fetch")
	}
	if got := rec.Calls(); len(got) != 0 {
		t.Errorf("recorded %+v for a cancelled run, want nothing", got)
	}
	if !sp.db.TakeAnnounceFound(job.ID) {
		t.Error("a cancelled run spent the flag; a run stopped by shutdown must leave it owed")
	}
}

// cancelOnFound records what it is sent and cancels the run once a "found"
// is: the run that announced then stops at waitForLive's first select rather
// than polling a YouTube the test does not have.
type cancelOnFound struct {
	notifications.Sender
	cancel func()
}

func (c cancelOnFound) Send(title, desc string, nt notifications.NotificationType, f []notifications.Field, o notifications.SendOptions) {
	c.Sender.Send(title, desc, nt, f, o)
	if o.Event == "found" {
		c.cancel()
	}
}

// A fetch can SUCCEED on a run already cancelled — GetVideoInfo returns the
// watch page's parse when the clients after it fail on the cancelled ctx —
// and that run stops there: it writes nothing and sends nothing, and leaves
// an owed flag owed. A shutdown landing mid-fetch on a fresh discovery used
// to send "YouTube Start Time Confirmed" on its own, and the restart's
// "Stream Found", carrying the same Starts At, came after it: the two embeds
// the deferral merged, apart and in reverse order. The restart below now
// sends the discovery as ONE embed. A job owed nothing (a manual add) stops
// the same way, with no confirmation for a run the operator just cancelled.
//
// Cancelled is what the row says too, not only ctx. The operator's Cancel
// (the real CancelJob below) writes the row before it cancels the run's ctx,
// and a fetch that returns in between — the window held open, as a slow
// subscriber to that write would hold it, until the run sends something —
// found ctx live and sent the confirmation for a job just cancelled. A
// channel removal's "delete its pending jobs" deletes the row and cancels no
// run at all, and the run sent the confirmation for a job no longer there.
//
// Mutants: Process going on past a cancelled fetch (run 1 sends the
// confirmation and writes the start time); the stop applied only to an owed
// job (the unowed run sends its confirmation); the stop returning anything
// but cancelledResult (processJob then records an error for a shutdown), or
// the row's stop doing so (every row-side run fails); the row not re-read,
// only ctx consulted (both Cancel runs and the channel removal send the
// confirmation); the re-read applied only to an owed job (the unowed Cancel
// run sends it); the re-read stopping on a Cancelled row but not a missing
// one (the channel removal sends it), or on a missing row but not a
// Cancelled one (both Cancel runs send it).
func TestCancelledSuccessfulFetchSendsNothing(t *testing.T) {
	info := func() *youtube.VideoInfo {
		return &youtube.VideoInfo{Title: "Fetched Title", ChannelName: "A Channel",
			ScheduledStartTime: "2026-10-20T18:00:00Z", IsUpcoming: true,
			StreamStatus: youtube.StreamUpcoming, PlayabilityError: youtube.PlayabilityOK}
	}
	cancelledFetch := func(sp *StreamProcessor, job *database.Job) *StreamProcessResult {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
			cancel() // the shutdown lands while the fetch runs, which still succeeds
			return info(), nil
		}
		res, err := sp.Process(ctx, job)
		if err != nil {
			t.Fatalf("cancelled run: %v", err)
		}
		return res
	}

	t.Run("owed discovery", func(t *testing.T) {
		sp, rec, job := foundTestJob(t)
		sp.cfg = &config.MoomboxConfig{}
		owe(t, sp.db, job)

		res := cancelledFetch(sp, job)
		if res == nil || !errors.Is(res.ErrSentinel, ErrCancelled) {
			t.Errorf("cancelled run returned %+v, want cancelledResult", res)
		}
		if got := rec.Calls(); len(got) != 0 {
			t.Fatalf("the cancelled run sent %+v, want nothing", got)
		}
		row, err := sp.db.GetJob(job.ID)
		if err != nil || row == nil {
			t.Fatalf("GetJob: %v", err)
		}
		if row.StreamStartTime != "" || row.Title != "Feed Title" {
			t.Errorf("the cancelled run wrote start %q, title %q; want the row as it was", row.StreamStartTime, row.Title)
		}
		if !row.AnnounceFound {
			t.Fatal("the cancelled run spent the flag; a shutdown must leave it owed")
		}

		// The restart: the flag is still owed, and its fetch takes it.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sp.notifier = cancelOnFound{Sender: rec, cancel: cancel}
		sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) { return info(), nil }
		if _, err := sp.Process(ctx, row); err != nil {
			t.Fatalf("restart: %v", err)
		}
		calls := rec.Calls()
		if len(calls) != 1 || calls[0].Opts.Event != "found" {
			t.Fatalf("the discovery arrived as %+v, want one \"found\"", calls)
		}
		if _, ok := calls[0].Field("Starts At"); !ok {
			t.Error("the restart's Stream Found carries no Starts At")
		}
	})

	t.Run("job owed nothing", func(t *testing.T) {
		sp, rec, job := foundTestJob(t)
		sp.cfg = &config.MoomboxConfig{}

		res := cancelledFetch(sp, job)
		if res == nil || !errors.Is(res.ErrSentinel, ErrCancelled) {
			t.Errorf("cancelled run returned %+v, want cancelledResult", res)
		}
		if got := rec.Calls(); len(got) != 0 {
			t.Errorf("the cancelled run sent %+v, want nothing", got)
		}
	})

	for _, owed := range []bool{true, false} {
		name := "operator's Cancel, write before ctx, owed discovery"
		if !owed {
			name = "operator's Cancel, write before ctx, job owed nothing"
		}
		t.Run(name, func(t *testing.T) {
			q := queuedFoundJob(t, owed)
			// The window every Cancel has between its write and its ctx
			// cancel, held open by a subscriber to that write until the
			// run sends something or returns.
			written, sent, returned := make(chan struct{}), make(chan struct{}, 8), make(chan struct{})
			unsub := q.db.OnJobUpdate(func(j *database.Job) {
				if j.ID != q.job.ID || j.Status != database.StatusCancelled {
					return
				}
				select {
				case <-written:
				default:
					close(written)
					select {
					case <-sent:
					case <-returned:
					case <-time.After(5 * time.Second):
					}
				}
			})
			defer unsub()
			q.sp.notifier = sendSignal{q.rec, sent}

			cancelDone := make(chan struct{})
			q.sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
				go func() {
					defer close(cancelDone)
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("CancelJob panicked: %v", r)
						}
					}()
					q.w.CancelJob(q.job.ID) // the operator clicks Cancel as the fetch completes
				}()
				select {
				case <-written:
				case <-time.After(5 * time.Second):
					t.Error("CancelJob's write never reached its subscribers")
				}
				return info(), nil
			}
			res, err := q.sp.Process(q.ctx, q.job)
			close(returned)
			<-cancelDone
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			if res == nil || !errors.Is(res.ErrSentinel, ErrCancelled) {
				t.Errorf("the cancelled run returned %+v, want cancelledResult", res)
			}
			if got := q.rec.Calls(); len(got) != 0 {
				t.Errorf("a run the operator cancelled during its fetch sent %+v, want nothing", got)
			}
			row, err := q.db.GetJob(q.job.ID)
			if err != nil || row == nil {
				t.Fatalf("GetJob: %v", err)
			}
			if row.Status != database.StatusCancelled || row.StreamStartTime != "" {
				t.Errorf("row status %q, start %q; want the Cancelled row as the Cancel left it", row.Status, row.StreamStartTime)
			}
		})
	}

	t.Run("channel removal deletes the row", func(t *testing.T) {
		sp, rec, job := foundTestJob(t)
		sp.cfg = &config.MoomboxConfig{}
		owe(t, sp.db, job)
		stagingBase := t.TempDir()
		sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
			// The operator removes the channel, deleting its pending jobs,
			// while the watch page loads.
			if n, _, err := DeletePendingChannelJobs(sp.db, stagingBase, "UC_abc"); err != nil || n != 1 {
				t.Fatalf("DeletePendingChannelJobs = %d, %v; want 1 deleted", n, err)
			}
			return info(), nil
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res, err := sp.Process(ctx, job)
		if err != nil {
			t.Fatalf("Process: %v", err)
		}
		if res == nil || !errors.Is(res.ErrSentinel, ErrCancelled) {
			t.Errorf("the run returned %+v, want cancelledResult", res)
		}
		if got := rec.Calls(); len(got) != 0 {
			t.Errorf("a job deleted with its channel during its fetch was sent %+v, want nothing", got)
		}
	})
}

// queuedFound is an Upcoming discovery held by a worker's queue, as
// processJob holds it: dequeued, so the worker's CancelJob cancels the ctx
// the run's Process is given.
type queuedFound struct {
	w   *DownloadWorker
	db  *database.Database
	sp  *StreamProcessor
	rec *notificationtest.Recorder
	job *database.Job
	ctx context.Context
}

// queuedFoundJob is foundTestJob's row in a worker's database, owed its
// "Stream Found" when owed is set.
func queuedFoundJob(t *testing.T, owed bool) queuedFound {
	t.Helper()
	w, db := testWorkerSetup(t)
	chID := "UC_abc"
	job := &database.Job{ID: "found1", VideoID: "found1", Platform: "youtube",
		Title: "Feed Title", ChannelName: "A Channel", Status: database.StatusUpcoming,
		ChannelID: &chID, AnnounceFound: owed}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob(job.ID)
	if err != nil || stored == nil || stored.AnnounceFound != owed {
		t.Fatalf("GetJob: %v %+v", err, stored)
	}
	w.queue.Enqueue(job.ID, database.StatusUpcoming)
	id, ctx, ok := w.queue.Dequeue(context.Background())
	if !ok || id != job.ID {
		t.Fatalf("Dequeue = %q, %v", id, ok)
	}
	rec := notificationtest.New()
	sp := &StreamProcessor{db: db, notifier: rec, logger: discardLogger{}, cfg: &config.MoomboxConfig{}}
	return queuedFound{w: w, db: db, sp: sp, rec: rec, job: stored, ctx: ctx}
}

// sendSignal records what it is sent and signals each send on sent.
type sendSignal struct {
	*notificationtest.Recorder
	sent chan struct{}
}

func (s sendSignal) Send(title, desc string, nt notifications.NotificationType, f []notifications.Field, o notifications.SendOptions) {
	s.Recorder.Send(title, desc, nt, f, o)
	select {
	case s.sent <- struct{}{}:
	default:
	}
}

// A Cancel that lands after the take leaves the row Cancelled. The run took
// its owed "Stream Found" first and sends it, as designed; the Cancel, made
// as CancelJob makes it (the row written, then the run's ctx cancelled),
// lands as it does. waitForLive's first write, which sets Upcoming, then
// applies only to a row with no outcome, and the loop stops. It used to be
// unconditional and turned the operator's Cancelled back into Upcoming, for
// as long as it took handleCancellation to write it again.
//
// Mutant: waitForLive's first write unconditional again (the row reads
// Upcoming).
func TestCancelAfterTheTakeLeavesTheRowCancelled(t *testing.T) {
	q := queuedFoundJob(t, true)
	q.sp.notifier = cancelOnFound{Sender: q.rec, cancel: func() { q.w.CancelJob(q.job.ID) }}
	q.sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
		return &youtube.VideoInfo{Title: "Fetched Title", ChannelName: "A Channel",
			ScheduledStartTime: "2026-10-20T18:00:00Z", IsUpcoming: true,
			StreamStatus: youtube.StreamUpcoming, PlayabilityError: youtube.PlayabilityOK}, nil
	}
	res, err := q.sp.Process(q.ctx, q.job)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res == nil || !errors.Is(res.ErrSentinel, ErrCancelled) {
		t.Errorf("the cancelled run returned %+v, want cancelledResult", res)
	}
	if calls := q.rec.Calls(); len(calls) != 1 || calls[0].Opts.Event != "found" {
		t.Errorf("recorded %+v, want the one \"found\" the run took", calls)
	}
	row, err := q.db.GetJob(q.job.ID)
	if err != nil || row == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if row.Status != database.StatusCancelled {
		t.Errorf("row status = %q after the Cancel, want %q", row.Status, database.StatusCancelled)
	}
}

// The later confirmation renders Starts At as "Stream Found" does, countdown
// included.
func TestLaterConfirmationRendersStartsAtLikeFound(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	const start = "2026-10-12T18:00:00Z"
	sp.updateJobMetadata(job, &youtube.VideoInfo{Title: "T", ScheduledStartTime: start, IsUpcoming: true}, true)

	got := rec.ByEvent("scheduled")
	if len(got) != 1 {
		t.Fatalf("recorded %+v, want one \"scheduled\"", rec.Calls())
	}
	ts, _ := time.Parse(time.RFC3339, start)
	if v, _ := got[0].Field("Starts At"); v != notifications.StartsAtValue(ts) {
		t.Errorf("Starts At = %q, want %q", v, notifications.StartsAtValue(ts))
	}
}

// Process honours the row's flag: an owed job's first fetch announces it,
// exactly once, and an unowed one's — a manual add, a job from before the
// flag existed — announces nothing. Driven through Process with a VOD, whose
// processing returns at once, so the test reaches the real take.
//
// Mutants: Process ignoring the flag (the unowed run announces, or the owed
// one does not); a flag not consumed by the run (the second run announces
// again).
func TestProcessAnnouncesOnlyAnOwedJob(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.getVideoInfoFn = func(context.Context, string) (*youtube.VideoInfo, error) {
		return &youtube.VideoInfo{Title: "T", StreamStatus: youtube.StreamVOD, PlayabilityError: youtube.PlayabilityOK}, nil
	}

	if _, err := sp.Process(context.Background(), job); err != nil {
		t.Fatalf("Process (unowed): %v", err)
	}
	if got := len(rec.ByEvent("found")); got != 0 {
		t.Fatalf("an unowed run announced %d discoveries, want 0", got)
	}

	owe(t, sp.db, job)
	for range 2 {
		// Re-read the row each run, as processJob does: the second run must
		// find the flag cleared in the DATABASE, not merely in this struct.
		fresh, err := sp.db.GetJob(job.ID)
		if err != nil || fresh == nil {
			t.Fatalf("GetJob: %v", err)
		}
		if _, err := sp.Process(context.Background(), fresh); err != nil {
			t.Fatalf("Process (owed): %v", err)
		}
	}
	found := rec.ByEvent("found")
	if len(found) != 1 {
		t.Fatalf("two runs on one flag announced %d discoveries, want 1", len(found))
	}
	if found[0].Description != "Video: T" {
		t.Errorf("description = %q, want \"Video: T\"", found[0].Description)
	}
}

// A cancel clears the flag, so a later Retry of the row does not announce a
// discovery long past.
//
// Mutant: announce_found dropped from CancelJob's write.
func TestCancelClearsTheOwedFound(t *testing.T) {
	w, db := testWorkerSetup(t)
	job := &database.Job{ID: "found_cancel", VideoID: "found_cancel", Platform: "youtube", Status: database.StatusUpcoming, AnnounceFound: true}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if cancelled, _ := w.CancelJob(job.ID); !cancelled {
		t.Fatal("CancelJob did not cancel the Upcoming row")
	}
	if db.TakeAnnounceFound(job.ID) {
		t.Error("the owed Stream Found survived the cancel")
	}
}

// Each kind of find says what it is, and carries only the fields that kind
// has: a schedule for an upcoming one, a length where a video exists, an end
// or publish date for a finished one. The last three rows are what any find
// may carry besides: a refused fetch's neutral wording, the restriction it
// names, and the description.
//
// Mutants: foundKind ignoring IsPremiere (the premiere rows read "Upcoming:" /
// "Live:"); Length set for a live broadcast; Ended or Published set for an
// upcoming stream; a day-only publish date rendered as a <t:…> moment, or a
// timestamped "day" date rendered as its UTC date (the wrong day for 20:00
// Pacific).
func TestDeferredFoundSaysWhatItFound(t *testing.T) {
	const start = "2026-10-12T18:00:00Z"
	startTS, _ := time.Parse(time.RFC3339, start)
	for _, tc := range []struct {
		name      string
		info      youtube.VideoInfo
		wantLead  string
		wantField map[string]string // "" = must be absent
	}{
		// End and publish dates an upcoming stream must NOT show, so a mutant
		// that sets them for every kind is seen.
		{"upcoming stream",
			youtube.VideoInfo{ScheduledStartTime: start, IsUpcoming: true, StreamStatus: youtube.StreamUpcoming,
				EndTimestamp: "2026-10-10T20:00:00Z", PublishedAt: "2026-10-09T12:00:00Z", PublishedPrecision: "started"},
			"Upcoming: ",
			map[string]string{"Starts At": fmt.Sprintf("<t:%d:f> (<t:%d:R>)", startTS.Unix(), startTS.Unix()), "Length": "", "Ended": "", "Published": ""}},
		{"upcoming premiere",
			youtube.VideoInfo{ScheduledStartTime: start, IsUpcoming: true, IsPremiere: true, LengthSeconds: new(600), StreamStatus: youtube.StreamUpcoming},
			"Upcoming premiere: ",
			map[string]string{"Length": "10m 0s"}},
		{"live broadcast",
			youtube.VideoInfo{ScheduledStartTime: start, IsLive: true, LengthSeconds: new(42), StreamStatus: youtube.StreamLive},
			"Live: ",
			map[string]string{"Starts At": "", "Length": ""}},
		{"premiering now",
			youtube.VideoInfo{IsLive: true, IsPremiere: true, StreamStatus: youtube.StreamLive},
			"Premiering: ", nil},
		{"ended broadcast",
			youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, IsPostLiveDVR: true, LengthSeconds: new(3725),
				EndTimestamp: "2026-10-10T20:00:00Z", PublishedAt: "2026-10-10T18:00:00Z", PublishedPrecision: "started"},
			"Ended stream: ",
			map[string]string{"Length": "1h 2m", "Ended": "<t:1791662400:f>", "Published": "", "Starts At": ""}},
		{"upload, day-precision publish date",
			youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, LengthSeconds: new(90),
				PublishedAt: "2026-10-09T23:59:59Z", PublishedPrecision: "day", UploadDate: "2026-10-09"},
			"Video: ",
			map[string]string{"Published": "Oct 9, 2026", "Ended": ""}},
		// "day" precision, but YouTube gave a time and an offset: the parse
		// converted it to its exact instant (20:00 Pacific on Oct 9 is 03:00 UTC
		// on Oct 10), so it renders as a moment, not as the UTC date.
		{"upload, day-precision publish date with a time",
			youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream,
				PublishedAt: "2026-10-10T03:00:00Z", PublishedPrecision: "day", UploadDate: "2026-10-09T20:00:00-07:00"},
			"Video: ",
			map[string]string{"Published": "<t:1791601200:f>"}},
		{"VOD, exact publish time",
			youtube.VideoInfo{StreamStatus: youtube.StreamVOD, PublishedAt: "2026-10-09T12:00:00Z", PublishedPrecision: "started"},
			"Video: ",
			map[string]string{"Published": "<t:1791547200:f>"}},
		// A refused fetch says little that can be trusted about what the video
		// is — a members-only response classifies as not_a_stream — so the
		// embed keeps its neutral wording, carries no Length, and names the
		// refusal. The shape is the one the parse produces for a refusal:
		// not_a_stream, not upcoming, no start time (its upload date is no
		// start), and a length it cannot vouch for.
		//
		// Mutant: foundKind classifying a refused fetch ("Video: T", Length
		// present).
		{"refused fetch keeps the neutral wording",
			youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, LengthSeconds: new(600),
				PlayabilityError: youtube.PlayabilityMembersOnly},
			"Found matching stream: ",
			map[string]string{"Length": "", "Access": "Members only", "Starts At": ""}},
		// A restriction on watching is named. The description is excerpted
		// into its own field.
		{"restricted, with a description",
			youtube.VideoInfo{IsUpcoming: true, Description: "Join us *live*", PlayabilityError: youtube.PlayabilityMembersOnly},
			"Found matching stream: ",
			map[string]string{"Access": "Members only", "Description": `Join us \*live\*`}},
		// An open video carries no Access field.
		{"open video",
			youtube.VideoInfo{IsUpcoming: true, PlayabilityError: youtube.PlayabilityOK},
			"Upcoming: ",
			map[string]string{"Access": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, rec, job := foundTestJob(t)
			tc.info.Title = "T"
			sp.applyJobMetadata(job, &tc.info, false, true)
			found := rec.ByEvent("found")
			if len(found) != 1 {
				t.Fatalf("recorded %+v, want one \"found\"", rec.Calls())
			}
			if want := tc.wantLead + "T"; found[0].Description != want {
				t.Errorf("description = %q, want %q", found[0].Description, want)
			}
			for name, want := range tc.wantField {
				got, ok := found[0].Field(name)
				switch {
				case want == "" && ok:
					t.Errorf("%s = %q, want no such field", name, got)
				case want != "" && got != want:
					t.Errorf("%s = %q (present %v), want %q", name, got, ok, want)
				}
			}
		})
	}
}

// An unscheduled waiting room's early chat still gets an epoch. Without one
// every live message is written with no offset and piles up at 0 in the
// player; the upload date was that epoch while the start time fell back to it.
//
// Mutant: earlyChatEpoch returning ScheduledStartTime alone.
func TestEarlyChatEpochFallsBackToTheUploadDate(t *testing.T) {
	for _, tc := range []struct {
		name string
		info youtube.VideoInfo
		want string
	}{
		{"scheduled", youtube.VideoInfo{ScheduledStartTime: "2026-10-12T18:00:00Z", UploadDate: "2026-10-01T08:00:00-07:00"}, "2026-10-12T18:00:00Z"},
		{"unscheduled", youtube.VideoInfo{UploadDate: "2026-10-01T08:00:00-07:00"}, "2026-10-01T08:00:00-07:00"},
		{"neither", youtube.VideoInfo{}, ""},
	} {
		if got := earlyChatEpoch(&tc.info); got != tc.want {
			t.Errorf("%s: epoch = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A stream with no schedule can go live at any moment, so it is probed at the
// imminent cadence — the 30 s it effectively had while its start time fell
// back to its (past) upload date. A schedule still selects its band.
//
// Mutant: the no-schedule arm returning probeIntervalNear.
func TestUnscheduledStreamIsProbedAsImminent(t *testing.T) {
	sp := &StreamProcessor{}
	if got := sp.calculateProbeInterval(&youtube.VideoInfo{}); got != probeIntervalImminent {
		t.Errorf("no schedule: interval %v, want %v", got, probeIntervalImminent)
	}
	far := time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339)
	if got := sp.calculateProbeInterval(&youtube.VideoInfo{ScheduledStartTime: far}); got != probeIntervalDistant {
		t.Errorf("scheduled 3 h out: interval %v, want %v", got, probeIntervalDistant)
	}
}

// owe sets job's announce_found flag, as the monitor's creator does in the
// row it inserts.
func owe(t *testing.T, db *database.Database, job *database.Job) {
	t.Helper()
	if j := db.UpdateJobFields(job.ID, map[string]any{"announce_found": true}); j == nil || !j.AnnounceFound {
		t.Fatalf("could not set announce_found on %s: %+v", job.ID, j)
	}
	job.AnnounceFound = true // as processJob would read it
}

// A re-run clears an owed "Stream Found": a job whose run ended before its
// first fetch (a panic, a park) and is retried or resumed later would
// otherwise announce a discovery long past.
//
// Mutants: announce_found dropped from ReinitializeJob's, AutoReinitializeJob's
// or ResumeJob's reset.
func TestRerunsClearTheOwedFound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rerun func(w *DownloadWorker, id string)
	}{
		{"retry", (*DownloadWorker).ReinitializeJob},
		{"auto-reinitialize", (*DownloadWorker).AutoReinitializeJob},
		{"resume", (*DownloadWorker).ResumeJob},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, db := testWorkerSetup(t)
			job := &database.Job{ID: "rerun_" + tc.name, VideoID: "rerun", Platform: "youtube",
				Status: database.StatusError, AnnounceFound: true}
			if _, err := db.AddJob(job); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			tc.rerun(w, job.ID)
			if db.TakeAnnounceFound(job.ID) {
				t.Errorf("the owed Stream Found survived the %s", tc.name)
			}
		})
	}
}
