package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// TestLiveTwitchChaptersVodFirstObservedFallback is owner decision D23
// follow-up 1: a live capture that began 90 s into the broadcast takes its
// archive VOD's chapters, shifted into its own timeline; with no VOD list, the
// categories it observed, each from the moment it saw it, the first one from
// the start; with no broadcast start, the observed list alone, from the
// capture's start.
//
// Mutants: the observed list used even when the VOD has chapters; the VOD
// list not shifted by the capture's offset; the first observation kept at its
// own time (an untitled sliver in front of it).
func TestLiveTwitchChaptersVodFirstObservedFallback(t *testing.T) {
	streamStart := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rec := streamStart.Add(90 * time.Second).UnixMilli()
	tl := twitchTimeline{
		RecordingStartMs: rec,
		Categories: []twitchCategoryMark{
			{AtMs: rec + 4000, Title: "Just Chatting"},
			{AtMs: streamStart.Add(10 * time.Minute).UnixMilli(), Title: "Elden Ring"},
		},
	}
	vod := []utils.Chapter{
		{StartMs: 0, EndMs: 300_000, Title: "VOD: Chatting"},
		{StartMs: 300_000, EndMs: 900_000, Title: "VOD: Game"},
	}
	got := liveTwitchChapters(streamStart, tl, vod)
	want := []utils.Chapter{
		{StartMs: 0, EndMs: 210_000, Title: "VOD: Chatting"},
		{StartMs: 210_000, EndMs: 810_000, Title: "VOD: Game"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("with a VOD list = %+v, want %+v", got, want)
	}

	got = liveTwitchChapters(streamStart, tl, nil)
	want = []utils.Chapter{
		{StartMs: 0, Title: "Just Chatting"},
		{StartMs: 510_000, Title: "Elden Ring"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("observed fallback = %+v, want %+v", got, want)
	}

	got = liveTwitchChapters(time.Time{}, tl, vod)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without a broadcast start = %+v, want the observed list %+v", got, want)
	}
}

// TestTwitchTimelineNotes: a category is recorded only when it changes, an
// empty one never, and the recording start is overwritten by each new file's
// first segment.
//
// Mutants: the repeat check removed (the same category twice); the empty
// check removed; the recording start written once only.
func TestTwitchTimelineNotes(t *testing.T) {
	dir := t.TempDir()
	at := time.Unix(1000, 0)
	noteTwitchCategory(dir, "A", at)
	noteTwitchCategory(dir, "A", at.Add(time.Minute))
	noteTwitchCategory(dir, "", at.Add(2*time.Minute))
	noteTwitchCategory(dir, "B", at.Add(3*time.Minute))
	noteTwitchRecordingStart(dir, at)
	noteTwitchRecordingStart(dir, at.Add(time.Hour))
	tl := readTwitchTimeline(dir)
	wantCats := []twitchCategoryMark{{AtMs: at.UnixMilli(), Title: "A"}, {AtMs: at.Add(3 * time.Minute).UnixMilli(), Title: "B"}}
	if !reflect.DeepEqual(tl.Categories, wantCats) {
		t.Errorf("categories = %+v, want %+v", tl.Categories, wantCats)
	}
	if tl.RecordingStartMs != at.Add(time.Hour).UnixMilli() {
		t.Errorf("recording start = %d, want the latest file's", tl.RecordingStartMs)
	}
}

// TestPollTwitchCategoryNotesEachMinute runs the category poll in a bubble:
// one read per twitchCategoryPollInterval, a change recorded at the minute it
// was read, a failed read skipped, and the poll gone when its context ends.
//
// Mutants: the poll reading once and stopping; a failed read ending the poll.
func TestPollTwitchCategoryNotesEachMinute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		start := time.Now()
		answers := []struct {
			cat string
			err error
		}{{"A", nil}, {"", context.DeadlineExceeded}, {"B", nil}}
		var mu sync.Mutex
		reads := 0
		read := func(context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			a := answers[min(reads, len(answers)-1)]
			reads++
			return a.cat, a.err
		}
		o := &DownloadOrchestrator{logger: discardLogger{}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			o.pollTwitchCategory(ctx, dir, read)
		}()
		time.Sleep(3*twitchCategoryPollInterval + time.Second)
		synctest.Wait()
		cancel()
		<-done
		tl := readTwitchTimeline(dir)
		want := []twitchCategoryMark{
			{AtMs: start.Add(twitchCategoryPollInterval).UnixMilli(), Title: "A"},
			{AtMs: start.Add(3 * twitchCategoryPollInterval).UnixMilli(), Title: "B"},
		}
		if !reflect.DeepEqual(tl.Categories, want) {
			t.Errorf("categories = %+v, want %+v", tl.Categories, want)
		}
		if reads != 3 {
			t.Errorf("reads = %d, want 3", reads)
		}
	})
}

// TestTwitchCategoryPollStopJoins: the stop ExecuteTwitch defers waits for
// the poll to end, and a read that returns after the cancel is not noted, so
// nothing writes into staging once ExecuteTwitch has returned and the worker
// removes it. The read here is still running when stop is called and takes
// another (fake) second to come back with a category.
//
// Mutants: stop only cancelling, not waiting (it returns while the read is
// still out); the ctx check after the read removed ("Late" is noted).
func TestTwitchCategoryPollStopJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dir := t.TempDir()
		entered := make(chan struct{})
		var returned atomic.Bool
		read := func(ctx context.Context) (string, error) {
			close(entered)
			<-ctx.Done()
			time.Sleep(time.Second)
			returned.Store(true)
			return "Late", nil
		}
		o := &DownloadOrchestrator{logger: discardLogger{}}
		stop := o.startTwitchCategoryPoll(context.Background(), dir, read)
		time.Sleep(twitchCategoryPollInterval)
		<-entered
		stop()
		if !returned.Load() {
			t.Fatal("stop returned while the poll's read was still running")
		}
		if tl := readTwitchTimeline(dir); len(tl.Categories) != 0 {
			t.Errorf("categories = %+v, want none noted after the cancel", tl.Categories)
		}
	})
}

// TestStageTwitchChapters: a VOD stages its own chapters; a live capture
// stages its archive VOD's, looked up at finalize, in its own timeline.
//
// Mutant: the live arm not asking ArchiveChaptersFn (the observed list is
// staged instead).
func TestStageTwitchChapters(t *testing.T) {
	o := &DownloadOrchestrator{logger: discardLogger{}}
	vodDir := t.TempDir()
	vodList := []utils.Chapter{{StartMs: 0, EndMs: 5000, Title: "Game"}}
	o.stageTwitchChapters(context.Background(), twitchTestJobCtx(vodDir, true, ""), &TwitchVariantInfo{VodChapters: vodList}, true)
	if got, _ := utils.ReadStagedChapters(vodDir); !reflect.DeepEqual(got, vodList) {
		t.Errorf("VOD staged %+v, want %+v", got, vodList)
	}

	liveDir := t.TempDir()
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	noteTwitchRecordingStart(liveDir, start.Add(time.Minute))
	noteTwitchCategory(liveDir, "Observed", start.Add(time.Minute))
	asked := false
	variant := &TwitchVariantInfo{ArchiveChaptersFn: func(context.Context) []utils.Chapter {
		asked = true
		return []utils.Chapter{{StartMs: 0, EndMs: 180_000, Title: "From the VOD"}}
	}}
	o.stageTwitchChapters(context.Background(), twitchTestJobCtx(liveDir, false, start.Format(time.RFC3339)), variant, false)
	want := []utils.Chapter{{StartMs: 0, EndMs: 120_000, Title: "From the VOD"}}
	if got, _ := utils.ReadStagedChapters(liveDir); !asked || !reflect.DeepEqual(got, want) {
		t.Errorf("live staged %+v (VOD asked: %v), want %+v", got, asked, want)
	}
}

// TestYouTubeChaptersSeamIsTheService pins the production value of the
// YouTube chapter seam.
func TestYouTubeChaptersSeamIsTheService(t *testing.T) {
	if reflect.ValueOf(youtubeChapters).Pointer() != reflect.ValueOf(fetchYouTubeChapters).Pointer() {
		t.Fatal("youtubeChapters is not fetchYouTubeChapters")
	}
}

// twitchTestJobCtx is a JobContext for the chapter staging tests.
func twitchTestJobCtx(dir string, isVod bool, streamStart string) *JobContext {
	return &JobContext{StagingDir: dir, Job: &database.Job{
		ID: "tw_chapters", Platform: "twitch", IsVod: isVod, StreamStartTime: streamStart,
	}}
}

// TestTwitchChapterHooksAreWired pins, by source, the call sites a unit test
// cannot reach without a real live capture: ExecuteTwitch notes the starting
// category, runs the category poll, records the root part's start and stages
// the chapters before the single-part finalize; processJob hands the VOD's
// chapters, the archive lookup and the category read to the variant.
//
// Mutant: any one of those lines removed.
func TestTwitchChapterHooksAreWired(t *testing.T) {
	for file, needles := range map[string][]string{
		"orchestrator_twitch.go": {
			"noteTwitchCategory(jobCtx.StagingDir, jobCtx.Job.TwitchCategory, time.Now())",
			"defer o.startTwitchCategoryPoll(parentCtx, jobCtx.StagingDir, variant.CategoryFn)()",
			"noteTwitchRecordingStart(stagingDir, pdt)",
			"o.stageTwitchChapters(muxCtx, jobCtx, variant, isVod)",
		},
		"worker.go": {
			"variant.VodChapters = result.TwitchVodChapters",
			"variant.CategoryFn = func(innerCtx context.Context) (string, error) {",
			"return w.twitchArchiveChapters(innerCtx, login, startedAt)",
		},
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range needles {
			if !strings.Contains(string(src), n) {
				t.Errorf("%s no longer carries %q", file, n)
			}
		}
	}
}

// TestCopyAssetsCopiesTheChatRawSidecar: a capture's chat.raw.jsonl (owner
// decision D16) goes beside the archive with its chat, as
// <name>.chat.raw.jsonl.
//
// Mutant: the raw copy removed.
func TestCopyAssetsCopiesTheChatRawSidecar(t *testing.T) {
	staging, outDir := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{"chat.json": `{"messages":[]}`, "chat.raw.jsonl": "{\"a\":1}\n"} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	o := &DownloadOrchestrator{logger: discardLogger{}}
	jobCtx := &JobContext{Job: &database.Job{ID: "yt_raw"}, StagingDir: staging}
	o.copyAssets(context.Background(), jobCtx, outDir, "A Job", "A Job", map[string]any{}, false)
	got, err := os.ReadFile(filepath.Join(outDir, "A Job.chat.raw.jsonl"))
	if err != nil || string(got) != "{\"a\":1}\n" {
		t.Errorf("raw sidecar beside the archive: %q, %v", got, err)
	}
}
