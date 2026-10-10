package worker

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/database"
)

// Trim names use whole-second bounds, so [1.2s-3s] and [1.4s-3s] both round
// to "<id> [1s-3s].mp4". uniqueTrimBasename suffixes the later ones; without
// it the second encode (FFmpeg runs with -y) overwrote the first trim's file
// and both rows named the same bytes, so deleting either clip lost both
// (W23-12: nothing covered the loop). The first file's bytes are compared
// before and after, which is the overwrite itself, not just its name.
//
// Mutants:
//   - uniqueTrimBasename returning the rounded name unchanged (`_ = taken;
//     return name`): the second trim shares the first's file.
//   - taken comparing the row's whole filename instead of its base name:
//     never taken, the same.
//   - the suffix loop starting at 3: the second trim is "(3)", not "(2)".
func TestCreateTrimKeepsRoundedNamesApart(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	src := filepath.Join(outputDir, "Chan", "Title [nmvid123456].mp4")
	writeTrimSource(t, ffmpegPath, src, "5")
	job := &database.Job{
		ID: "nmvid123456", VideoID: "nmvid123456", URL: "u", Platform: "youtube", Title: "t",
		Status: database.StatusFinished, OutputFile: src, Filename: filepath.Join("Chan", "Title [nmvid123456].mp4"),
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	ts := NewTrimService(db, ffmpegPath, discardLogger{})
	trimDir := filepath.Join(outputDir, "Chan", "trim")

	first, err := ts.CreateTrim(t.Context(), job, 1.2, 3, nil)
	if err != nil {
		t.Fatalf("first CreateTrim: %v", err)
	}
	firstFile := filepath.Join(trimDir, filepath.Base(first.Filename))
	before, err := os.ReadFile(firstFile)
	if err != nil {
		t.Fatalf("read the first trim: %v", err)
	}

	second, err := ts.CreateTrim(t.Context(), job, 1.4, 3, nil)
	if err != nil {
		t.Fatalf("second CreateTrim: %v", err)
	}
	third, err := ts.CreateTrim(t.Context(), job, 1.3, 3.2, nil)
	if err != nil {
		t.Fatalf("third CreateTrim: %v", err)
	}

	want := []string{
		filepath.Join("Chan", "trim", "nmvid123456 [1s-3s].mp4"),
		filepath.Join("Chan", "trim", "nmvid123456 [1s-3s] (2).mp4"),
		filepath.Join("Chan", "trim", "nmvid123456 [1s-3s] (3).mp4"),
	}
	for i, rec := range []string{first.Filename, second.Filename, third.Filename} {
		if rec != want[i] {
			t.Errorf("trim %d is named %q, want %q", i+1, rec, want[i])
		}
		if _, err := os.Stat(filepath.Join(outputDir, rec)); err != nil {
			t.Errorf("trim %d's file is missing: %v", i+1, err)
		}
	}
	after, err := os.ReadFile(firstFile)
	if err != nil {
		t.Fatalf("read the first trim again: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the first trim's file changed when the later ones were encoded — a later encode overwrote it")
	}
	if strings.EqualFold(first.Filename, second.Filename) {
		t.Errorf("two trims share one file %q", first.Filename)
	}
}

// The same disambiguation on the other call site: a job that finalized as
// parts (a quality or gap split, as most long lives do) trims through
// createMultiSegmentTrimInternal, which names its file on its own line. The
// single-file test above never reaches it — its job has no parts, so
// CreateTrim never takes the multi-part branch — and with the plain rounded
// name there the second encode overwrote the first trim's file while both
// rows named it. Both ranges here cross the part boundary at 2s, so each
// encode is the real trim-and-concat of the two parts.
//
// Mutants:
//   - createMultiSegmentTrimInternal naming the file with the plain rounded
//     name (`fmt.Sprintf("%s [%.0fs-%.0fs].mp4", job.VideoID, startTime,
//     endTime)`) instead of calling uniqueTrimBasename: both trims are named
//     "[1s-3s].mp4" and the first file's bytes change.
//   - createMultiSegmentTrimInternal passing an empty list to
//     uniqueTrimBasename: the same.
func TestCreateMultiPartTrimKeepsRoundedNamesApart(t *testing.T) {
	ffmpegPath, _ := requireFFmpegTools(t)
	_, db := testWorkerSetup(t)
	outputDir := t.TempDir()
	id := "mpvid123456"
	p1 := filepath.Join(outputDir, "Chan", "Title ["+id+"] - part1.mp4")
	p2 := filepath.Join(outputDir, "Chan", "Title ["+id+"] - part2.mp4")
	writeTrimSource(t, ffmpegPath, p1, "2")
	writeTrimSource(t, ffmpegPath, p2, "2")
	job := &database.Job{
		ID: id, VideoID: id, URL: "u", Platform: "youtube", Title: "t",
		Status: database.StatusFinished, Filename: filepath.Join("Chan", "Title ["+id+"]"),
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatal(err)
	}
	// The parts' own quality, so the concat joins them as they are.
	w, h, fps := 64, 64, 10
	if err := db.ReplaceJobSegments(id, []database.Segment{
		{JobID: id, SegmentIndex: 0, Filename: filepath.Base(p1), FilePath: p1, DurationSeconds: 2, VideoWidth: &w, VideoHeight: &h, VideoFps: &fps},
		{JobID: id, SegmentIndex: 1, Filename: filepath.Base(p2), FilePath: p2, DurationSeconds: 2, VideoWidth: &w, VideoHeight: &h, VideoFps: &fps},
	}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJob(id)
	if err != nil || len(job.Segments) != 2 {
		t.Fatalf("GetJob: %v (%d parts)", err, len(job.Segments))
	}
	ts := NewTrimService(db, ffmpegPath, discardLogger{})

	first, err := ts.CreateTrim(t.Context(), job, 1.2, 3, nil)
	if err != nil {
		t.Fatalf("first CreateTrim: %v", err)
	}
	firstFile := filepath.Join(outputDir, first.Filename)
	before, err := os.ReadFile(firstFile)
	if err != nil {
		t.Fatalf("read the first trim: %v", err)
	}

	second, err := ts.CreateTrim(t.Context(), job, 1.4, 3, nil)
	if err != nil {
		t.Fatalf("second CreateTrim: %v", err)
	}

	want := []string{
		filepath.Join("Chan", "trim", id+" [1s-3s].mp4"),
		filepath.Join("Chan", "trim", id+" [1s-3s] (2).mp4"),
	}
	for i, rec := range []string{first.Filename, second.Filename} {
		if rec != want[i] {
			t.Errorf("trim %d is named %q, want %q", i+1, rec, want[i])
		}
		if _, err := os.Stat(filepath.Join(outputDir, rec)); err != nil {
			t.Errorf("trim %d's file is missing: %v", i+1, err)
		}
	}
	after, err := os.ReadFile(firstFile)
	if err != nil {
		t.Fatalf("read the first trim again: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the first trim's file changed when the second was encoded — the second encode overwrote it")
	}
	if strings.EqualFold(first.Filename, second.Filename) {
		t.Errorf("two trims share one file %q", first.Filename)
	}
}
