package worker

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
	"github.com/vampiricwulf/Moombox/internal/youtube"
)

// TestStreamProcessorNotifiesThroughTheSenderSeam is the whole point of the
// seam: 43 of the 46 trigger rows the audit inventoried have no test because
// every producer holds the CONCRETE *notifications.Manager, whose only
// constructor builds targets from Discord webhook URLs. Nothing outside the
// notifications package can substitute a recorder, so nothing asserts what an
// operator actually receives.
//
// This test does not assert the embed's content — that is Task 6's job. It
// asserts that a recorder can be INSTALLED at all, which is the change.
//
// THE MUTANT: revert any one of the field/parameter types to
// *notifications.Manager and this file stops compiling.
func TestStreamProcessorNotifiesThroughTheSenderSeam(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "seam.db")
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	rec := notificationtest.New()
	sp := &StreamProcessor{db: db, notifier: rec}

	job := &database.Job{ID: "yt_seam", VideoID: "seam", Status: database.StatusUpcoming}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	stored, err := db.GetJob("yt_seam")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	sp.updateJobMetadata(stored, &youtube.VideoInfo{
		Title:              "A Scheduled Stream",
		ChannelName:        "A Channel",
		ScheduledStartTime: "2026-10-01T12:00:00Z",
		IsUpcoming:         true,
	}, false)

	if got := rec.ByEvent("scheduled"); len(got) != 1 {
		t.Fatalf("recorded %d \"scheduled\" notifications, want 1 — the seam did not carry the send: %+v", len(got), rec.Calls())
	}
}

// TestScheduledDoesNotFireForAStreamAlreadyLive is C6. The condition was
// `IsUpcoming || IsLive`, so a stream first observed already live produced a
// "Scheduled: <title>" embed seconds before "YouTube Download Starting" —
// which carries the same start time in its own "Scheduled For" field. Two
// embeds for one moment, the first of them announcing a schedule for a stream
// that had already begun.
//
// THE MUTANT: restoring the ||. The "already live" subtest then records one.
func TestScheduledDoesNotFireForAStreamAlreadyLive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		isUpcoming bool
		isLive     bool
		want       int
	}{
		{"upcoming", true, false, 1},
		{"already live on first sight", true, true, 0},
		{"live, not upcoming", false, true, 0},
		{"neither (a VOD)", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := database.Open(filepath.Join(t.TempDir(), "c6.db"))
			if err != nil {
				t.Fatalf("database.Open: %v", err)
			}
			t.Cleanup(func() { db.Close() })

			rec := notificationtest.New()
			sp := &StreamProcessor{db: db, notifier: rec}
			job := &database.Job{ID: "yt_c6", VideoID: "c6", Status: database.StatusUpcoming}
			if _, err := db.AddJob(job); err != nil {
				t.Fatalf("AddJob: %v", err)
			}
			stored, _ := db.GetJob("yt_c6")

			sp.updateJobMetadata(stored, &youtube.VideoInfo{
				Title:              "A Stream",
				ScheduledStartTime: "2026-10-01T12:00:00Z",
				IsUpcoming:         tc.isUpcoming,
				IsLive:             tc.isLive,
			}, false)

			if got := len(rec.ByEvent("scheduled")); got != tc.want {
				t.Errorf("recorded %d \"scheduled\" notifications, want %d: %+v", got, tc.want, rec.Calls())
			}
		})
	}
}

// TestRecorderSatisfiesBothInterfaces pins the two seams by assignment. A
// Recorder that stops satisfying Notifier cannot stand in for runState's
// notifyMgr, which is where cmd/moombox's defect tests install it (Task 6).
func TestRecorderSatisfiesBothInterfaces(t *testing.T) {
	var _ notifications.Sender = notificationtest.New()
	var _ notifications.Notifier = notificationtest.New()
	var _ notifications.Sender = (*notifications.Manager)(nil)
	var _ notifications.Notifier = (*notifications.Manager)(nil)
}

// TestTwitchResumeEmbedCarriesThePause is C8's payload half. The pause embed
// was sent WHILE THE MACHINE WAS OFFLINE — three attempts inside ~7s of backoff
// plus dial timeouts, so it arrived only when the outage was shorter than the
// send — and the resume that followed said nothing about how long the download
// had been down. One embed now carries both.
//
// THE MUTANT: restoring the pause send (the pause subtest records two), or
// dropping the Paused field (the resume subtest's field lookup fails).
func TestTwitchResumeEmbedCarriesThePause(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{notifier: rec, logger: discardLogger{}} // the package's existing no-op logger (Task 6 used it instead of adding muxTestLogger)
	jobCtx := &JobContext{Job: &database.Job{ID: "tw_1", Title: "A Stream", ChannelName: "chan", Platform: "twitch"}}
	pausedAt := time.Now().Add(-4 * time.Minute)

	o.sendTwitchSessionNotification(jobCtx, "Twitch Download Resumed",
		"Connectivity restored, resuming download: "+jobCtx.Job.Title,
		notifications.TypeDownload, "connectivity_resume", QualityInfo{Label: "1080p60"}, 2,
		twitchOutageField(pausedAt))

	if got := len(rec.ByEvent("connectivity_pause")); got != 0 {
		t.Errorf("recorded %d connectivity_pause notifications — the undeliverable pause embed is retired", got)
	}
	got := rec.ByEvent("connectivity_resume")
	if len(got) != 1 {
		t.Fatalf("recorded %d resume notifications, want 1", len(got))
	}
	if got[0].Title != "Twitch Download Resumed" {
		t.Errorf("title = %q, want \"Twitch Download Resumed\" — the event key alone does not prove which embed went out", got[0].Title)
	}
	paused, ok := got[0].Field("Paused")
	if !ok {
		t.Fatalf("the resume embed carries no Paused field: %+v", got[0].Fields)
	}
	if !strings.Contains(paused, fmt.Sprintf("<t:%d:R>", pausedAt.Unix())) {
		t.Errorf("Paused = %q, want a <t:%d:R> relative timestamp", paused, pausedAt.Unix())
	}
	if !strings.Contains(paused, "resumed after") {
		t.Errorf("Paused = %q, want the outage duration", paused)
	}
	if part, _ := got[0].Field("Part"); part != "2" {
		t.Errorf("Part = %q, want 2", part)
	}
}
