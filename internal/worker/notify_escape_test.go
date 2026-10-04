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
