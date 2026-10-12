package worker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

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
	db, err := database.Open(filepath.Join(t.TempDir(), "found.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
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

// A refused fetch says little that can be trusted about what the video is —
// a members-only response classifies as not_a_stream — so the embed keeps its
// neutral wording, carries no Length, and names the refusal. The shape is
// the one the parse produces for a refusal: not_a_stream, not upcoming, no
// start time (its upload date is no start), and a length it cannot vouch for.
//
// Mutant: foundKind classifying a refused fetch ("Video: T", Length present).
func TestRefusedFetchKeepsTheNeutralWording(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.applyJobMetadata(job, &youtube.VideoInfo{
		Title: "T", StreamStatus: youtube.StreamNotAStream, LengthSeconds: intPtr(600),
		PlayabilityError: youtube.PlayabilityMembersOnly,
	}, false, true)

	found := rec.ByEvent("found")
	if len(found) != 1 {
		t.Fatalf("recorded %+v, want one \"found\"", rec.Calls())
	}
	if want := "Found matching stream: T"; found[0].Description != want {
		t.Errorf("description = %q, want %q", found[0].Description, want)
	}
	if v, ok := found[0].Field("Length"); ok {
		t.Errorf("Length = %q on a refused fetch, want no field", v)
	}
	if v, _ := found[0].Field("Access"); v != "Members only" {
		t.Errorf("Access = %q, want \"Members only\"", v)
	}
	if v, ok := found[0].Field("Starts At"); ok {
		t.Errorf("Starts At = %q on a refused fetch, want no field", v)
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

// An unowed job's first fetch still confirms its schedule — a manual add
// keeps the embed it had.
func TestUnowedJobKeepsTheConfirmation(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.applyJobMetadata(job, &youtube.VideoInfo{
		Title: "T", ScheduledStartTime: "2026-10-12T18:00:00Z", IsUpcoming: true,
	}, false, false)

	if got := len(rec.ByEvent("found")); got != 0 {
		t.Errorf("recorded %d \"found\" embeds for an unowed job, want 0", got)
	}
	if got := len(rec.ByEvent("scheduled")); got != 1 {
		t.Errorf("recorded %d \"scheduled\" embeds, want 1", got)
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

func intPtr(n int) *int { return &n }

// Each kind of find says what it is, and carries only the fields that kind
// has: a schedule for an upcoming one, a length where a video exists, an end
// or publish date for a finished one.
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
			youtube.VideoInfo{ScheduledStartTime: start, IsUpcoming: true, IsPremiere: true, LengthSeconds: intPtr(600), StreamStatus: youtube.StreamUpcoming},
			"Upcoming premiere: ",
			map[string]string{"Length": "10m 0s"}},
		{"live broadcast",
			youtube.VideoInfo{ScheduledStartTime: start, IsLive: true, LengthSeconds: intPtr(42), StreamStatus: youtube.StreamLive},
			"Live: ",
			map[string]string{"Starts At": "", "Length": ""}},
		{"premiering now",
			youtube.VideoInfo{IsLive: true, IsPremiere: true, StreamStatus: youtube.StreamLive},
			"Premiering: ", nil},
		{"ended broadcast",
			youtube.VideoInfo{StreamStatus: youtube.StreamPostLive, IsPostLiveDVR: true, LengthSeconds: intPtr(3725),
				EndTimestamp: "2026-10-10T20:00:00Z", PublishedAt: "2026-10-10T18:00:00Z", PublishedPrecision: "started"},
			"Ended stream: ",
			map[string]string{"Length": "1h 2m", "Ended": "<t:1791662400:f>", "Published": "", "Starts At": ""}},
		{"upload, day-precision publish date",
			youtube.VideoInfo{StreamStatus: youtube.StreamNotAStream, LengthSeconds: intPtr(90),
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

// A restriction on watching is named; an open video carries no Access field.
// The description is excerpted into its own field.
func TestDeferredFoundCarriesAccessAndDescription(t *testing.T) {
	sp, rec, job := foundTestJob(t)
	sp.applyJobMetadata(job, &youtube.VideoInfo{
		Title: "T", IsUpcoming: true, Description: "Join us *live*",
		PlayabilityError: youtube.PlayabilityMembersOnly,
	}, false, true)
	found := rec.ByEvent("found")
	if len(found) != 1 {
		t.Fatalf("recorded %+v, want one \"found\"", rec.Calls())
	}
	if v, _ := found[0].Field("Access"); v != "Members only" {
		t.Errorf("Access = %q, want \"Members only\"", v)
	}
	if v, _ := found[0].Field("Description"); v != `Join us \*live\*` {
		t.Errorf("Description = %q, want the escaped description", v)
	}

	sp2, rec2, job2 := foundTestJob(t)
	sp2.applyJobMetadata(job2, &youtube.VideoInfo{Title: "T", IsUpcoming: true, PlayabilityError: youtube.PlayabilityOK}, false, true)
	if v, ok := rec2.ByEvent("found")[0].Field("Access"); ok {
		t.Errorf("Access = %q on an open video, want no field", v)
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
// Mutants: announce_found dropped from ReinitializeJob's or ResumeJob's reset.
func TestRerunsClearTheOwedFound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rerun func(w *DownloadWorker, id string)
	}{
		{"retry", (*DownloadWorker).ReinitializeJob},
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
