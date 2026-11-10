package engine

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTrimSegmentArgsLeaveFaststartToTheConcat pins which FFmpeg run of a
// multi-segment trim moves the moov atom to the front. The per-segment
// intermediates (seg_N.mp4) are read once by the concat demuxer and deleted,
// so `-movflags faststart` on them only rewrote every intermediate a second
// time for nothing. The concat writes the clip the user keeps, and it keeps
// the flag.
//
// Mutant: buildTrimSegmentArgs ending in `-movflags faststart` again.
func TestTrimSegmentArgsLeaveFaststartToTheConcat(t *testing.T) {
	m := NewMuxer("ffmpeg", &testLogger{})
	seg := TrimSegmentInput{InputPath: "in.mp4", StartTime: 1.5, Duration: 30, NeedScale: true}

	got := m.buildTrimSegmentArgs(seg, "seg_0.mp4", 1920, 1080, 30, 18, 128)
	want := []string{"-y", "-ss", "1.500", "-i", "in.mp4", "-t", "30.000",
		"-c:v", "libx264", "-crf", "18", "-preset", "slow", "-vf", "scale=1920:1080,fps=30",
		"-c:a", "aac", "-b:a", "128k", "seg_0.mp4"}
	if !slices.Equal(got, want) {
		t.Errorf("trim segment argv:\n got %v\nwant %v", got, want)
	}

	concat := m.buildConcatArgs("concat.txt", "out.mp4")
	wantConcat := []string{"-y", "-f", "concat", "-safe", "0", "-i", "concat.txt",
		"-c", "copy", "-movflags", "faststart", "out.mp4"}
	if !slices.Equal(concat, wantConcat) {
		t.Errorf("concat argv:\n got %v\nwant %v", concat, wantConcat)
	}
}

// TestMultiSegmentTrimOutputIsFaststart runs a real two-segment trim and
// checks the clip it leaves: both streams present, the full length, and the
// moov atom ahead of the media data — the concat's faststart, which is the
// only one the trim now runs. Skipped where FFmpeg is not installed.
//
// Mutant: buildConcatArgs without `-movflags faststart` (moov lands after
// mdat).
func TestMultiSegmentTrimOutputIsFaststart(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH, skipping live-tool test")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH, skipping live-tool test")
	}
	dir := t.TempDir()
	var segs []TrimSegmentInput
	for i, name := range []string{"a.mp4", "b.mp4"} {
		in := filepath.Join(dir, name)
		cmd := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc=duration=3:size=128x72:rate=30",
			"-f", "lavfi", "-i", "sine=f=440:d=3",
			"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", in)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("this FFmpeg cannot make the fixture: %v\n%s", err, b)
		}
		segs = append(segs, TrimSegmentInput{InputPath: in, StartTime: float64(i), Duration: 2})
	}

	out := filepath.Join(dir, "clip.mp4")
	m := NewMuxer(ffmpegPath, &testLogger{})
	if err := m.TrimAndConcatWithProgress(context.Background(), segs, out, 128, 72, 30, 23, 96, func(float64) {}); err != nil {
		t.Fatalf("TrimAndConcatWithProgress: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	moov, mdat := bytes.Index(data, []byte("moov")), bytes.Index(data, []byte("mdat"))
	if moov < 0 || mdat < 0 || moov > mdat {
		t.Errorf("moov at %d, mdat at %d: want the moov atom ahead of the media data", moov, mdat)
	}

	probe, err := exec.Command(ffprobePath, "-v", "error",
		"-show_entries", "stream=codec_name,nb_frames:format=duration",
		"-of", "default=noprint_wrappers=1", out).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	text := string(probe)
	for _, want := range []string{"codec_name=h264", "codec_name=aac", "nb_frames=120"} {
		if !strings.Contains(text, want) {
			t.Errorf("ffprobe output lacks %q:\n%s", want, text)
		}
	}

	// MOOMBOX_TRIM_HASH_OUT names a file to write the clip's per-stream
	// packet hashes to, for comparing two builds by hand.
	if dst := os.Getenv("MOOMBOX_TRIM_HASH_OUT"); dst != "" {
		hash, err := exec.Command(ffmpegPath, "-v", "error", "-i", out, "-map", "0", "-c", "copy",
			"-f", "streamhash", "-hash", "md5", "-").Output()
		if err != nil {
			t.Fatalf("streamhash: %v", err)
		}
		if err := os.WriteFile(dst, append(hash, probe...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
