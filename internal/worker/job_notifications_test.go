package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/notifications"
	"github.com/vampiricwulf/Moombox/internal/notifications/notificationtest"
)

// notifyField is the fatal-on-missing wrapper around N1's Call.Field
// (notificationtest/recorder.go:35), which already answers
// `value, ok := call.Field(name)`. This adds only the t.Fatalf, so a missing
// field names the embed it was missing from instead of failing three
// assertions later on an empty string.
func notifyField(t *testing.T, c notificationtest.Call, name string) string {
	t.Helper()
	v, ok := c.Field(name)
	if !ok {
		t.Fatalf("field %q missing from %q; got %+v", name, c.Title, c.Fields)
	}
	return v
}

// TestUserCancelSendsTheOneCancelEmbed is audit C5's worker half: the
// mid-job cancel said "Download Cancelled" while the route's cancel of the
// same job a second earlier would have said "Job Cancelled".
//
// Mutant: leaving the old title in place — a subscriber filtering on the
// title (not the event) sees two different cancels for one action.
func TestUserCancelSendsTheOneCancelEmbed(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec

	job := &database.Job{
		ID: "tw_c1", VideoID: "c1", Platform: "twitch", Title: "Cancel Me",
		ChannelName: "Streamer", URL: "https://twitch.tv/streamer",
		Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// Cancel flags ONLY a job the queue is already PROCESSING
	// (internal/worker/queue.go:387 — "Only flag jobs that are actually
	// processing"), so the row has to be enqueued and dequeued first.
	// Without that, nothing is flagged, handleCancellation takes the SHUTDOWN
	// branch and sends nothing — the test would be red before the change and
	// red after it.
	w.queue.Enqueue(job.ID, database.StatusDownloading)
	if _, _, ok := w.queue.Dequeue(context.Background()); !ok {
		t.Fatal("Dequeue returned no job — the cancel path needs a processing run")
	}
	if !w.queue.Cancel(job.ID) {
		t.Fatal("Cancel did not flag a user cancel")
	}
	w.handleCancellation(job)

	calls := rec.ByEvent("cancelled")
	if len(calls) != 1 {
		t.Fatalf("recorded %d cancelled calls, want 1", len(calls))
	}
	if calls[0].Title != "Job Cancelled" {
		t.Errorf("title = %q, want %q", calls[0].Title, "Job Cancelled")
	}
	if got := notifyField(t, calls[0], "Stream ID"); got != "c1" {
		t.Errorf("Stream ID = %q", got)
	}
}

// finishedJobRow is the smallest row the finished embed reads.
func finishedJobRow() *database.Job {
	msgs := 4210
	vSeq, aSeq := 1234, 1230
	length := 3600
	return &database.Job{
		ID: "vidF", VideoID: "vidF", Platform: "youtube", Title: "Archived Stream",
		ChannelName: "A Channel", URL: "https://www.youtube.com/watch?v=vidF",
		ThumbnailURL:      "https://i.ytimg.example/t.jpg",
		TotalChatMessages: &msgs, LastVideoSeq: &vSeq, LastAudioSeq: &aSeq,
		LengthSeconds: &length,
	}
}

// TestFinishedEmbedIsWarningWhenTheTailIsIncomplete is audit A5. The archive is
// short, the fix is one Resume click, and the embed said "Successfully
// archived" in green.
//
// Mutants this kill:
//   - keeping TypeSuccess: the colour is the only thing most readers see.
//   - adding the Tail field without the colour, or vice versa.
func TestFinishedEmbedIsWarningWhenTheTailIsIncomplete(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()
	j.IncompleteTail = true

	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})

	calls := rec.ByEvent("finished")
	if len(calls) != 1 {
		t.Fatalf("recorded %d finished calls, want 1", len(calls))
	}
	if calls[0].Type != notifications.TypeWarning {
		t.Errorf("type = %v, want TypeWarning for an incomplete tail", calls[0].Type)
	}
	if got := notifyField(t, calls[0], "Tail"); got != "incomplete — Resume appends the rest" {
		t.Errorf("Tail = %q", got)
	}
}

// TestFinishedEmbedReportsAnIncompleteChat: chatStatusForOutcome already wrote
// the verdict to the row and both UIs render it; the embed did not.
//
// Mutant: reading TotalChatMessages instead of ChatStatus — a truncated
// capture with thousands of messages reads as complete, which is the exact
// ranking bug chatStatusForOutcome exists to prevent.
func TestFinishedEmbedReportsAnIncompleteChat(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()
	j.ChatStatus = chatStatusIncomplete

	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})

	if got := notifyField(t, rec.ByEvent("finished")[0], "Chat"); got != "incomplete" {
		t.Errorf("Chat = %q, want %q", got, "incomplete")
	}
	if rec.ByEvent("finished")[0].Type != notifications.TypeSuccess {
		t.Error("an incomplete chat alone must not recolour the embed — the recording is whole")
	}
}

// TestFinishedEmbedCountsSetAsideRecordings is audit A6: Arc A built the
// Recover verb, and nothing ever told an operator there was something to
// recover.
//
// Mutant: scanning the OUTPUT dir instead of the staging dir — the count is
// always zero and the field never appears.
func TestFinishedEmbedCountsSetAsideRecordings(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()

	staging := t.TempDir()
	writeAsidePair(t, staging, "1700000000", 8, false, false)

	o.sendDownloadFinished(&JobContext{Job: j, StagingDir: staging}, j, []notifications.Part{{File: "out.mp4"}})

	if got := notifyField(t, rec.ByEvent("finished")[0], "Set-aside recordings"); got != "1 — Recover to mux them" {
		t.Errorf("Set-aside recordings = %q", got)
	}
}

// TestFinishedEmbedShapesPerPartCount is audit C1: the two builders' field
// sets are now one, and the job-level facts the multi-part path used to drop
// arrive on both.
//
// Mutants this kill:
//   - emitting "Parts" for a single-part job (noise on the common case).
//   - keeping the multi-part path's blindness to Format Selection.
//   - labelling the summed size "File Size" for a split job.
func TestFinishedEmbedShapesPerPartCount(t *testing.T) {
	vItag, aItag := 299, 251
	t.Run("single part", func(t *testing.T) {
		rec := notificationtest.New()
		o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
		j := finishedJobRow()
		j.SelectedVideoItag, j.SelectedAudioItag = &vItag, &aItag

		o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{
			{File: "out.mp4", Size: 1 << 30, Width: 1920, Height: 1080, Fps: 60},
		})

		c := rec.ByEvent("finished")[0]
		if notifyField(t, c, "File") != "out.mp4" {
			t.Error("the single-part shape lost its File field")
		}
		notifyField(t, c, "File Size")
		notifyField(t, c, "Resolution")
		notifyField(t, c, "Format Selection")
		for _, f := range c.Fields {
			if f.Name == "Parts" {
				t.Error("a single-part job must not report a part count")
			}
		}
	})

	t.Run("three parts", func(t *testing.T) {
		rec := notificationtest.New()
		o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
		j := finishedJobRow()
		j.SelectedVideoItag, j.SelectedAudioItag = &vItag, &aItag

		o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{
			{Quality: "1080p60", Size: 1 << 29, Width: 1920, Height: 1080},
			{Quality: "720p60", Size: 1 << 28},
			{Quality: "720p60", Size: 1 << 28},
		})

		c := rec.ByEvent("finished")[0]
		if got := notifyField(t, c, "Parts"); got != "3" {
			t.Errorf("Parts = %q", got)
		}
		if got := notifyField(t, c, "Qualities"); got != "1080p60 -> 720p60 -> 720p60" {
			t.Errorf("Qualities = %q", got)
		}
		notifyField(t, c, "Total Size")
		// The job-level facts the multi-part builder used to drop.
		notifyField(t, c, "Format Selection")
		notifyField(t, c, "Segments")
		notifyField(t, c, "Chat Messages")
	})

	// The single-part path's own Part assembly. Every other subtest drives
	// sendDownloadFinished directly, which leaves the probeData / os.FileInfo /
	// LengthSeconds -> Part mapping unasserted — and a Part with a zero
	// Duration silently produces an embed with NO Duration field at all.
	//
	// Mutants this kills: dropping the LengthSeconds -> Part.Duration line, or
	// the probeData -> Width/Height/Fps lines, or filepath.Base.
	t.Run("the single-part producer fills the Part", func(t *testing.T) {
		rec := notificationtest.New()
		o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
		j := finishedJobRow() // LengthSeconds = 3600

		o.sendFinishedNotification(&JobContext{Job: j}, j,
			filepath.Join("C:", "out", "out.mp4"),
			&ffprobeData{Width: 1920, Height: 1080, Fps: 60}, nil)

		c := rec.ByEvent("finished")[0]
		if got := notifyField(t, c, "File"); got != "out.mp4" {
			t.Errorf("File = %q, want the base name", got)
		}
		if got := notifyField(t, c, "Resolution"); got != "1920x1080 @60fps" {
			t.Errorf("Resolution = %q", got)
		}
		notifyField(t, c, "Duration") // from LengthSeconds; absent if the mapping is dropped
	})
}

// TestFinishedEmbedDropsTheDeadTwitchImage is the §0 ruling: a Twitch preview
// URL 404s once the stream ends, so the image on a finished Twitch embed is
// broken by the time anybody reads it.
//
// Mutant: setting Image unconditionally — the Twitch embed renders a broken
// picture and nothing in the suite notices.
func TestFinishedEmbedDropsTheDeadTwitchImage(t *testing.T) {
	rec := notificationtest.New()
	o := &DownloadOrchestrator{logger: discardLogger{}, notifier: rec}
	j := finishedJobRow()
	j.Platform = "twitch"
	j.ID, j.VideoID = "tw_F", "F"

	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})

	c := rec.ByEvent("finished")[0]
	if c.Opts.Image != "" || c.Opts.Thumbnail != "" {
		t.Errorf("twitch finished carries Image=%q Thumbnail=%q, want neither", c.Opts.Image, c.Opts.Thumbnail)
	}

	rec.Reset()
	j.Platform, j.ID, j.VideoID = "youtube", "vidF", "vidF"
	o.sendDownloadFinished(&JobContext{Job: j}, j, []notifications.Part{{File: "out.mp4"}})
	c = rec.ByEvent("finished")[0]
	if c.Opts.Image == "" {
		t.Error("the YouTube finished embed lost the full-width image it has always had")
	}
	if c.Opts.Thumbnail != "" {
		t.Error("the finished embed shows the same picture twice — it has always used the full-width image alone")
	}
}

// TestErrorStageClassifiesTheFinalizeErrors pins the one signal there is: the
// prefixes the orchestrator itself writes.
//
// Mutants this kill:
//   - matching "mux" anywhere in the string: an ffmpeg stderr tail from a
//     DOWNLOAD failure that happens to mention muxing flips the answer.
//   - dropping the two non-"mux" finalize prefixes: "no media files to mux"
//     and "create output dir" are mux-stage failures that do not start with
//     the word.
func TestErrorStageClassifiesTheFinalizeErrors(t *testing.T) {
	for msg, want := range map[string]string{
		"mux: ffmpeg: exit status 1 (stderr: …)":                           "mux",
		"mux produced 12s from a 3600s input (3588s missing) — the copy …": "mux",
		"mux segment 3: ffmpeg: exit status 1":                             "mux",
		"no media files to mux":                                            "mux",
		"no media files to mux for segment 2":                              "mux",
		"create output dir: mkdir E:\\out: access is denied":               "mux",
		"twitch channel is offline":                                        "download",
		"failed to fetch segment 1234: context deadline exceeded":          "download",
		"ffmpeg stderr mentions muxing somewhere in the tail":              "download",
		// The two finalize returns the prefixes still miss — pinned so the
		// residual is a fact in the suite, not only in a report.
		"create segment output dir: mkdir: denied":    "download",
		"no segment files found in staging directory": "download",
	} {
		if got := errorStage(msg); got != want {
			t.Errorf("errorStage(%q) = %q, want %q", msg, got, want)
		}
	}
}

// TestJobFailedNamesTheStageAndTheStaging is audit M8: the embed reported an
// error string and left the operator to guess whether Retry (which DELETES
// staging) or Resume (which preserves it) is the right button.
//
// Mutants this kill:
//   - claiming "Resume available" for a Twitch job: Resume is YouTube-only in
//     both UIs and the route answers 400.
//   - reading the staging flag before the error is committed, or from the
//     output directory.
func TestJobFailedNamesTheStageAndTheStaging(t *testing.T) {
	w, db := testWorkerSetup(t)
	t.Cleanup(w.Stop)
	rec := notificationtest.New()
	w.notifier = rec

	var stagingBase string
	w.readConfig(func(c *config.MoomboxConfig) { stagingBase = c.Paths.EffectiveStagingDir() })

	t.Run("mux failure with staging preserved", func(t *testing.T) {
		rec.Reset()
		job := &database.Job{ID: "vidE1", VideoID: "vidE1", Platform: "youtube", Title: "Failed", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(stagingBase, job.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video.mp4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		w.setJobError(job, errors.New("mux: ffmpeg: exit status 1"))

		calls := rec.ByEvent("error")
		if len(calls) != 1 {
			t.Fatalf("recorded %d error calls, want 1", len(calls))
		}
		if got := notifyField(t, calls[0], "Stage"); got != "mux" {
			t.Errorf("Stage = %q", got)
		}
		if got := notifyField(t, calls[0], "Staging"); got != "preserved — Resume available" {
			t.Errorf("Staging = %q", got)
		}
	})

	t.Run("download failure with nothing staged", func(t *testing.T) {
		rec.Reset()
		job := &database.Job{ID: "vidE2", VideoID: "vidE2", Platform: "youtube", Title: "Failed", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}

		w.setJobError(job, errors.New("twitch channel is offline"))

		c := rec.ByEvent("error")[0]
		if got := notifyField(t, c, "Stage"); got != "download" {
			t.Errorf("Stage = %q", got)
		}
		if got := notifyField(t, c, "Staging"); got != "removed" {
			t.Errorf("Staging = %q", got)
		}
	})

	t.Run("twitch never promises Resume", func(t *testing.T) {
		rec.Reset()
		job := &database.Job{ID: "tw_E3", VideoID: "E3", Platform: "twitch", Title: "Failed", Status: database.StatusDownloading}
		if _, err := db.AddJob(job); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(stagingBase, job.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "video.ts"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}

		w.setJobError(job, errors.New("mux: ffmpeg: exit status 1"))

		if got := notifyField(t, rec.ByEvent("error")[0], "Staging"); got != "preserved" {
			t.Errorf("Staging = %q — Resume is YouTube-only in both UIs and the route answers 400", got)
		}
	})
}

// TestTrimCreatedIsOneEmbed is audit C2.
//
// Mutant: emitting Segments for a single-file trim — the field claims a split
// that never happened.
func TestTrimCreatedIsOneEmbed(t *testing.T) {
	rec := notificationtest.New()
	size := int64(1 << 20)
	f := NotifyFacts(&database.Job{
		ID: "vidT", VideoID: "vidT", Platform: "youtube", Title: "Source",
		ChannelName: "A Channel", URL: "https://www.youtube.com/watch?v=vidT",
	})

	rec.Send(notifications.TrimCreated(f, notifications.TrimFacts{
		TimeRange: "0:10 - 0:40", Duration: 30 * time.Second, Size: &size,
	}))
	c := rec.ByEvent("trim_created")[0]
	if c.Title != "Trim Created" {
		t.Errorf("title = %q", c.Title)
	}
	notifyField(t, c, "Source Video")
	notifyField(t, c, "File Size")
	for _, fl := range c.Fields {
		if fl.Name == "Segments" {
			t.Error("a single-file trim must not report Segments")
		}
	}

	rec.Reset()
	rec.Send(notifications.TrimCreated(f, notifications.TrimFacts{
		TimeRange: "0:10 - 0:40", Duration: 30 * time.Second, Parts: 3,
	}))
	if got := notifyField(t, rec.ByEvent("trim_created")[0], "Segments"); got != "3 segments" {
		t.Errorf("Segments = %q", got)
	}
}
