package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestChatStatusForOutcome pins the rule O2 is about: the verdict is what the
// downloader DID, not what it counted.
//
// Mutants this kills:
//   - checking the count first (today's shape): a stalled VOD chat with 5,000
//     messages on disk reads "finished", both UIs show a short archive as
//     complete, and nothing tells the operator to act.
//   - dropping the outcome arm entirely: same result.
//   - reporting "unavailable" for a stall that captured nothing: the archive
//     did not turn out to be empty, the capture stopped — the row says so.
func TestChatStatusForOutcome(t *testing.T) {
	stalled := errors.New("vod chat paging stalled at offset 12 cursor \"abc\": cursor did not advance")
	for _, tc := range []struct {
		name    string
		count   int
		outcome error
		want    string
	}{
		{"clean run with messages", 5000, nil, "finished"},
		{"clean run with no chat at all", 0, nil, "unavailable"},
		{"stalled run with messages", 5000, stalled, "incomplete"},
		{"stalled run with no messages", 0, stalled, "incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatStatusForOutcome(tc.count, tc.outcome); got != tc.want {
				t.Errorf("chatStatusForOutcome(%d, %v) = %q, want %q", tc.count, tc.outcome, got, tc.want)
			}
		})
	}
}

// TestChatOutcomeKeepsTheLastRunsVerdict pins the relaunch rule. The Twitch
// orchestrator relaunches chat after a connectivity outage, so one job can run
// several chat sessions; the job's verdict belongs to the LAST of them.
//
// Mutant: a recorder that keeps the FIRST error — a job whose chat recovered
// after an outage would still be reported incomplete forever.
func TestChatOutcomeKeepsTheLastRunsVerdict(t *testing.T) {
	var rec chatOutcome
	if rec.verdict() != nil {
		t.Fatalf("a recorder that has seen nothing has verdict %v, want nil", rec.verdict())
	}
	first := errors.New("connectivity lost")
	rec.record(first)
	if !errors.Is(rec.verdict(), first) {
		t.Fatalf("verdict = %v, want the recorded error", rec.verdict())
	}
	rec.record(nil)
	if rec.verdict() != nil {
		t.Errorf("verdict = %v after a later run succeeded, want nil — the relaunch's outcome is "+
			"the job's outcome", rec.verdict())
	}
}

// TestChatFileStatusDoesNotOverwriteAnIncompleteVerdict pins the mux path.
//
// copyAssets and the per-part chat block both set chat_status to "finished"
// whenever they COPY a chat file, and they run after the verdict is written —
// so without this guard the fix would be undone at mux time by a file that is
// short precisely because the capture stalled.
//
// Mutant: the unconditional `"finished"` those two sites carried before.
func TestChatFileStatusDoesNotOverwriteAnIncompleteVerdict(t *testing.T) {
	for _, tc := range []struct {
		name   string
		jobCtx *JobContext
		want   string
	}{
		{"incomplete verdict survives the copy", &JobContext{ChatStatus: "incomplete"}, "incomplete"},
		{"no verdict recorded", &JobContext{}, "finished"},
		{"unavailable verdict still yields to an archived file", &JobContext{ChatStatus: "unavailable"}, "finished"},
		{"finished verdict", &JobContext{ChatStatus: "finished"}, "finished"},
		{"no context at all (standalone Mux action)", nil, "finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatFileStatus(tc.jobCtx); got != tc.want {
				t.Errorf("chatFileStatus(%+v) = %q, want %q", tc.jobCtx, got, tc.want)
			}
		})
	}
}

// TestRecordChatOutcomeWritesTheRowAndRemembersItOnTheContext is the wiring:
// the verdict has to reach BOTH the job row (which is what the two UIs read)
// and the JobContext (which is what stops the mux path overwriting it).
//
// Mutant: writing the DB row without setting jobCtx.ChatStatus — the row is
// right until copyAssets runs, and then the job finishes reporting "finished".
func TestRecordChatOutcomeWritesTheRowAndRemembersItOnTheContext(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.AddJob(&database.Job{ID: "j1", VideoID: "v1", URL: "https://twitch.tv/videos/1", Platform: "twitch"}); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	o := &DownloadOrchestrator{db: db, logger: discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: "j1"}}
	o.recordChatOutcome(jobCtx, 4211, errors.New("vod chat paging stalled"))

	if jobCtx.ChatStatus != "incomplete" {
		t.Errorf("jobCtx.ChatStatus = %q, want \"incomplete\" — the mux path reads this to avoid "+
			"overwriting the verdict when it copies the (short) chat file", jobCtx.ChatStatus)
	}
	job, err := db.GetJob("j1")
	if err != nil || job == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.ChatStatus != "incomplete" {
		t.Errorf("job.ChatStatus = %q, want \"incomplete\"", job.ChatStatus)
	}
	if job.TotalChatMessages == nil || *job.TotalChatMessages != 4211 {
		t.Errorf("job.TotalChatMessages = %v, want 4211 — the count is still recorded, it is just "+
			"no longer what decides the status", job.TotalChatMessages)
	}
}

// --- Fix round 1 ------------------------------------------------------------
//
// Review finding (Important 3): chatRec.verdict() was read without being
// ordered by the chat goroutine's completion signal. In orchestrator_twitch.go
// the wait on chatDone sat INSIDE `if twitchChatDl.IsRunning()`, but `running`
// is cleared by a defer INSIDE Start — strictly before the wrapper goroutine's
// chatRec.record(Start's return value) runs — so the guard could read false
// in exactly the window the outcome had not landed yet, skip the wait
// entirely, and read a stale nil verdict. The fix, resolveChatOutcome, waits
// on the completion signal UNCONDITIONALLY (bounded by a timeout, then a
// short cleanup grace) and turns an unconfirmed completion into an explicit
// non-nil outcome rather than letting nil read as "finished"/"unavailable".

// blockingChatSource is a ChatSource whose Start blocks on release until the
// test lets it proceed, then returns err. running reports true from the
// moment Start is entered until it returns — mirroring the real production
// race window (twitch.ChatDownloader/twitch.VodChatDownloader clear their own
// "running" flag via an internal defer, strictly before the ORCHESTRATOR's
// wrapper goroutine gets to call chatRec.record on Start's return value).
type blockingChatSource struct {
	release chan struct{}
	err     error
	running atomic.Bool
	stopped atomic.Bool
}

func (b *blockingChatSource) Start(context.Context) error {
	b.running.Store(true)
	<-b.release
	b.running.Store(false)
	return b.err
}
func (b *blockingChatSource) Stop()             { b.stopped.Store(true) }
func (b *blockingChatSource) MarkStreamEnded()  {}
func (b *blockingChatSource) MessageCount() int { return 0 }
func (b *blockingChatSource) IsRunning() bool   { return b.running.Load() }

// TestResolveChatOutcomeWaitsForTheGoroutinesRecordBeforeReadingIt pins the
// ordering: reading chatRec must happen only after the chat goroutine's
// record() — signalled by done closing — not race it.
//
// Mutant: resolveChatOutcome reading rec.verdict() (or checking dl.IsRunning())
// before selecting on done. The goroutine below only records+closes done
// after the test releases it, well after resolveChatOutcome has started
// waiting; a premature read observes the recorder's zero value (nil) instead
// of the real error and this test fails.
func TestResolveChatOutcomeWaitsForTheGoroutinesRecordBeforeReadingIt(t *testing.T) {
	o := &DownloadOrchestrator{logger: discardLogger{}}
	var rec chatOutcome
	done := make(chan struct{})
	stall := errors.New("vod chat paging stalled at offset 12 cursor \"abc\": cursor did not advance")
	dl := &blockingChatSource{release: make(chan struct{})}
	dl.running.Store(true)
	dl.err = stall

	go func() {
		defer close(done)
		rec.record(dl.Start(context.Background()))
	}()

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- o.resolveChatOutcome(dl, &rec, done, time.Second, 200*time.Millisecond)
	}()
	// Give resolveChatOutcome a head start before letting the goroutine
	// finish — a correct implementation blocks on <-done for this whole
	// interval; a mutant that reads first has already pushed its (wrong)
	// answer into resultCh by now.
	time.Sleep(20 * time.Millisecond)
	close(dl.release)

	select {
	case got := <-resultCh:
		if !errors.Is(got, stall) {
			t.Errorf("resolveChatOutcome = %v, want %v — it must wait for the goroutine's "+
				"close(done) signal before reading the recorder, not race it", got, stall)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolveChatOutcome did not return")
	}
}

// TestResolveChatOutcomeNeverReturnsNilOnATimeout pins the RULING half of
// Important 3: on a wait TIMEOUT the write must never be "finished" — a chat
// downloader whose completion the orchestrator could not confirm before the
// job finalized is, by definition, not complete.
//
// Mutant: the final branch returning nil instead of a non-nil sentinel —
// chatStatusForOutcome would then derive "finished"/"unavailable" from the
// message count alone, exactly the bug this whole task exists to close.
func TestResolveChatOutcomeNeverReturnsNilOnATimeout(t *testing.T) {
	o := &DownloadOrchestrator{logger: discardLogger{}}
	var rec chatOutcome // never recorded — the goroutine is presumed stuck
	done := make(chan struct{})
	dl := &blockingChatSource{release: make(chan struct{})}
	dl.running.Store(true)

	got := o.resolveChatOutcome(dl, &rec, done, 10*time.Millisecond, 10*time.Millisecond)
	if got == nil {
		t.Fatal("resolveChatOutcome returned nil after the wait AND its cleanup grace both timed " +
			"out — chatStatusForOutcome would write \"finished\"/\"unavailable\" for a chat " +
			"downloader whose completion was never confirmed")
	}
	if !dl.stopped.Load() {
		t.Error("resolveChatOutcome did not Stop() the still-running downloader once the wait timed out")
	}
}

// --- Fix round 1, Important 1: pin the four write sites --------------------
//
// The tests above exercise chatStatusForOutcome/chatOutcome/chatFileStatus/
// recordChatOutcome/resolveChatOutcome as pure functions. None of them can
// see whether the four call sites still ROUTE through those functions —
// reverting any one of them to the pre-fix count-only rule left every test
// above green (review Mutants A, A2, D). The tests below drive the real
// production call sites instead.

// instantChatSource is a ChatSource whose Start returns immediately with a
// fixed (possibly non-nil) err and a fixed message count — used to drive
// ExecuteTwitch end to end without a live network connection.
type instantChatSource struct {
	err   error
	count int
}

func (f *instantChatSource) Start(context.Context) error { return f.err }
func (f *instantChatSource) Stop()                       {}
func (f *instantChatSource) MarkStreamEnded()            {}
func (f *instantChatSource) MessageCount() int           { return f.count }
func (f *instantChatSource) IsRunning() bool             { return false }

// TestExecuteTwitchWritesIncompleteForANonFinishedChatOutcome pins write site
// 2 (internal/worker/orchestrator_twitch.go, ExecuteTwitch).
//
// ExecuteTwitch's post-loop dispatch takes the "preserve staging for resume"
// early return (before ever reaching the chat write site) whenever
// ctx.Err() != nil — exactly like a genuine shutdown/cancel — so a
// pre-cancelled ctx (as used elsewhere in this package, e.g.
// TestExecuteTwitchRegistersTheLiveChatDownloaderForItsWholeRun in
// twitch_chat_registry_test.go) never reaches this site; the chat verdict is
// only finalized on the natural-end path. A dead TCP port reaches that path
// too, but only after the engine's segment-retry backoff burns real wall
// time (empirically ~25s, MaxSegmentRetries=5 with a growing per-attempt
// delay — internal/engine's per-test speedup knob, the `delays` struct in
// internal/engine/delays.go, is package-private and not reachable from here).
// A tiny local httptest server serving an already-ended HLS playlist
// (#EXT-X-ENDLIST, zero segments) reaches the SAME natural-end path — the
// engine's HLS loop takes the clean pl.EndList branch — in milliseconds,
// without touching the retry/backoff path at all.
//
// Mutant: reverting the site to
//
//	chatCount := twitchChatDl.MessageCount()
//	chatStatus := "finished"
//	if chatCount == 0 { chatStatus = "unavailable" }
//	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"chat_status": chatStatus, ...})
//
// — the review's Mutant A — writes "finished" here (count=5000, nonzero) and
// this test fails.
func TestExecuteTwitchWritesIncompleteForANonFinishedChatOutcome(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n" +
			"#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-ENDLIST\n"))
	}))
	defer srv.Close()

	w, db := testWorkerSetup(t)
	o := w.orchestrator

	job := &database.Job{
		ID: "tw_site_incomplete", VideoID: "site_incomplete", URL: "https://twitch.tv/videos/1",
		Platform: "twitch", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	stall := errors.New("vod chat paging stalled at offset 12 cursor \"abc\": cursor did not advance")
	cd := &instantChatSource{err: stall, count: 5000}

	jobCtx := &JobContext{Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(), Logger: &discardLogger{}}
	_ = o.ExecuteTwitch(context.Background(), jobCtx, &TwitchVariantInfo{URL: srv.URL + "/x.m3u8", Name: "720p"}, false, cd)

	got, err := db.GetJob("tw_site_incomplete")
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ChatStatus != chatStatusIncomplete {
		t.Errorf("job.ChatStatus = %q, want %q — ExecuteTwitch's write site must route the chat "+
			"downloader's returned error through recordChatOutcome, not the count-only rule",
			got.ChatStatus, chatStatusIncomplete)
	}
	if got.TotalChatMessages == nil || *got.TotalChatMessages != 5000 {
		t.Errorf("job.TotalChatMessages = %v, want 5000", got.TotalChatMessages)
	}
}

// TestFinalizeMultiSegmentJobRoutesThePerPartChatWriteThroughChatFileStatus
// pins write site 3 (internal/worker/orchestrator_mux.go,
// finalizeMultiSegmentJob's per-part chat block).
//
// Mutant: reverting `updates["chat_status"] = chatFileStatus(jobCtx)` back to
// the unconditional `updates["chat_status"] = "finished"` (review Mutant D)
// overwrites the incomplete verdict recordChatOutcome already wrote, and this
// test fails.
func TestFinalizeMultiSegmentJobRoutesThePerPartChatWriteThroughChatFileStatus(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator

	job := &database.Job{
		ID: "multi_incomplete", VideoID: "multi", URL: "https://twitch.tv/videos/2",
		Platform: "twitch", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	outDir := t.TempDir()
	part1 := filepath.Join(outDir, "part1.mp4")
	if err := os.WriteFile(part1, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	part2 := filepath.Join(outDir, "part2.mp4")
	if err := os.WriteFile(part2, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobCtx := &JobContext{
		Job: job, DB: db, Config: &JobConfig{}, StagingDir: t.TempDir(),
		OutputDir: outDir, Filename: "show.mp4", Logger: &discardLogger{},
		// Set by a prior recordChatOutcome, as it would be in production —
		// the VOD chat stalled during this job's download.
		ChatStatus: chatStatusIncomplete,
	}
	segments := []database.Segment{
		{SegmentIndex: 0, FilePath: part1, Quality: "720p", ChatFile: filepath.Join(jobCtx.StagingDir, "seg_0", "chat.json")},
		{SegmentIndex: 1, FilePath: part2, Quality: "720p"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no network for the thumbnail/description fallbacks inside copyAssets
	if err := o.finalizeMultiSegmentJob(ctx, jobCtx, segments); err != nil {
		t.Fatalf("finalizeMultiSegmentJob: %v", err)
	}

	got, err := db.GetJob("multi_incomplete")
	if err != nil || got == nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.ChatStatus != chatStatusIncomplete {
		t.Errorf("job.ChatStatus = %q, want %q — the per-part chat write must route through "+
			"chatFileStatus, not overwrite an incomplete verdict with \"finished\"",
			got.ChatStatus, chatStatusIncomplete)
	}
}

// TestCopyAssetsRoutesTheWholeJobChatCopyThroughChatFileStatus pins write
// site 4 (internal/worker/orchestrator_mux.go, copyAssets).
//
// Mutant: reverting `updates["chat_status"] = chatFileStatus(jobCtx)` back to
// the unconditional `updates["chat_status"] = "finished"` (review Mutant D)
// overwrites the incomplete verdict, and this test fails.
func TestCopyAssetsRoutesTheWholeJobChatCopyThroughChatFileStatus(t *testing.T) {
	w, db := testWorkerSetup(t)
	o := w.orchestrator

	job := &database.Job{
		ID: "copyassets_incomplete", URL: "https://www.youtube.com/watch?v=v1",
		Platform: "youtube", Status: database.StatusDownloading,
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	stagingDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stagingDir, "chat.json"), []byte(`{"messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// VideoID/ThumbnailURL/ChannelAvatarURL/Description all left empty so
	// copyAssets' thumbnail/description fallbacks are no-ops — this test
	// drives only the chat-copy branch.
	jobCtx := &JobContext{
		Job: job, DB: db, Config: &JobConfig{}, StagingDir: stagingDir,
		OutputDir: outDir, Filename: "show.mp4", Logger: &discardLogger{},
		ChatStatus: chatStatusIncomplete,
	}

	updates := map[string]any{}
	o.copyAssets(context.Background(), jobCtx, outDir, "show", "show", updates, false)

	if updates["chat_status"] != chatStatusIncomplete {
		t.Errorf("copyAssets wrote chat_status = %v, want %q — the whole-job chat copy must route "+
			"through chatFileStatus, not overwrite an incomplete verdict with \"finished\"",
			updates["chat_status"], chatStatusIncomplete)
	}
}

// TestOrchestratorGoRoutesItsChatStatusWriteThroughRecordChatOutcome pins
// write site 1 (internal/worker/orchestrator.go, ExecuteWithChat) by source
// inspection rather than by driving the function end to end.
//
// chat.ChatDownloader.Start (internal/chat/downloader.go) has exactly two
// `return` statements and both return nil unconditionally — there is
// currently no way, short of a panic, for a REAL *chat.ChatDownloader to hand
// this site a non-nil outcome, and ExecuteWithChat's existingChat parameter
// is the concrete *chat.ChatDownloader (not the ChatSource interface: several
// calls on it — SetOutputFile, SetOnProgress, and buildMayResume's
// LiveContinuationOpen/LastMessageAt — are not part of ChatSource). Widening
// that parameter to make this site fake-injectable is a larger refactor than
// this fix round's scope. This test instead pins the exact regression the
// review's mutant produced: the count-only literal pattern the site used to
// be is gone from orchestrator.go, and the site still calls
// recordChatOutcome.
//
// Mutant: reverting the site to
//
//	chatCount := chatDl.MessageCount()
//	chatStatus := "finished"
//	if chatCount == 0 { chatStatus = "unavailable" }
//	o.db.UpdateJobFields(jobCtx.Job.ID, map[string]any{"chat_status": chatStatus, ...})
//
// reintroduces the literal `chatStatus := "finished"` this test forbids.
func TestOrchestratorGoRoutesItsChatStatusWriteThroughRecordChatOutcome(t *testing.T) {
	src, err := os.ReadFile("orchestrator.go")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(src)
	if strings.Contains(text, `chatStatus := "finished"`) {
		t.Error(`orchestrator.go still contains the count-only chatStatus := "finished" literal — ` +
			`the YouTube chat write site must go through recordChatOutcome instead`)
	}
	if !strings.Contains(text, "o.recordChatOutcome(jobCtx, chatDl.MessageCount()") {
		t.Error("orchestrator.go no longer calls o.recordChatOutcome(jobCtx, chatDl.MessageCount(), " +
			"...) — the YouTube chat write site must route through it")
	}
}
