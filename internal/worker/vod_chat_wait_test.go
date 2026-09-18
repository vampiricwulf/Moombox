package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestVodChatWaitTimeout pins owner decision O-A's bound: a finished VOD
// download waits for chat paging roughly as long as the video itself runs,
// floored so a short chat-heavy VOD still gets a real allowance and capped so
// a stalled pager cannot hold a job forever.
//
// Mutants, one per row:
//   - returning chatWaitTimeout: a chat-heavy VOD is cut at two minutes, which
//     is the defect.
//   - dropping the floor: a 3-minute VOD with 40k comments is cut at 3 minutes.
//   - dropping the ceiling: an absurd length_seconds holds the job open for
//     days.
func TestVodChatWaitTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		job  *database.Job
		want time.Duration
	}{
		{"no length falls back to the floor", &database.Job{}, vodChatWaitFloor},
		{"short VOD gets the floor", &database.Job{LengthSeconds: ptrInt(180)}, vodChatWaitFloor},
		{"long VOD gets its own duration", &database.Job{LengthSeconds: ptrInt(4 * 3600)}, 4 * time.Hour},
		{"absurd length is capped", &database.Job{LengthSeconds: ptrInt(90 * 3600)}, vodChatWaitCeiling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vodChatWaitTimeout(tc.job); got != tc.want {
				t.Errorf("vodChatWaitTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestVodChatWaitSlotIsReleasedBeforeTheWait pins the other half of O-A: the
// download slot is released BEFORE the chat wait begins, not after it. The
// video is finished — a wait that can run for hours must not hold a slot the
// next VOD is queued behind.
//
// Mutants:
//   - releasing after resolveChatOutcome returns (or not at all): the second
//     acquire below blocks for its whole 2 s window and this test fails on the
//     "still holding" arm.
//   - a raw activeDownloads decrement in place of ReleaseDownloadSlot: the
//     orchestrators release again below the mux, so the pool would leak a slot
//     per VOD; the ActiveCount check at the end fails.
func TestVodChatWaitSlotIsReleasedBeforeTheWait(t *testing.T) {
	q := NewJobQueue(1) // exactly one download slot, held by the VOD below
	q.Enqueue("vod1", database.StatusUpcoming)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q.Dequeue(ctx)
	if !q.AcquireDownloadSlot(ctx, "vod1") {
		t.Fatal("setup: the VOD must hold the only download slot")
	}

	o := &DownloadOrchestrator{logger: discardLogger{}, queue: q}
	var rec chatOutcome
	done := make(chan struct{})
	stall := errors.New("vod chat paging stalled at offset 12 cursor \"abc\": cursor did not advance")
	dl := &blockingChatSource{release: make(chan struct{}), err: stall}

	go func() {
		defer close(done)
		rec.record(dl.Start(context.Background()))
	}()

	result := make(chan error, 1)
	go func() {
		// 4 h of video ⇒ a 4 h bound: nothing in this test may depend on it
		// elapsing.
		result <- o.resolveVodChatOutcome(ctx, dl, &rec, done,
			&database.Job{ID: "vod1", LengthSeconds: ptrInt(4 * 3600)})
	}()

	acquired := make(chan bool, 1)
	go func() {
		actx, acancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer acancel()
		acquired <- q.AcquireDownloadSlot(actx, "next")
	}()

	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("the next VOD could not acquire a download slot while the chat wait was " +
				"running — the slot must be released BEFORE the wait, not after it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second AcquireDownloadSlot never returned")
	}

	// The wait must still be in flight: the point of the release is that the
	// pool moves on while this job keeps paging chat.
	select {
	case got := <-result:
		t.Fatalf("the chat wait had already returned (%v) — this test cannot tell whether the "+
			"slot was released before or after it", got)
	default:
	}

	close(dl.release)
	select {
	case got := <-result:
		if !errors.Is(got, stall) {
			t.Errorf("resolveVodChatOutcome = %v, want %v — the wait still resolves through "+
				"resolveChatOutcome", got, stall)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolveVodChatOutcome did not return after the chat goroutine finished")
	}

	// The orchestrators release again below the mux. That second release is a
	// no-op, so the slot "next" holds is still the only one counted.
	q.ReleaseDownloadSlot("vod1")
	if active := q.ActiveCount(); active != 1 {
		t.Errorf("ActiveCount() = %d, want 1 — the VOD's slot must be given up exactly once, "+
			"however many times ReleaseDownloadSlot is called for it", active)
	}
}

// TestVodChatWaitStopsPromptlyOnCancel pins the bound's escape hatch: a Stop()
// (or user cancel) during a wait measured in hours returns promptly and
// records the chat as incomplete — a downloader that was still paging when the
// job was stopped has not finished.
//
// Mutant: dropping the ctx arm from the first wait — the cancel is invisible,
// the 30-minute floor runs to term and this test fails on its deadline.
func TestVodChatWaitStopsPromptlyOnCancel(t *testing.T) {
	o := &DownloadOrchestrator{logger: discardLogger{}}
	var rec chatOutcome
	done := make(chan struct{})
	// Start returns nil on its Stop()-exit, as every real downloader does —
	// so nothing but the wait itself can report this chat as incomplete.
	dl := &stopUnwoundChatSource{started: make(chan struct{}), release: make(chan struct{})}

	go func() {
		defer close(done)
		rec.record(dl.Start(context.Background()))
	}()
	<-dl.started

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- o.resolveVodChatOutcome(ctx, dl, &rec, done, &database.Job{ID: "vod2"})
	}()
	time.Sleep(20 * time.Millisecond) // let the wait get into its select
	cancel()

	select {
	case got := <-result:
		if got == nil {
			t.Fatal("resolveVodChatOutcome returned nil for a chat that was still paging when the " +
				"job was stopped — chatStatusForOutcome would write \"finished\" over it")
		}
		if status := chatStatusForOutcome(dl.MessageCount(), got); status != chatStatusIncomplete {
			t.Errorf("chatStatusForOutcome(%d, %v) = %q, want %q", dl.MessageCount(), got, status, chatStatusIncomplete)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolveVodChatOutcome did not return after its context was cancelled — a Stop() " +
			"must not be parked behind the VOD bound")
	}
}

// slotWatchingChatSource is a ChatSource whose Start returns as soon as its
// job's download slot is free — the exact handshake owner decision O-A is
// about, driven through the real ExecuteTwitch. It gives up after a bounded
// poll so a regression FAILS the test rather than parking it behind the VOD
// bound for half an hour.
type slotWatchingChatSource struct {
	q       *JobQueue
	sawFree atomic.Bool
	running atomic.Bool
}

func (s *slotWatchingChatSource) Start(context.Context) error {
	s.running.Store(true)
	defer s.running.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.q.ActiveCount() == 0 {
			s.sawFree.Store(true)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("vod chat paging stalled at offset 12 cursor \"abc\": cursor did not advance")
}
func (s *slotWatchingChatSource) Stop()             {}
func (s *slotWatchingChatSource) MarkStreamEnded()  {}
func (s *slotWatchingChatSource) MessageCount() int { return 5000 }
func (s *slotWatchingChatSource) IsRunning() bool   { return s.running.Load() }

// TestExecuteTwitchVodReleasesTheSlotBeforeItsChatWait drives the real Twitch
// VOD call site (the same already-ended httptest playlist
// TestExecuteTwitchWritesIncompleteForANonFinishedChatOutcome uses, which
// reaches the natural-end path in milliseconds). The chat source returns only
// once the job's download slot is free, so the test passes only if the
// release genuinely precedes the wait in production code.
//
// Mutant: reverting this site to
// o.resolveChatOutcome(twitchChatDl, &chatRec, chatDone, chatWaitTimeout, …)
// — the slot is then held until the mux release below, the chat source's poll
// gives up, and sawFree is false.
func TestExecuteTwitchVodReleasesTheSlotBeforeItsChatWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n" +
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()

	w, db := testWorkerSetup(t)
	o := w.orchestrator

	job := &database.Job{
		ID: "tw_vod_slot", VideoID: "vod_slot", URL: "https://twitch.tv/videos/3",
		Platform: "twitch", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	w.queue.Enqueue(job.ID, database.StatusUpcoming)
	w.queue.Dequeue(ctx)
	if !w.queue.AcquireDownloadSlot(ctx, job.ID) {
		t.Fatal("setup: the VOD must hold a download slot before it downloads")
	}

	cd := &slotWatchingChatSource{q: w.queue}
	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(), Logger: &discardLogger{}}
	_ = o.ExecuteTwitch(ctx, jobCtx, &TwitchVariantInfo{URL: srv.URL + "/x.m3u8", Name: "720p"}, true, cd)

	if !cd.sawFree.Load() {
		t.Error("the VOD's download slot was still held while its chat was paging — ExecuteTwitch " +
			"must release it BEFORE the chat wait (owner decision O-A), not after the mux")
	}
	if active := w.queue.ActiveCount(); active != 0 {
		t.Errorf("ActiveCount() = %d after the job finished, want 0", active)
	}
}

// TestYouTubeVodChatWaitRoutesThroughResolveVodChatOutcome pins the YouTube
// call site by source inspection, for the same reason
// TestOrchestratorGoRoutesItsChatStatusWriteThroughRecordChatOutcome does:
// ExecuteWithChat's chat parameter is the concrete *chat.ChatDownloader, so
// the site cannot be driven with a fake. It also pins the live half of O-A —
// a live chat ends with its broadcast, so that path keeps the two-minute cut
// unchanged.
//
// Mutant: reverting the VOD branch to the plain chatWaitTimeout call — the
// first check fails; extending the VOD bound to live jobs — the second does.
func TestYouTubeVodChatWaitRoutesThroughResolveVodChatOutcome(t *testing.T) {
	for _, file := range []string{"orchestrator.go", "orchestrator_twitch.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", file, err)
		}
		text := string(src)
		if !strings.Contains(text, "o.resolveVodChatOutcome(ctx,") {
			t.Errorf("%s's chat wait no longer routes a VOD through resolveVodChatOutcome — the "+
				"slot is then held through the wait and a chat-heavy VOD is still cut at two "+
				"minutes", file)
		}
		if !strings.Contains(text, "chatDone, chatWaitTimeout, 2*time.Second)") {
			t.Errorf("%s no longer has the live two-minute chat cut — O-A changes the VOD path "+
				"only", file)
		}
	}
}

// TestStagingKeptForIncompleteChat pins merge M5's cheap half: a Finished job
// whose chat_status is "incomplete" keeps its staging dir, because the chat
// resume sidecar lives there and deleting it makes the truncation
// unrecoverable.
//
// Mutant: dropping the chat_status term from jobNeedsStaging — the orphan
// scanner offers the preserved dir for deletion and the tail is gone for good.
func TestStagingKeptForIncompleteChat(t *testing.T) {
	_, db := testWorkerSetup(t)
	cfg := &config.MoomboxConfig{}

	for _, tc := range []struct {
		name string
		job  *database.Job
		want bool
	}{
		{"incomplete chat keeps staging", &database.Job{ID: "a", Status: database.StatusFinished, ChatStatus: chatStatusIncomplete}, true},
		{"finished chat does not", &database.Job{ID: "b", Status: database.StatusFinished, ChatStatus: "finished"}, false},
		{"unavailable chat does not", &database.Job{ID: "c", Status: database.StatusFinished, ChatStatus: "unavailable"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobNeedsStaging(db, cfg, tc.job, t.TempDir()); got != tc.want {
				t.Errorf("jobNeedsStaging(chat_status=%q) = %v, want %v", tc.job.ChatStatus, got, tc.want)
			}
		})
	}
}

// TestStagingKeptForIncompleteChatExpires pins the other side of the keep: the
// chat shield defers to the SAME age rule the incomplete_tail shield does, so
// a preserved dir is never preserved forever.
//
// Mutant: an unconditional chat term (no incompleteStagingExpired call) — an
// abandoned job's staging is shielded from the orphan scanner indefinitely.
func TestStagingKeptForIncompleteChatExpires(t *testing.T) {
	_, db := testWorkerSetup(t)
	cfg := &config.MoomboxConfig{Downloader: config.DownloaderConfig{
		IncompleteStagingExpiryDays: config.FlexDuration{Value: 7},
	}}
	aged := &database.Job{
		ID: "aged", Status: database.StatusFinished, ChatStatus: chatStatusIncomplete,
		UpdatedAt: time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	if jobNeedsStaging(db, cfg, aged, t.TempDir()) {
		t.Error("an aged incomplete-chat job's staging must stop being shielded once the " +
			"downloader.incomplete_staging_expiry_days window has lapsed")
	}
}

// TestWorkerFinishKeepsStagingForAnIncompleteChat pins the finalize half of
// merge M5 by source inspection — the same technique
// TestOrchestratorGoRoutesItsChatStatusWriteThroughRecordChatOutcome uses for
// the YouTube chat write site, and for the same reason: the cleanup block
// lives at the tail of processJob, which cannot be driven without a live
// stream. jobNeedsStaging (above) covers the orphan scanner; this covers the
// dir's first chance to be deleted, minutes earlier.
//
// Mutant: dropping the preserveForChat branch — the job finishes, staging is
// removed with the resume sidecar in it, and the chat truncation the row
// reports becomes permanent.
func TestWorkerFinishKeepsStagingForAnIncompleteChat(t *testing.T) {
	src, err := os.ReadFile("worker.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "fresh.ChatStatus == chatStatusIncomplete") {
		t.Error("worker.go's staging cleanup no longer tests the fresh row's chat_status — a job " +
			"whose chat capture ended incomplete must keep the staging dir its resume sidecar " +
			"lives in")
	}
	if !strings.Contains(text, "} else if preserveForChat {") {
		t.Error("worker.go's staging cleanup no longer has a preserveForChat branch between the " +
			"incomplete_tail branch and os.RemoveAll")
	}
}
