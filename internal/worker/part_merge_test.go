package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/chat"
	"github.com/vampiricwulf/Moombox/internal/database"
	"github.com/vampiricwulf/Moombox/internal/engine"
)

// --- groupMergeRuns ---------------------------------------------------

func TestGroupMergeRuns(t *testing.T) {
	a := &streamParams{VCodec: "h264"}
	aAgain := &streamParams{VCodec: "h264"} // distinct pointer, equal value
	b := &streamParams{VCodec: "vp9"}

	t.Run("contiguous identical runs", func(t *testing.T) {
		got := groupMergeRuns([]*streamParams{a, aAgain, b, a})
		want := [][]int{{0, 1}, {2}, {3}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("groupMergeRuns = %v, want %v", got, want)
		}
	})

	t.Run("all identical collapses to one run", func(t *testing.T) {
		got := groupMergeRuns([]*streamParams{a, aAgain, a})
		want := [][]int{{0, 1, 2}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("groupMergeRuns = %v, want %v", got, want)
		}
	})

	t.Run("nil param is its own run and merges with nothing", func(t *testing.T) {
		got := groupMergeRuns([]*streamParams{a, nil, aAgain})
		want := [][]int{{0}, {1}, {2}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("groupMergeRuns = %v, want %v", got, want)
		}
	})

	t.Run("consecutive nils never merge with each other", func(t *testing.T) {
		got := groupMergeRuns([]*streamParams{nil, nil})
		want := [][]int{{0}, {1}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("groupMergeRuns = %v, want %v", got, want)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		got := groupMergeRuns(nil)
		if len(got) != 0 {
			t.Errorf("groupMergeRuns(nil) = %v, want empty", got)
		}
	})
}

// --- mergedSegmentRow ---------------------------------------------------

func TestMergedSegmentRow(t *testing.T) {
	width, height, fps := 1920, 1080, 60
	size0 := int64(100)

	run := []database.Segment{
		{
			ID: 1, JobID: "job1", SegmentIndex: 0, UnixStart: 1000, UnixEnd: 1100,
			Quality: "1080p60", Filename: "base - part1.mp4", FilePath: "/out/base - part1.mp4",
			FileSize: &size0, VideoWidth: &width, VideoHeight: &height, VideoFps: &fps,
			DurationSeconds: 100, ChatFile: "/out/base - part1.chat.json",
		},
		{
			ID: 2, JobID: "job1", SegmentIndex: 1, UnixStart: 1100, UnixEnd: 1250,
			Quality: "720p60", Filename: "base - part2.mp4", FilePath: "/out/base - part2.mp4",
			DurationSeconds: 150,
		},
	}

	got := mergedSegmentRow(run, "/out/base - merged0.mp4", 987654)

	if got.SegmentIndex != 0 {
		t.Errorf("SegmentIndex = %d, want 0 (first's)", got.SegmentIndex)
	}
	if got.UnixStart != 1000 {
		t.Errorf("UnixStart = %d, want 1000 (first's)", got.UnixStart)
	}
	if got.UnixEnd != 1250 {
		t.Errorf("UnixEnd = %d, want 1250 (last's)", got.UnixEnd)
	}
	if got.DurationSeconds != 250 {
		t.Errorf("DurationSeconds = %v, want 250 (sum)", got.DurationSeconds)
	}
	if got.Quality != "1080p60" {
		t.Errorf("Quality = %q, want first's 1080p60", got.Quality)
	}
	if got.VideoWidth == nil || *got.VideoWidth != width || got.VideoHeight == nil || *got.VideoHeight != height || got.VideoFps == nil || *got.VideoFps != fps {
		t.Errorf("dimensions = %v/%v/%v, want first's %d/%d/%d", got.VideoWidth, got.VideoHeight, got.VideoFps, width, height, fps)
	}
	if got.FilePath != "/out/base - merged0.mp4" {
		t.Errorf("FilePath = %q, want outPath", got.FilePath)
	}
	if got.Filename != "base - merged0.mp4" {
		t.Errorf("Filename = %q, want basename of outPath", got.Filename)
	}
	if got.FileSize == nil || *got.FileSize != 987654 {
		t.Errorf("FileSize = %v, want 987654", got.FileSize)
	}
	wantChat := "/out/base - merged0.chat.json"
	if got.ChatFile != wantChat {
		t.Errorf("ChatFile = %q, want %q (merged path since a part had chat)", got.ChatFile, wantChat)
	}

	t.Run("no part had chat", func(t *testing.T) {
		noChat := []database.Segment{run[1], run[1]}
		got := mergedSegmentRow(noChat, "/out/x - merged0.mp4", 1)
		if got.ChatFile != "" {
			t.Errorf("ChatFile = %q, want empty when no part had chat", got.ChatFile)
		}
	})
}

// --- mergeChatFiles ---------------------------------------------------

func TestMergeChatFiles(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, data chat.ChatData) string {
		t.Helper()
		p := filepath.Join(dir, name)
		b, err := json.MarshalIndent(data, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	f1 := write("a.chat.json", chat.ChatData{
		VideoID: "vid1", VideoTitle: "Title", ChannelName: "Chan", StreamStartTime: "2026-01-01T00:00:00Z",
		DownloadedAt: "2026-01-01T01:00:00Z", MessageCount: 2,
		Messages: []chat.ChatMessage{{ID: "m1"}, {ID: "m2"}},
	})
	f2 := write("b.chat.json", chat.ChatData{
		VideoID: "vid1-part2", VideoTitle: "Should not win", MessageCount: 1,
		Messages: []chat.ChatMessage{{ID: "m3"}},
	})

	out := filepath.Join(dir, "merged.chat.json")
	if err := mergeChatFiles([]string{f1, f2}, out); err != nil {
		t.Fatalf("mergeChatFiles: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got chat.ChatData
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.VideoID != "vid1" || got.VideoTitle != "Title" || got.ChannelName != "Chan" || got.StreamStartTime != "2026-01-01T00:00:00Z" {
		t.Errorf("identity fields not kept from first file: %+v", got)
	}
	if got.MessageCount != 3 {
		t.Errorf("MessageCount = %d, want 3 (summed)", got.MessageCount)
	}
	if len(got.Messages) != 3 || got.Messages[0].ID != "m1" || got.Messages[1].ID != "m2" || got.Messages[2].ID != "m3" {
		t.Errorf("Messages not concatenated in file order: %+v", got.Messages)
	}

	t.Run("missing input file errors", func(t *testing.T) {
		err := mergeChatFiles([]string{filepath.Join(dir, "nonexistent.json")}, filepath.Join(dir, "out2.json"))
		if err == nil {
			t.Fatal("expected error for missing chat file")
		}
	})

	t.Run("corrupt input file errors", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := mergeChatFiles([]string{bad}, filepath.Join(dir, "out3.json"))
		if err == nil {
			t.Fatal("expected error for corrupt chat file")
		}
	})

	t.Run("zero paths errors", func(t *testing.T) {
		if err := mergeChatFiles(nil, filepath.Join(dir, "out4.json")); err == nil {
			t.Fatal("expected error for zero input paths")
		}
	})
}

// --- partMerger.merge (fallback matrix, no ffmpeg) ---------------------

func matchingParams(_ context.Context, _, _ string) (*streamParams, error) {
	return &streamParams{VCodec: "h264", Width: 1920, Height: 1080, FrameRate: "30/1", ACodec: "aac", SampleRate: "44100", Channels: 2}, nil
}

func writeConcatStub(content []byte) func(ctx context.Context, inputs []string, outputPath string) error {
	return func(_ context.Context, _ []string, outputPath string) error {
		return os.WriteFile(outputPath, content, 0o644)
	}
}

func newTestPartMerger(t *testing.T) *partMerger {
	t.Helper()
	return &partMerger{
		ffprobePath: "unused",
		logger:      &discardLogger{},
	}
}

func TestPartMergerMerge_TrivialInputsPassThrough(t *testing.T) {
	pm := newTestPartMerger(t)
	probeCalls := 0
	pm.probe = func(ctx context.Context, ffprobePath, filePath string) (*streamParams, error) {
		probeCalls++
		return matchingParams(ctx, ffprobePath, filePath)
	}

	for _, segs := range [][]database.Segment{nil, {}, {{FilePath: "a.mp4"}}} {
		got := pm.merge(context.Background(), "job1", segs)
		if len(got) != len(segs) {
			t.Errorf("merge(%v) = %v, want unchanged", segs, got)
		}
	}
	if probeCalls != 0 {
		t.Errorf("probe called %d times for <2 segment inputs, want 0", probeCalls)
	}
}

// TestPartMergerMerge_ProbeFailureIdentity pins the interface contract: a
// probe error on ANY part aborts the WHOLE merge, returning the input slice
// completely untouched (no rows touched, concat/replace never invoked).
func TestPartMergerMerge_ProbeFailureIdentity(t *testing.T) {
	pm := newTestPartMerger(t)
	pm.probe = func(_ context.Context, _, filePath string) (*streamParams, error) {
		if filePath == "b.mp4" {
			return nil, errors.New("ffprobe: boom")
		}
		return matchingParams(context.Background(), "", filePath)
	}
	concatCalled := false
	pm.concat = func(context.Context, []string, string) error {
		concatCalled = true
		return nil
	}
	replaceCalled := false
	pm.replace = func(string, []database.Segment) error {
		replaceCalled = true
		return nil
	}

	segments := []database.Segment{
		{SegmentIndex: 0, FilePath: "a.mp4", Filename: "a.mp4"},
		{SegmentIndex: 1, FilePath: "b.mp4", Filename: "b.mp4"},
	}
	got := pm.merge(context.Background(), "job1", segments)

	if !reflect.DeepEqual(got, segments) {
		t.Errorf("merge with a probe failure = %+v, want input unchanged %+v", got, segments)
	}
	if concatCalled {
		t.Error("concat must not be called when a probe fails")
	}
	if replaceCalled {
		t.Error("db replace must not be called when a probe fails")
	}
}

// TestPartMergerMerge_NoMergeableRunIdentity confirms distinct stream params
// (no contiguous run of len>1) leaves the input untouched without ever
// calling concat/replace.
func TestPartMergerMerge_NoMergeableRunIdentity(t *testing.T) {
	pm := newTestPartMerger(t)
	pm.probe = func(_ context.Context, _, filePath string) (*streamParams, error) {
		return &streamParams{VCodec: filePath}, nil // every file gets a distinct codec
	}
	concatCalled := false
	pm.concat = func(context.Context, []string, string) error {
		concatCalled = true
		return nil
	}
	replaceCalled := false
	pm.replace = func(string, []database.Segment) error {
		replaceCalled = true
		return nil
	}

	segments := []database.Segment{
		{SegmentIndex: 0, FilePath: "a.mp4"},
		{SegmentIndex: 1, FilePath: "b.mp4"},
		{SegmentIndex: 2, FilePath: "c.mp4"},
	}
	got := pm.merge(context.Background(), "job1", segments)

	if !reflect.DeepEqual(got, segments) {
		t.Errorf("merge with no mergeable run = %+v, want input unchanged %+v", got, segments)
	}
	if concatCalled || replaceCalled {
		t.Error("concat/replace must not be called when no run has len>1")
	}
}

// TestPartMergerMerge_ConcatFailureAborts confirms a concat failure on any
// run aborts the WHOLE merge (not just that run), leaving every original
// part file untouched and never calling db replace.
func TestPartMergerMerge_ConcatFailureAborts(t *testing.T) {
	dir := t.TempDir()
	part1 := filepath.Join(dir, "base - part1.mp4")
	part2 := filepath.Join(dir, "base - part2.mp4")
	if err := os.WriteFile(part1, []byte("orig1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part2, []byte("orig2"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	pm.concat = func(context.Context, []string, string) error {
		return errors.New("ffmpeg: boom")
	}
	replaceCalled := false
	pm.replace = func(string, []database.Segment) error {
		replaceCalled = true
		return nil
	}

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2},
	}
	got := pm.merge(context.Background(), "job1", segments)

	if !reflect.DeepEqual(got, segments) {
		t.Errorf("merge after concat failure = %+v, want input unchanged", got)
	}
	if replaceCalled {
		t.Error("db replace must not be called after a concat failure")
	}
	if b, err := os.ReadFile(part1); err != nil || string(b) != "orig1" {
		t.Errorf("part1 mutated by aborted merge: content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(part2); err != nil || string(b) != "orig2" {
		t.Errorf("part2 mutated by aborted merge: content=%q err=%v", b, err)
	}
}

// TestPartMergerMerge_ReplaceFailureAborts confirms a db.ReplaceJobSegments
// failure leaves the original parts on disk untouched (no rename ever
// happens before a successful replace) and cleans up the temp concat output.
func TestPartMergerMerge_ReplaceFailureAborts(t *testing.T) {
	dir := t.TempDir()
	part1 := filepath.Join(dir, "base - part1.mp4")
	part2 := filepath.Join(dir, "base - part2.mp4")
	if err := os.WriteFile(part1, []byte("orig1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part2, []byte("orig2"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	pm.concat = writeConcatStub([]byte("merged-bytes"))
	pm.replace = func(string, []database.Segment) error {
		return errors.New("db: locked")
	}

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2},
	}
	got := pm.merge(context.Background(), "job1", segments)

	if !reflect.DeepEqual(got, segments) {
		t.Errorf("merge after replace failure = %+v, want input unchanged", got)
	}
	if b, err := os.ReadFile(part1); err != nil || string(b) != "orig1" {
		t.Errorf("part1 mutated after aborted replace: content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(part2); err != nil || string(b) != "orig2" {
		t.Errorf("part2 mutated after aborted replace: content=%q err=%v", b, err)
	}
	tempPath := filepath.Join(dir, "base - merged0.mp4")
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf("temp concat output not cleaned up after aborted replace: stat err = %v", err)
	}
}

// TestPartMergerMerge_HappyPath exercises the full commit pipeline with
// stubbed probe/concat/replace/updateFile but REAL filesystem renames and a
// REAL chat merge: two contiguous same-format parts collapse into one row
// whose FilePath/ChatFile land on the first part's original names, and the
// superseded second part's video+chat files are deleted.
func TestPartMergerMerge_HappyPath(t *testing.T) {
	dir := t.TempDir()
	part1 := filepath.Join(dir, "base - part1.mp4")
	part2 := filepath.Join(dir, "base - part2.mp4")
	chat1 := filepath.Join(dir, "base - part1.chat.json")
	chat2 := filepath.Join(dir, "base - part2.chat.json")

	if err := os.WriteFile(part1, []byte("orig1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part2, []byte("orig2"), 0o644); err != nil {
		t.Fatal(err)
	}
	chatJSON := func(id string) []byte {
		b, _ := json.Marshal(chat.ChatData{VideoID: "v1", MessageCount: 1, Messages: []chat.ChatMessage{{ID: id}}})
		return b
	}
	if err := os.WriteFile(chat1, chatJSON("m1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chat2, chatJSON("m2"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	pm.concat = writeConcatStub([]byte("merged-bytes"))

	var replacedWith []database.Segment
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 500 + i
			segs[i].JobID = jobID
		}
		replacedWith = segs
		return nil
	}
	var updateFileCalls int
	pm.updateFile = func(id int, filename, filePath, chatFile string) error {
		updateFileCalls++
		return nil
	}

	segments := []database.Segment{
		{SegmentIndex: 0, UnixStart: 1000, UnixEnd: 1100, Quality: "1080p60", Filename: "base - part1.mp4", FilePath: part1, DurationSeconds: 100, ChatFile: chat1},
		{SegmentIndex: 1, UnixStart: 1100, UnixEnd: 1200, Quality: "1080p60", Filename: "base - part2.mp4", FilePath: part2, DurationSeconds: 100, ChatFile: chat2},
	}
	got := pm.merge(context.Background(), "job1", segments)

	if len(got) != 1 {
		t.Fatalf("merged output = %d rows, want 1", len(got))
	}
	if replacedWith == nil {
		t.Fatal("db replace was never called")
	}
	if updateFileCalls != 1 {
		t.Errorf("updateFile called %d times, want 1", updateFileCalls)
	}

	row := got[0]
	if row.FilePath != part1 {
		t.Errorf("merged FilePath = %q, want %q (renamed onto first part)", row.FilePath, part1)
	}
	if row.DurationSeconds != 200 {
		t.Errorf("merged DurationSeconds = %v, want 200", row.DurationSeconds)
	}
	if row.UnixStart != 1000 || row.UnixEnd != 1200 {
		t.Errorf("merged span = [%d,%d], want [1000,1200]", row.UnixStart, row.UnixEnd)
	}

	// Final video content is the concat output, at the first part's name.
	if b, err := os.ReadFile(part1); err != nil || string(b) != "merged-bytes" {
		t.Errorf("part1 path after merge: content=%q err=%v, want merged-bytes", b, err)
	}
	// Second part's video+chat are superseded and removed.
	if _, err := os.Stat(part2); !os.IsNotExist(err) {
		t.Errorf("superseded part2 video not deleted: stat err = %v", err)
	}
	if _, err := os.Stat(chat2); !os.IsNotExist(err) {
		t.Errorf("superseded part2 chat not deleted: stat err = %v", err)
	}
	// Merged chat lands at part1's chat name with both messages combined.
	if row.ChatFile != chat1 {
		t.Errorf("merged ChatFile = %q, want %q", row.ChatFile, chat1)
	}
	raw, err := os.ReadFile(chat1)
	if err != nil {
		t.Fatalf("read merged chat: %v", err)
	}
	var mergedChat chat.ChatData
	if err := json.Unmarshal(raw, &mergedChat); err != nil {
		t.Fatalf("unmarshal merged chat: %v", err)
	}
	if mergedChat.MessageCount != 2 || len(mergedChat.Messages) != 2 {
		t.Errorf("merged chat = %+v, want 2 combined messages", mergedChat)
	}
}

// requireFFmpegTools is defined in probe_params_test.go (same package).

// TestMergeSameFormatParts_RealFFmpegEndToEnd exercises
// DownloadOrchestrator.mergeSameFormatParts through a real ffmpeg/ffprobe
// toolchain and a real database: two identically-encoded part fixtures
// collapse into a single segment row renamed onto the first part's path.
// Skips when ffmpeg/ffprobe aren't on PATH.
func TestMergeSameFormatParts_RealFFmpegEndToEnd(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)

	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	job := &database.Job{ID: "yt_mergeE2E", VideoID: "mergeE2E", URL: "u", Status: database.StatusMuxing}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}

	part1 := filepath.Join(dir, "Show - part1.mp4")
	part2 := filepath.Join(dir, "Show - part2.mp4")
	for _, p := range []string{part1, part2} {
		cmd := exec.Command(ffmpegPath,
			"-y",
			"-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=30",
			"-f", "lavfi", "-i", "sine=duration=1",
			"-c:v", "libx264",
			"-c:a", "aac",
			p,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to generate fixture %s: %v\n%s", p, err, out)
		}
	}

	if err := db.AddSegment(&database.Segment{JobID: job.ID, SegmentIndex: 0, UnixStart: 1000, UnixEnd: 1010, Quality: "72p30", Filename: "Show - part1.mp4", FilePath: part1, DurationSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSegment(&database.Segment{JobID: job.ID, SegmentIndex: 1, UnixStart: 1010, UnixEnd: 1020, Quality: "72p30", Filename: "Show - part2.mp4", FilePath: part2, DurationSeconds: 1}); err != nil {
		t.Fatal(err)
	}

	segs, err := db.GetSegments(job.ID)
	if err != nil {
		t.Fatal(err)
	}

	muxer := engine.NewMuxer(ffmpegPath, &discardLogger{})
	o := &DownloadOrchestrator{muxer: muxer, db: db, logger: &discardLogger{}}
	jobCtx := &JobContext{Job: job}

	got := o.mergeSameFormatParts(context.Background(), jobCtx, segs)
	if len(got) != 1 {
		t.Fatalf("segments after merge = %d, want 1", len(got))
	}
	if got[0].FilePath != part1 {
		t.Errorf("merged FilePath = %q, want %q (renamed onto first part)", got[0].FilePath, part1)
	}
	if info, err := os.Stat(part1); err != nil || info.Size() == 0 {
		t.Errorf("merged output at %s: info=%v err=%v", part1, info, err)
	}
	if _, err := os.Stat(part2); !os.IsNotExist(err) {
		t.Errorf("superseded part2 should have been deleted, stat err = %v", err)
	}

	dbSegs, err := db.GetSegments(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dbSegs) != 1 {
		t.Fatalf("db segments after merge = %d, want 1", len(dbSegs))
	}
	if dbSegs[0].FilePath != part1 {
		t.Errorf("db segment FilePath = %q, want %q", dbSegs[0].FilePath, part1)
	}
}
