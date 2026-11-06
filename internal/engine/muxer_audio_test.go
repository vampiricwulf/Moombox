package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtractAudioCopyKeepsOnlyTheAudio is owner decision FQ-4's engine half:
// a clip rendition (an MP4 with video and AAC audio) comes out as its audio
// alone, the AAC stream copied, not re-encoded, under the output's name and
// through the checks, which see it under its temporary name; an input with
// no audio stream is ErrNoAudioTrack and writes nothing. Skipped where FFmpeg
// is not installed.
//
// Mutants: `-map 0:a` dropped (the output keeps the video); `-c copy`
// dropped (the audio is re-encoded: its packets are not the input's); the
// ErrNoAudioTrack wrap dropped (the no-audio input's error is not it); the
// checks not run (the rejecting check's output is renamed into place).
func TestExtractAudioCopyKeepsOnlyTheAudio(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	fixture := func(name string, args ...string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		argv := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
		if b, err := exec.Command(ffmpegPath, append(argv, p)...).CombinedOutput(); err != nil {
			t.Fatalf("fixture %s: %v\n%s", name, err, b)
		}
		return p
	}
	clip := fixture("clip.mp4", "-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=10",
		"-f", "lavfi", "-i", "sine=duration=1", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest")
	silent := fixture("silent.mp4", "-f", "lavfi", "-i", "testsrc=duration=1:size=128x72:rate=10",
		"-c:v", "libx264", "-pix_fmt", "yuv420p")
	streams := func(path string) string {
		t.Helper()
		b, err := exec.Command(ffprobePath, "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatalf("ffprobe %s: %v", path, err)
		}
		return strings.Join(strings.Fields(string(b)), " ")
	}

	m := NewMuxer(ffmpegPath, &testLogger{})
	out := filepath.Join(dir, "audio.m4a")
	var checked string
	check := func(_ context.Context, path string) error { checked = path; return nil }
	if err := m.ExtractAudioCopy(t.Context(), clip, out, check); err != nil {
		t.Fatalf("ExtractAudioCopy: %v", err)
	}
	if got := streams(out); got != "aac,audio" {
		t.Errorf("output streams = %q, want the AAC audio alone", got)
	}
	packets := func(path string) string {
		t.Helper()
		b, err := exec.Command(ffprobePath, "-v", "error", "-select_streams", "a", "-show_entries", "packet=size", "-of", "csv=p=0", path).Output()
		if err != nil {
			t.Fatalf("ffprobe %s: %v", path, err)
		}
		return strings.Join(strings.Fields(string(b)), " ")
	}
	if in, got := packets(clip), packets(out); in == "" || got != in {
		t.Errorf("audio packets = %q, want the input's own %q (a stream copy)", got, in)
	}
	if checked == "" || checked == out || filepath.Dir(checked) != dir {
		t.Errorf("the check saw %q, want the temporary name beside %s", checked, out)
	}

	noAudio := filepath.Join(dir, "none.m4a")
	err = m.ExtractAudioCopy(t.Context(), silent, noAudio)
	if !errors.Is(err, ErrNoAudioTrack) {
		t.Errorf("no-audio input: error = %v, want ErrNoAudioTrack", err)
	}
	if _, statErr := os.Stat(noAudio); !os.IsNotExist(statErr) {
		t.Errorf("no-audio input left %s: %v", noAudio, statErr)
	}

	rejected := filepath.Join(dir, "rejected.m4a")
	refuse := errors.New("refused")
	if err := m.ExtractAudioCopy(t.Context(), clip, rejected, func(context.Context, string) error { return refuse }); !errors.Is(err, refuse) {
		t.Errorf("a refusing check: error = %v, want it", err)
	}
	if _, statErr := os.Stat(rejected); !os.IsNotExist(statErr) {
		t.Errorf("a refused output took its name: %v", statErr)
	}
}
