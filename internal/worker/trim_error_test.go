package worker

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vampiricwulf/Moombox/internal/engine"
)

// TestTrimFailureKeepsFFmpegsReason: a Trim Video whose FFmpeg fails says why
// in its error, which is what its log lines carry. The runner a trim with
// progress takes (engine runFFmpegWithProgress — Trim Video's alone; every
// archive mux runs through runFFmpeg) kept FFmpeg's last stderr line alone,
// usually the closing "Conversion failed!", and the line that names the
// error comes before it. Onto a disk that fills mid-trim: MPEG-TS buffers its
// header, so the write that fails is the trailer's. Linux only: /dev/full
// answers every write with ENOSPC.
//
// Mutant: keep only FFmpeg's last stderr line in runFFmpegWithProgress — the
// error says "Conversion failed!" and nothing else.
func TestTrimFailureKeepsFFmpegsReason(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /dev/full")
	}
	ffmpegPath, _ := requireFFmpegTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	writeMuxFixture(t, ffmpegPath, src, 3)
	out := filepath.Join(dir, "out.ts")
	if err := os.Symlink("/dev/full", out); err != nil {
		t.Skipf("cannot link to /dev/full: %v", err)
	}

	progressed := false
	err := engine.NewMuxer(ffmpegPath, discardLogger{}).Mux(context.Background(), src, "", out, &engine.TrimOptions{
		TrimStartOffset: 0.5, TrimDuration: 2, ProgressFn: func(float64) { progressed = true },
	})
	if err == nil {
		t.Fatal("trimming onto /dev/full succeeded")
	}
	if !progressed {
		t.Fatalf("the trim reported no progress, so it did not run through the progress runner: %v", err)
	}
	if !strings.Contains(err.Error(), "Error writing trailer: No space left on device") {
		t.Errorf("the trim's error does not say why FFmpeg failed: %v", err)
	}
}
