package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/utils"
)

// chapterLogger counts the Warn lines GET /api/jobs/{id}/chapters writes, so
// "a failed probe is reported once per file version" can be pinned. The
// anonymous four-method interface, repeated — never extracted.
type chapterLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *chapterLogger) Debug(msg string, args ...any) {}
func (l *chapterLogger) Info(msg string, args ...any)  {}
func (l *chapterLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg+" "+fmt.Sprint(args...))
}
func (l *chapterLogger) Error(msg string, args ...any) {}
func (l *chapterLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

// chapterFixture is the jobs fixture with GET /api/jobs/{id}/chapters wired
// over a cache whose probe is the real ffprobeChapters, counted.
type chapterFixture struct {
	*jobsFixture
	cache  *chapterCache
	logger *chapterLogger
	mu     sync.Mutex
	probes int
}

func newChapterFixture(t *testing.T) *chapterFixture {
	t.Helper()
	f := &chapterFixture{jobsFixture: newJobsFixture(t), logger: &chapterLogger{}}
	f.cache = newChapterCache(chapterCacheSize)
	real := f.cache.probe
	f.cache.probe = func(ctx context.Context, ffprobePath, file string) ([]Chapter, error) {
		f.mu.Lock()
		f.probes++
		f.mu.Unlock()
		return real(ctx, ffprobePath, file)
	}
	chapterRoutes(f.router, &ChapterRouteDeps{DB: f.db, Store: f.store, Logger: f.logger}, f.cache)
	return f
}

func (f *chapterFixture) probeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes
}

// get answers GET /api/jobs/{id}/chapters, asserting the 200 every answer for
// an existing job must be and the never-null list the player reads.
func (f *chapterFixture) get(t *testing.T, jobID string) []Chapter {
	t.Helper()
	rec := doRequest(t, f.router, "GET", "/api/jobs/"+jobID+"/chapters", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET chapters: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Chapters []Chapter `json:"chapters"`
	}
	if !strings.Contains(rec.Body.String(), `"chapters":[`) {
		t.Fatalf("body %s: want a chapters array, never null", rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return body.Chapters
}

// requireFFmpeg skips a live-tool test where FFmpeg is not installed, as
// internal/engine's muxer_chapters_test.go does.
func requireFFmpeg(t *testing.T) string {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	return ffmpegPath
}

// makeChapteredMP4 writes a seconds-long MP4 at path carrying the given
// chapters (none when nil), muxed the way W22 embeds them: an FFMETADATA1
// input mapped with -map_chapters.
func makeChapteredMP4(t *testing.T, ffmpegPath, path string, seconds int, chapters []utils.Chapter) {
	t.Helper()
	args := []string{"-nostdin", "-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=32x32:rate=5:duration=%d", seconds)}
	if chapters != nil {
		meta := path + ".ffmeta"
		list := utils.SanitizeChapters(chapters, int64(seconds)*1000)
		if err := os.WriteFile(meta, []byte(utils.FFMetadataChapters(list)), 0o644); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(meta)
		args = append(args, "-f", "ffmetadata", "-i", meta, "-map", "0", "-map_chapters", "1")
	}
	args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", path)
	if out, err := exec.Command(ffmpegPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate %s: %v\n%s", filepath.Base(path), err, out)
	}
}

// near compares a probed time with a tolerance for the container's timebase.
func near(got, want float64) bool { return got > want-0.05 && got < want+0.05 }

func assertChapters(t *testing.T, got []Chapter, want []Chapter) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("chapters = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Title != want[i].Title || !near(got[i].Start, want[i].Start) || !near(got[i].End, want[i].End) {
			t.Errorf("chapter %d = %+v, want %+v (all: %+v)", i, got[i], want[i], got)
		}
	}
}

// TestChaptersSingleFileArchive: a single-file job's archive with chapters
// embedded answers them, in the file's own seconds (one file IS the global
// timeline), titles intact.
//
// Mutant: the route answering an empty list without probing — this fails.
func TestChaptersSingleFileArchive(t *testing.T) {
	ffmpegPath := requireFFmpeg(t)
	f := newChapterFixture(t)
	makeChapteredMP4(t, ffmpegPath, filepath.Join(f.outputDir, "vod.mp4"), 6, []utils.Chapter{
		{StartMs: 0, Title: "Just Chatting"},
		{StartMs: 2500, Title: "Elden Ring"},
	})
	f.addJob(t, "tw_v1", func(j *database.Job) { j.Filename = "vod.mp4" })

	assertChapters(t, f.get(t, "tw_v1"), []Chapter{
		{Start: 0, End: 2.5, Title: "Just Chatting"},
		{Start: 2.5, End: 6, Title: "Elden Ring"},
	})
}

// TestChaptersMultiPartShiftedByPartOffset: a multi-part job's chapters come
// from each part's own file, shifted onto the player's global timeline by the
// sum of the earlier parts' durationSeconds (SegmentPlayer.init), and come
// back as one list in time order. A part's chapter never runs past its own
// part: part 1's file is 6 s long but its row says 5, so its last chapter
// ends at 5 where part 2 begins. Part rows are inserted out of order to pin
// that the offsets follow segment_index, not insertion.
//
// Mutants: no shift (part 2's chapters land at 0 and 2); the offset taken
// from the part's own index instead of the running sum; the end clamp
// dropped (part 1's last chapter ends at 6).
func TestChaptersMultiPartShiftedByPartOffset(t *testing.T) {
	ffmpegPath := requireFFmpeg(t)
	f := newChapterFixture(t)
	p1 := filepath.Join(f.outputDir, "live - part1.mp4")
	p2 := filepath.Join(f.outputDir, "live - part2.mp4")
	makeChapteredMP4(t, ffmpegPath, p1, 6, []utils.Chapter{
		{StartMs: 0, Title: "Just Chatting"},
		{StartMs: 3000, Title: "Elden Ring"},
	})
	makeChapteredMP4(t, ffmpegPath, p2, 4, []utils.Chapter{
		{StartMs: 0, Title: "Elden Ring"},
		{StartMs: 2000, Title: "Outro"},
	})
	f.addJob(t, "tw_live", func(j *database.Job) {
		j.Platform = "twitch"
		j.Segments = []database.Segment{
			{SegmentIndex: 1, Filename: filepath.Base(p2), FilePath: p2, DurationSeconds: 4},
			{SegmentIndex: 0, Filename: filepath.Base(p1), FilePath: p1, DurationSeconds: 5},
		}
	})
	if segs, err := f.db.GetSegments("tw_live"); err != nil || len(segs) != 2 {
		t.Fatalf("seed parts: %v, %+v", err, segs)
	}

	assertChapters(t, f.get(t, "tw_live"), []Chapter{
		{Start: 0, End: 3, Title: "Just Chatting"},
		{Start: 3, End: 5, Title: "Elden Ring"},
		{Start: 5, End: 7, Title: "Elden Ring"},
		{Start: 7, End: 9, Title: "Outro"},
	})
}

// TestChaptersEmptyAnswers: an archive with no chapters, a file that is not
// media at all (ffprobe refuses it), and a job whose file is gone each answer
// 200 with an empty list — the player shows nothing and never errors.
//
// Mutants: the missing-file arm answering the /video route's 404 (get's 200
// assertion fails); a probe failure answered as a 500.
func TestChaptersEmptyAnswers(t *testing.T) {
	ffmpegPath := requireFFmpeg(t)
	f := newChapterFixture(t)
	makeChapteredMP4(t, ffmpegPath, filepath.Join(f.outputDir, "plain.mp4"), 2, nil)
	if err := os.WriteFile(filepath.Join(f.outputDir, "junk.mp4"), []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.addJob(t, "yt_plain", func(j *database.Job) { j.Filename = "plain.mp4" })
	f.addJob(t, "yt_junk", func(j *database.Job) { j.Filename = "junk.mp4" })
	f.addJob(t, "yt_gone", func(j *database.Job) { j.Filename = "gone.mp4" })
	f.addJob(t, "yt_none", nil)

	for _, id := range []string{"yt_plain", "yt_junk", "yt_gone", "yt_none"} {
		if got := f.get(t, id); len(got) != 0 {
			t.Errorf("%s: chapters = %+v, want none", id, got)
		}
	}
	// The refused file is reported, once.
	if n := f.logger.warnCount(); n != 1 {
		t.Errorf("Warn lines = %d (%v), want 1 for the file ffprobe refused", n, f.logger.warns)
	}
	f.get(t, "yt_junk")
	if n := f.logger.warnCount(); n != 1 {
		t.Errorf("Warn lines after a second open = %d, want still 1: the refusal is cached with the file's version", n)
	}
	if rec := doRequest(t, f.router, "GET", "/api/jobs/yt_nosuchjob/chapters", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown job: want 404, got %d", rec.Code)
	}
}

// TestChaptersCacheFollowsTheFileVersion: an unchanged file is probed once
// however often the player opens it; a rewrite that changes its size, and
// one that changes only its modification time, are each probed afresh — the
// cache invalidates itself on a re-mux.
//
// Mutants: the size comparison dropped (the same-mtime rewrite is answered
// from the stale entry: old chapters, no probe); the mtime comparison dropped
// (the touch is not probed); no cache at all (the second open probes).
func TestChaptersCacheFollowsTheFileVersion(t *testing.T) {
	ffmpegPath := requireFFmpeg(t)
	f := newChapterFixture(t)
	path := filepath.Join(f.outputDir, "vod.mp4")
	makeChapteredMP4(t, ffmpegPath, path, 4, []utils.Chapter{{StartMs: 0, Title: "First"}})
	f.addJob(t, "tw_v2", func(j *database.Job) { j.Filename = "vod.mp4" })

	f.get(t, "tw_v2")
	f.get(t, "tw_v2")
	if n := f.probeCount(); n != 1 {
		t.Fatalf("probes after two opens of an unchanged file = %d, want 1", n)
	}

	// A re-mux: a different file (and size) at the same path.
	makeChapteredMP4(t, ffmpegPath, path, 6, []utils.Chapter{{StartMs: 0, Title: "First"}, {StartMs: 3000, Title: "Second"}})
	if got := f.get(t, "tw_v2"); len(got) != 2 || got[1].Title != "Second" {
		t.Errorf("after the rewrite: chapters = %+v, want the new file's two", got)
	}
	if n := f.probeCount(); n != 2 {
		t.Fatalf("probes after the rewrite = %d, want 2", n)
	}

	// A rewrite to a different size that keeps the modification time (a
	// copy that preserves it, a coarse-mtime filesystem): the size alone
	// must miss.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	makeChapteredMP4(t, ffmpegPath, path, 2, []utils.Chapter{{StartMs: 0, Title: "Only"}})
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, "tw_v2"); len(got) != 1 || got[0].Title != "Only" {
		t.Errorf("after a same-mtime rewrite: chapters = %+v, want the new file's one", got)
	}
	if n := f.probeCount(); n != 3 {
		t.Fatalf("probes after a same-mtime rewrite = %d, want 3", n)
	}

	// Same bytes, a new modification time.
	if info, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	f.get(t, "tw_v2")
	if n := f.probeCount(); n != 4 {
		t.Fatalf("probes after an mtime-only change = %d, want 4", n)
	}
	f.get(t, "tw_v2")
	if n := f.probeCount(); n != 4 {
		t.Fatalf("probes after reopening the touched file = %d, want still 4", n)
	}
}

// TestChaptersAnsweredInTimeOrder: whatever order a file lists its chapters
// in, and however the parts' lists interleave, the answer is in time order.
// Offline: the probe is stood in with out-of-order lists (ffprobe answers in
// the container's order, which nothing obliges to be chronological).
//
// Mutant: the sort dropped.
func TestChaptersAnsweredInTimeOrder(t *testing.T) {
	f := newChapterFixture(t)
	lists := map[string][]Chapter{
		"one.mp4": {{Start: 4, End: 6, Title: "late"}, {Start: 0, End: 4, Title: "early"}},
		"p1.mp4":  {{Start: 3, End: 5, Title: "p1 late"}, {Start: 0, End: 3, Title: "p1 early"}},
		"p2.mp4":  {{Start: 1, End: 4, Title: "p2 late"}, {Start: 0, End: 1, Title: "p2 early"}},
	}
	for name := range lists {
		if err := os.WriteFile(filepath.Join(f.outputDir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.cache.probe = func(ctx context.Context, ffprobePath, file string) ([]Chapter, error) {
		return slices.Clone(lists[filepath.Base(file)]), nil
	}
	f.addJob(t, "yt_one", func(j *database.Job) { j.Filename = "one.mp4" })
	f.addJob(t, "tw_parts", func(j *database.Job) {
		j.Segments = []database.Segment{
			{SegmentIndex: 0, FilePath: filepath.Join(f.outputDir, "p1.mp4"), DurationSeconds: 5},
			{SegmentIndex: 1, FilePath: filepath.Join(f.outputDir, "p2.mp4"), DurationSeconds: 4},
		}
	})
	titles := func(cs []Chapter) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Title)
		}
		return out
	}
	if got, want := titles(f.get(t, "yt_one")), []string{"early", "late"}; !slices.Equal(got, want) {
		t.Errorf("single file: %v, want %v", got, want)
	}
	if got, want := titles(f.get(t, "tw_parts")), []string{"p1 early", "p1 late", "p2 early", "p2 late"}; !slices.Equal(got, want) {
		t.Errorf("parts: %v, want %v", got, want)
	}
}

// TestChapterCacheIsBounded: the cache holds at most its bound, dropping the
// least recently read file first. Offline: the probe is stood in.
//
// Mutants: the eviction loop dropped (the first file is still held); the
// read not refreshing an entry's recency (the re-read file is the one
// dropped).
func TestChapterCacheIsBounded(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i := range 3 {
		p := filepath.Join(dir, fmt.Sprintf("f%d.mp4", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	probed := map[string]int{}
	c := newChapterCache(2)
	c.probe = func(ctx context.Context, ffprobePath, file string) ([]Chapter, error) {
		probed[file]++
		return []Chapter{{Title: filepath.Base(file)}}, nil
	}
	log := &chapterLogger{}
	read := func(i int) { c.chapters(context.Background(), "ffprobe", files[i], "job", log) }

	read(0)
	read(1)
	read(0) // f0 is now the most recently read
	read(2) // over the bound: f1, the least recently read, goes
	if c.order.Len() != 2 || len(c.byKey) != 2 {
		t.Fatalf("cache holds %d/%d entries, want 2", c.order.Len(), len(c.byKey))
	}
	read(0)
	if probed[files[0]] != 1 {
		t.Errorf("f0 probed %d times, want 1: it was read most recently and must be kept", probed[files[0]])
	}
	read(1)
	if probed[files[1]] != 2 {
		t.Errorf("f1 probed %d times, want 2: it was the least recently read and must have been dropped", probed[files[1]])
	}
}

// TestChapterCacheRetriesWhatSaysNothingAboutTheFile: a probe that never ran
// (ffprobe not installed yet) or that the request abandoned is not a verdict
// on the file, so it is not cached and the next open probes again; ffprobe's
// refusal of the file (unreadable output here) is, and is reported once.
//
// The request-gone case is the real shape ffprobeChapters returns for it:
// CommandContext kills the child, which answers an *exec.ExitError ("signal:
// killed") joined with the context's error, so only the ctx check keeps it
// from reading as an exit-status refusal. A probe that hit its own timeout
// is retried too: the bound is for a stalled share, which recovers with the
// file's size and mtime unchanged.
//
// Mutants: every error cached (the not-installed probe is not retried); none
// cached (the refused file is probed and warned about twice); the ctx check
// dropped (the killed probe is cached and warned about); a timeout cached.
//
// Covered too: an exit status alone is a refusal, cached and warned once.
func TestChapterCacheRetriesWhatSaysNothingAboutTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		err       error
		cancel    bool
		wantProbe int
		wantWarns int
	}{
		{"ffprobe did not start", exec.ErrNotFound, false, 2, 0},
		{"request went away", errors.Join(&exec.ExitError{}, context.Canceled), true, 2, 0},
		{"ffprobe refused the file", errors.Join(errUnreadableChapters, errors.New("bad json")), false, 1, 1},
		{"ffprobe exited non-zero", &exec.ExitError{}, false, 1, 1},
		{"probe timed out", errors.Join(&exec.ExitError{}, context.DeadlineExceeded), false, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			c := newChapterCache(4)
			c.probe = func(ctx context.Context, ffprobePath, file string) ([]Chapter, error) {
				probes++
				return nil, tc.err
			}
			log := &chapterLogger{}
			ctx := context.Background()
			if tc.cancel {
				cctx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cctx
			}
			for range 2 {
				if got := c.chapters(ctx, "ffprobe", path, "job", log); len(got) != 0 {
					t.Fatalf("chapters = %+v, want none", got)
				}
			}
			if probes != tc.wantProbe || log.warnCount() != tc.wantWarns {
				t.Errorf("probes = %d, warns = %d; want %d, %d", probes, log.warnCount(), tc.wantProbe, tc.wantWarns)
			}
		})
	}
}

// TestChaptersPartsFollowThePlayersTimeline: the multi-part route lays parts
// out as SegmentPlayer.init does and reads only what segment /video would
// serve. Offline: the probe is stood in. The parts, in segment_index order:
//   - 0: no file yet, 50 s — skipped, but still holds its 50 s of timeline;
//   - 1: a file outside the job's output directory, 10 s — refused by the
//     containment guard (its titles never served), still 10 s of timeline;
//   - 2: in the directory, 200 s — shifted by both, 60 s; its row is a
//     whole-second length a little short of the file (a W14 gap-fill's
//     original-file row), so its chapter starting past 200 s is dropped
//     rather than kept as a zero-length span inside part 3;
//   - 3: in the directory, no duration yet — its chapter's end is kept as
//     read, not collapsed onto its start.
//
// Mutants: the offset advanced only past a part with a file (C lands at 150);
// the containment guard bypassed (the outside part's chapter is served); the
// late chapter kept; the end clamp applied to a part with no duration (D's
// end is 260).
func TestChaptersPartsFollowThePlayersTimeline(t *testing.T) {
	f := newChapterFixture(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.mp4")
	p2 := filepath.Join(f.outputDir, "p2.mp4")
	p3 := filepath.Join(f.outputDir, "p3.mp4")
	for _, p := range []string{outside, p2, p3} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lists := map[string][]Chapter{
		outside: {{Start: 0, End: 10, Title: "outside"}},
		p2:      {{Start: 0, End: 100, Title: "B"}, {Start: 100, End: 200.5, Title: "C"}, {Start: 200.5, End: 200.9, Title: "C tail"}},
		p3:      {{Start: 0, End: 30, Title: "D"}},
	}
	f.cache.probe = func(ctx context.Context, ffprobePath, file string) ([]Chapter, error) {
		for want, l := range lists {
			if same, _ := filepath.EvalSymlinks(want); file == want || file == same {
				return slices.Clone(l), nil
			}
		}
		return nil, nil
	}
	f.addJob(t, "tw_gaps", func(j *database.Job) {
		j.Platform = "twitch"
		j.Segments = []database.Segment{
			{SegmentIndex: 0, DurationSeconds: 50},
			{SegmentIndex: 1, FilePath: outside, DurationSeconds: 10},
			{SegmentIndex: 2, FilePath: p2, DurationSeconds: 200},
			{SegmentIndex: 3, FilePath: p3},
		}
	})
	got := f.get(t, "tw_gaps")
	want := []Chapter{
		{Start: 60, End: 160, Title: "B"},
		{Start: 160, End: 260, Title: "C"},
		{Start: 260, End: 290, Title: "D"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chapters = %+v, want %+v", got, want)
	}
}

// TestParseFFprobeChaptersReadsTheTitleTag: ffprobe's JSON chapter list is
// read in the file's seconds, a chapter's title from its "title" tag or, for
// a muxer that writes the tag in capitals (Matroska), its "TITLE" tag; an
// untitled chapter keeps an empty title, and output that is not ffprobe's
// JSON is errUnreadableChapters.
//
// Mutant: the TITLE fallback dropped (the second chapter's title is empty).
func TestParseFFprobeChaptersReadsTheTitleTag(t *testing.T) {
	got, err := parseFFprobeChapters([]byte(`{"chapters":[
		{"start_time":"0.000000","end_time":"12.500000","tags":{"title":"Lower"}},
		{"start_time":"12.500000","end_time":"30.000000","tags":{"TITLE":"Upper"}},
		{"start_time":"30.000000","end_time":"31.000000"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Chapter{{Start: 0, End: 12.5, Title: "Lower"}, {Start: 12.5, End: 30, Title: "Upper"}, {Start: 30, End: 31}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chapters = %+v, want %+v", got, want)
	}
	if _, err := parseFFprobeChapters([]byte("not json")); !errors.Is(err, errUnreadableChapters) {
		t.Errorf("unreadable output: err = %v, want errUnreadableChapters", err)
	}
}

// TestChapterRouteSeamsProductionValues pins the production values behind
// the route's seams: the cache's probe is the real ffprobe reader, the probe
// is bounded at 10 s, and the cache at 256 files.
func TestChapterRouteSeamsProductionValues(t *testing.T) {
	if got, want := reflect.ValueOf(newChapterCache(1).probe).Pointer(), reflect.ValueOf(ffprobeChapters).Pointer(); got != want {
		t.Error("newChapterCache's probe is not ffprobeChapters")
	}
	if chapterProbeTimeout != 10*time.Second {
		t.Errorf("chapterProbeTimeout = %v, want 10s", chapterProbeTimeout)
	}
	if chapterCacheSize != 256 {
		t.Errorf("chapterCacheSize = %d, want 256", chapterCacheSize)
	}
}
