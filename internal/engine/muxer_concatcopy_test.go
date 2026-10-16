package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConcatCopy_Fixture exercises ConcatCopy against two tiny real
// ffmpeg-generated fixtures, confirming the exported wrapper's own temp dir
// plumbing works end-to-end with concatIntermediates (which expects to
// write concat.txt into the tempDir it's handed).
func TestConcatCopy_Fixture(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}

	dir := t.TempDir()
	seg0 := filepath.Join(dir, "seg_0.mp4")
	seg1 := filepath.Join(dir, "seg_1.mp4")

	for _, seg := range []string{seg0, seg1} {
		cmd := exec.Command(ffmpegPath,
			"-y",
			"-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=30",
			"-f", "lavfi", "-i", "sine=duration=1",
			"-c:v", "libx264",
			"-c:a", "aac",
			seg,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to generate fixture %s: %v\n%s", seg, err, out)
		}
	}

	outputPath := filepath.Join(dir, "concat_out.mp4")
	m := NewMuxer(ffmpegPath, &testLogger{})

	if err := m.ConcatCopy(t.Context(), []string{seg0, seg1}, outputPath); err != nil {
		t.Fatalf("ConcatCopy: %v", err)
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("output not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("output file is empty")
	}
}

// TestConcatListNamesEveryEntryAbsolutely: the list concatIntermediates
// writes names each input by an absolute path that opens the real file, so
// the concat demuxer — which resolves a relative entry against the list's own
// (temp) directory — finds an input named relative to the working directory.
// No FFmpeg needed: the list is written before FFmpeg runs, and a missing
// binary only fails the run after it.
//
// Mutant it kills: buildConcatList(intermediates) in place of
// buildConcatList(entries) (the entry is "output/Chan/…", not absolute).
func TestConcatListNamesEveryEntryAbsolutely(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	rel := filepath.Join("output", "Chan", "Stream [abcdefghijk] - part1.mp4")
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rel, []byte("part"), 0o644); err != nil {
		t.Fatal(err)
	}
	listDir := t.TempDir()
	m := NewMuxer(filepath.Join(root, "no-such-ffmpeg"), &testLogger{})
	_ = m.concatIntermediates(t.Context(), []string{rel}, filepath.Join(root, "out.mp4"), listDir)

	list, err := os.ReadFile(filepath.Join(listDir, "concat.txt"))
	if err != nil {
		t.Fatalf("concat list not written: %v", err)
	}
	entry, ok := strings.CutPrefix(strings.TrimSuffix(string(list), "\n"), "file '")
	entry, ok2 := strings.CutSuffix(entry, "'")
	if !ok || !ok2 || strings.Contains(entry, "\n") {
		t.Fatalf("concat list %q: want one quoted file entry", list)
	}
	native := filepath.FromSlash(entry)
	if !filepath.IsAbs(native) {
		t.Fatalf("concat list entry %q is relative: the demuxer resolves it against %s", entry, listDir)
	}
	if _, err := os.Stat(native); err != nil {
		t.Errorf("concat list entry %q does not name the input %q: %v", entry, rel, err)
	}
}

// TestConcatCopy_RelativeInputs is the W27-02 reproduction through real
// FFmpeg: parts named relative to the working directory, the way the part
// merge's rows are under the default paths.output_directory ("./output"),
// concatenate. It failed with "Impossible to open
// '<temp>/moombox-concat-N/output/Chan/…'".
//
// Mutant it kills: buildConcatList(intermediates) in place of
// buildConcatList(entries).
func TestConcatCopy_RelativeInputs(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Join("output", "Chan"), 0o755); err != nil {
		t.Fatal(err)
	}
	seg0 := filepath.Join("output", "Chan", "Stream [abcdefghijk] - part1.mp4")
	seg1 := filepath.Join("output", "Chan", "Stream [abcdefghijk] - part2.mp4")
	for _, seg := range []string{seg0, seg1} {
		cmd := exec.Command(ffmpegPath, "-y",
			"-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=30",
			"-f", "lavfi", "-i", "sine=duration=1",
			"-c:v", "libx264", "-c:a", "aac", seg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("failed to generate fixture %s: %v\n%s", seg, err, out)
		}
	}
	out := filepath.Join("output", "Chan", "Stream [abcdefghijk] - merged.mp4")
	m := NewMuxer(ffmpegPath, &testLogger{})
	if err := m.ConcatCopy(t.Context(), []string{seg0, seg1}, out); err != nil {
		t.Fatalf("ConcatCopy of parts named relative to the working directory: %v", err)
	}
	if info, err := os.Stat(out); err != nil || info.Size() == 0 {
		t.Errorf("merged output %q: info=%v err=%v", out, info, err)
	}
}

func TestConcatCopy_NoInputs(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	if err := m.ConcatCopy(t.Context(), nil, "out.mp4"); err == nil {
		t.Error("expected an error when inputs is empty")
	}
}
