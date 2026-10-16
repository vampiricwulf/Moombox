package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vampiricwulf/Moombox/internal/config"
	"github.com/vampiricwulf/Moombox/internal/database"
)

// TestMultiSegmentTrimIntermediatesAreOwnedWhileItRuns: a multi-segment trim
// writes its re-encoded intermediates beside the clip now, in the job's
// trim/ directory, not under os.TempDir() — on the output volume the disk
// alerts watch, not the container's /tmp. Inside the output tree they are
// files the orphan sweep walks, so they must be owned for as long as the
// encode writes them: the trim's claim on its clip's stem covers them,
// because their directory is named after it. Once the trim is done nothing of
// them is left.
//
// Mutants: name the engine's intermediates directory "moombox-trim-*"
// without the stem (the sweep offers the intermediate mid-encode and nothing
// claims it); MkdirTemp under "" again (the intermediate lands under the
// stand-in /tmp); drop run's claimOutputStem (nothing claims it).
func TestMultiSegmentTrimIntermediatesAreOwnedWhileItRuns(t *testing.T) {
	ffmpeg, gate := gatedFFmpeg(t)
	root := t.TempDir()
	tmpRoot := filepath.Join(root, "tmp") // stands in for the container's /tmp
	if err := os.Mkdir(tmpRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpRoot)

	db, err := database.Open(filepath.Join(root, "trim.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	outDir := filepath.Join(root, "out")
	chanDir := filepath.Join(outDir, "chan")
	if err := os.MkdirAll(chanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	job := &database.Job{
		ID: "yt_split", VideoID: "split", Platform: "youtube", URL: "u", Title: "A split stream",
		Status: database.StatusFinished, Filename: filepath.Join("chan", "archive.mp4"),
	}
	if _, err := db.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for i, h := range []int{1080, 720} {
		part := filepath.Join(chanDir, "archive - part"+string(rune('1'+i))+".mp4")
		if err := os.WriteFile(part, []byte("src"), 0o644); err != nil {
			t.Fatal(err)
		}
		height := h
		if err := db.AddSegment(&database.Segment{
			JobID: job.ID, SegmentIndex: i, FilePath: part, Filename: filepath.Base(part),
			VideoHeight: &height, DurationSeconds: 60,
		}); err != nil {
			t.Fatalf("AddSegment: %v", err)
		}
	}
	job, err = db.GetJob(job.ID)
	if err != nil || len(job.Segments) != 2 {
		t.Fatalf("GetJob: %v, %d segments", err, len(job.Segments))
	}
	cfg := &config.MoomboxConfig{Paths: config.PathsConfig{OutputDirectory: outDir, StagingDirectory: filepath.Join(root, "staging")}}

	ts := NewTrimService(db, ffmpeg, discardLogger{})
	events := &trimEvents{}
	ts.SetOnEvent(events.add)
	t.Cleanup(ts.Stop)
	if _, err := ts.StartTrim(job, 30, 90); err != nil {
		t.Fatalf("StartTrim: %v", err)
	}

	// The first segment's encode is waiting at the gate.
	var seg0 string
	deadline := time.Now().Add(10 * time.Second)
	for seg0 == "" && time.Now().Before(deadline) {
		if raw, err := os.ReadFile(filepath.Join(gate, "outputs")); err == nil && len(raw) > 0 {
			seg0 = strings.SplitN(string(raw), "\n", 2)[0]
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if seg0 == "" {
		t.Fatal("the first segment's encode never started")
	}
	trimDir := filepath.Join(chanDir, "trim")
	if filepath.Dir(filepath.Dir(seg0)) != trimDir {
		t.Fatalf("intermediate %q is not beside the clip in %q", seg0, trimDir)
	}
	if strings.HasPrefix(seg0, tmpRoot) {
		t.Fatalf("intermediate %q was written under os.TempDir()", seg0)
	}
	if id := outputClaimOwner(seg0); id != job.ID {
		t.Errorf("outputClaimOwner(%q) = %q mid-encode, want %q", seg0, id, job.ID)
	}
	if typ := orphanTypeOf(t, db, cfg, seg0); typ != "" {
		t.Errorf("the orphan sweep offers the intermediate an encode is writing, as %q", typ)
	}

	touch(t, gate, "gate")
	events.wait(t, TrimStateFinished)
	entries, err := os.ReadDir(trimDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("the intermediates directory %q outlived the trim", e.Name())
		}
	}
}

// TestCleanupOldTrimTempSweepsEveryTrimDirectory: the startup sweep reads the
// directories a multi-segment trim now writes its intermediates into —
// trim/ beside a job's output and beside each of its parts — as well as the
// system temp directory older versions wrote them to. It only knew the
// latter, so what a hard abort left beside a clip would have stayed for good.
// A fresh directory, and a clip, are left alone; a job with no trim/ directory
// is no error.
//
// Mutants: drop the per-job loop (the stale directories beside the output and
// the part survive); drop the engine.CleanupOldTrimTempDirs call (the legacy
// one in the temp directory survives); drop the segments half of trimDirsOf
// (the one beside the part survives); RemoveStaleTrimTempDirs returning
// ReadDir's error for a missing directory (the sweep fails on the untrimmed
// job).
func TestCleanupOldTrimTempSweepsEveryTrimDirectory(t *testing.T) {
	root := t.TempDir()
	tmpRoot := filepath.Join(root, "tmp")
	if err := os.Mkdir(tmpRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmpRoot)
	t.Setenv("TMP", tmpRoot)
	t.Setenv("TEMP", tmpRoot)
	if os.TempDir() != tmpRoot {
		t.Skipf("os.TempDir() = %q, not the test's %q", os.TempDir(), tmpRoot)
	}

	db, err := database.Open(filepath.Join(root, "trim.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	outDir := filepath.Join(root, "out")
	single := filepath.Join(outDir, "a", "archive.mp4")
	part := filepath.Join(outDir, "b", "split - part1.mp4")
	if _, err := db.AddJob(&database.Job{
		ID: "yt_one", VideoID: "one", Platform: "youtube", URL: "u", Status: database.StatusFinished,
		OutputFile: single, Filename: filepath.Join("a", "archive.mp4"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddJob(&database.Job{
		ID: "yt_two", VideoID: "two", Platform: "youtube", URL: "u", Status: database.StatusFinished,
		Filename: filepath.Join("b", "split.mp4"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSegment(&database.Segment{JobID: "yt_two", SegmentIndex: 0, FilePath: part, Filename: filepath.Base(part)}); err != nil {
		t.Fatal(err)
	}
	// A job never trimmed has no trim/ directory: nothing to sweep, no error.
	if _, err := db.AddJob(&database.Job{
		ID: "yt_three", VideoID: "three", Platform: "youtube", URL: "u", Status: database.StatusFinished,
		OutputFile: filepath.Join(outDir, "c", "untrimmed.mp4"), Filename: filepath.Join("c", "untrimmed.mp4"),
	}); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-48 * time.Hour)
	mkdir := func(p string, mtime time.Time) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "seg_0.mp4"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	besideOutput := mkdir(filepath.Join(outDir, "a", "trim", "one [0s-60s].partial.moombox-trim-1"), old)
	besidePart := mkdir(filepath.Join(outDir, "b", "trim", "two [0s-60s].partial.moombox-trim-2"), old)
	legacy := mkdir(filepath.Join(tmpRoot, "moombox-trim-3"), old)
	fresh := mkdir(filepath.Join(outDir, "a", "trim", "one [0s-90s].partial.moombox-trim-4"), time.Now())
	clip := filepath.Join(outDir, "a", "trim", "one [0s-30s].mp4")
	if err := os.WriteFile(clip, []byte("clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(clip, old, old); err != nil {
		t.Fatal(err)
	}

	removed, err := CleanupOldTrimTemp(db, outDir)
	if err != nil {
		t.Fatalf("CleanupOldTrimTemp: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed %d, want 3", removed)
	}
	for p, want := range map[string]bool{besideOutput: false, besidePart: false, legacy: false, fresh: true, clip: true} {
		_, err := os.Stat(p)
		if exists := err == nil; exists != want {
			t.Errorf("%s exists = %v, want %v", p, exists, want)
		}
	}
}
