package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

	if err := m.ConcatCopy(context.Background(), []string{seg0, seg1}, outputPath); err != nil {
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

func TestConcatCopy_NoInputs(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	if err := m.ConcatCopy(context.Background(), nil, "out.mp4"); err == nil {
		t.Error("expected an error when inputs is empty")
	}
}
