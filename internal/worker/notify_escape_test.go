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
// raw form — the four hand-built sends below used to inject the raw text
// while every builder in internal/notifications and the worker's own
// Authentication Required / Job Failed sends went through EscapeMarkdown.
// fieldName/fieldRaw name the one job-text field the send carries (Channel,
// or Source Video for the trim-deleted send, which repeats the title).
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
	val, ok := call.Field(fieldName)
	if !ok {
		t.Fatalf("%s: no %q field in %+v", event, fieldName, call.Fields)
	}
	if want := notifications.EscapeMarkdown(fieldRaw); val != want {
		t.Errorf("%s: %s field = %q, want the escaped %q", event, fieldName, val, want)
	}
}

// TestHandBuiltSendsEscapeJobText covers the four sends that build their
// embed by hand rather than through a builder: `trim_error` (sendTrimFailed),
// `trim_deleted` (TrimService.DeleteTrim), and the `scheduled` /
// `rescheduled` pair in updateJobMetadata. The time fields of the last two
// carry <t:…> markup on purpose and are left alone (EscapeMarkdown's doc);
// only the title and the channel are job text.
//
// MUTANT: dropping any one EscapeMarkdown wrap — that send's description or
// field carries the raw "*Live*".
func TestHandBuiltSendsEscapeJobText(t *testing.T) {
	job := &database.Job{
		ID: "yt_esc", VideoID: "esc", Platform: "youtube",
		Title: markdownTitle, ChannelName: markdownChannel,
		Status: database.StatusFinished,
	}

	t.Run("trim_error", func(t *testing.T) {
		rec := notificationtest.New()
		o := &DownloadOrchestrator{notifier: rec, logger: discardLogger{}}
		o.sendTrimFailed(job, errors.New("ffmpeg: exit status 1"))
		assertEscaped(t, rec, "trim_error", "Channel", markdownChannel)
	})

	t.Run("trim_deleted", func(t *testing.T) {
		db, err := database.Open(filepath.Join(t.TempDir(), "trim.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		if _, err := db.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		if err := db.AddTrim(&database.TrimRecord{
			ID: "trim1", JobID: job.ID, StartTime: 10, EndTime: 40,
			Filename: "clip.mp4", CreatedAt: "2026-09-27T00:00:00Z", Duration: 30,
		}); err != nil {
			t.Fatalf("AddTrim: %v", err)
		}
		rec := notificationtest.New()
		ts := NewTrimService(db, "ffmpeg", discardLogger{})
		ts.SetNotifier(rec)
		if err := ts.DeleteTrim(job.ID, "trim1"); err != nil {
			t.Fatalf("DeleteTrim: %v", err)
		}
		assertEscaped(t, rec, "trim_deleted", "Source Video", markdownTitle)
	})

	t.Run("scheduled and rescheduled", func(t *testing.T) {
		db, err := database.Open(filepath.Join(t.TempDir(), "sched.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		if _, err := db.AddJob(&database.Job{
			ID: "yt_sched_esc", VideoID: "sched", Platform: "youtube",
			Status: database.StatusUpcoming,
		}); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		stored, err := db.GetJob("yt_sched_esc")
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		rec := notificationtest.New()
		sp := &StreamProcessor{db: db, notifier: rec}
		// Driven exactly as TestLifecycleScheduleSendsCarryTheirJob drives
		// the pair: first sight confirms, a moved time reschedules.
		sp.updateJobMetadata(stored, &youtube.VideoInfo{
			Title: markdownTitle, ChannelName: markdownChannel,
			ScheduledStartTime: "2026-10-01T12:00:00Z", IsUpcoming: true,
		}, false)
		sp.updateJobMetadata(stored, &youtube.VideoInfo{
			Title: markdownTitle, ChannelName: markdownChannel,
			ScheduledStartTime: "2026-10-01T14:00:00Z", IsUpcoming: true,
		}, true)
		assertEscaped(t, rec, "scheduled", "Channel", markdownChannel)
		assertEscaped(t, rec, "rescheduled", "Channel", markdownChannel)
		// The timestamp fields are markup on purpose and stay markup.
		for _, ev := range []string{"scheduled", "rescheduled"} {
			for _, f := range rec.ByEvent(ev)[0].Fields {
				if f.Name == "Starts At" || f.Name == "New Time" || f.Name == "Old Time" {
					if !strings.HasPrefix(f.Value, "<t:") {
						t.Errorf("%s: %s = %q, want the <t:…> markup left unescaped", ev, f.Name, f.Value)
					}
				}
			}
		}
	})
}

// TestLifecycleSendsEscapeJobText covers the lifecycle sends that still put
// the raw title and channel into their embed: the gap and quality splits, the
// Twitch session resume/split, and Muxing Starting (title only — its fields
// are counts). A title like "*LIVE* <3 || secret" italicised the rest of the
// description or opened a spoiler.
//
// MUTANT: dropping any one EscapeMarkdown wrap — that send carries the raw
// "*Live*".
func TestLifecycleSendsEscapeJobText(t *testing.T) {
	job := &database.Job{
		ID: "tw_esc", VideoID: "esc", Platform: "twitch",
		Title: markdownTitle, ChannelName: markdownChannel,
		Status: database.StatusDownloading,
	}
	q := QualityInfo{Label: "1080p60", Width: 1920, Height: 1080, FPS: 60}

	newOrch := func(t *testing.T) (*DownloadOrchestrator, *notificationtest.Recorder, *JobContext) {
		t.Helper()
		db, err := database.Open(filepath.Join(t.TempDir(), "esc.db"))
		if err != nil {
			t.Fatalf("database.Open: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		if _, err := db.AddJob(job); err != nil {
			t.Fatalf("AddJob: %v", err)
		}
		rec := notificationtest.New()
		return &DownloadOrchestrator{notifier: rec, db: db, logger: discardLogger{}}, rec, &JobContext{Job: job, DB: db}
	}

	t.Run("gap_split", func(t *testing.T) {
		o, rec, jc := newOrch(t)
		o.sendGapSplitNotification(jc, 0, q)
		assertEscaped(t, rec, "gap_split", "Channel", markdownChannel)
	})
	t.Run("quality_split", func(t *testing.T) {
		o, rec, jc := newOrch(t)
		o.sendQualitySplitNotification(jc, "Twitch", q, QualityInfo{Label: "720p", Height: 720}, 0, true)
		assertEscaped(t, rec, "quality_split", "Channel", markdownChannel)
	})
	t.Run("connectivity_resume", func(t *testing.T) {
		o, rec, jc := newOrch(t)
		o.sendTwitchSessionNotification(jc, "Twitch Download Resumed",
			"Connectivity restored, resuming download: "+notifications.EscapeMarkdown(job.Title),
			notifications.TypeDownload, "connectivity_resume", q, 2)
		assertEscaped(t, rec, "connectivity_resume", "Channel", markdownChannel)
	})
	t.Run("muxing", func(t *testing.T) {
		o, rec, jc := newOrch(t)
		o.sendMuxingStarting(jc)
		got := rec.ByEvent("muxing")
		if len(got) != 1 {
			t.Fatalf("muxing sends = %d, want 1", len(got))
		}
		if want := notifications.EscapeMarkdown(markdownTitle); !strings.Contains(got[0].Description, want) ||
			strings.Contains(got[0].Description, markdownTitle) {
			t.Errorf("muxing description = %q, want the escaped title %q", got[0].Description, want)
		}
	})
}
