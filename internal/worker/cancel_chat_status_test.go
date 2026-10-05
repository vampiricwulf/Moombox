package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestUserCancelSettlesARunningChatStatus: neither orchestrator's cancel arm
// records a chat verdict, so a user-Cancelled job went on showing its chat as
// "downloading" (or "pending") in both UIs indefinitely. handleCancellation
// now settles it on a user cancel — a running capture is "incomplete", one
// that never started reports nothing — and leaves a settled verdict and a
// shutdown's row alone (the capture resumes on the next start).
//
// Mutants: dropping the chat_status write — the first two rows keep their
// live value; applying it on a shutdown too — the last row changes.
func TestUserCancelSettlesARunningChatStatus(t *testing.T) {
	w, db := testWorkerSetup(t)
	run := func(id, chat string, userCancel bool) *database.Job {
		t.Helper()
		if _, err := db.AddJob(&database.Job{ID: id, VideoID: id, URL: "u", Platform: "youtube",
			Status: database.StatusDownloading, ChatStatus: chat}); err != nil {
			t.Fatal(err)
		}
		w.queue.Enqueue(id, database.StatusDownloading)
		if got, _, ok := w.queue.Dequeue(context.Background()); !ok || got != id {
			t.Fatalf("Dequeue = %q, %v", got, ok)
		}
		if userCancel {
			w.CancelJob(id)
		}
		job, _ := db.GetJob(id)
		w.handleCancellation(job)
		after, _ := db.GetJob(id)
		return after
	}

	for _, tc := range []struct {
		id, chat   string
		userCancel bool
		want       string
	}{
		{"running", "downloading", true, chatStatusIncomplete},
		{"never-started", "pending", true, ""},
		{"settled", "finished", true, "finished"},
		{"shutdown", "downloading", false, "downloading"},
	} {
		got := run(tc.id, tc.chat, tc.userCancel)
		if got == nil {
			t.Fatalf("%s: row vanished", tc.id)
		}
		if got.ChatStatus != tc.want {
			t.Errorf("%s: chat_status = %q, want %q", tc.id, got.ChatStatus, tc.want)
		}
	}
}

// TestTwitchChatReadsPendingUntilItStarts: the stream processor wrote chat
// "downloading" for a Twitch job before ExecuteTwitch existed for it — and a
// VOD still had the download-slot wait ahead of it, so its chat showed
// downloading through all of it. The processor now writes "pending" and
// ExecuteTwitch's startChat writes "downloading" when the capture starts, the
// way the YouTube downloader's OnStart does.
//
// Mutant: dropping startChat's write — "downloading" never appears.
func TestTwitchChatReadsPendingUntilItStarts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n" +
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()

	w, db := testWorkerSetup(t)
	job := &database.Job{
		ID: "tw_chat_start", VideoID: "chat_start", URL: "https://twitch.tv/videos/1",
		Platform: "twitch", Status: database.StatusDownloading, ChatStatus: "pending",
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	unsub := db.OnJobUpdate(func(j *database.Job) {
		if j.ID != job.ID {
			return
		}
		mu.Lock()
		if n := len(seen); n == 0 || seen[n-1] != j.ChatStatus {
			seen = append(seen, j.ChatStatus)
		}
		mu.Unlock()
	})
	defer unsub()

	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(), Logger: &discardLogger{}}
	_ = w.orchestrator.ExecuteTwitch(context.Background(), jobCtx,
		&TwitchVariantInfo{URL: srv.URL + "/x.m3u8", Name: "720p"}, false, &instantChatSource{count: 3})

	mu.Lock()
	defer mu.Unlock()
	started := -1
	for i, cs := range seen {
		if cs == "downloading" {
			started = i
			break
		}
	}
	if started < 0 || started == len(seen)-1 {
		t.Errorf("chat_status went %v, want downloading once the capture starts, then the verdict", seen)
	}
}
