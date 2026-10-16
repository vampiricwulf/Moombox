package worker

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// Job-supplied text with every character Discord's markdown subset gives
// meaning to, so an unescaped send is visible in the recorded call.
const (
	markdownTitle   = "*Live* _now_ ~tilde~ |pipe| `tick` <3"
	markdownChannel = "__The_Channel__"
)

// assertEscaped checks that the recorded send carries the ESCAPED form of
// title and channel in its description and in the named field, and never the
// raw form — the hand-built sends below used to inject the raw text while
// every builder in internal/notifications and the worker's own Authentication
// Required / Job Failed sends went through EscapeMarkdown. fieldName/fieldRaw
// name the one job-text field the send carries (Channel, or Source Video for
// the trim-deleted send, which repeats the title); an empty fieldName checks
// the description alone, for Muxing Starting, whose fields are counts.
func assertEscaped(t *testing.T, rec *notificationtest.Recorder, event, fieldName, fieldRaw string) {
	t.Helper()
	got := rec.ByEvent(event)
	if len(got) != 1 {
		t.Fatalf("%s sends = %d, want 1: %+v", event, len(got), rec.Calls())
	}
	call := got[0]
	wantTitle := notifications.EscapeMarkdown(markdownTitle)
	if !strings.Contains(call.Description, wantTitle) {
		t.Errorf("%s: description = %q, want it to carry the escaped title %q", event, call.Description, wantTitle)
	}
	if strings.Contains(call.Description, markdownTitle) {
		t.Errorf("%s: description = %q carries the RAW title — markdown injected", event, call.Description)
	}
	if fieldName == "" {
		return
	}
	val, ok := call.Field(fieldName)
	if !ok {
		t.Fatalf("%s: no %q field in %+v", event, fieldName, call.Fields)
	}
	if want := notifications.EscapeMarkdown(fieldRaw); val != want {
		t.Errorf("%s: %s field = %q, want the escaped %q", event, fieldName, val, want)
	}
}

// TestHandBuiltSendsEscapeAndCarryTheirJob drives each send that builds its
// embed by hand rather than through a builder once, on a job whose title and
// channel carry markdown, and holds the embed to both things it owes that job:
// the job's text arrives escaped (assertEscaped) and the embed names its job
// (assertCarriesItsJob, notify_lifecycle_ids_test.go). A title like
// "*LIVE* <3 || secret" italicised the rest of the description or opened a
// spoiler.
//
// The time fields of `scheduled` and `rescheduled` carry <t:…> markup on
// purpose and are left alone (EscapeMarkdown's doc); only the title and the
// channel are job text.
//
// MUTANT: dropping any one EscapeMarkdown wrap — that send's description or
// field carries the raw "*Live*".
func TestHandBuiltSendsEscapeAndCarryTheirJob(t *testing.T) {
	q := QualityInfo{Label: "1080p60", Width: 1920, Height: 1080, FPS: 60}
	// stored adds job to db and returns the row as read back.
	stored := func(t *testing.T, db *database.Database, job *database.Job) *database.Job {
		t.Helper()
		if _, err := db.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		row, err := db.GetJob(job.ID)
		if err != nil || row == nil {
			t.Fatalf("GetJob: %v", err)
		}
		return row
	}
	// marked is a job carrying the markdown title and channel.
	marked := func(id, platform string, status database.JobStatus) *database.Job {
		return &database.Job{ID: id, VideoID: id, Platform: platform,
			Title: markdownTitle, ChannelName: markdownChannel, Status: status}
	}
	orch := func(db *database.Database, rec *notificationtest.Recorder) *DownloadOrchestrator {
		return &DownloadOrchestrator{db: db, notifier: rec, logger: discardLogger{}}
	}
	// schedulePair drives the `scheduled` and `rescheduled` halves of
	// updateJobMetadata: first sight confirms, a moved time reschedules, and
	// the confirmation does not fire twice. One processor and one row for
	// both: the reschedule only fires on a refresh whose ScheduledStartTime
	// differs from the one already stored, so the second call needs the first
	// one's write. The row starts with no title or channel; the fetches
	// supply them.
	schedulePair := func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job {
		job := stored(t, db, &database.Job{ID: "yt_sched", VideoID: "sched", Platform: "youtube",
			Status: database.StatusUpcoming})
		sp := &StreamProcessor{db: db, notifier: rec}
		sp.updateJobMetadata(job, &youtube.VideoInfo{
			Title: markdownTitle, ChannelName: markdownChannel,
			ScheduledStartTime: "2026-10-01T12:00:00Z", IsUpcoming: true,
		}, false)
		sp.updateJobMetadata(job, &youtube.VideoInfo{
			Title: markdownTitle, ChannelName: markdownChannel,
			ScheduledStartTime: "2026-10-01T14:00:00Z", IsUpcoming: true,
		}, true)
		return job
	}
	// twitchSession drives sendTwitchSessionNotification as its caller for
	// event does.
	twitchSession := func(event, title, desc string, part int) func(*testing.T, *database.Database, *notificationtest.Recorder) *database.Job {
		return func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job {
			job := stored(t, db, marked("tw_esc", "twitch", database.StatusDownloading))
			orch(db, rec).sendTwitchSessionNotification(&JobContext{Job: job}, title,
				desc+notifications.EscapeMarkdown(job.Title), notifications.TypeDownload, event, q, part)
			return job
		}
	}

	for _, tc := range []struct {
		event string
		// field names the one job-text field the send carries and fieldRaw its
		// raw value; "" checks the description alone.
		field, fieldRaw string
		// drive makes the send and returns the job it was made for.
		drive func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job
	}{
		// `trim_error` (sendTrimFailed, orchestrator.go) and `trim_deleted`
		// (TrimService.DeleteTrim, trim.go) were the last two producers that
		// built their SendOptions by hand. Neither is a lifecycle event — both
		// stay separate posts, which is what lets them ping an edit-mode
		// target — but the footer's platform, the author line and the
		// dashboard deep link are the job's either way, and Manager.Send
		// applies the deep link only when publicURL, JobID and Author are all
		// there. This closes the last of the "bare SendOptions" sites (N2b
		// review L2).
		{"trim_error", "Channel", markdownChannel, func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job {
			job := stored(t, db, marked("yt_esc", "youtube", database.StatusFinished))
			orch(db, rec).sendTrimFailed(job, errors.New("ffmpeg: exit status 1"))
			return job
		}},
		{"trim_deleted", "Source Video", markdownTitle, func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job {
			job := stored(t, db, marked("yt_esc", "youtube", database.StatusFinished))
			if err := db.AddTrim(&database.TrimRecord{
				ID: "trim1", JobID: job.ID, StartTime: 10, EndTime: 40,
				Filename: "clip.mp4", CreatedAt: "2026-09-27T00:00:00Z", Duration: 30,
			}); err != nil {
				t.Fatalf("AddTrim: %v", err)
			}
			ts := NewTrimService(db, "ffmpeg", discardLogger{})
			ts.SetNotifier(rec)
			if err := ts.DeleteTrim(job.ID, "trim1"); err != nil {
				t.Fatalf("DeleteTrim: %v", err)
			}
			return job
		}},
		{"scheduled", "Channel", markdownChannel, schedulePair},
		{"rescheduled", "Channel", markdownChannel, schedulePair},
		// `gap_split` and `quality_split`, the two mid-download part
		// boundaries.
		{"gap_split", "Channel", markdownChannel, func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job {
			job := stored(t, db, marked("tw_esc", "twitch", database.StatusDownloading))
			orch(db, rec).sendGapSplitNotification(&JobContext{Job: job}, 0, q)
			return job
		}},
		{"quality_split", "Channel", markdownChannel, func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job {
			job := stored(t, db, marked("tw_esc", "twitch", database.StatusDownloading))
			orch(db, rec).sendQualitySplitNotification(&JobContext{Job: job}, "Twitch", q, QualityInfo{Label: "720p", Height: 720}, 0, true)
			return job
		}},
		// Both keys sendTwitchSessionNotification delivers. The event stays a
		// PARAMETER of that helper — only the three identity fields are added
		// — so both of its callers have to be exercised to prove the change is
		// in the helper and not in one call site.
		{"connectivity_resume", "Channel", markdownChannel, twitchSession("connectivity_resume",
			"Twitch Download Resumed", "Connectivity restored, resuming download: ", 2)},
		{"connectivity_split", "Channel", markdownChannel, twitchSession("connectivity_split",
			"Twitch Download Finalizing — Connectivity Lost", "Broadcast ended while connectivity was down: ", 3)},
		// `muxing`: title only — its fields are counts. sendMuxingStarting
		// re-reads the row before building its embed, so the fixture writes
		// the channel the author line needs to the DATABASE, not just to the
		// JobContext.
		{"muxing", "", "", func(t *testing.T, db *database.Database, rec *notificationtest.Recorder) *database.Job {
			job := stored(t, db, marked("tw_esc", "twitch", database.StatusDownloading))
			orch(db, rec).sendMuxingStarting(&JobContext{Job: job})
			return job
		}},
	} {
		t.Run(tc.event, func(t *testing.T) {
			db, err := database.Open(filepath.Join(t.TempDir(), "esc.db"))
			if err != nil {
				t.Fatalf("database.Open: %v", err)
			}
			t.Cleanup(func() { db.Close() })
			rec := notificationtest.New()

			job := tc.drive(t, db, rec)

			assertEscaped(t, rec, tc.event, tc.field, tc.fieldRaw)
			assertCarriesItsJob(t, rec, tc.event, job)
			// The timestamp fields are markup on purpose and stay markup.
			for _, f := range rec.ByEvent(tc.event)[0].Fields {
				if f.Name == "Starts At" || f.Name == "New Time" || f.Name == "Old Time" {
					if !strings.HasPrefix(f.Value, "<t:") {
						t.Errorf("%s: %s = %q, want the <t:…> markup left unescaped", tc.event, f.Name, f.Value)
					}
				}
			}
		})
	}
}
