package engine

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestWithDisplayAspect: the yt:stretch fix is one output option, `-aspect
// <ratio>`, ahead of the output path — FFmpeg takes options before the file
// they apply to — and none at all without a ratio, so every other mux's argv
// is unchanged.
//
// Mutants: the option appended after the output path (FFmpeg reads a
// trailing option as "no output file" and fails); written for an empty
// aspect.
func TestWithDisplayAspect(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	args := m.buildArgs("video.mp4", "audio.m4a", "out.mp4", nil, false, InputOffsets{})
	got := withDisplayAspect(args, "1.777778")
	want := append(slices.Clone(args[:len(args)-1]), "-aspect", "1.777778", "out.mp4")
	if !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
	if got := withDisplayAspect(args, ""); !slices.Equal(got, args) {
		t.Errorf("argv without an aspect = %v, want %v", got, args)
	}
}

// TestMuxCopyAspectSetsTheDisplayAspect: on a real stream copy, the aspect
// reaches the archive as its display aspect ratio, the frames untouched —
// a 4:3 frame (320x240) shows 16:9 — as yt-dlp's FFmpegFixupStretchedPP
// leaves an old anamorphic upload. MuxCopyAt sets nothing. Skipped where
// FFmpeg is not installed.
//
// Mutant: mux dropping the withDisplayAspect call (DAR stays 4:3).
func TestMuxCopyAspectSetsTheDisplayAspect(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "video.mp4")
	if b, err := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc=duration=1:size=320x240:rate=10", "-c:v", "libx264", "-pix_fmt", "yuv420p", video).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, b)
	}
	m := NewMuxer(ffmpegPath, &testLogger{})
	dar := func(path string) string {
		t.Helper()
		b, err := exec.Command(ffprobePath, "-v", "error", "-select_streams", "v", "-show_entries", "stream=width,height,display_aspect_ratio", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatalf("ffprobe: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	stretched := filepath.Join(dir, "stretched.mp4")
	if err := m.MuxCopyAspect(t.Context(), video, "", stretched, InputOffsets{}, "1.777778"); err != nil {
		t.Fatalf("MuxCopyAspect: %v", err)
	}
	if got := dar(stretched); got != "320,240,16:9" {
		t.Errorf("stretched copy = %q, want 320,240,16:9", got)
	}
	plain := filepath.Join(dir, "plain.mp4")
	if err := m.MuxCopyAt(t.Context(), video, "", plain, InputOffsets{}); err != nil {
		t.Fatalf("MuxCopyAt: %v", err)
	}
	if got := dar(plain); got != "320,240,4:3" {
		t.Errorf("plain copy = %q, want 320,240,4:3", got)
	}
}
