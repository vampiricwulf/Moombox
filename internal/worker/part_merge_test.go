package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
		rename:      os.Rename,
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
		got := pm.merge(context.Background(), "job1", "", segs)
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
	got := pm.merge(context.Background(), "job1", "", segments)

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
	got := pm.merge(context.Background(), "job1", "", segments)

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
	got := pm.merge(context.Background(), "job1", "", segments)

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
	got := pm.merge(context.Background(), "job1", "", segments)

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
	got := pm.merge(context.Background(), "job1", "", segments)

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

// TestPartMergerMerge_ChatMergeFailureAbortsRun is the coordinator's C1
// (Tier-4 twitch chat orphan) regression test: when mergeChatFiles fails for
// a run (here, part2's chat JSON is corrupt — standing in for the real
// production case, a Twitch part whose chat is twitch.TwitchChatData,
// unmarshaled here as chat.ChatData, which always errors on the schema
// mismatch), the WHOLE run aborts in full identity: media is NOT merged
// either, db.ReplaceJobSegments/UpdateSegmentFile are never called, and
// every original file (video + chat, both parts) is left byte-for-byte
// untouched. This replaces the old "keep the media merge, blank the row's
// ChatFile" fallback, which orphaned the per-part chat files' rows and lost
// chat replay for exactly the one production case (Twitch) that ever
// carries per-part ChatFile.
func TestPartMergerMerge_ChatMergeFailureAbortsRun(t *testing.T) {
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
	validChat, _ := json.Marshal(chat.ChatData{VideoID: "v1", MessageCount: 1, Messages: []chat.ChatMessage{{ID: "m1"}}})
	if err := os.WriteFile(chat1, validChat, 0o644); err != nil {
		t.Fatal(err)
	}
	// part2's chat is corrupt — mergeChatFiles must fail on it.
	if err := os.WriteFile(chat2, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	pm.concat = writeConcatStub([]byte("merged-bytes"))
	replaceCalled := false
	pm.replace = func(jobID string, segs []database.Segment) error {
		replaceCalled = true
		return nil
	}
	updateFileCalled := false
	pm.updateFile = func(id int, filename, filePath, chatFile string) error {
		updateFileCalled = true
		return nil
	}

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1, DurationSeconds: 100, ChatFile: chat1},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2, DurationSeconds: 100, ChatFile: chat2},
	}
	got := pm.merge(context.Background(), "job1", "", segments)

	// Full identity: the run (here, the entire batch — its only run) is
	// returned completely untouched, and neither DB call ever fires.
	if !reflect.DeepEqual(got, segments) {
		t.Errorf("merge with a failed chat merge = %+v, want input unchanged (C1 run-abort)", got)
	}
	if replaceCalled {
		t.Error("db.ReplaceJobSegments must not be called when the only run's chat merge fails (C1 run-abort)")
	}
	if updateFileCalled {
		t.Error("db.UpdateSegmentFile must not be called when the only run's chat merge fails (C1 run-abort)")
	}

	// Media is NOT merged: both parts hold their original bytes.
	if b, err := os.ReadFile(part1); err != nil || string(b) != "orig1" {
		t.Errorf("part1 must be untouched (media must not merge on a chat-merge failure): content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(part2); err != nil || string(b) != "orig2" {
		t.Errorf("part2 must be untouched: content=%q err=%v", b, err)
	}
	// Both per-part chat files survive on disk, byte-identical.
	if b, err := os.ReadFile(chat1); err != nil || string(b) != string(validChat) {
		t.Errorf("part1 chat file must survive untouched: content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(chat2); err != nil || string(b) != "{not valid json" {
		t.Errorf("part2 chat file must survive untouched: content=%q err=%v", b, err)
	}
	// The orphaned merge temp output must not be left behind either.
	if _, err := os.Stat(filepath.Join(dir, "base - merged0.mp4")); !os.IsNotExist(err) {
		t.Errorf("temp concat output must be cleaned up after a chat-merge-failure run-abort: stat err = %v", err)
	}
}

// TestPartMergerMerge_ChatMergeFailureRunIsolatesFromOtherRuns is the C1
// two-run test: run A ([0,1], chat fails) must abort in full identity while
// run B ([2,3], no chat) still merges — proving the abort is per-RUN, not
// whole-batch, exactly as the ruling requires ("other runs in the batch
// proceed independently").
func TestPartMergerMerge_ChatMergeFailureRunIsolatesFromOtherRuns(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 4)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("base - part%d.mp4", i+1))
		if err := os.WriteFile(paths[i], []byte(fmt.Sprintf("orig%d", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	chatA1 := filepath.Join(dir, "base - part1.chat.json")
	chatA2 := filepath.Join(dir, "base - part2.chat.json")
	if err := os.WriteFile(chatA1, []byte(`{"videoId":"v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// part2's chat is corrupt — run A's chat merge fails.
	if err := os.WriteFile(chatA2, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}

	paramA := &streamParams{VCodec: "h264"}
	paramB := &streamParams{VCodec: "vp9"}

	pm := newTestPartMerger(t)
	pm.probe = func(_ context.Context, _, filePath string) (*streamParams, error) {
		if filePath == paths[2] || filePath == paths[3] {
			return paramB, nil
		}
		return paramA, nil
	}
	concatCalls := 0
	pm.concat = func(_ context.Context, inputs []string, outputPath string) error {
		concatCalls++
		return os.WriteFile(outputPath, []byte(fmt.Sprintf("merged:%d", len(inputs))), 0o644)
	}
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 1900 + i
			segs[i].JobID = jobID
		}
		return nil
	}
	pm.updateFile = func(id int, filename, filePath, chatFile string) error { return nil }

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: paths[0], DurationSeconds: 100, ChatFile: chatA1},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: paths[1], DurationSeconds: 100, ChatFile: chatA2},
		{SegmentIndex: 2, Filename: "base - part3.mp4", FilePath: paths[2], DurationSeconds: 100},
		{SegmentIndex: 3, Filename: "base - part4.mp4", FilePath: paths[3], DurationSeconds: 100},
	}
	got := pm.merge(context.Background(), "job1", "", segments)

	if concatCalls != 2 {
		t.Errorf("concat called %d times, want 2 (run A's attempt + run B's)", concatCalls)
	}
	if len(got) != 3 {
		t.Fatalf("merged output = %d rows, want 3 (run A's 2 original parts + run B's 1 merged row)", len(got))
	}

	// Run A: aborted in full identity — both original rows present,
	// unchanged content, files untouched.
	if got[0].FilePath != paths[0] || got[0].DurationSeconds != 100 {
		t.Errorf("run A part1 = %+v, want unchanged original", got[0])
	}
	if got[1].FilePath != paths[1] || got[1].DurationSeconds != 100 {
		t.Errorf("run A part2 = %+v, want unchanged original", got[1])
	}
	if b, err := os.ReadFile(paths[0]); err != nil || string(b) != "orig1" {
		t.Errorf("run A part1 file must be untouched: content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(paths[1]); err != nil || string(b) != "orig2" {
		t.Errorf("run A part2 file must be untouched: content=%q err=%v", b, err)
	}
	if b, err := os.ReadFile(chatA2); err != nil || string(b) != "{not valid json" {
		t.Errorf("run A part2 chat file must survive: content=%q err=%v", b, err)
	}

	// Run B: merged normally, independent of run A's failure.
	if got[2].FilePath != paths[2] {
		t.Errorf("run B merged FilePath = %q, want %q", got[2].FilePath, paths[2])
	}
	if b, err := os.ReadFile(paths[2]); err != nil || string(b) != "merged:2" {
		t.Errorf("run B part3 content = %q err=%v, want merged:2", b, err)
	}
	if _, err := os.Stat(paths[3]); !os.IsNotExist(err) {
		t.Errorf("run B's superseded part4 should be deleted: stat err = %v", err)
	}
}

// TestPartMergerMerge_StagingDirSupersededCleanup is the C2 regression test:
// after a merge commits, the superseded LATER part's pre-mux seg_N staging
// directory must be removed — leaving it behind is what makes
// hasUnmuxedPartsForJob/muxUnrecordedSegments believe that part was never
// muxed and resurrect + re-merge it on a later finalize re-entry. The run's
// FIRST part keeps its own row identity (SegmentIndex = first's) and so its
// own seg_N dir is deliberately left alone (see merge's doc comment) —
// verified here too, so a future change can't "fix" that into a regression.
func TestPartMergerMerge_StagingDirSupersededCleanup(t *testing.T) {
	outDir := t.TempDir()
	stagingDir := t.TempDir()

	part1 := filepath.Join(outDir, "base - part1.mp4")
	part2 := filepath.Join(outDir, "base - part2.mp4")
	if err := os.WriteFile(part1, []byte("orig1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part2, []byte("orig2"), 0o644); err != nil {
		t.Fatal(err)
	}

	seg0Dir := filepath.Join(stagingDir, "seg_0")
	seg1Dir := filepath.Join(stagingDir, "seg_1")
	if err := os.MkdirAll(seg0Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(seg1Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seg0Dir, "video_stream"), []byte("raw0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seg1Dir, "video_stream"), []byte("raw1"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	pm.concat = writeConcatStub([]byte("merged-bytes"))
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 900 + i
			segs[i].JobID = jobID
		}
		return nil
	}
	pm.updateFile = func(id int, filename, filePath, chatFile string) error { return nil }

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1, DurationSeconds: 100},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2, DurationSeconds: 100},
	}
	got := pm.merge(context.Background(), "job1", stagingDir, segments)

	if len(got) != 1 {
		t.Fatalf("merged output = %d rows, want 1", len(got))
	}

	if _, err := os.Stat(seg1Dir); !os.IsNotExist(err) {
		t.Errorf("superseded part's seg_1 staging dir must be removed: stat err = %v", err)
	}
	if _, err := os.Stat(seg0Dir); err != nil {
		t.Errorf("first part's own seg_0 staging dir must be left alone: stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(seg0Dir, "video_stream")); err != nil {
		t.Errorf("first part's seg_0 contents must survive: %v", err)
	}
}

// TestPartMergerMerge_RenameFailureKeepsRowAtTempPath is the I2 rename-
// failure test: when the post-commit rename onto the first part's original
// name fails, the already-committed row keeps naming the (still valid) temp
// concat output, the original first-part file is left completely alone, and
// — since the row still resolves to a real file — superseded-part cleanup
// still proceeds normally.
func TestPartMergerMerge_RenameFailureKeepsRowAtTempPath(t *testing.T) {
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
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 1100 + i
			segs[i].JobID = jobID
		}
		return nil
	}
	updateFileCalled := false
	pm.updateFile = func(id int, filename, filePath, chatFile string) error {
		updateFileCalled = true
		return nil
	}
	pm.rename = func(string, string) error {
		return errors.New("rename: access denied")
	}

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1, DurationSeconds: 100},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2, DurationSeconds: 100},
	}
	got := pm.merge(context.Background(), "job1", "", segments)

	if len(got) != 1 {
		t.Fatalf("merged output = %d rows, want 1", len(got))
	}
	wantTemp := filepath.Join(dir, "base - merged0.mp4")
	if got[0].FilePath != wantTemp {
		t.Errorf("FilePath = %q, want committed temp path %q (rename never landed)", got[0].FilePath, wantTemp)
	}
	if updateFileCalled {
		t.Error("updateFile must not be called when the forward rename itself failed")
	}
	if b, err := os.ReadFile(wantTemp); err != nil || string(b) != "merged-bytes" {
		t.Errorf("temp merged file: content=%q err=%v, want merged-bytes", b, err)
	}
	if b, err := os.ReadFile(part1); err != nil || string(b) != "orig1" {
		t.Errorf("part1 must be untouched when rename fails: content=%q err=%v", b, err)
	}
	// The row still resolves fine (at the temp path), so superseded cleanup
	// proceeds as normal.
	if _, err := os.Stat(part2); !os.IsNotExist(err) {
		t.Errorf("superseded part2 should still be deleted when only the rename failed: stat err = %v", err)
	}
}

// TestPartMergerMerge_UpdateFileFailureRestoresRow is the I1 single-fault
// test: the rename onto the final name succeeds, but persisting that path
// back to the DB (UpdateSegmentFile) fails. merge must rename the file BACK
// onto its committed temp path so the row — which was never updated, so it
// still names the temp path — resolves to a real file again. Since that
// undo succeeds, the run is still fully recoverable and superseded cleanup
// proceeds.
func TestPartMergerMerge_UpdateFileFailureRestoresRow(t *testing.T) {
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
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 1300 + i
			segs[i].JobID = jobID
		}
		return nil
	}
	pm.updateFile = func(id int, filename, filePath, chatFile string) error {
		return errors.New("db: locked")
	}

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1, DurationSeconds: 100},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2, DurationSeconds: 100},
	}
	got := pm.merge(context.Background(), "job1", "", segments)

	if len(got) != 1 {
		t.Fatalf("merged output = %d rows, want 1", len(got))
	}
	wantTemp := filepath.Join(dir, "base - merged0.mp4")
	if got[0].FilePath != wantTemp {
		t.Errorf("FilePath = %q, want restored temp path %q", got[0].FilePath, wantTemp)
	}
	// The undo must have moved the merged bytes back to the temp path...
	if b, err := os.ReadFile(wantTemp); err != nil || string(b) != "merged-bytes" {
		t.Errorf("restored temp file: content=%q err=%v, want merged-bytes", b, err)
	}
	// ...and part1's original name must no longer exist (content was moved
	// there, then moved back out) — NOT silently left holding merged bytes
	// under a name the row doesn't claim.
	if _, err := os.Stat(part1); !os.IsNotExist(err) {
		t.Errorf("part1's original name should not exist after a successful undo: stat err = %v", err)
	}
	// The row is recoverable (resolves to wantTemp), so superseded cleanup
	// still proceeds.
	if _, err := os.Stat(part2); !os.IsNotExist(err) {
		t.Errorf("superseded part2 should still be deleted when the undo succeeded: stat err = %v", err)
	}
}

// TestPartMergerMerge_UpdateFileDoubleFaultSkipsCleanup is the I1
// double-fault test: both UpdateSegmentFile AND the undo rename fail. merge
// must NOT delete this run's superseded parts (their files are the last
// remaining, independently-resolvable copy of that span, since the
// committed row can no longer be trusted to point anywhere real).
func TestPartMergerMerge_UpdateFileDoubleFaultSkipsCleanup(t *testing.T) {
	dir := t.TempDir()
	part1 := filepath.Join(dir, "base - part1.mp4")
	part2 := filepath.Join(dir, "base - part2.mp4")
	if err := os.WriteFile(part1, []byte("orig1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part2, []byte("orig2"), 0o644); err != nil {
		t.Fatal(err)
	}

	videoTemp := filepath.Join(dir, "base - merged0.mp4")

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	pm.concat = writeConcatStub([]byte("merged-bytes"))
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 1500 + i
			segs[i].JobID = jobID
		}
		return nil
	}
	pm.updateFile = func(id int, filename, filePath, chatFile string) error {
		return errors.New("db: locked")
	}
	// Forward rename (temp -> final) succeeds for real; the undo
	// (final -> temp) is the one call that fails, forcing the double-fault.
	pm.rename = func(oldpath, newpath string) error {
		if oldpath == part1 && newpath == videoTemp {
			return errors.New("undo rename: boom")
		}
		return os.Rename(oldpath, newpath)
	}

	segments := []database.Segment{
		{SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: part1, DurationSeconds: 100},
		{SegmentIndex: 1, Filename: "base - part2.mp4", FilePath: part2, DurationSeconds: 100},
	}
	got := pm.merge(context.Background(), "job1", "", segments)

	if len(got) != 1 {
		t.Fatalf("merged output = %d rows, want 1 (the DB commit itself still succeeded)", len(got))
	}
	// The committed row still names the temp path (never updated)...
	if got[0].FilePath != videoTemp {
		t.Errorf("FilePath = %q, want committed temp path %q", got[0].FilePath, videoTemp)
	}
	// ...but that path no longer exists (forward rename moved it away, undo
	// failed to bring it back) — the documented narrow residual.
	if _, err := os.Stat(videoTemp); !os.IsNotExist(err) {
		t.Errorf("temp path should be gone after the forward rename with a failed undo: stat err = %v", err)
	}
	// The actual bytes are safe under part1's original name, just
	// unreferenced by the (unfixed) row.
	if b, err := os.ReadFile(part1); err != nil || string(b) != "merged-bytes" {
		t.Errorf("part1 (holding the merged bytes): content=%q err=%v", b, err)
	}
	// Critical: superseded part2's video must NOT be deleted — it's the
	// last independently-resolvable copy of its span while the row is broken.
	if _, err := os.Stat(part2); err != nil {
		t.Errorf("superseded part2 must survive a double-fault (I1): stat err = %v", err)
	}
}

// TestPartMergerMerge_MultiRunBatch is the I2 multi-run test: [A A B A A]
// produces two independent merged runs ([0,1] and [3,4]) with the singleton
// B part (index 2) passed through completely untouched in between — proving
// runs are grouped by CONTIGUOUS position, not by which stream params
// happen to match across the whole job.
func TestPartMergerMerge_MultiRunBatch(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 5)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("base - part%d.mp4", i+1))
		if err := os.WriteFile(paths[i], []byte(fmt.Sprintf("orig%d", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	paramA := &streamParams{VCodec: "h264"}
	paramB := &streamParams{VCodec: "vp9"}

	pm := newTestPartMerger(t)
	pm.probe = func(_ context.Context, _, filePath string) (*streamParams, error) {
		if filePath == paths[2] {
			return paramB, nil
		}
		return paramA, nil
	}
	concatCalls := 0
	pm.concat = func(_ context.Context, inputs []string, outputPath string) error {
		concatCalls++
		return os.WriteFile(outputPath, []byte(fmt.Sprintf("merged:%d", len(inputs))), 0o644)
	}
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 1700 + i
			segs[i].JobID = jobID
		}
		return nil
	}
	pm.updateFile = func(id int, filename, filePath, chatFile string) error { return nil }

	segments := make([]database.Segment, 5)
	for i := range segments {
		segments[i] = database.Segment{
			SegmentIndex:    i,
			Filename:        fmt.Sprintf("base - part%d.mp4", i+1),
			FilePath:        paths[i],
			DurationSeconds: 100,
		}
	}

	got := pm.merge(context.Background(), "job1", "", segments)

	if concatCalls != 2 {
		t.Errorf("concat called %d times, want 2 (one per mergeable run)", concatCalls)
	}
	if len(got) != 3 {
		t.Fatalf("merged output = %d rows, want 3 ([0,1] merged, [2] untouched, [3,4] merged)", len(got))
	}

	// First run: parts 1+2 merged onto part1's name.
	if got[0].FilePath != paths[0] {
		t.Errorf("run0 FilePath = %q, want %q", got[0].FilePath, paths[0])
	}
	if b, err := os.ReadFile(paths[0]); err != nil || string(b) != "merged:2" {
		t.Errorf("part1 content = %q err=%v, want merged:2", b, err)
	}
	if _, err := os.Stat(paths[1]); !os.IsNotExist(err) {
		t.Errorf("superseded part2 should be deleted: stat err = %v", err)
	}

	// Middle: the B-codec singleton (part3, index 2) passes through
	// unmerged — same SegmentIndex/FilePath/Filename/Duration as the
	// original row. ID/JobID legitimately get stamped by db.replace (the
	// real ReplaceJobSegments re-stamps EVERY row it (re)inserts, merged or
	// not — see database.ReplaceJobSegments' doc comment), so this compares
	// the content fields rather than the whole struct.
	mid := got[1]
	want := segments[2]
	if mid.SegmentIndex != want.SegmentIndex || mid.FilePath != want.FilePath || mid.Filename != want.Filename || mid.DurationSeconds != want.DurationSeconds {
		t.Errorf("middle singleton = %+v, want content matching untouched original %+v", mid, want)
	}
	if b, err := os.ReadFile(paths[2]); err != nil || string(b) != "orig3" {
		t.Errorf("part3 (untouched singleton) content = %q err=%v, want orig3", b, err)
	}

	// Second run: parts 4+5 merged onto part4's name.
	if got[2].FilePath != paths[3] {
		t.Errorf("run2 FilePath = %q, want %q", got[2].FilePath, paths[3])
	}
	if b, err := os.ReadFile(paths[3]); err != nil || string(b) != "merged:2" {
		t.Errorf("part4 content = %q err=%v, want merged:2", b, err)
	}
	if _, err := os.Stat(paths[4]); !os.IsNotExist(err) {
		t.Errorf("superseded part5 should be deleted: stat err = %v", err)
	}
}

// --- adoptMergedFinal / merge()'s I7 crash re-entry repair pass --------
//
// Scenario: merge() committed a run's row at its temp path
// ("<base> - mergedN.mp4"), then successfully renamed the file onto the
// run's first part's original name ("<base> - part<SegmentIndex+1>.mp4"),
// then the process crashed BEFORE the follow-up UpdateSegmentFile call
// persisted that new path back to the row. On re-entry, the row still
// names the (now-vanished) temp path while the real file sits unreferenced
// under its run-head name.

// TestAdoptMergedFinal_PatternMismatchNotAdopted confirms a normal
// (non-merge-temp) filename is never touched — no stat, no probe, no
// updateFile call.
func TestAdoptMergedFinal_PatternMismatchNotAdopted(t *testing.T) {
	pm := newTestPartMerger(t)
	probeCalled := false
	pm.probe = func(context.Context, string, string) (*streamParams, error) {
		probeCalled = true
		return matchingParams(context.Background(), "", "")
	}
	updateCalled := false
	pm.updateFile = func(int, string, string, string) error {
		updateCalled = true
		return nil
	}

	seg := database.Segment{ID: 1, JobID: "job1", SegmentIndex: 0, Filename: "base - part1.mp4", FilePath: "/x/base - part1.mp4"}
	if pm.adoptMergedFinal(context.Background(), &seg) {
		t.Error("a normal part filename must never be adopted")
	}
	if probeCalled || updateCalled {
		t.Error("no probe or updateFile call should happen for a filename that doesn't match the merge-temp pattern")
	}
}

// TestAdoptMergedFinal_FinalNameAbsentNotAdopted confirms a merge-temp
// filename whose reconstructed run-head final name does NOT exist on disk
// is left alone — this is the ordinary "file is just gone" case, not the
// crash-repair signature.
func TestAdoptMergedFinal_FinalNameAbsentNotAdopted(t *testing.T) {
	dir := t.TempDir()
	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	updateCalled := false
	pm.updateFile = func(int, string, string, string) error {
		updateCalled = true
		return nil
	}

	seg := database.Segment{ID: 1, JobID: "job1", SegmentIndex: 0, Filename: "base - merged0.mp4", FilePath: filepath.Join(dir, "base - merged0.mp4")}
	if pm.adoptMergedFinal(context.Background(), &seg) {
		t.Error("must not adopt when the run-head final name does not exist on disk either")
	}
	if updateCalled {
		t.Error("updateFile must not be called when there is no candidate file to adopt")
	}
	if seg.FilePath != filepath.Join(dir, "base - merged0.mp4") {
		t.Errorf("seg must be left untouched: FilePath = %q", seg.FilePath)
	}
}

// TestAdoptMergedFinal_CandidateProbeFailsNotAdopted confirms a candidate
// that exists but doesn't probe cleanly is refused — an adopt-and-repair
// must never point the row at something unverified.
func TestAdoptMergedFinal_CandidateProbeFailsNotAdopted(t *testing.T) {
	dir := t.TempDir()
	finalVideo := filepath.Join(dir, "base - part1.mp4")
	if err := os.WriteFile(finalVideo, []byte("not really playable"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = func(context.Context, string, string) (*streamParams, error) {
		return nil, errors.New("ffprobe: boom")
	}
	updateCalled := false
	pm.updateFile = func(int, string, string, string) error {
		updateCalled = true
		return nil
	}

	seg := database.Segment{ID: 1, JobID: "job1", SegmentIndex: 0, Filename: "base - merged0.mp4", FilePath: filepath.Join(dir, "base - merged0.mp4")}
	if pm.adoptMergedFinal(context.Background(), &seg) {
		t.Error("must not adopt a candidate that fails to probe cleanly")
	}
	if updateCalled {
		t.Error("updateFile must not be called when the candidate fails to probe")
	}
}

// TestAdoptMergedFinal_HappyPathRepairsRowAndSeg is the core I7 regression
// test: a crash-orphaned row (temp path gone, run-head final name present
// and playable) is repaired both in the DB (via updateFile) and in the
// caller's in-memory seg.
func TestAdoptMergedFinal_HappyPathRepairsRowAndSeg(t *testing.T) {
	dir := t.TempDir()
	finalVideo := filepath.Join(dir, "base - part2.mp4") // SegmentIndex 1 -> part2
	if err := os.WriteFile(finalVideo, []byte("real playable media"), 0o644); err != nil {
		t.Fatal(err)
	}
	finalChat := filepath.Join(dir, "base - part2.chat.json")
	if err := os.WriteFile(finalChat, []byte(`{"videoId":"v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	var gotID int
	var gotFilename, gotFilePath, gotChatFile string
	pm.updateFile = func(id int, filename, filePath, chatFile string) error {
		gotID, gotFilename, gotFilePath, gotChatFile = id, filename, filePath, chatFile
		return nil
	}

	vanishedTemp := filepath.Join(dir, "base - merged0.mp4")
	vanishedChat := filepath.Join(dir, "base - merged0.chat.json")
	seg := database.Segment{ID: 77, JobID: "job1", SegmentIndex: 1, Filename: "base - merged0.mp4", FilePath: vanishedTemp, ChatFile: vanishedChat}

	if !pm.adoptMergedFinal(context.Background(), &seg) {
		t.Fatal("expected adoption to succeed")
	}

	if gotID != 77 || gotFilename != "base - part2.mp4" || gotFilePath != finalVideo || gotChatFile != finalChat {
		t.Errorf("updateFile called with (%d, %q, %q, %q), want (77, %q, %q, %q)",
			gotID, gotFilename, gotFilePath, gotChatFile, "base - part2.mp4", finalVideo, finalChat)
	}
	if seg.FilePath != finalVideo || seg.Filename != "base - part2.mp4" || seg.ChatFile != finalChat {
		t.Errorf("seg after adoption = %+v, want FilePath=%q Filename=%q ChatFile=%q", seg, finalVideo, "base - part2.mp4", finalChat)
	}
}

// TestAdoptMergedFinal_MissingChatSiblingClearsChatFile confirms that when
// the video adopts successfully but no chat sibling exists at the adopted
// name, ChatFile is cleared to "" in BOTH the persisted row and seg itself
// — never left pointing at the vanished temp chat path in one place while
// the other says something different.
func TestAdoptMergedFinal_MissingChatSiblingClearsChatFile(t *testing.T) {
	dir := t.TempDir()
	finalVideo := filepath.Join(dir, "base - part1.mp4")
	if err := os.WriteFile(finalVideo, []byte("real playable media"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Deliberately no "base - part1.chat.json" sibling written.

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	var gotChatFile string
	chatFileSet := false
	pm.updateFile = func(id int, filename, filePath, chatFile string) error {
		gotChatFile = chatFile
		chatFileSet = true
		return nil
	}

	seg := database.Segment{ID: 1, JobID: "job1", SegmentIndex: 0, Filename: "base - merged0.mp4", FilePath: filepath.Join(dir, "base - merged0.mp4"), ChatFile: filepath.Join(dir, "base - merged0.chat.json")}

	if !pm.adoptMergedFinal(context.Background(), &seg) {
		t.Fatal("expected adoption to succeed (video-only)")
	}
	if !chatFileSet || gotChatFile != "" {
		t.Errorf("updateFile ChatFile arg = %q, want empty (no sibling found)", gotChatFile)
	}
	if seg.ChatFile != "" {
		t.Errorf("seg.ChatFile = %q, want cleared to empty, matching what was persisted", seg.ChatFile)
	}
}

// TestPartMergerMerge_AdoptsCrashOrphanedSingleRow is the I7 end-to-end
// test for the len==1 (fully-collapsed-to-one-row) case: merge()'s repair
// pass must run BEFORE the len<2 short-circuit, since a job that already
// merged down to a single row on a prior run carries exactly the same
// crash signature and would otherwise never reach a probe here at all —
// nor get recognized by finalizeMultiSegmentJob's renameSinglePartToPlain,
// which looks for a different (plain job-level) final name.
func TestPartMergerMerge_AdoptsCrashOrphanedSingleRow(t *testing.T) {
	dir := t.TempDir()
	finalVideo := filepath.Join(dir, "base - part1.mp4")
	if err := os.WriteFile(finalVideo, []byte("real playable media"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	pm.probe = matchingParams
	updateFileCalls := 0
	pm.updateFile = func(int, string, string, string) error {
		updateFileCalls++
		return nil
	}
	replaceCalled := false
	pm.replace = func(string, []database.Segment) error {
		replaceCalled = true
		return nil
	}

	segments := []database.Segment{
		{ID: 1, JobID: "job1", SegmentIndex: 0, Filename: "base - merged0.mp4", FilePath: filepath.Join(dir, "base - merged0.mp4")},
	}
	got := pm.merge(context.Background(), "job1", "", segments)

	if len(got) != 1 {
		t.Fatalf("merge() = %d rows, want 1 (repaired in place, not merged with anything)", len(got))
	}
	if got[0].FilePath != finalVideo {
		t.Errorf("FilePath = %q, want adopted run-head name %q", got[0].FilePath, finalVideo)
	}
	if updateFileCalls != 1 {
		t.Errorf("updateFile called %d times, want 1 (the repair pass)", updateFileCalls)
	}
	if replaceCalled {
		t.Error("db.ReplaceJobSegments must not be called for a single-row repair -- only UpdateSegmentFile")
	}
}

// TestPartMergerMerge_AdoptsCrashOrphanedRowThenContinuesMultiRun proves the
// repair composes with the rest of merge(): a crash-orphaned row (index 0,
// SegmentIndex 0) is repaired in place, and an UNRELATED mergeable pair
// (indices 1,2) still merges normally in the same call — the repair pass
// does not abort or otherwise disturb the rest of the batch.
func TestPartMergerMerge_AdoptsCrashOrphanedRowThenContinuesMultiRun(t *testing.T) {
	dir := t.TempDir()
	orphanFinal := filepath.Join(dir, "base - part1.mp4")
	if err := os.WriteFile(orphanFinal, []byte("real playable media"), 0o644); err != nil {
		t.Fatal(err)
	}
	part2 := filepath.Join(dir, "orig - part2.mp4")
	part3 := filepath.Join(dir, "orig - part3.mp4")
	if err := os.WriteFile(part2, []byte("orig2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part3, []byte("orig3"), 0o644); err != nil {
		t.Fatal(err)
	}

	pm := newTestPartMerger(t)
	// The repaired orphan probes as a DISTINCT codec from parts 2+3, so
	// groupMergeRuns keeps it a singleton run instead of grouping it in
	// with the contiguous, actually-matching [1,2] run.
	pm.probe = func(_ context.Context, _, filePath string) (*streamParams, error) {
		if filePath == orphanFinal {
			return &streamParams{VCodec: "vp9", ACodec: "opus"}, nil
		}
		return &streamParams{VCodec: "h264", ACodec: "aac"}, nil
	}
	pm.concat = writeConcatStub([]byte("merged-bytes"))
	updateFileCalls := 0
	pm.updateFile = func(int, string, string, string) error {
		updateFileCalls++
		return nil
	}
	pm.replace = func(jobID string, segs []database.Segment) error {
		for i := range segs {
			segs[i].ID = 3000 + i
			segs[i].JobID = jobID
		}
		return nil
	}

	segments := []database.Segment{
		{ID: 1, JobID: "job1", SegmentIndex: 0, Filename: "base - merged0.mp4", FilePath: filepath.Join(dir, "base - merged0.mp4")},
		{ID: 2, JobID: "job1", SegmentIndex: 1, Filename: "orig - part2.mp4", FilePath: part2, DurationSeconds: 100},
		{ID: 3, JobID: "job1", SegmentIndex: 2, Filename: "orig - part3.mp4", FilePath: part3, DurationSeconds: 100},
	}
	got := pm.merge(context.Background(), "job1", "", segments)

	if len(got) != 2 {
		t.Fatalf("merge() = %d rows, want 2 (repaired orphan + one merged run)", len(got))
	}
	if got[0].FilePath != orphanFinal {
		t.Errorf("repaired orphan FilePath = %q, want %q", got[0].FilePath, orphanFinal)
	}
	// updateFile is called once for the repair pass, once more for the
	// post-commit rename-onto-final-name step of the [1,2] merge run.
	if updateFileCalls != 2 {
		t.Errorf("updateFile called %d times, want 2 (1 repair + 1 normal merge commit)", updateFileCalls)
	}
	if got[1].FilePath != part2 {
		t.Errorf("merged run FilePath = %q, want %q (renamed onto its first part)", got[1].FilePath, part2)
	}
	if b, err := os.ReadFile(part2); err != nil || string(b) != "merged-bytes" {
		t.Errorf("merged run content = %q err=%v, want merged-bytes", b, err)
	}
}

// requireFFmpegTools is defined in probe_params_test.go (same package).

// TestMergeSameFormatParts_RealFFmpegEndToEnd exercises
// DownloadOrchestrator.mergeSameFormatParts through a real ffmpeg/ffprobe
// toolchain and a real database: two identically-encoded part fixtures
// collapse into a single segment row renamed onto the first part's path.
// The merged output is re-probed for real (both structurally, via
// probeStreamParams, and for duration, via runFFprobe) rather than just
// checking size>0 — a non-empty file is not proof the concat produced a
// playable result. Skips when ffmpeg/ffprobe aren't on PATH.
func TestMergeSameFormatParts_RealFFmpegEndToEnd(t *testing.T) {
	ffmpegPath, ffprobePath := requireFFmpegTools(t)

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
	const partDurationSec = 1.0
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

	if err := db.AddSegment(&database.Segment{JobID: job.ID, SegmentIndex: 0, UnixStart: 1000, UnixEnd: 1010, Quality: "72p30", Filename: "Show - part1.mp4", FilePath: part1, DurationSeconds: partDurationSec}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSegment(&database.Segment{JobID: job.ID, SegmentIndex: 1, UnixStart: 1010, UnixEnd: 1020, Quality: "72p30", Filename: "Show - part2.mp4", FilePath: part2, DurationSeconds: partDurationSec}); err != nil {
		t.Fatal(err)
	}

	segs, err := db.GetSegments(job.ID)
	if err != nil {
		t.Fatal(err)
	}

	muxer := engine.NewMuxer(ffmpegPath, &discardLogger{})
	o := &DownloadOrchestrator{muxer: muxer, db: db, logger: &discardLogger{}}
	jobCtx := &JobContext{Job: job, StagingDir: filepath.Join(dir, "staging")}

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

	// Structural playability: re-probe the merged output for real rather
	// than trusting size>0. A broken concat (mismatched params slipping
	// through, or a truncated write) would fail here even though the file
	// is non-empty.
	mergedParams, err := probeStreamParams(context.Background(), ffprobePath, part1)
	if err != nil {
		t.Fatalf("probeStreamParams on merged output: %v", err)
	}
	if mergedParams.VCodec == "" || mergedParams.ACodec == "" {
		t.Errorf("merged output missing video/audio codec: %+v", mergedParams)
	}

	// Duration must reflect BOTH parts concatenated, not just one — this is
	// what actually catches a concat that silently dropped a segment.
	probeData := o.runFFprobe(context.Background(), part1)
	if probeData == nil {
		t.Fatal("runFFprobe on merged output returned nil")
	}
	wantDuration := 2 * partDurationSec
	if diff := math.Abs(probeData.DurationSec - wantDuration); diff > 0.5 {
		t.Errorf("merged duration = %.3fs, want %.3fs ± 0.5s", probeData.DurationSec, wantDuration)
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
